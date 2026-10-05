// SPDX-License-Identifier: AGPL-3.0-or-later

// Package db owns Suchi's local SQLite and optional remote libSQL bootstrap.
//
// The rule of the house: **one writer, many readers.** The write pool has one
// connection so Suchi serializes mutations before they reach either local
// SQLite or remote libSQL:
//
//   - DB.Write:  one connection; local SQLite uses BEGIN IMMEDIATE and
//     remote libSQL relies on the driver's transaction handling.
//     All writes go through this handle. It queues; it does not race.
//   - DB.Read:   MaxOpenConns=4 for concurrent readers.
//
// Local pools point at the same file and apply boot pragmas. Remote pools
// connect directly to the configured libSQL database. The local write pool
// additionally applies "PRAGMA foreign_keys=ON" per connection because
// SQLite scopes that pragma per-connection, not per-database.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	libsql "github.com/tursodatabase/libsql-client-go/libsql"
	_ "modernc.org/sqlite"
)

// DB is the pair of pools plus the file path. Close closes both.
type DB struct {
	Write  *sql.DB
	Read   *sql.DB
	Path   string
	Remote bool
}

// OpenOptions selects local SQLite (zero value) or direct remote libSQL.
type OpenOptions struct {
	TursoURL   string
	TursoToken string
}

// Open opens (or creates) the SQLite database at path and returns the
// two-pool handle. The caller is responsible for running migrations
// before serving traffic.
func Open(ctx context.Context, path string) (*DB, error) {
	return OpenWithOptions(ctx, path, OpenOptions{})
}

// OpenWithOptions opens the configured database and returns the two-pool
// handle. Remote mode uses Turso as the metadata authority; Path remains the
// local data directory's conventional database path for diagnostics only.
func OpenWithOptions(ctx context.Context, path string, opts OpenOptions) (*DB, error) {
	if opts.TursoURL != "" || opts.TursoToken != "" {
		if opts.TursoURL == "" || opts.TursoToken == "" {
			return nil, errors.New("TURSO_DATABASE_URL and TURSO_AUTH_TOKEN must both be set")
		}
		return openRemote(ctx, path, opts)
	}
	return openLocal(ctx, path)
}

func openLocal(ctx context.Context, path string) (*DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve db path: %w", err)
	}

	// Boot pragmas run in the DSN so the very first connection is
	// already correctly configured; nothing observes a pre-pragma window.
	//
	// _pragma=journal_mode(WAL) — many readers + one writer without blocking.
	// _pragma=synchronous(NORMAL) — durable across app crashes, faster than FULL,
	//   loses at most last-committed txn on OS crash. Standard WAL guidance.
	// _pragma=busy_timeout(5000) — 5s wait before SQLITE_BUSY. With the
	//   single-writer pool this is a belt to the suspenders.
	// _pragma=foreign_keys(ON) — the SQLite default is OFF; nothing about
	//   that default is a good idea.
	// _pragma=mmap_size(268435456) — 256MiB memory-mapped read window;
	//   improves read locality without touching the OS page cache accounting.
	//   Trade-off: on 32-bit builds this eats a big chunk of the 4GiB
	//   address space and can OOM the process. suchi targets 64-bit hosts
	//   (Go 1.25 + modernc/sqlite; the release matrix ships only amd64 +
	//   arm64). If someone cross-compiles for 32-bit, drop this to 64MiB
	//   or 0. mmap is a read-side optimization only — writes go through
	//   the normal page cache path, so lowering this does not affect the
	//   Write pool's single-writer discipline.
	// _pragma=temp_store(MEMORY) — temp tables/indexes stay in RAM.
	dsn := (&url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}).String() + "?" + url.Values{
		"_pragma": []string{
			"journal_mode(WAL)",
			"synchronous(NORMAL)",
			"busy_timeout(5000)",
			"foreign_keys(ON)",
			"mmap_size(268435456)",
			"temp_store(MEMORY)",
		},
	}.Encode()

	write, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open write pool: %w", err)
	}
	write.SetMaxOpenConns(1)
	write.SetMaxIdleConns(1)
	write.SetConnMaxLifetime(0) // never recycle the sole writer
	if err := ping(ctx, write); err != nil {
		_ = write.Close()
		return nil, fmt.Errorf("write pool ping: %w", err)
	}

	read, err := sql.Open("sqlite", dsn)
	if err != nil {
		_ = write.Close()
		return nil, fmt.Errorf("open read pool: %w", err)
	}
	read.SetMaxOpenConns(4)
	read.SetMaxIdleConns(4)
	read.SetConnMaxIdleTime(5 * time.Minute)
	if err := ping(ctx, read); err != nil {
		_ = write.Close()
		_ = read.Close()
		return nil, fmt.Errorf("read pool ping: %w", err)
	}

	return &DB{Write: write, Read: read, Path: abs}, nil
}

func openRemote(ctx context.Context, path string, opts OpenOptions) (*DB, error) {
	parsed, err := url.Parse(opts.TursoURL)
	if err != nil || parsed.Scheme != "libsql" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("TURSO_DATABASE_URL must be a libsql:// URL without credentials, query, or fragment")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve db path: %w", err)
	}
	newRemotePool := func(maxOpen int) (*sql.DB, error) {
		connector, err := libsql.NewConnector(opts.TursoURL, libsql.WithAuthToken(opts.TursoToken))
		if err != nil {
			return nil, errors.New("configure Turso connector")
		}
		db := sql.OpenDB(connector)
		db.SetMaxOpenConns(maxOpen)
		db.SetMaxIdleConns(maxOpen)
		db.SetConnMaxIdleTime(5 * time.Minute)
		if maxOpen == 1 {
			db.SetConnMaxLifetime(0)
		}
		if err := ping(ctx, db); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("Turso connection failed: %w", err)
		}
		return db, nil
	}
	write, err := newRemotePool(1)
	if err != nil {
		return nil, fmt.Errorf("open Turso write pool: %w", err)
	}
	read, err := newRemotePool(4)
	if err != nil {
		_ = write.Close()
		return nil, fmt.Errorf("open Turso read pool: %w", err)
	}
	return &DB{Write: write, Read: read, Path: abs, Remote: true}, nil
}

// Close closes both pools. Errors from either are joined.
func (d *DB) Close() error {
	return errors.Join(d.Write.Close(), d.Read.Close())
}

// WriteTx runs fn inside a transaction on the serialized write pool. Local
// SQLite begins with BEGIN IMMEDIATE to acquire its RESERVED lock up front;
// remote libSQL uses the driver's standard transaction API.
func (d *DB) WriteTx(ctx context.Context, fn func(*sql.Tx) error) error {
	var tx *sql.Tx
	var err error
	if d.Remote {
		tx, err = d.Write.BeginTx(ctx, nil)
	} else {
		tx, err = d.Write.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	}
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if !d.Remote {
		// modernc/sqlite honors LevelSerializable as BEGIN, not BEGIN IMMEDIATE.
		// Force it explicitly so our local SQLite discipline is preserved.
		if _, err := tx.ExecContext(ctx, "ROLLBACK; BEGIN IMMEDIATE"); err != nil {
			return fmt.Errorf("begin immediate: %w", err)
		}
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// ExecWrite executes one statement through the serialized writer.
func (d *DB) ExecWrite(ctx context.Context, query string, args ...any) (sql.Result, error) {
	var result sql.Result
	err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = tx.ExecContext(ctx, query, args...)
		return err
	})
	return result, err
}

func ping(ctx context.Context, d *sql.DB) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return d.PingContext(ctx)
}
