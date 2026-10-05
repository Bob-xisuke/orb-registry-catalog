package service_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Bob-xisuke/orb-registry-catalog/internal/service"
)

// These business-level cases run entirely against the in-memory service.Store
// double in memory_store_test.go: no SQLite file, database/sql handle or
// store-package type is opened. They pin the outcomes a caller must be able to
// rely on from any conforming backend; the real SQLite backend is verified
// separately in internal/api.

const (
	digestA = "sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func newTestService(t *testing.T) (*service.Service, *memoryStore) {
	t.Helper()
	backend := newMemoryStore()
	return service.New(backend), backend
}

func validInput(repo, digest, tag string) service.RegisterInput {
	return service.RegisterInput{
		Repository:        repo,
		Digest:            digest,
		Tag:               tag,
		SignatureVerified: true,
		RetentionDays:     30,
		SizeBytes:         1024,
	}
}

func wantRecord(t *testing.T, got service.Artifact, in service.RegisterInput, pushedAt string) {
	t.Helper()
	want := service.Artifact{
		Repository:        in.Repository,
		Digest:            in.Digest,
		Tag:               in.Tag,
		SignatureVerified: in.SignatureVerified,
		RetentionDays:     in.RetentionDays,
		SizeBytes:         in.SizeBytes,
		PushedAt:          pushedAt,
	}
	if got != want {
		t.Fatalf("record = %+v, want %+v", got, want)
	}
}

func assertRFC3339UTC(t *testing.T, value string) {
	t.Helper()
	if value == "" {
		t.Fatal("pushed_at is empty")
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("pushed_at %q is not RFC3339: %v", value, err)
	}
	if parsed.Location() != time.UTC {
		t.Fatalf("pushed_at %q is not UTC", value)
	}
}

func TestRegisterCreatedThenDuplicate(t *testing.T) {
	svc, backend := newTestService(t)
	input := validInput("team/app", digestA, "latest")

	first, err := svc.Register(input)
	if err != nil {
		t.Fatalf("first register: %v", err)
	}
	if first.Outcome != service.OutcomeCreated {
		t.Fatalf("first outcome = %v, want OutcomeCreated", first.Outcome)
	}
	assertRFC3339UTC(t, first.Artifact.PushedAt)
	wantRecord(t, first.Artifact, input, first.Artifact.PushedAt)

	// The service stamps pushed_at before handing the record to the backend;
	// a caller can never supply or influence it.
	if backend.captured.PushedAt != first.Artifact.PushedAt {
		t.Fatalf("backend received pushed_at %q, want %q", backend.captured.PushedAt, first.Artifact.PushedAt)
	}
	pushedAt := first.Artifact.PushedAt

	// An identical retry is distinguishable as a duplicate and returns the
	// original record, including its push time, without a new record.
	second, err := svc.Register(input)
	if err != nil {
		t.Fatalf("duplicate register: %v", err)
	}
	if second.Outcome != service.OutcomeDuplicate {
		t.Fatalf("duplicate outcome = %v, want OutcomeDuplicate", second.Outcome)
	}
	if second.Artifact != first.Artifact {
		t.Fatalf("duplicate record = %+v, want original %+v", second.Artifact, first.Artifact)
	}
	if second.Artifact.PushedAt != pushedAt {
		t.Fatalf("pushed_at changed on duplicate: %q -> %q", pushedAt, second.Artifact.PushedAt)
	}
	if len(backend.records) != 1 {
		t.Fatalf("duplicate added a record: %d stored", len(backend.records))
	}
}

