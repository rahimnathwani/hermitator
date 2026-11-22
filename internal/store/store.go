// Package store is hermitator's control-plane database.
//
// It is SQLite in WAL mode. The SQL sticks to the subset Postgres also
// accepts (apart from placeholder syntax), and timestamps are fixed-width
// UTC strings so ordering by them is ordering by time.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

// ErrNotFound is returned when a lookup matches no row.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned when a write violates a uniqueness constraint.
var ErrConflict = errors.New("already exists")

// Store wraps the control-plane database.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database at path and applies any
// pending migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	q := url.Values{}
	for _, p := range []string{"foreign_keys(1)", "journal_mode(WAL)", "busy_timeout(10000)", "synchronous(NORMAL)"} {
		q.Add("_pragma", p)
	}
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for backups and tests.
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) migrate(ctx context.Context) error { return s.migrateTo(ctx, math.MaxInt) }

// migrateTo applies pending migrations up to version last; tests use it
// to put data in an old schema and watch a migration move it.
func (s *Store) migrateTo(ctx context.Context, last int) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return err
	}
	var current int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return err
	}
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		version, err := strconv.Atoi(strings.SplitN(e.Name(), "_", 2)[0])
		if err != nil {
			return fmt.Errorf("bad migration name %q", e.Name())
		}
		if version <= current {
			continue
		}
		if version > last {
			break
		}
		body, err := migrations.ReadFile("migrations/" + e.Name())
		if err != nil {
			return err
		}
		if err := s.apply(ctx, version, e.Name(), string(body)); err != nil {
			return err
		}
	}
	return nil
}

// noForeignKeys marks a migration that rebuilds a table other tables refer
// to, which must run with foreign keys off.
const noForeignKeys = "-- hermitator: foreign keys off"

// apply runs one migration in a transaction of its own connection. One
// marked noForeignKeys runs with foreign keys off, as SQLite's table
// rebuild needs: they cannot be turned off inside a transaction, and with
// them on, dropping the old table cascades into every table that refers
// to it. They are checked before the migration commits, and back on
// before the connection goes back to the pool.
func (s *Store) apply(ctx context.Context, version int, name, body string) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	off := strings.HasPrefix(body, noForeignKeys)
	if off {
		if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
			return err
		}
		defer conn.ExecContext(context.WithoutCancel(ctx), `PRAGMA foreign_keys = ON`)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, body); err != nil {
		tx.Rollback()
		return fmt.Errorf("%s: %w", name, err)
	}
	if off {
		var problems int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&problems); err != nil || problems > 0 {
			tx.Rollback()
			return fmt.Errorf("%s: %d foreign key problems (%v)", name, problems, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, version, ts(time.Now())); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

const timeFormat = "2006-01-02T15:04:05.000000Z"

func ts(t time.Time) string { return t.UTC().Format(timeFormat) }

func parseTS(s string) time.Time {
	t, _ := time.Parse(timeFormat, s)
	return t
}

func nullTS(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return ts(t)
}

func parseNullTS(ns sql.NullString) time.Time {
	if !ns.Valid {
		return time.Time{}
	}
	return parseTS(ns.String)
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// one maps sql.ErrNoRows to ErrNotFound.
func one(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func affected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
