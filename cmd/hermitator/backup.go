package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/rahimnathwani/hermitator/internal/config"
	"github.com/rahimnathwani/hermitator/internal/store"
)

func backupCmd(args []string) error {
	if len(args) != 1 || args[0] == "" || args[0][0] == '-' {
		return errors.New("usage: hermitator backup DIR")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	dest, files, err := backup(context.Background(), cfg, args[0], time.Now())
	if err != nil {
		return err
	}
	fmt.Println(dest)
	for _, f := range files {
		fmt.Println("  " + f)
	}
	fmt.Println(`
The master key is not in it. Keep HERMITATOR_MASTER_KEY (from
` + config.DefaultEnvFile + `) somewhere else: without it, the
credentials in this backup cannot be read. Agents' files are not in it
either; they are under ` + cfg.AgentsDir() + `.`)
	return nil
}

// backup copies the state that cannot be rebuilt into a new directory
// under dir and returns it, with the names of the files it wrote. Images,
// the tools bundle and whisper's model are left out: install.sh and the
// image build make them again.
func backup(ctx context.Context, cfg *config.Config, dir string, now time.Time) (string, []string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, err
	}
	dest := filepath.Join(dir, "hermitator-"+now.UTC().Format("20060102T150405Z"))
	if err := os.Mkdir(dest, 0o700); err != nil {
		return "", nil, err
	}
	var files []string
	// Users, agents, chats, tasks and sealed credentials.
	if err := store.BackupFile(ctx, cfg.DBPath(), filepath.Join(dest, filepath.Base(cfg.DBPath()))); err != nil {
		return dest, files, err
	}
	files = append(files, filepath.Base(cfg.DBPath()))
	// WhatsApp's device keys: without them every number has to be linked
	// again from its phone.
	if _, err := os.Stat(cfg.WhatsAppDB()); err == nil {
		if err := store.BackupFile(ctx, cfg.WhatsAppDB(), filepath.Join(dest, filepath.Base(cfg.WhatsAppDB()))); err != nil {
			return dest, files, err
		}
		files = append(files, filepath.Base(cfg.WhatsAppDB()))
	}
	// The egress CA is in the tools bundle of every sandbox booted since
	// it was made; a new one would break egress for checkpointed ones.
	for _, name := range []string{"egress-ca.crt", "egress-ca.key"} {
		ok, err := copyIfExists(filepath.Join(cfg.DataDir, name), filepath.Join(dest, name))
		if err != nil {
			return dest, files, err
		}
		if ok {
			files = append(files, name)
		}
	}
	return dest, files, nil
}

func copyIfExists(src, dest string) (bool, error) {
	in, err := os.Open(src)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false, err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return false, err
	}
	return true, out.Close()
}
