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

// Artifact is one registered record. Repository and digest form its identity; the tag is the
// label it was registered with and never changes afterwards.
type Artifact struct {
	Repository        string
	Digest            string
	Tag               string
	SignatureVerified bool
	RetentionDays     int64
	SizeBytes         int64
	PushedAt          string
}

var (
	// ErrArtifactConflict reports that the repository and digest already exist with different content.
	ErrArtifactConflict = errors.New("store: artifact conflicts with existing record")
	// ErrArtifactNotFound reports that no record matches the query.
	ErrArtifactNotFound = errors.New("store: artifact not found")
)

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

// RegisterArtifact stores a new artifact and moves its tag to point at it, atomically. If the
// repository and digest already exist with identical content the stored record is returned
// unchanged (and the current tag pointer is left alone); different content yields
// ErrArtifactConflict.
func (s *Store) RegisterArtifact(a Artifact) (Artifact, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Artifact{}, fmt.Errorf("begin register: %w", err)
	}
	defer tx.Rollback()

	existing, err := scanArtifact(tx.QueryRow(
		"SELECT "+artifactColumns+" FROM artifacts WHERE repository = ? AND digest = ?",
		a.Repository, a.Digest,
	))
	switch {
	case err == nil:
		if existing.Tag == a.Tag &&
			existing.SignatureVerified == a.SignatureVerified &&
			existing.RetentionDays == a.RetentionDays &&
			existing.SizeBytes == a.SizeBytes {
			return existing, nil
		}
		return Artifact{}, ErrArtifactConflict
	case !errors.Is(err, sql.ErrNoRows):
		return Artifact{}, fmt.Errorf("lookup artifact: %w", err)
	}

	if _, err := tx.Exec(
		"INSERT INTO artifacts ("+artifactColumns+") VALUES (?, ?, ?, ?, ?, ?, ?)",
		a.Repository, a.Digest, a.Tag, boolToInt(a.SignatureVerified), a.RetentionDays, a.SizeBytes, a.PushedAt,
	); err != nil {
		return Artifact{}, fmt.Errorf("insert artifact: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO tag_pointers (repository, tag, digest) VALUES (?, ?, ?)
		 ON CONFLICT (repository, tag) DO UPDATE SET digest = excluded.digest`,
		a.Repository, a.Tag, a.Digest,
	); err != nil {
		return Artifact{}, fmt.Errorf("move tag pointer: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Artifact{}, fmt.Errorf("commit register: %w", err)
	}
	return a, nil
}

// ListArtifacts returns every record for a repository, ordered by first registration.
func (s *Store) ListArtifacts(repository string) ([]Artifact, error) {
	rows, err := s.db.Query(
		"SELECT "+artifactColumns+" FROM artifacts WHERE repository = ? ORDER BY id ASC",
		repository,
	)
	if err != nil {
		return nil, fmt.Errorf("list artifacts: %w", err)
	}
	defer rows.Close()

	var artifacts []Artifact
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, fmt.Errorf("scan artifact: %w", err)
		}
		artifacts = append(artifacts, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate artifacts: %w", err)
	}
	return artifacts, nil
}

// GetArtifactByTag returns the record the tag currently points at.
func (s *Store) GetArtifactByTag(repository, tag string) (Artifact, error) {
	a, err := scanArtifact(s.db.QueryRow(
		`SELECT a.repository, a.digest, a.tag, a.signature_verified, a.retention_days, a.size_bytes, a.pushed_at
		 FROM artifacts a
		 JOIN tag_pointers p ON p.repository = a.repository AND p.digest = a.digest
		 WHERE p.repository = ? AND p.tag = ?`,
		repository, tag,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return Artifact{}, ErrArtifactNotFound
	}
	if err != nil {
		return Artifact{}, fmt.Errorf("get artifact by tag: %w", err)
	}
	return a, nil
}

// GetArtifactByDigest returns the record with this exact repository and digest.
func (s *Store) GetArtifactByDigest(repository, digest string) (Artifact, error) {
	a, err := scanArtifact(s.db.QueryRow(
		"SELECT "+artifactColumns+" FROM artifacts WHERE repository = ? AND digest = ?",
		repository, digest,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return Artifact{}, ErrArtifactNotFound
	}
	if err != nil {
		return Artifact{}, fmt.Errorf("get artifact by digest: %w", err)
	}
	return a, nil
}

const artifactColumns = "repository, digest, tag, signature_verified, retention_days, size_bytes, pushed_at"

type scanner interface {
	Scan(dest ...any) error
}

func scanArtifact(row scanner) (Artifact, error) {
	var a Artifact
	var verified int
	if err := row.Scan(&a.Repository, &a.Digest, &a.Tag, &verified, &a.RetentionDays, &a.SizeBytes, &a.PushedAt); err != nil {
		return Artifact{}, err
	}
	a.SignatureVerified = verified != 0
	return a, nil
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
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
