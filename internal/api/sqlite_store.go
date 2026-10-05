package api

import (
	"errors"
	"fmt"

	"github.com/Bob-xisuke/orb-registry-catalog/internal/service"
	"github.com/Bob-xisuke/orb-registry-catalog/internal/store"
)

// sqliteStore adapts the SQLite-backed *store.Store to the business layer's
// service.Store port. It is the only production code that knows about the
// concrete engine: internal/service never imports internal/store, database/sql
// or a SQLite driver. The adapter translates records, outcomes and the miss
// sentinel; every other storage error passes through unchanged so the service
// can classify it as a storage failure rather than a miss.
type sqliteStore struct {
	db *store.Store
}

// newSQLiteStore wraps the default SQLite backend as a service.Store.
func newSQLiteStore(st *store.Store) service.Store {
	return &sqliteStore{db: st}
}

func (s *sqliteStore) RegisterArtifact(record service.Artifact) (service.Artifact, service.StoreOutcome, error) {
	stored, outcome, err := s.db.RegisterArtifact(toStoreRecord(record))
	if err != nil {
		return service.Artifact{}, 0, err
	}
	return fromStoreRecord(stored), fromStoreOutcome(outcome), nil
}

func (s *sqliteStore) ListArtifacts(repository string) ([]service.Artifact, error) {
	stored, err := s.db.ListArtifacts(repository)
	if err != nil {
		return nil, err
	}
	out := make([]service.Artifact, len(stored))
	for i, record := range stored {
		out[i] = fromStoreRecord(record)
	}
	return out, nil
}

func (s *sqliteStore) ArtifactByDigest(repository, digest string) (service.Artifact, error) {
	stored, err := s.db.ArtifactByDigest(repository, digest)
	if err != nil {
		return service.Artifact{}, mapLookupError(err)
	}
	return fromStoreRecord(stored), nil
}

func (s *sqliteStore) ArtifactByTag(repository, tag string) (service.Artifact, error) {
	stored, err := s.db.ArtifactByTag(repository, tag)
	if err != nil {
		return service.Artifact{}, mapLookupError(err)
	}
	return fromStoreRecord(stored), nil
}

// mapLookupError translates the engine's miss sentinel into the business
// contract's miss sentinel while preserving errors.Is on the wrapped value.
// Every other error is returned as-is so a backend fault cannot be read as
// ErrNotFound.
func mapLookupError(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%w: %v", service.ErrNotFound, err)
	}
	return err
}

func toStoreRecord(a service.Artifact) store.Artifact {
	return store.Artifact{
		Repository:        a.Repository,
		Digest:            a.Digest,
		Tag:               a.Tag,
		SignatureVerified: a.SignatureVerified,
		RetentionDays:     a.RetentionDays,
		SizeBytes:         a.SizeBytes,
		PushedAt:          a.PushedAt,
	}
}

func fromStoreRecord(a store.Artifact) service.Artifact {
	return service.Artifact{
		Repository:        a.Repository,
		Digest:            a.Digest,
		Tag:               a.Tag,
		SignatureVerified: a.SignatureVerified,
		RetentionDays:     a.RetentionDays,
		SizeBytes:         a.SizeBytes,
		PushedAt:          a.PushedAt,
	}
}

func fromStoreOutcome(outcome store.RegisterOutcome) service.StoreOutcome {
	switch outcome {
	case store.RegisterDuplicate:
		return service.StoreDuplicate
	case store.RegisterConflict:
		return service.StoreConflict
	default:
		return service.StoreCreated
	}
}
