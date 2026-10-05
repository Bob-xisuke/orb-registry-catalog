// Package service holds the artifact business operations, independent of any
// HTTP framework and of any concrete storage engine. Callers register and
// query artifacts through this package and interpret the outcome without
// touching *gin.Context, http.ResponseWriter, database/sql or a particular
// database driver.
//
// A caller supplies the persistence side by implementing Store against
// whatever backend it likes (a relational database, an in-memory map in
// tests, a remote catalog, ...). The business layer only speaks the types and
// sentinels declared in this package; it never imports the engine or its
// driver.
package service

import (
	"errors"
	"time"
)

// Artifact is one registration record exchanged with the storage layer and
// returned to business callers. Field names mirror the persisted record and
// the published JSON contract.
type Artifact struct {
	Repository        string
	Digest            string
	Tag               string
	SignatureVerified bool
	RetentionDays     int64
	SizeBytes         int64
	PushedAt          string
}

// RegisterInput is a registration request after the caller has validated and
// normalized every field. The service adds no transport-level validation; it
// assumes non-empty repository/tag, a well-formed digest and an in-range
// retention period.
type RegisterInput struct {
	Repository        string
	Digest            string
	Tag               string
	SignatureVerified bool
	RetentionDays     int64
	SizeBytes         int64
}

// QueryKind selects how a Query is resolved.
type QueryKind int

const (
	// QueryByRepository lists every record of one repository.
	QueryByRepository QueryKind = iota
	// QueryByTag resolves a tag to its current record.
	QueryByTag
	// QueryByDigest resolves an exact (repository, digest) identity.
	QueryByDigest
)

// Query is a validated business query: repository always set; Tag or Digest
// populated only when Kind asks for them.
type Query struct {
	Kind       QueryKind
	Repository string
	Tag        string
	Digest     string
}

// RegisterOutcome distinguishes a first registration from an identical retry
// in the result returned to a business caller.
type RegisterOutcome int

const (
	// OutcomeCreated means a record was written and its tag pointer moved.
	OutcomeCreated RegisterOutcome = iota
	// OutcomeDuplicate means identical content already existed; nothing changed.
	OutcomeDuplicate
)

// Sentinel failures a business caller must be able to tell apart. They carry
// no database detail, so callers may surface them directly to their own
// clients.
var (
	// ErrConflict reports a (repository, digest) identity whose stored content
	// differs from the submitted content.
	ErrConflict = errors.New("artifact content conflicts with the existing record")
	// ErrNotFound reports a legal query that matched no committed record. It is
	// also the miss sentinel a Store implementation must return (possibly
	// wrapped) for a lookup or an empty repository list.
	ErrNotFound = errors.New("no artifact matches this query")
	// ErrStorage reports that the storage layer was not usable.
	ErrStorage = errors.New("database is not available")
)

// RegisterResult pairs the resulting record with whether it was newly written.
type RegisterResult struct {
	Artifact Artifact
	Outcome  RegisterOutcome
}

// StoreOutcome is the storage-layer resolution of one RegisterArtifact call.
// A backend owns the lookup-and-compare decision and reports it back; the
// business layer never inspects backend internals.
type StoreOutcome int

const (
	// StoreCreated means the record was newly written and its tag pointer moved
	// in the backend's single transaction.
	StoreCreated StoreOutcome = iota
	// StoreDuplicate means identical content already existed; nothing was
	// written and the stored record (with its original push time) is returned.
	StoreDuplicate
	// StoreConflict means the (repository, digest) identity exists with
	// different content; nothing was written and no tag pointer moved.
	StoreConflict
)

// Store is the persistence contract the business layer depends on. Any
// implementation is acceptable as long as it keeps these guarantees:
//
//   - identity is (repository, digest);
//   - RegisterArtifact writes the record and moves the repository's tag
//     pointer in one atomic transaction, so partial state is never visible;
//   - an identical resubmission changes nothing and returns the original
//     record (including its original PushedAt) as StoreDuplicate;
//   - different content for an existing identity changes nothing and reports
//     StoreConflict;
//   - a miss, including an empty repository list, returns an error wrapping
//     ErrNotFound; every other failure returns a different error, since the
//     business layer must never mistake a backend fault for a miss.
//
// ListArtifacts returns records in first-registration order. The Artifact
// values passed in and returned are this package's own type: an implementation
// needs no other package's data structures, result enums or error constants.
type Store interface {
	RegisterArtifact(record Artifact) (Artifact, StoreOutcome, error)
	ListArtifacts(repository string) ([]Artifact, error)
	ArtifactByDigest(repository, digest string) (Artifact, error)
	ArtifactByTag(repository, tag string) (Artifact, error)
}

// Service runs artifact operations against a Store.
type Service struct {
	store Store
}

// New builds a Service over the given Store.
func New(st Store) *Service {
	return &Service{store: st}
}

// Register records a validated registration. A first registration stores the
// service-generated UTC RFC3339 push time and moves the repository's tag
// pointer in the store's single transaction. An identical retry returns the
// original record as OutcomeDuplicate without writes. Different content for an
// existing identity returns ErrConflict and changes nothing. Storage failures
// return ErrStorage.
func (s *Service) Register(input RegisterInput) (RegisterResult, error) {
	record, outcome, err := s.store.RegisterArtifact(Artifact{
		Repository:        input.Repository,
		Digest:            input.Digest,
		Tag:               input.Tag,
		SignatureVerified: input.SignatureVerified,
		RetentionDays:     input.RetentionDays,
		SizeBytes:         input.SizeBytes,
		PushedAt:          time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return RegisterResult{}, ErrStorage
	}
	if outcome == StoreConflict {
		return RegisterResult{}, ErrConflict
	}
	return RegisterResult{
		Artifact: record,
		Outcome:  mapOutcome(outcome),
	}, nil
}

// Query resolves a validated business query against committed state. A
// repository list returns records in first-registration order; tag and digest
// lookups return the single matching record. No match is ErrNotFound; storage
// trouble is ErrStorage and is never mistaken for a miss.
func (s *Service) Query(q Query) ([]Artifact, error) {
	var (
		records []Artifact
		err     error
	)
	switch q.Kind {
	case QueryByTag:
		var record Artifact
		record, err = s.store.ArtifactByTag(q.Repository, q.Tag)
		if err == nil {
			records = []Artifact{record}
		}
	case QueryByDigest:
		var record Artifact
		record, err = s.store.ArtifactByDigest(q.Repository, q.Digest)
		if err == nil {
			records = []Artifact{record}
		}
	default:
		records, err = s.store.ListArtifacts(q.Repository)
	}

	switch {
	case errors.Is(err, ErrNotFound):
		return nil, ErrNotFound
	case err != nil:
		return nil, ErrStorage
	case len(records) == 0:
		return nil, ErrNotFound
	default:
		return records, nil
	}
}

func mapOutcome(outcome StoreOutcome) RegisterOutcome {
	if outcome == StoreDuplicate {
		return OutcomeDuplicate
	}
	return OutcomeCreated
}
