// Package service holds the artifact business operations, independent of any
// HTTP framework. Callers register and query artifacts through this package and
// interpret the outcome without touching *gin.Context or http.ResponseWriter.
package service

import (
	"errors"
	"time"

	"github.com/Bob-xisuke/orb-registry-catalog/internal/store"
)

// Artifact is one registration record returned to business callers. Field
// names mirror the persisted record and the published JSON contract.
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

// RegisterOutcome distinguishes a first registration from an identical retry.
type RegisterOutcome int

const (
	// OutcomeCreated means a record was written and its tag pointer moved.
	OutcomeCreated RegisterOutcome = iota
	// OutcomeDuplicate means identical content already existed; nothing changed.
	OutcomeDuplicate
)

// Sentinel failures a business caller must be able to tell apart. They carry no
// database detail, so callers may surface them directly to their own clients.
var (
	// ErrConflict reports a (repository, digest) identity whose stored content
	// differs from the submitted content.
	ErrConflict = errors.New("artifact content conflicts with the existing record")
	// ErrNotFound reports a legal query that matched no committed record.
	ErrNotFound = errors.New("no artifact matches this query")
	// ErrStorage reports that the storage layer was not usable.
	ErrStorage = errors.New("database is not available")
)

// RegisterResult pairs the resulting record with whether it was newly written.
type RegisterResult struct {
	Artifact Artifact
	Outcome  RegisterOutcome
}

// Service runs artifact operations against a store.
type Service struct {
	store artifactStore
}

// artifactStore is the subset of *store.Store the business layer needs.
type artifactStore interface {
	RegisterArtifact(store.Artifact) (store.Artifact, store.RegisterOutcome, error)
	ListArtifacts(repository string) ([]store.Artifact, error)
	ArtifactByDigest(repository, digest string) (store.Artifact, error)
	ArtifactByTag(repository, tag string) (store.Artifact, error)
}

// New builds a Service over the given store.
func New(st artifactStore) *Service {
	return &Service{store: st}
}

// Register records a validated registration. A first registration stores the
// service-generated UTC RFC3339 push time and moves the repository's tag
// pointer in the store's single transaction. An identical retry returns the
// original record as OutcomeDuplicate without writes. Different content for an
// existing identity returns ErrConflict and changes nothing. Storage failures
// return ErrStorage.
func (s *Service) Register(input RegisterInput) (RegisterResult, error) {
	record, outcome, err := s.store.RegisterArtifact(store.Artifact{
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
	if outcome == store.RegisterConflict {
		return RegisterResult{}, ErrConflict
	}
	return RegisterResult{
		Artifact: fromStore(record),
		Outcome:  mapOutcome(outcome),
	}, nil
}

// Query resolves a validated business query against committed state. A
// repository list returns records in first-registration order; tag and digest
// lookups return the single matching record. No match is ErrNotFound; storage
// trouble is ErrStorage and is never mistaken for a miss.
func (s *Service) Query(q Query) ([]Artifact, error) {
	var (
		records []store.Artifact
		err     error
	)
	switch q.Kind {
	case QueryByTag:
		var record store.Artifact
		record, err = s.store.ArtifactByTag(q.Repository, q.Tag)
		if err == nil {
			records = []store.Artifact{record}
		}
	case QueryByDigest:
		var record store.Artifact
		record, err = s.store.ArtifactByDigest(q.Repository, q.Digest)
		if err == nil {
			records = []store.Artifact{record}
		}
	default:
		records, err = s.store.ListArtifacts(q.Repository)
	}

	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, ErrNotFound
	case err != nil:
		return nil, ErrStorage
	case len(records) == 0:
		return nil, ErrNotFound
	default:
		return fromStoreList(records), nil
	}
}

func mapOutcome(outcome store.RegisterOutcome) RegisterOutcome {
	if outcome == store.RegisterDuplicate {
		return OutcomeDuplicate
	}
	return OutcomeCreated
}

func fromStore(a store.Artifact) Artifact {
	return Artifact{
		Repository:        a.Repository,
		Digest:            a.Digest,
		Tag:               a.Tag,
		SignatureVerified: a.SignatureVerified,
		RetentionDays:     a.RetentionDays,
		SizeBytes:         a.SizeBytes,
		PushedAt:          a.PushedAt,
	}
}

func fromStoreList(records []store.Artifact) []Artifact {
	out := make([]Artifact, len(records))
	for i, record := range records {
		out[i] = fromStore(record)
	}
	return out
}
