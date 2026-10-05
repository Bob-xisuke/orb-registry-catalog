package service

// These tests call the business entry points directly, without any Gin
// context or HTTP response, and pin the result and error classification the
// transports rely on. HTTP compatibility of the same behavior is covered by
// the existing tests in internal/api.

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Bob-xisuke/orb-registry-catalog/internal/store"
)

const (
	testDigestA = "sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testDigestB = "sha256:" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testRepo    = "team/app"
	testTag     = "latest"
)

func newService(t *testing.T) (*Service, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st), st
}

func artifact(digest string) store.Artifact {
	return store.Artifact{
		Repository:        testRepo,
		Digest:            digest,
		Tag:               testTag,
		SignatureVerified: true,
		RetentionDays:     30,
		SizeBytes:         1024,
	}
}

func mustRegister(t *testing.T, svc *Service, a store.Artifact) RegisterResult {
	t.Helper()
	result, err := svc.Register(a)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	return result
}

func mustQuery(t *testing.T, svc *Service, q Query) []store.Artifact {
	t.Helper()
	artifacts, err := svc.Query(q)
	if err != nil {
		t.Fatalf("query %+v: %v", q, err)
	}
	return artifacts
}

func soleArtifact(t *testing.T, artifacts []store.Artifact) store.Artifact {
	t.Helper()
	if len(artifacts) != 1 {
		t.Fatalf("got %d artifacts, want exactly 1: %v", len(artifacts), artifacts)
	}
	return artifacts[0]
}

func TestRegisterCreatedAssignsPushedAt(t *testing.T) {
	svc, _ := newService(t)

	result := mustRegister(t, svc, artifact(testDigestA))
	if !result.Created {
		t.Fatalf("first registration reported Created = false")
	}
	record := result.Artifact
	if record.Repository != testRepo || record.Digest != testDigestA || record.Tag != testTag ||
		!record.SignatureVerified || record.RetentionDays != 30 || record.SizeBytes != 1024 {
		t.Fatalf("stored record does not match input: %+v", record)
	}
	parsed, err := time.Parse(time.RFC3339, record.PushedAt)
	if err != nil {
		t.Fatalf("pushed_at %q is not RFC3339: %v", record.PushedAt, err)
	}
	if parsed.Location() != time.UTC {
		t.Fatalf("pushed_at %q is not UTC", record.PushedAt)
	}
}

// TestRetryReturnsOriginalAndKeepsState registers A then B under one tag and
// retries A: the retry must report Created=false, return the original record
// with its first pushed_at, and leave the tag pointer and list order alone.
func TestRetryReturnsOriginalAndKeepsState(t *testing.T) {
	svc, _ := newService(t)

	first := mustRegister(t, svc, artifact(testDigestA))
	second := mustRegister(t, svc, artifact(testDigestB))
	if !first.Created || !second.Created {
		t.Fatalf("initial registrations must be Created: %v %v", first.Created, second.Created)
	}

	retry := mustRegister(t, svc, artifact(testDigestA))
	if retry.Created {
		t.Fatalf("identical retry reported Created = true")
	}
	if retry.Artifact != first.Artifact {
		t.Fatalf("retry record = %+v, want original %+v", retry.Artifact, first.Artifact)
	}

	// List still returns A then B in first-registration order.
	list := mustQuery(t, svc, Query{Repository: testRepo})
	if len(list) != 2 || list[0].Digest != testDigestA || list[1].Digest != testDigestB {
		t.Fatalf("list = %v, want A then B", list)
	}

	// The tag still resolves to B; A is still reachable by digest.
	if got := soleArtifact(t, mustQuery(t, svc, Query{Repository: testRepo, Tag: testTag})); got.Digest != testDigestB {
		t.Fatalf("tag points at %v, want %s", got.Digest, testDigestB)
	}
	if got := soleArtifact(t, mustQuery(t, svc, Query{Repository: testRepo, Digest: testDigestA})); got != first.Artifact {
		t.Fatalf("digest lookup = %+v, want %+v", got, first.Artifact)
	}
}

// TestConflictRejectsAndKeepsState mutates each comparable field of an
// existing identity: every variant must fail with ErrConflict and neither the
// stored record nor the tag pointer may change.
func TestConflictRejectsAndKeepsState(t *testing.T) {
	base := artifact(testDigestA)

	for name, mutate := range map[string]func(store.Artifact) store.Artifact{
		"changed tag": func(a store.Artifact) store.Artifact { a.Tag = "other"; return a },
		"changed signature": func(a store.Artifact) store.Artifact {
			a.SignatureVerified = false
			return a
		},
		"changed retention": func(a store.Artifact) store.Artifact { a.RetentionDays = 31; return a },
		"changed size":      func(a store.Artifact) store.Artifact { a.SizeBytes = 2048; return a },
	} {
		t.Run(name, func(t *testing.T) {
			svc, _ := newService(t)
			original := mustRegister(t, svc, base)

			_, err := svc.Register(mutate(base))
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("error = %v, want ErrConflict", err)
			}
			if errors.Is(err, ErrNotFound) || errors.Is(err, ErrStorage) {
				t.Fatalf("conflict misclassified: %v", err)
			}

			if got := soleArtifact(t, mustQuery(t, svc, Query{Repository: testRepo, Digest: testDigestA})); got != original.Artifact {
				t.Fatalf("record changed after conflict: %+v, want %+v", got, original.Artifact)
			}
			if got := soleArtifact(t, mustQuery(t, svc, Query{Repository: testRepo, Tag: testTag})); got.Digest != testDigestA {
				t.Fatalf("tag pointer moved after conflict: %v", got.Digest)
			}
			if list := mustQuery(t, svc, Query{Repository: testRepo}); len(list) != 1 {
				t.Fatalf("conflict added a record: %v", list)
			}
		})
	}
}