func TestRegisterConflictLeavesRecordAndTagUntouched(t *testing.T) {
	svc, _ := newTestService(t)
	input := validInput("team/app", digestA, "latest")

	first, err := svc.Register(input)
	if err != nil {
		t.Fatalf("first register: %v", err)
	}

	// Move the tag to B so we can prove a rejected re-registration of A never
	// moves it back.
	inputB := validInput("team/app", digestB, "latest")
	inputB.SizeBytes = 2048
	if _, err := svc.Register(inputB); err != nil {
		t.Fatalf("register B: %v", err)
	}

	conflicts := map[string]service.RegisterInput{
		"changed tag":       withChanges(input, func(in *service.RegisterInput) { in.Tag = "other" }),
		"changed signature": withChanges(input, func(in *service.RegisterInput) { in.SignatureVerified = false }),
		"changed retention": withChanges(input, func(in *service.RegisterInput) { in.RetentionDays = 31 }),
		"changed size":      withChanges(input, func(in *service.RegisterInput) { in.SizeBytes = 2048 }),
	}
	for name, bad := range conflicts {
		t.Run(name, func(t *testing.T) {
			if _, err := svc.Register(bad); !errors.Is(err, service.ErrConflict) {
				t.Fatalf("register error = %v, want ErrConflict", err)
			}

			// The immutable A record is unchanged, including pushed_at.
			byDigest, err := svc.Query(service.Query{Kind: service.QueryByDigest, Repository: "team/app", Digest: digestA})
			if err != nil {
				t.Fatalf("query A after conflict: %v", err)
			}
			if len(byDigest) != 1 || byDigest[0] != first.Artifact {
				t.Fatalf("A record changed after conflict: %+v", byDigest)
			}

			// The tag still resolves to B.
			byTag, err := svc.Query(service.Query{Kind: service.QueryByTag, Repository: "team/app", Tag: "latest"})
			if err != nil {
				t.Fatalf("query tag after conflict: %v", err)
			}
			if len(byTag) != 1 || byTag[0].Digest != digestB {
				t.Fatalf("tag pointer moved after conflict: %+v", byTag)
			}

			// No third record and registration order is intact.
			list, err := svc.Query(service.Query{Kind: service.QueryByRepository, Repository: "team/app"})
			if err != nil {
				t.Fatalf("list after conflict: %v", err)
			}
			if len(list) != 2 || list[0].Digest != digestA || list[1].Digest != digestB {
				t.Fatalf("list changed after conflict: %+v", list)
			}

			// The rejected body's tag was never persisted.
			if _, err := svc.Query(service.Query{Kind: service.QueryByTag, Repository: "team/app", Tag: "other"}); !errors.Is(err, service.ErrNotFound) {
				t.Fatalf("rejected tag lookup error = %v, want ErrNotFound", err)
			}
		})
	}
}

func withChanges(in service.RegisterInput, change func(*service.RegisterInput)) service.RegisterInput {
	change(&in)
	return in
}

// TestRegistrationSequenceAndQueries walks: register A, register B under the
// same tag, retry A, then read through all three query kinds.
func TestRegistrationSequenceAndQueries(t *testing.T) {
	svc, _ := newTestService(t)
	inputA := validInput("team/app", digestA, "latest")
	inputB := validInput("team/app", digestB, "latest")
	inputB.SizeBytes = 2048

	first, err := svc.Register(inputA)
	if err != nil {
		t.Fatalf("register A: %v", err)
	}
	if _, err := svc.Register(inputB); err != nil {
		t.Fatalf("register B: %v", err)
	}
	retry, err := svc.Register(inputA)
	if err != nil {
		t.Fatalf("retry A: %v", err)
	}
	if retry.Outcome != service.OutcomeDuplicate || retry.Artifact.PushedAt != first.Artifact.PushedAt {
		t.Fatalf("retry A = %+v, want duplicate with original pushed_at", retry)
	}

	// List keeps first-registration order A, B.
	list, err := svc.Query(service.Query{Kind: service.QueryByRepository, Repository: "team/app"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 || list[0].Digest != digestA || list[1].Digest != digestB {
		t.Fatalf("list = %+v, want [A B] in registration order", list)
	}

	// The tag resolves to the newer record B even after retrying A.
	byTag, err := svc.Query(service.Query{Kind: service.QueryByTag, Repository: "team/app", Tag: "latest"})
	if err != nil {
		t.Fatalf("by tag: %v", err)
	}
	if len(byTag) != 1 || byTag[0].Digest != digestB {
		t.Fatalf("tag = %+v, want B", byTag)
	}

	// A is still retrievable by digest, byte for byte the first record.
	byDigest, err := svc.Query(service.Query{Kind: service.QueryByDigest, Repository: "team/app", Digest: digestA})
	if err != nil {
		t.Fatalf("by digest: %v", err)
	}
	if len(byDigest) != 1 || byDigest[0] != first.Artifact {
		t.Fatalf("digest lookup = %+v, want original A %+v", byDigest, first.Artifact)
	}

	// Single-record lookups keep the array-shaped response contract: one
	// element, never the bare record.
	if len(byTag) != 1 || len(byDigest) != 1 {
		t.Fatalf("single lookups must return a one-element slice")
	}
}

func TestRepositoriesDoNotInterfere(t *testing.T) {
	svc, _ := newTestService(t)
	inputOne := validInput("repo/one", digestA, "latest")
	inputTwo := validInput("repo/two", digestA, "latest")

	if _, err := svc.Register(inputOne); err != nil {
		t.Fatalf("register repo one: %v", err)
	}
	if _, err := svc.Register(inputTwo); err != nil {
		t.Fatalf("register repo two: %v", err)
	}

	one, err := svc.Query(service.Query{Kind: service.QueryByTag, Repository: "repo/one", Tag: "latest"})
	if err != nil {
		t.Fatalf("repo one tag: %v", err)
	}
	two, err := svc.Query(service.Query{Kind: service.QueryByTag, Repository: "repo/two", Tag: "latest"})
	if err != nil {
		t.Fatalf("repo two tag: %v", err)
	}
	if one[0].Repository != "repo/one" || two[0].Repository != "repo/two" {
		t.Fatalf("tag lookups crossed repositories: %+v %+v", one, two)
	}

	// A conflict in repo two leaves repo one's record untouched.
	conflict := inputTwo
	conflict.SizeBytes = 4096
	if _, err := svc.Register(conflict); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("cross-repo conflict error = %v, want ErrConflict", err)
	}
	oneAgain, err := svc.Query(service.Query{Kind: service.QueryByDigest, Repository: "repo/one", Digest: digestA})
	if err != nil || oneAgain[0].SizeBytes != inputOne.SizeBytes {
		t.Fatalf("repo one changed after repo two conflict: %+v %v", oneAgain, err)
	}

	oneList, err := svc.Query(service.Query{Kind: service.QueryByRepository, Repository: "repo/one"})
	if err != nil {
		t.Fatalf("repo one list: %v", err)
	}
	twoList, err := svc.Query(service.Query{Kind: service.QueryByRepository, Repository: "repo/two"})
	if err != nil {
		t.Fatalf("repo two list: %v", err)
	}
	if len(oneList) != 1 || len(twoList) != 1 {
		t.Fatalf("lists leaked across repositories: %+v %+v", oneList, twoList)
	}
}

func TestQueryNotFound(t *testing.T) {
	svc, _ := newTestService(t)
	if _, err := svc.Register(validInput("team/app", digestA, "latest")); err != nil {
		t.Fatalf("register: %v", err)
	}

	cases := map[string]service.Query{
		"unknown repository": {Kind: service.QueryByRepository, Repository: "other/repo"},
		"unknown tag":        {Kind: service.QueryByTag, Repository: "team/app", Tag: "missing"},
		"unknown digest":     {Kind: service.QueryByDigest, Repository: "team/app", Digest: digestB},
	}
	for name, query := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := svc.Query(query)
			if !errors.Is(err, service.ErrNotFound) {
				t.Fatalf("error = %v, want ErrNotFound", err)
			}
			if errors.Is(err, service.ErrStorage) {
				t.Fatalf("a miss must not also be ErrStorage: %v", err)
			}
		})
	}
}

