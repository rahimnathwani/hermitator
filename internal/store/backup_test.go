package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestBackupFileCopiesUncheckpointedWrites(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(ctx, filepath.Join(dir, "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// While s is open these rows sit in the WAL file, not the main one,
	// which is what copying the file would miss.
	u := mkUser(t, s, "someone@example.com")

	dest := filepath.Join(dir, "copy.db")
	if err := BackupFile(ctx, filepath.Join(dir, "live.db"), dest); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dest); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode %v, want 0600", fi.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(dir, ".copy.db.partial")); !os.IsNotExist(err) {
		t.Fatalf("partial file left behind: %v", err)
	}

	c, err := Open(ctx, dest)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	got, err := c.UserByEmail(ctx, "someone@example.com")
	if err != nil {
		t.Fatalf("user missing from the backup: %v", err)
	}
	if got.ID != u.ID {
		t.Fatalf("backup has user %s, want %s", got.ID, u.ID)
	}
}

func TestBackupFileRefusesToOverwrite(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "src.db")
	if err := BackupFile(ctx, src, filepath.Join(dir, "x.db")); err == nil {
		t.Fatal("backed up a database that does not exist")
	}
	live, err := Open(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	live.Close()
	dest := filepath.Join(dir, "dest.db")
	if err := os.WriteFile(dest, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := BackupFile(ctx, src, dest); err == nil {
		t.Fatal("overwrote an existing file")
	}
	if b, _ := os.ReadFile(dest); string(b) != "keep me" {
		t.Fatalf("existing file changed to %q", b)
	}
}