// TestQueryModes exercises all three lookup modes over the same committed
// state: list order, tag resolution and digest identity.
func TestQueryModes(t *testing.T) {
	svc, _ := newService(t)
	first := mustRegister(t, svc, artifact(testDigestA))
	second := mustRegister(t, svc, artifact(testDigestB))

	list := mustQuery(t, svc, Query{Repository: testRepo})
	if len(list) != 2 || list[0] != first.Artifact || list[1] != second.Artifact {
		t.Fatalf("list = %v, want [A B] as registered", list)
	}
	if got := soleArtifact(t, mustQuery(t, svc, Query{Repository: testRepo, Tag: testTag})); got != second.Artifact {
		t.Fatalf("tag query = %+v, want %+v", got, second.Artifact)
	}
	if got := soleArtifact(t, mustQuery(t, svc, Query{Repository: testRepo, Digest: testDigestA})); got != first.Artifact {
		t.Fatalf("digest query = %+v, want %+v", got, first.Artifact)
	}
}

// TestRepositoriesDoNotInterfere checks that identities and tag pointers are
// scoped to their repository when called through the business entry.
func TestRepositoriesDoNotInterfere(t *testing.T) {
	svc, _ := newService(t)

	one := artifact(testDigestA)
	one.Repository = "team/one"
	two := artifact(testDigestA)
	two.Repository = "team/two"
	mustRegister(t, svc, one)
	mustRegister(t, svc, two)

	twoB := artifact(testDigestB)
	twoB.Repository = "team/two"
	mustRegister(t, svc, twoB)

	if got := soleArtifact(t, mustQuery(t, svc, Query{Repository: "team/one", Tag: testTag})); got.Digest != testDigestA {
		t.Fatalf("team/one tag = %v, want %s", got.Digest, testDigestA)
	}
	if got := soleArtifact(t, mustQuery(t, svc, Query{Repository: "team/two", Tag: testTag})); got.Digest != testDigestB {
		t.Fatalf("team/two tag = %v, want %s", got.Digest, testDigestB)
	}
	if list := mustQuery(t, svc, Query{Repository: "team/one"}); len(list) != 1 {
		t.Fatalf("team/one list leaked records: %v", list)
	}
}

func TestQueryNotFound(t *testing.T) {
	svc, _ := newService(t)
	mustRegister(t, svc, artifact(testDigestA))

	for name, q := range map[string]Query{
		"unknown repository": {Repository: "other/repo"},
		"unknown tag":        {Repository: testRepo, Tag: "missing"},
		"unknown digest":     {Repository: testRepo, Digest: testDigestB},
		"empty list":         {Repository: "empty/repo"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.Query(q)
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("error = %v, want ErrNotFound", err)
			}
			if errors.Is(err, ErrStorage) {
				t.Fatalf("empty result misclassified as storage failure: %v", err)
			}
		})
	}
}

// TestStorageUnavailableIsNotNotFound closes the database underneath the
// service: every entry must report ErrStorage and never ErrNotFound, so a
// storage outage can never be answered as an empty result.
func TestStorageUnavailableIsNotNotFound(t *testing.T) {
	svc, st := newService(t)
	mustRegister(t, svc, artifact(testDigestA))
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	if _, err := svc.Register(artifact(testDigestB)); !errors.Is(err, ErrStorage) {
		t.Fatalf("register error = %v, want ErrStorage", err)
	}
	for name, q := range map[string]Query{
		"list":      {Repository: testRepo},
		"by tag":    {Repository: testRepo, Tag: testTag},
		"by digest": {Repository: testRepo, Digest: testDigestA},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.Query(q)
			if !errors.Is(err, ErrStorage) {
				t.Fatalf("error = %v, want ErrStorage", err)
			}
			if errors.Is(err, ErrNotFound) {
				t.Fatalf("storage failure misclassified as not found: %v", err)
			}
		})
	}
}

// TestRestartPreservesState reopens the same SQLite file behind a new
// Service: records, order, timestamps and the tag pointer must be unchanged.
func TestRestartPreservesState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	first := mustRegister(t, New(st), artifact(testDigestA))
	second := mustRegister(t, New(st), artifact(testDigestB))
	if err := st.Close(); err != nil {
		t.Fatalf("close before reopen: %v", err)
	}

	reopened, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { reopened.Close() })
	svc := New(reopened)

	list := mustQuery(t, svc, Query{Repository: testRepo})
	if len(list) != 2 || list[0] != first.Artifact || list[1] != second.Artifact {
		t.Fatalf("list after reopen = %v, want [A B] unchanged", list)
	}
	if got := soleArtifact(t, mustQuery(t, svc, Query{Repository: testRepo, Tag: testTag})); got != second.Artifact {
		t.Fatalf("tag pointer after reopen = %+v, want %+v", got, second.Artifact)
	}
}
