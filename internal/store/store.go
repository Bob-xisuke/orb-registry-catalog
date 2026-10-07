// Package store is the SQLite-backed adapter for the service.Store port. It
// owns the SQLite file and every write the service performs, but defines no
// business data types of its own: records, register statuses and the
// not-found sentinel all come from the service storage contract.
package store

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/Bob-xisuke/orb-registry-catalog/internal/service"

	_ "modernc.org/sqlite"
)

// rowScanner is the part of *sql.Row and *sql.Rows that turns the current
// record into a service.Record. It lets every read share one field map.
type rowScanner interface {
	Scan(dest ...any) error
}

// Compile-time assertion that *Store is a service.Store.
var _ service.Store = (*Store)(nil)

// Store wraps the SQLite handle so callers never touch database/sql directly.
type Store struct {
	db *sql.DB
}

// Open prepares the database file and the schema this service needs.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable wal: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Ping reports whether the storage layer is usable.
func (s *Store) Ping() error { return s.db.Ping() }

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// recordColumns lists the persisted fields in INSERT order; it stays
// unqualified because the INSERT statement names artifacts without an alias.
const recordColumns = `repository, digest, tag, signature_verified, retention_days, size_bytes, pushed_at`

// recordSelectColumns is the single field map every read projects. It is
// qualified by the artifacts alias a, so it works identically for a direct
// lookup and for the tag-pointer join, and every read — the register-time
// identity lookup, the repository list, the digest lookup and the tag
// lookup — scans it through the one scanRecord rule below.
const recordSelectColumns = `a.repository, a.digest, a.tag, a.signature_verified, a.retention_days, a.size_bytes, a.pushed_at`

// Register writes a new artifact and moves its tag pointer in a single
// transaction, so readers only ever see both changes or neither. Re-submitting
// identical content returns the stored record without moving the tag pointer;
// different content for an existing (repository, digest) is a conflict.
func (s *Store) Register(a service.Record) (service.Record, service.RegisterStatus, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return service.Record{}, 0, fmt.Errorf("begin register: %w", err)
	}
	defer tx.Rollback()

	existing, err := scanRecord(tx.QueryRow(
		`SELECT `+recordSelectColumns+` FROM artifacts a WHERE a.repository = ? AND a.digest = ?`,
		a.Repository, a.Digest))
	switch {
	case errors.Is(err, service.ErrRecordNotFound):
		if _, err := tx.Exec(
			`INSERT INTO artifacts (`+recordColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			a.Repository, a.Digest, a.Tag, a.SignatureVerified,
			a.RetentionDays, a.SizeBytes, a.PushedAt); err != nil {
			return service.Record{}, 0, fmt.Errorf("insert artifact: %w", err)
		}
		if _, err := tx.Exec(
			`INSERT INTO tag_pointers (repository, tag, digest) VALUES (?, ?, ?)
			 ON CONFLICT (repository, tag) DO UPDATE SET digest = excluded.digest`,
			a.Repository, a.Tag, a.Digest); err != nil {
			return service.Record{}, 0, fmt.Errorf("move tag pointer: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return service.Record{}, 0, fmt.Errorf("commit register: %w", err)
		}
		return a, service.StatusCreated, nil
	case err != nil:
		return service.Record{}, 0, fmt.Errorf("lookup artifact: %w", err)
	}

	if existing.Tag == a.Tag &&
		existing.SignatureVerified == a.SignatureVerified &&
		existing.RetentionDays == a.RetentionDays &&
		existing.SizeBytes == a.SizeBytes {
		return existing, service.StatusDuplicate, nil
	}
	return service.Record{}, service.StatusConflict, nil
}

// ListRecords returns every record for a repository in first-registration order.
func (s *Store) ListRecords(repository string) ([]service.Record, error) {
	rows, err := s.db.Query(
		`SELECT `+recordSelectColumns+` FROM artifacts a WHERE a.repository = ? ORDER BY a.id`, repository)
	if err != nil {
		return nil, fmt.Errorf("list artifacts: %w", err)
	}
	defer rows.Close()

	var out []service.Record
	for rows.Next() {
		a, err := scanRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("scan artifact: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list artifacts: %w", err)
	}
	return out, nil
}

// RecordByDigest returns the record for an exact (repository, digest) identity.
func (s *Store) RecordByDigest(repository, digest string) (service.Record, error) {
	return scanRecord(s.db.QueryRow(
		`SELECT `+recordSelectColumns+` FROM artifacts a WHERE a.repository = ? AND a.digest = ?`,
		repository, digest))
}

// RecordByTag resolves the current tag pointer to the record it references.
func (s *Store) RecordByTag(repository, tag string) (service.Record, error) {
	return scanRecord(s.db.QueryRow(
		`SELECT `+recordSelectColumns+`
		 FROM tag_pointers p
		 JOIN artifacts a ON a.repository = p.repository AND a.digest = p.digest
		 WHERE p.repository = ? AND p.tag = ?`, repository, tag))
}

// scanRecord turns one result row into a service.Record. It takes the
// rowScanner interface so both *sql.Row and *sql.Rows decode with the same
// field order recordSelectColumns projects; a backend miss and a backend
// fault stay distinct through ErrRecordNotFound versus the original error.
func scanRecord(row rowScanner) (service.Record, error) {
	var a service.Record
	if err := row.Scan(&a.Repository, &a.Digest, &a.Tag, &a.SignatureVerified,
		&a.RetentionDays, &a.SizeBytes, &a.PushedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return service.Record{}, service.ErrRecordNotFound
		}
		return service.Record{}, err
	}
	return a, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS service_metadata (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS artifacts (
	id                 INTEGER PRIMARY KEY AUTOINCREMENT,
	repository         TEXT NOT NULL,
	digest             TEXT NOT NULL,
	tag                TEXT NOT NULL,
	signature_verified INTEGER NOT NULL,
	retention_days     INTEGER NOT NULL,
	size_bytes         INTEGER NOT NULL,
	pushed_at          TEXT NOT NULL,
	UNIQUE (repository, digest)
);
CREATE TABLE IF NOT EXISTS tag_pointers (
	repository TEXT NOT NULL,
	tag        TEXT NOT NULL,
	digest     TEXT NOT NULL,
	PRIMARY KEY (repository, tag)
);
`
