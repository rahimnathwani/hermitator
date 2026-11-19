package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
)

// BackupFile writes a consistent copy of the SQLite database at src to
// dest with VACUUM INTO, which reads one snapshot of src, so it is safe
// while hermitator is running and writing to it. Copying the file itself
// is not: with WAL, the newest writes are in a second file. dest must not
// exist; the copy appears there whole, or not at all.
func BackupFile(ctx context.Context, src, dest string) error {
	if _, err := os.Stat(src); err != nil {
		return err
	}
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("%s already exists", dest)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(10000)")
	db, err := sql.Open("sqlite", "file:"+src+"?"+q.Encode())
	if err != nil {
		return err
	}
	defer db.Close()
	tmp := filepath.Join(filepath.Dir(dest), "."+filepath.Base(dest)+".partial")
	os.Remove(tmp)
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, tmp); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("copy %s: %w", src, err)
	}
	// The copy holds password hashes and sealed credentials.
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}
