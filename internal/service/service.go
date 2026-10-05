// Package service holds the artifact business operations, independent of any
// HTTP framework and of any concrete storage backend. Callers register and
// query artifacts through this package and interpret the outcome without
// touching *gin.Context, http.ResponseWriter, database/sql or a specific
// database driver.
//
// Persistence is reached only through the Store interface. A caller may supply
// its own implementation; the bundled SQLite adapter lives in internal/store.
package service

import (
	"errors"
	"time"
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

// Record is one persisted artifact as seen across the storage contract. It is
// the data shape Store implementations exchange; business callers work with
// Artifact instead. PushedAt is assigned by the service at first registration
// and never changes afterwards.
type Record struct {
	Repository        string
	Digest            string
	Tag               string
	SignatureVerified bool
	RetentionDays     int64
	SizeBytes         int64
	PushedAt          string
}

// RegisterStatus describes how a Store.Register resolution ended. It is the
// storage-level tri-state a backend must distinguish, independent of the
// business-level OutcomeCreated/OutcomeDuplicate pair.
type RegisterStatus int

const (
	// StatusCreated means the record was newly written and its tag pointer moved.
	StatusCreated RegisterStatus = iota
	// StatusDuplicate means identical content already existed; nothing was written.
	StatusDuplicate
	// StatusConflict means the (repository, digest) identity exists with different content.
	StatusConflict
)

// ErrRecordNotFound is the storage-layer miss signal. A Store returns it when a
// tag or digest lookup matches no committed row. It is deliberately distinct
// from a transport or other failure, which a Store reports as a non-nil error
// of its own, so the service never mistakes a backend fault for a miss.
var ErrRecordNotFound = errors.New("store: no matching record")

// Store is the persistence port the business layer depends on. Any backend —
// the bundled SQLite adapter or a caller-supplied implementation — may satisfy
// it with its own types; none of database/sql or the concrete driver leaks
// through this contract.
//
// Register writes the record and moves its tag pointer as one atomic
// operation. An identical re-submission returns the stored record with
// StatusDuplicate and changes nothing; different content for an existing
// (repository, digest) is StatusConflict and also changes nothing.
//
// ListRecords returns a repository's records in first-registration order.
// RecordByDigest and RecordByTag return ErrRecordNotFound on a miss and a
// different non-nil error only when the backend itself failed.
type Store interface {
	Register(record Record) (Record, RegisterStatus, error)
	ListRecords(repository string) ([]Record, error)
	RecordByDigest(repository, digest string) (Record, error)
	RecordByTag(repository, tag string) (Record, error)
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
// pointer in the backend's single transaction. An identical retry returns the
// original record as OutcomeDuplicate without writes. Different content for an
// existing identity returns ErrConflict and changes nothing. Storage failures
// return ErrStorage.
func (s *Service) Register(input RegisterInput) (RegisterResult, error) {
	record, status, err := s.store.Register(Record{
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
	if status == StatusConflict {
		return RegisterResult{}, ErrConflict
	}
	return RegisterResult{
		Artifact: fromRecord(record),
		Outcome:  mapStatus(status),
	}, nil
}

// Query resolves a validated business query against committed state. A
// repository list returns records in first-registration order; tag and digest
// lookups return the single matching record. No match is ErrNotFound; storage
// trouble is ErrStorage and is never mistaken for a miss.
func (s *Service) Query(q Query) ([]Artifact, error) {
	var (
		records []Record
		err     error
	)
	switch q.Kind {
	case QueryByTag:
		var record Record
		record, err = s.store.RecordByTag(q.Repository, q.Tag)
		if err == nil {
			records = []Record{record}
		}
	case QueryByDigest:
		var record Record
		record, err = s.store.RecordByDigest(q.Repository, q.Digest)
		if err == nil {
			records = []Record{record}
		}
	default:
		records, err = s.store.ListRecords(q.Repository)
	}

	switch {
	case errors.Is(err, ErrRecordNotFound):
		return nil, ErrNotFound
	case err != nil:
		return nil, ErrStorage
	case len(records) == 0:
		return nil, ErrNotFound
	default:
		return fromRecordList(records), nil
	}
}

func mapStatus(status RegisterStatus) RegisterOutcome {
	if status == StatusDuplicate {
		return OutcomeDuplicate
	}
	return OutcomeCreated
}

func fromRecord(r Record) Artifact {
	return Artifact{
		Repository:        r.Repository,
		Digest:            r.Digest,
		Tag:               r.Tag,
		SignatureVerified: r.SignatureVerified,
		RetentionDays:     r.RetentionDays,
		SizeBytes:         r.SizeBytes,
		PushedAt:          r.PushedAt,
	}
}

func fromRecordList(records []Record) []Artifact {
	out := make([]Artifact, len(records))
	for i, record := range records {
		out[i] = fromRecord(record)
	}
	return out
}
