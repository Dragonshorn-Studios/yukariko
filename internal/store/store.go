// Package store provides Yukariko's durable state: crash-safe SQLite
// persistence for observed versions, deployment history, events, health,
// reporting queues, and replay protection.
//
// Design invariants:
//
//   - The deployed version advances only inside the same transaction that
//     confirms a deployment succeeded; failures and cancellations can never
//     advance it.
//   - Database errors are always returned to the caller and never treated as
//     success by higher layers.
//   - Migrations are forward-only and applied exactly once.
//   - No secret values are ever passed to or stored by this package.
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store is a handle to one SQLite database under the data directory.
type Store struct {
	db *sql.DB
}

// DBFileName is the SQLite database file Yukariko creates inside the data
// directory (WAL sidecars append -wal/-shm to this name).
const DBFileName = "yukariko.db"

// Open opens (creating if needed) the Yukariko database in dataDir and
// applies pending migrations. WAL mode is enabled deliberately so readers
// (status API, dashboard) never block while the scheduler records events.
func Open(dataDir string) (*Store, error) {
	path := filepath.ToSlash(filepath.Join(dataDir, DBFileName))
	// busy_timeout keeps concurrent writers from failing fast under
	// contention; synchronous=NORMAL is the standard WAL durability trade.
	dsn := "file:" + escapeDSNPath(path) +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database handle.
func (s *Store) Close() error {
	return s.db.Close()
}

// migrate applies pending forward-only migrations inside one transaction per
// migration, guarded by the schema_migrations table.
func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	for _, m := range migrations {
		var done bool
		if err := s.db.QueryRow(
			`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = ?)`, m.version,
		).Scan(&done); err != nil {
			return fmt.Errorf("check migration %d: %w", m.version, err)
		}
		if done {
			continue
		}
		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", m.version, err)
		}
		if _, err := tx.Exec(m.ddl); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %d: %w", m.version, err)
		}
		if _, err := tx.Exec(
			`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			m.version, time.Now().UTC().Format(time.RFC3339Nano),
		); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %d: %w", m.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", m.version, err)
		}
	}
	return nil
}

// Version returns the highest applied schema version.
func (s *Store) Version() (int, error) {
	var v sql.NullInt64
	if err := s.db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return int(v.Int64), nil
}

// newID returns a random 128-bit identifier formatted as hex. It carries no
// meaning and never encodes secrets or hostnames.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("store: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// rfc3339 formats timestamps deterministically for storage and ordering.
func rfc3339(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// escapeDSNPath percent-escapes characters that would terminate or corrupt a
// SQLite file: URI (space, '?', '#', '%', and the query separator '&'),
// leaving ':' (drive letters) and '/' untouched.
func escapeDSNPath(path string) string {
	replacer := strings.NewReplacer(
		"%", "%25",
		"?", "%3F",
		"#", "%23",
		" ", "%20",
		"&", "%26",
	)
	return replacer.Replace(path)
}
