package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/rahimnathwani/hermitator/internal/egress"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing/fstest"
	"time"

	"github.com/rahimnathwani/hermitator/internal/config"
	"github.com/rahimnathwani/hermitator/internal/gateway"
	"github.com/rahimnathwani/hermitator/internal/guestfiles"
	"github.com/rahimnathwani/hermitator/internal/media"
	"github.com/rahimnathwani/hermitator/internal/orchestrator"
	"github.com/rahimnathwani/hermitator/internal/sandbox"
	"github.com/rahimnathwani/hermitator/internal/secrets"
	"github.com/rahimnathwani/hermitator/internal/store"
	"github.com/rahimnathwani/hermitator/internal/web"
)

func logger(level string) *slog.Logger {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l}))
}

func serve() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := logger(cfg.LogLevel)
	slog.SetDefault(log)

	if cfg.MasterKey == "" {
		return errors.New("HERMITATOR_MASTER_KEY is not set; generate one with `hermitator keygen`")
	}
	key, err := secrets.ParseKey(cfg.MasterKey)
	if err != nil {
		return err
	}
	box, err := secrets.New(key)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	for _, d := range []string{cfg.DataDir, cfg.AgentsDir(), cfg.ImagesDir(), cfg.ToolsDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	st, err := store.Open(ctx, cfg.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()

	// The egress CA goes into the tools bundle, so sandboxes booted from
	// now on trust the egress proxy. It is created once and kept.
	ca, err := egress.LoadCA(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("egress CA: %w", err)
	}
	toolFiles, err := withTrust(guestfiles.FS, ca.PEM)
	if err != nil {
		return err
	}
	tools, err := sandbox.InstallTools(cfg.ToolsDir(), cfg.Guest, toolFiles)
	if err != nil {
		return fmt.Errorf("install guest tools from %s: %w", cfg.Guest, err)
	}
	var egressKey []byte

	var rt sandbox.Runtime
	switch cfg.Runtime {
	case "runsc":
		network := &sandbox.Network{Bridge: cfg.Bridge, Subnet: cfg.Subnet, EgressPort: egressPort}
		peers, err := knownPeers(ctx, st)
		if err != nil {
			return err
		}
		if err := network.Setup(ctx, peers...); err != nil {
			return fmt.Errorf("network: %w", err)
		}
		egressKey = derive(key, "egress placeholders")
		if total, err := sandbox.MemTotal(); err != nil {
			log.Warn("agents' shared limits not set", "err", err)
		} else if limit, err := sandbox.LimitAll("/sys/fs/cgroup", total); err != nil {
			log.Warn("agents' shared limits not set", "err", err)
		} else {
			log.Info("all agents together", "memory_max_gib", limit>>30, "cpu_weight", "half of the host's")
		}
		runsc := &sandbox.Runsc{Binary: cfg.Runsc, Root: cfg.RunscRoot, Platform: cfg.Platform, Network: network, Log: log}
		if err := runsc.Check(ctx); err != nil {
			return err
		}
		if err := os.MkdirAll(cfg.RunscRoot, 0o700); err != nil {
			return err
		}
		rt = runsc
	case "unsafe-local":
		log.Warn("HERMITATOR_RUNTIME=unsafe-local: agents run directly on this machine with no isolation")
		ensureLocalImage(cfg.ImagesDir())
		rt = &sandbox.Local{Log: log}
	}

	images := &sandbox.Images{Dir: cfg.ImagesDir()}
	if len(images.Families()) == 0 {
		log.Warn("no base images found; build one with deploy/build-image.sh", "dir", cfg.ImagesDir())
	}

	mgr := orchestrator.New(orchestrator.Config{
		AgentsDir: cfg.AgentsDir(),
		Images:    images,
		Tools:     tools,
		Subnet:    cfg.Subnet,
		PublicURL: cfg.PublicURL,
		EgressKey: egressKey,
	}, st, box, rt, log)
	if egressKey != nil {
		gateway := netip.PrefixFrom(cfg.Subnet.Masked().Addr().Next(), 0).Addr()
		l, err := net.Listen("tcp", netip.AddrPortFrom(gateway, egressPort).String())
		if err != nil {
			return fmt.Errorf("egress proxy: %w", err)
		}
		defer l.Close()
		proxy := &egress.Proxy{CA: ca, Resolve: mgr.EgressSecrets, Log: log}
		go proxy.ServeTransparent(ctx, l)
	}
	whisper := &media.Whisper{Binary: cfg.Whisper, Model: cfg.Model}
	if whisper.Available() {
		mgr.SetTranscriber(whisper)
	} else {
		log.Info("voice notes will not be transcribed (whisper-cli or model not found)", "model", cfg.Model)
	}

	gw, err := gateway.Open(ctx, cfg.WhatsAppDB(), st, log)
	if err != nil {
		return err
	}
	gw.SetHandler(mgr.HandleMessage)
	mgr.SetMessenger(gw)

	if err := mgr.Start(ctx); err != nil {
		return err
	}
	if err := gw.Start(ctx); err != nil {
		return err
	}

	srv := &web.Server{Store: st, Agents: mgr, Gateway: gw, Images: images, Log: log, PublicURL: cfg.PublicURL, Version: version}
	handler, err := srv.Handler()
	if err != nil {
		return err
	}
	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				st.PruneSessions(ctx)
			}
		}
	}()

	errc := make(chan error, 1)
	go func() {
		log.Info("hermitator listening", "addr", cfg.Listen, "url", cfg.PublicURL, "version", version, "runtime", cfg.Runtime)
		errc <- httpServer.ListenAndServe()
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down; running sandboxes keep running")
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	httpServer.Shutdown(shutdown)
	gw.Close()
	return nil
}

