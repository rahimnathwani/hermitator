package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
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

	tools, err := sandbox.InstallTools(cfg.ToolsDir(), cfg.Guest, guestfiles.FS)
	if err != nil {
		return fmt.Errorf("install guest tools from %s: %w", cfg.Guest, err)
	}

	var rt sandbox.Runtime
	switch cfg.Runtime {
	case "runsc":
		network := &sandbox.Network{Bridge: cfg.Bridge, Subnet: cfg.Subnet}
		if err := network.Setup(ctx); err != nil {
			return fmt.Errorf("network: %w", err)
		}
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
	}, st, box, rt, log)
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
