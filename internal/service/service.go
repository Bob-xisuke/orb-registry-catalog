// Package service exposes the registration and query business logic without
// any HTTP or Gin dependency. Callers pass already validated and normalized
// input and receive typed results; failures are classified by the sentinel
// errors below so transports can map them without inspecting store details.
package service

import (
	"errors"
	"fmt"
	"time"

	"github.com/Bob-xisuke/orb-registry-catalog/internal/store"
)

var (
	// ErrConflict reports that the (repository, digest) identity already
	// exists with different content. Nothing was written.
	ErrConflict = errors.New("service: artifact content conflicts with the existing record")
	// ErrNotFound reports that a query matched no committed record.
	ErrNotFound = errors.New("service: no artifact matches this query")
	// ErrStorage reports that the storage layer could not serve the call.
	// It is never used for an empty result; that is ErrNotFound.
	ErrStorage = errors.New("service: storage is not available")
)

// Service runs the artifact business logic on top of the store.
type Service struct {
	st  *store.Store
	now func() time.Time
}

// New builds a Service over an open store.
func New(st *store.Store) *Service {
	return &Service{st: st, now: time.Now}
}

// RegisterResult carries the stored record and whether this call created it.
type RegisterResult struct {
	Artifact store.Artifact
	// Created is true for a first registration and false for a retry of
	// identical content, which returns the original record unchanged.
	Created bool
}

// Register records a validated, normalized artifact. The service assigns the
// UTC RFC3339 PushedAt timestamp on first registration; a retry with
// identical content returns the original record with Created false and moves
// nothing. Different content for an existing identity fails with ErrConflict
// and leaves the stored record and tag pointer untouched.
func (s *Service) Register(input store.Artifact) (RegisterResult, error) {
	input.PushedAt = s.now().UTC().Format(time.RFC3339)
	record, outcome, err := s.st.RegisterArtifact(input)
	if err != nil {
		return RegisterResult{}, fmt.Errorf("%w: %v", ErrStorage, err)
	}
	switch outcome {
	case store.RegisterCreated:
		return RegisterResult{Artifact: record, Created: true}, nil
	case store.RegisterDuplicate:
		return RegisterResult{Artifact: record}, nil
	default:
		return RegisterResult{}, ErrConflict
	}
}

// Query selects one lookup mode. Tag and Digest are mutually exclusive; when
// both are empty the whole repository is listed in first-registration order.
type Query struct {
	Repository string
	Tag        string
	Digest     string
}

// Query runs a validated lookup against committed state. An empty result is
// ErrNotFound; a storage failure is ErrStorage and is never reported as a
// miss.
func (s *Service) Query(q Query) ([]store.Artifact, error) {
	var (
		artifacts []store.Artifact
		err       error
	)
	switch {
	case q.Tag != "":
		var record store.Artifact
		if record, err = s.st.ArtifactByTag(q.Repository, q.Tag); err == nil {
			artifacts = []store.Artifact{record}
		}
	case q.Digest != "":
		var record store.Artifact
		if record, err = s.st.ArtifactByDigest(q.Repository, q.Digest); err == nil {
			artifacts = []store.Artifact{record}
		}
	default:
		artifacts, err = s.st.ListArtifacts(q.Repository)
	}

	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, ErrNotFound
	case err != nil:
		return nil, fmt.Errorf("%w: %v", ErrStorage, err)
	case len(artifacts) == 0:
		return nil, ErrNotFound
	default:
		return artifacts, nil
	}
}
