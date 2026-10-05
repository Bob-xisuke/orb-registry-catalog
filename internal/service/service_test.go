package service

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Bob-xisuke/orb-registry-catalog/internal/store"
)

const (
	digestA = "sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func newTestService(t *testing.T) (*Service, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "service.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st), path
}

func validInput(repo, digest, tag string) RegisterInput {
	return RegisterInput{
		Repository:        repo,
		Digest:            digest,
		Tag:               tag,
		SignatureVerified: true,
		RetentionDays:     30,
		SizeBytes:         1024,
	}
}

func wantRecord(t *testing.T, got Artifact, in RegisterInput, pushedAt string) {
	t.Helper()
	want := Artifact{
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
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("pushed_at %q is not RFC3339: %v", value, err)
	}
	if parsed.Location() != time.UTC {
		t.Fatalf("pushed_at %q is not UTC", value)
	}
}

func TestRegisterCreatedThenDuplicate(t *testing.T) {
	svc, _ := newTestService(t)
	input := validInput("team/app", digestA, "latest")

	first, err := svc.Register(input)
	if err != nil {
		t.Fatalf("first register: %v", err)
	}
	if first.Outcome != OutcomeCreated {
		t.Fatalf("first outcome = %v, want OutcomeCreated", first.Outcome)
	}
	assertRFC3339UTC(t, first.Artifact.PushedAt)
	wantRecord(t, first.Artifact, input, first.Artifact.PushedAt)
	pushedAt := first.Artifact.PushedAt

	// An identical retry is distinguishable as a duplicate and returns the
	// original record, including its push time.
	second, err := svc.Register(input)
	if err != nil {
		t.Fatalf("duplicate register: %v", err)
	}
	if second.Outcome != OutcomeDuplicate {
		t.Fatalf("duplicate outcome = %v, want OutcomeDuplicate", second.Outcome)
	}
	if second.Artifact != first.Artifact {
		t.Fatalf("duplicate record = %+v, want original %+v", second.Artifact, first.Artifact)
	}
	if second.Artifact.PushedAt != pushedAt {
		t.Fatalf("pushed_at changed on duplicate: %q -> %q", pushedAt, second.Artifact.PushedAt)
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

	conflicts := map[string]RegisterInput{
		"changed tag":       withChanges(input, func(in *RegisterInput) { in.Tag = "other" }),
		"changed signature": withChanges(input, func(in *RegisterInput) { in.SignatureVerified = false }),
		"changed retention": withChanges(input, func(in *RegisterInput) { in.RetentionDays = 31 }),
		"changed size":      withChanges(input, func(in *RegisterInput) { in.SizeBytes = 2048 }),
	}
	for name, bad := range conflicts {
		t.Run(name, func(t *testing.T) {
			if _, err := svc.Register(bad); !errors.Is(err, ErrConflict) {
				t.Fatalf("register error = %v, want ErrConflict", err)
			}

			// The immutable A record is unchanged, including pushed_at.
			byDigest, err := svc.Query(Query{Kind: QueryByDigest, Repository: "team/app", Digest: digestA})
			if err != nil {
				t.Fatalf("query A after conflict: %v", err)
			}
			if len(byDigest) != 1 || byDigest[0] != first.Artifact {
				t.Fatalf("A record changed after conflict: %+v", byDigest)
			}

			// The tag still resolves to B.
			byTag, err := svc.Query(Query{Kind: QueryByTag, Repository: "team/app", Tag: "latest"})
			if err != nil {
				t.Fatalf("query tag after conflict: %v", err)
			}
			if len(byTag) != 1 || byTag[0].Digest != digestB {
				t.Fatalf("tag pointer moved after conflict: %+v", byTag)
			}

			// No third record and registration order is intact.
			list, err := svc.Query(Query{Kind: QueryByRepository, Repository: "team/app"})
			if err != nil {
				t.Fatalf("list after conflict: %v", err)
			}
			if len(list) != 2 || list[0].Digest != digestA || list[1].Digest != digestB {
				t.Fatalf("list changed after conflict: %+v", list)
			}

			// The rejected body's tag was never persisted.
			if _, err := svc.Query(Query{Kind: QueryByTag, Repository: "team/app", Tag: "other"}); !errors.Is(err, ErrNotFound) {
				t.Fatalf("rejected tag lookup error = %v, want ErrNotFound", err)
			}
		})
	}
}

func withChanges(in RegisterInput, change func(*RegisterInput)) RegisterInput {
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
	if retry.Outcome != OutcomeDuplicate || retry.Artifact.PushedAt != first.Artifact.PushedAt {
		t.Fatalf("retry A = %+v, want duplicate with original pushed_at", retry)
	}

	// List keeps first-registration order A, B.
	list, err := svc.Query(Query{Kind: QueryByRepository, Repository: "team/app"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 || list[0].Digest != digestA || list[1].Digest != digestB {
		t.Fatalf("list = %+v, want [A B] in registration order", list)
	}

	// The tag resolves to the newer record B even after retrying A.
	byTag, err := svc.Query(Query{Kind: QueryByTag, Repository: "team/app", Tag: "latest"})
	if err != nil {
		t.Fatalf("by tag: %v", err)
	}
	if len(byTag) != 1 || byTag[0].Digest != digestB {
		t.Fatalf("tag = %+v, want B", byTag)
	}

	// A is still retrievable by digest, byte for byte the first record.
	byDigest, err := svc.Query(Query{Kind: QueryByDigest, Repository: "team/app", Digest: digestA})
	if err != nil {
		t.Fatalf("by digest: %v", err)
	}
	if len(byDigest) != 1 || byDigest[0] != first.Artifact {
		t.Fatalf("digest lookup = %+v, want original A %+v", byDigest, first.Artifact)
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

	one, err := svc.Query(Query{Kind: QueryByTag, Repository: "repo/one", Tag: "latest"})
	if err != nil {
		t.Fatalf("repo one tag: %v", err)
	}
	two, err := svc.Query(Query{Kind: QueryByTag, Repository: "repo/two", Tag: "latest"})
	if err != nil {
		t.Fatalf("repo two tag: %v", err)
	}
	if one[0].Repository != "repo/one" || two[0].Repository != "repo/two" {
		t.Fatalf("tag lookups crossed repositories: %+v %+v", one, two)
	}

	// A conflict in repo two leaves repo one's record untouched.
	conflict := inputTwo
	conflict.SizeBytes = 4096
	if _, err := svc.Register(conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("cross-repo conflict error = %v, want ErrConflict", err)
	}
	oneAgain, err := svc.Query(Query{Kind: QueryByDigest, Repository: "repo/one", Digest: digestA})
	if err != nil || oneAgain[0].SizeBytes != inputOne.SizeBytes {
		t.Fatalf("repo one changed after repo two conflict: %+v %v", oneAgain, err)
	}

	oneList, err := svc.Query(Query{Kind: QueryByRepository, Repository: "repo/one"})
	if err != nil {
		t.Fatalf("repo one list: %v", err)
	}
	twoList, err := svc.Query(Query{Kind: QueryByRepository, Repository: "repo/two"})
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

	cases := map[string]Query{
		"unknown repository": {Kind: QueryByRepository, Repository: "other/repo"},
		"unknown tag":        {Kind: QueryByTag, Repository: "team/app", Tag: "missing"},
		"unknown digest":     {Kind: QueryByDigest, Repository: "team/app", Digest: digestB},
	}
	for name, query := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := svc.Query(query); !errors.Is(err, ErrNotFound) {
				t.Fatalf("error = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestStorageFailureIsNotNotFound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	svc := New(st)
	if _, err := svc.Register(validInput("team/app", digestA, "latest")); err != nil {
		t.Fatalf("seed register: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	// Every operation reports ErrStorage, even lookups that would otherwise hit.
	if _, err := svc.Register(validInput("team/app", digestB, "latest")); !errors.Is(err, ErrStorage) {
		t.Fatalf("register error = %v, want ErrStorage", err)
	}
	for name, query := range map[string]Query{
		"list":   {Kind: QueryByRepository, Repository: "team/app"},
		"tag":    {Kind: QueryByTag, Repository: "team/app", Tag: "latest"},
		"digest": {Kind: QueryByDigest, Repository: "team/app", Digest: digestA},
		"miss":   {Kind: QueryByDigest, Repository: "team/app", Digest: digestB},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := svc.Query(query); !errors.Is(err, ErrStorage) {
				t.Fatalf("query error = %v, want ErrStorage", err)
			}
		})
	}
}

// TestRecordsSurviveReopen uses an existing SQLite file without conversion and
// checks records, first-registration order, push times and tag pointers.
func TestRecordsSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")

	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	svc := New(st)
	inputA := validInput("team/app", digestA, "latest")
	inputB := validInput("team/app", digestB, "latest")
	inputB.SizeBytes = 2048
	first, err := svc.Register(inputA)
	if err != nil {
		t.Fatalf("register A: %v", err)
	}
	second, err := svc.Register(inputB)
	if err != nil {
		t.Fatalf("register B: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { reopened.Close() })
	svc = New(reopened)

	list, err := svc.Query(Query{Kind: QueryByRepository, Repository: "team/app"})
	if err != nil {
		t.Fatalf("list after reopen: %v", err)
	}
	if len(list) != 2 ||
		list[0].Digest != digestA || list[0].PushedAt != first.Artifact.PushedAt ||
		list[1].Digest != digestB || list[1].PushedAt != second.Artifact.PushedAt {
		t.Fatalf("records/order/timestamps changed after reopen: %+v", list)
	}

	byTag, err := svc.Query(Query{Kind: QueryByTag, Repository: "team/app", Tag: "latest"})
	if err != nil {
		t.Fatalf("tag after reopen: %v", err)
	}
	if len(byTag) != 1 || byTag[0].Digest != digestB || byTag[0].PushedAt != second.Artifact.PushedAt {
		t.Fatalf("tag pointer changed after reopen: %+v", byTag)
	}

	byDigest, err := svc.Query(Query{Kind: QueryByDigest, Repository: "team/app", Digest: digestA})
	if err != nil {
		t.Fatalf("digest after reopen: %v", err)
	}
	if len(byDigest) != 1 || byDigest[0] != first.Artifact {
		t.Fatalf("A record changed after reopen: %+v", byDigest)
	}
}