// ensureLocalImage gives the unsandboxed runtime an (empty) image to pin,
// since it runs agents on the host filesystem.
func ensureLocalImage(dir string) {
	if _, err := os.Lstat(filepath.Join(dir, "universal")); err == nil {
		return
	}
	os.MkdirAll(filepath.Join(dir, "universal-local", "rootfs", "bin"), 0o755)
	os.Symlink("universal-local", filepath.Join(dir, "universal"))
}

// egressPort is where the egress proxy listens on the bridge's gateway.
const egressPort = 3129

// knownPeers lists every agent's address, for the network's rules, and
// whether its owner routes it through the egress proxy.
func knownPeers(ctx context.Context, st *store.Store) ([]sandbox.Peer, error) {
	agents, err := st.Agents(ctx, "")
	if err != nil {
		return nil, err
	}
	egressOn := map[string]bool{}
	var peers []sandbox.Peer
	for _, a := range agents {
		ip, err := netip.ParseAddr(a.IP)
		if err != nil {
			continue
		}
		on, seen := egressOn[a.UserID]
		if !seen {
			if u, err := st.User(ctx, a.UserID); err == nil {
				on = u.Egress
			}
			egressOn[a.UserID] = on
		}
		peers = append(peers, sandbox.Peer{ID: a.ID, IP: ip, Egress: on})
	}
	return peers, nil
}

// withTrust adds the egress CA, and a bundle of the host's roots plus the
// CA, to the files the tools bundle carries.
func withTrust(files fs.FS, caPEM []byte) (fs.FS, error) {
	out := fstest.MapFS{}
	err := fs.WalkDir(files, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(files, path)
		out[path] = &fstest.MapFile{Data: b, Mode: 0o644}
		return err
	})
	if err != nil {
		return nil, err
	}
	out[sandbox.EgressCAFile] = &fstest.MapFile{Data: caPEM, Mode: 0o644}
	for _, roots := range []string{"/etc/ssl/certs/ca-certificates.crt", "/etc/ssl/cert.pem", "/etc/pki/tls/certs/ca-bundle.crt"} {
		if b, err := os.ReadFile(roots); err == nil {
			out[sandbox.CABundleFile] = &fstest.MapFile{Data: append(append(b, '\n'), caPEM...), Mode: 0o644}
			break
		}
	}
	return out, nil
}

// derive makes a key for one purpose from the master key.
func derive(key []byte, purpose string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(purpose))
	return mac.Sum(nil)
}