// TestBackendMissWrappersAreRecognized pins how a backend reports a miss: the
// error may carry the backend's own context or sentinel, as long as it wraps
// service.ErrNotFound. Both errors.Join-style and fmt.Errorf%%w-style misses
// must be classified as ErrNotFound by errors.Is — a caller never switches on
// the backend's concrete error type.
func TestBackendMissWrappersAreRecognized(t *testing.T) {
	joined := errors.Join(errMemoryMiss, service.ErrNotFound)
	wrapped := fmt.Errorf("lookup by digest failed: %w", service.ErrNotFound)
	for name, err := range map[string]error{"joined": joined, "wrapped": wrapped} {
		t.Run(name, func(t *testing.T) {
			if !errors.Is(err, service.ErrNotFound) {
				t.Fatalf("%v does not wrap service.ErrNotFound", err)
			}
			if errors.Is(err, service.ErrStorage) {
				t.Fatalf("%v must not be ErrStorage", err)
			}
		})
	}
	if !errors.Is(missError(), errMemoryMiss) {
		t.Fatal("the backend loses visibility of its own sentinel when joined with the port's")
	}
}

// TestEmptyListIsNotFound checks the defensive half of the miss rule: a
// repository list that returns no rows is ErrNotFound even when the backend
// answers with a nil error instead of the miss sentinel.
func TestEmptyListIsNotFound(t *testing.T) {
	svc := service.New(emptyListStore{})
	if _, err := svc.Query(service.Query{Kind: service.QueryByRepository, Repository: "team/app"}); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

func TestStorageFailureIsNotNotFound(t *testing.T) {
	svc, backend := newTestService(t)
	if _, err := svc.Register(validInput("team/app", digestA, "latest")); err != nil {
		t.Fatalf("seed register: %v", err)
	}
	backend.fail(errMemoryFault)

	// Every operation reports ErrStorage, even lookups that would otherwise
	// hit, and even a query that would have been a plain miss.
	if _, err := svc.Register(validInput("team/app", digestB, "latest")); !errors.Is(err, service.ErrStorage) {
		t.Fatalf("register error = %v, want ErrStorage", err)
	}
	for name, query := range map[string]service.Query{
		"list":   {Kind: service.QueryByRepository, Repository: "team/app"},
		"tag":    {Kind: service.QueryByTag, Repository: "team/app", Tag: "latest"},
		"digest": {Kind: service.QueryByDigest, Repository: "team/app", Digest: digestA},
		"miss":   {Kind: service.QueryByDigest, Repository: "team/app", Digest: digestB},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.Query(query)
			if !errors.Is(err, service.ErrStorage) {
				t.Fatalf("query error = %v, want ErrStorage", err)
			}
			if errors.Is(err, service.ErrNotFound) {
				t.Fatalf("backend fault must never classify as ErrNotFound: %v", err)
			}
		})
	}
}
