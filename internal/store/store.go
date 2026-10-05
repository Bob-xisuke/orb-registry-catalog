// Package store owns the SQLite file and every write the service performs.
package store

import (
	"database/sql"
	"errors"
	"fmt"

	_ "modernc.org/sqlite"
)

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

// Artifact is a single registered record. PushedAt is assigned by the service at
// first registration and never changes afterwards.
type Artifact struct {
	Repository        string
	Digest            string
	Tag               string
	SignatureVerified bool
	RetentionDays     int64
	SizeBytes         int64
	PushedAt          string
}

// ErrNotFound reports that a lookup matched no record.
var ErrNotFound = errors.New("store: no matching record")

// RegisterOutcome describes how a RegisterArtifact call resolved.
type RegisterOutcome int

const (
	// RegisterCreated means the record was newly written and its tag pointer moved.
	RegisterCreated RegisterOutcome = iota
	// RegisterDuplicate means identical content already existed; nothing was written.
	RegisterDuplicate
	// RegisterConflict means the (repository, digest) identity exists with different content.
	RegisterConflict
)

const artifactColumns = `repository, digest, tag, signature_verified, retention_days, size_bytes, pushed_at`

// RegisterArtifact writes a new artifact and moves its tag pointer in a single
// transaction, so readers only ever see both changes or neither. Re-submitting
// identical content returns the stored record without moving the tag pointer;
// different content for an existing (repository, digest) is a conflict.
func (s *Store) RegisterArtifact(a Artifact) (Artifact, RegisterOutcome, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Artifact{}, 0, fmt.Errorf("begin register: %w", err)
	}
	defer tx.Rollback()

	existing, err := scanArtifact(tx.QueryRow(
		`SELECT `+artifactColumns+` FROM artifacts WHERE repository = ? AND digest = ?`,
		a.Repository, a.Digest))
	switch {
	case errors.Is(err, ErrNotFound):
		if _, err := tx.Exec(
			`INSERT INTO artifacts (`+artifactColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			a.Repository, a.Digest, a.Tag, a.SignatureVerified,
			a.RetentionDays, a.SizeBytes, a.PushedAt); err != nil {
			return Artifact{}, 0, fmt.Errorf("insert artifact: %w", err)
		}
		if _, err := tx.Exec(
			`INSERT INTO tag_pointers (repository, tag, digest) VALUES (?, ?, ?)
			 ON CONFLICT (repository, tag) DO UPDATE SET digest = excluded.digest`,
			a.Repository, a.Tag, a.Digest); err != nil {
			return Artifact{}, 0, fmt.Errorf("move tag pointer: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return Artifact{}, 0, fmt.Errorf("commit register: %w", err)
		}
		return a, RegisterCreated, nil
	case err != nil:
		return Artifact{}, 0, fmt.Errorf("lookup artifact: %w", err)
	}

	if existing.Tag == a.Tag &&
		existing.SignatureVerified == a.SignatureVerified &&
		existing.RetentionDays == a.RetentionDays &&
		existing.SizeBytes == a.SizeBytes {
		return existing, RegisterDuplicate, nil
	}
	return Artifact{}, RegisterConflict, nil
}

// ListArtifacts returns every record for a repository in first-registration order.
func (s *Store) ListArtifacts(repository string) ([]Artifact, error) {
	rows, err := s.db.Query(
		`SELECT `+artifactColumns+` FROM artifacts WHERE repository = ? ORDER BY id`, repository)
	if err != nil {
		return nil, fmt.Errorf("list artifacts: %w", err)
	}
	defer rows.Close()

	var out []Artifact
	for rows.Next() {
		var a Artifact
		if err := rows.Scan(&a.Repository, &a.Digest, &a.Tag, &a.SignatureVerified,
			&a.RetentionDays, &a.SizeBytes, &a.PushedAt); err != nil {
			return nil, fmt.Errorf("scan artifact: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list artifacts: %w", err)
	}
	return out, nil
}

// ArtifactByDigest returns the record for an exact (repository, digest) identity.
func (s *Store) ArtifactByDigest(repository, digest string) (Artifact, error) {
	return scanArtifact(s.db.QueryRow(
		`SELECT `+artifactColumns+` FROM artifacts WHERE repository = ? AND digest = ?`,
		repository, digest))
}

// ArtifactByTag resolves the current tag pointer to the record it references.
func (s *Store) ArtifactByTag(repository, tag string) (Artifact, error) {
	return scanArtifact(s.db.QueryRow(
		`SELECT a.repository, a.digest, a.tag, a.signature_verified,
		        a.retention_days, a.size_bytes, a.pushed_at
		 FROM tag_pointers p
		 JOIN artifacts a ON a.repository = p.repository AND a.digest = p.digest
		 WHERE p.repository = ? AND p.tag = ?`, repository, tag))
}

func scanArtifact(row *sql.Row) (Artifact, error) {
	var a Artifact
	if err := row.Scan(&a.Repository, &a.Digest, &a.Tag, &a.SignatureVerified,
		&a.RetentionDays, &a.SizeBytes, &a.PushedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Artifact{}, ErrNotFound
		}
		return Artifact{}, err
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
