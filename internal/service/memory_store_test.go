package service_test

import (
	"errors"
	"fmt"
	"sync"

	"github.com/Bob-xisuke/orb-registry-catalog/internal/service"
)

// errMemoryMiss and errMemoryFault belong to this alternative backend: the
// business layer never sees store-package types or constants. A miss is
// reported by joining the backend's own sentinel with the port's
// service.ErrNotFound, so errors.Is keeps working for both the backend and
// the business layer.
var (
	errMemoryMiss  = errors.New("memory store: no matching record")
	errMemoryFault = errors.New("memory store: simulated backend failure")
)

// memoryStore is a from-scratch service.Store implementation. It exists to
// prove the business layer runs against any conforming backend without
// opening SQLite: it imports only the service package and honors the same
// contract — identity (repository, digest), record insert plus tag-pointer
// move as one logical commit, first-registration ordering, identical-retry
// detection and wrapped ErrNotFound misses.
type memoryStore struct {
	mu       sync.Mutex
	records  []service.Artifact
	pointers map[[2]string]string
	fault    error
	// captured is the last record handed to RegisterArtifact, letting tests
	// confirm the service (not the backend) generates pushed_at.
	captured service.Artifact
}

func newMemoryStore() *memoryStore {
	return &memoryStore{pointers: map[[2]string]string{}}
}

// fail injects a backend fault; every operation then returns it.
func (m *memoryStore) fail(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fault = err
}

func missError() error { return errors.Join(errMemoryMiss, service.ErrNotFound) }

func (m *memoryStore) RegisterArtifact(in service.Artifact) (service.Artifact, service.StoreOutcome, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.captured = in
	if m.fault != nil {
		return service.Artifact{}, 0, m.fault
	}
	if existing, ok := m.findLocked(in.Repository, in.Digest); ok {
		if existing.Tag == in.Tag &&
			existing.SignatureVerified == in.SignatureVerified &&
			existing.RetentionDays == in.RetentionDays &&
			existing.SizeBytes == in.SizeBytes {
			return existing, service.StoreDuplicate, nil
		}
		return service.Artifact{}, service.StoreConflict, nil
	}
	m.records = append(m.records, in)
	m.pointers[[2]string{in.Repository, in.Tag}] = in.Digest
	return in, service.StoreCreated, nil
}

func (m *memoryStore) ListArtifacts(repository string) ([]service.Artifact, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fault != nil {
		return nil, m.fault
	}
	var out []service.Artifact
	for _, record := range m.records {
		if record.Repository == repository {
			out = append(out, record)
		}
	}
	if len(out) == 0 {
		return nil, missError()
	}
	return out, nil
}

func (m *memoryStore) ArtifactByDigest(repository, digest string) (service.Artifact, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fault != nil {
		return service.Artifact{}, m.fault
	}
	record, ok := m.findLocked(repository, digest)
	if !ok {
		return service.Artifact{}, missError()
	}
	return record, nil
}

func (m *memoryStore) ArtifactByTag(repository, tag string) (service.Artifact, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fault != nil {
		return service.Artifact{}, m.fault
	}
	digest, ok := m.pointers[[2]string{repository, tag}]
	if !ok {
		return service.Artifact{}, missError()
	}
	record, ok := m.findLocked(repository, digest)
	if !ok {
		return service.Artifact{}, fmt.Errorf("memory store: dangling tag pointer: %w", service.ErrNotFound)
	}
	return record, nil
}

func (m *memoryStore) findLocked(repository, digest string) (service.Artifact, bool) {
	for _, record := range m.records {
		if record.Repository == repository && record.Digest == digest {
			return record, true
		}
	}
	return service.Artifact{}, false
}

// emptyListStore answers a repository list with an empty slice and a nil
// error, exercising the service's defensive rule that an empty result is a
// miss even when a backend does not report one itself.
type emptyListStore struct{}

func (emptyListStore) RegisterArtifact(service.Artifact) (service.Artifact, service.StoreOutcome, error) {
	return service.Artifact{}, 0, errors.New("emptyListStore: RegisterArtifact not expected")
}

func (emptyListStore) ListArtifacts(string) ([]service.Artifact, error) {
	return []service.Artifact{}, nil
}

func (emptyListStore) ArtifactByDigest(string, string) (service.Artifact, error) {
	return service.Artifact{}, missError()
}

func (emptyListStore) ArtifactByTag(string, string) (service.Artifact, error) {
	return service.Artifact{}, missError()
}

// Compile-time proof that both backends satisfy the published port.
var (
	_ service.Store = (*memoryStore)(nil)
	_ service.Store = emptyListStore{}
)
