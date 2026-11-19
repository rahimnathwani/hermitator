package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/rahimnathwani/hermitator/internal/config"
	"github.com/rahimnathwani/hermitator/internal/store"
)

func TestBackupWritesStateThatCannotBeRebuilt(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{DataDir: t.TempDir()}
	for _, p := range []string{cfg.DBPath(), cfg.WhatsAppDB()} {
		s, err := store.Open(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
	}
	if err := os.WriteFile(filepath.Join(cfg.DataDir, "egress-ca.key"), []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.DataDir, "egress-ca.crt"), []byte("crt"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "backups")
	now := time.Date(2026, 11, 19, 3, 4, 5, 0, time.UTC)
	dest, files, err := backup(ctx, cfg, out, now)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(out, "hermitator-20261119T030405Z"); dest != want {
		t.Fatalf("backup in %s, want %s", dest, want)
	}
	want := []string{"hermitator.db", "whatsapp.db", "egress-ca.crt", "egress-ca.key"}
	if !slices.Equal(files, want) {
		t.Fatalf("files %v, want %v", files, want)
	}
	for _, f := range want {
		fi, err := os.Stat(filepath.Join(dest, f))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s has mode %v, want 0600", f, fi.Mode().Perm())
		}
	}
	if fi, _ := os.Stat(dest); fi.Mode().Perm() != 0o700 {
		t.Errorf("backup directory has mode %v, want 0700", fi.Mode().Perm())
	}

	// A second backup in the same second must not write into the first.
	if _, _, err := backup(ctx, cfg, out, now); err == nil {
		t.Fatal("second backup reused the first one's directory")
	}
}

func TestBackupWithoutWhatsAppOrCA(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{DataDir: t.TempDir()}
	s, err := store.Open(ctx, cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	_, files, err := backup(ctx, cfg, t.TempDir(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(files, []string{"hermitator.db"}) {
		t.Fatalf("files %v, want only hermitator.db", files)
	}
}
