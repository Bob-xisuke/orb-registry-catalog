package service_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/Bob-xisuke/orb-registry-catalog/internal/service"
	"github.com/Bob-xisuke/orb-registry-catalog/internal/store"
)

const (
	sqliteDigestA = "sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	sqliteDigestB = "sha256:" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// newSQLiteService opens a real SQLite file and wires the bundled adapter into
// a business Service through the service.Store port, returning the handle so
// tests can close or reopen the same file.
func newSQLiteService(t *testing.T) (*service.Service, *store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "service.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return service.New(st), st, path
}

func sqliteValidInput(repo, digest, tag string) service.RegisterInput {
	return service.RegisterInput{
		Repository:        repo,
		Digest:            digest,
		Tag:               tag,
		SignatureVerified: true,
		RetentionDays:     30,
		SizeBytes:         1024,
	}
}

// TestSQLiteRegisterAndQuery drives registration plus all three query kinds
// through the business layer against the real adapter, including the
// created/duplicate distinction and the tag pointer that moves only on a new
// record.
func TestSQLiteRegisterAndQuery(t *testing.T) {
	svc, st, _ := newSQLiteService(t)
	t.Cleanup(func() { st.Close() })

	inputA := sqliteValidInput("team/app", sqliteDigestA, "latest")
	inputB := sqliteValidInput("team/app", sqliteDigestB, "latest")
	inputB.SizeBytes = 2048

	first, err := svc.Register(inputA)
	if err != nil {
		t.Fatalf("register A: %v", err)
	}
	if first.Outcome != service.OutcomeCreated {
		t.Fatalf("first outcome = %v, want OutcomeCreated", first.Outcome)
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

	list, err := svc.Query(service.Query{Kind: service.QueryByRepository, Repository: "team/app"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 || list[0].Digest != sqliteDigestA || list[1].Digest != sqliteDigestB {
		t.Fatalf("list = %+v, want [A B] in registration order", list)
	}

	byTag, err := svc.Query(service.Query{Kind: service.QueryByTag, Repository: "team/app", Tag: "latest"})
	if err != nil || len(byTag) != 1 || byTag[0].Digest != sqliteDigestB {
		t.Fatalf("tag query = %+v, %v; want one record pointing at B", byTag, err)
	}

	byDigest, err := svc.Query(service.Query{Kind: service.QueryByDigest, Repository: "team/app", Digest: sqliteDigestA})
	if err != nil || len(byDigest) != 1 || byDigest[0] != first.Artifact {
		t.Fatalf("digest query = %+v, %v; want the original A", byDigest, err)
	}

	// A content conflict on the real adapter is reported as ErrConflict.
	bad := inputA
	bad.SizeBytes = 4096
	if _, err := svc.Register(bad); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("conflict error = %v, want ErrConflict", err)
	}
}

// TestSQLiteStorageFailureIsNotNotFound closes the underlying file so the
// adapter fails; the service must classify every call as ErrStorage, even a
// lookup that would otherwise miss.
func TestSQLiteStorageFailureIsNotNotFound(t *testing.T) {
	svc, st, _ := newSQLiteService(t)
	if _, err := svc.Register(sqliteValidInput("team/app", sqliteDigestA, "latest")); err != nil {
		t.Fatalf("seed register: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	if _, err := svc.Register(sqliteValidInput("team/app", sqliteDigestB, "latest")); !errors.Is(err, service.ErrStorage) {
		t.Fatalf("register error = %v, want ErrStorage", err)
	}
	for name, query := range map[string]service.Query{
		"list":   {Kind: service.QueryByRepository, Repository: "team/app"},
		"tag":    {Kind: service.QueryByTag, Repository: "team/app", Tag: "latest"},
		"digest": {Kind: service.QueryByDigest, Repository: "team/app", Digest: sqliteDigestA},
		"miss":   {Kind: service.QueryByDigest, Repository: "team/app", Digest: sqliteDigestB},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := svc.Query(query); !errors.Is(err, service.ErrStorage) {
				t.Fatalf("query error = %v, want ErrStorage", err)
			}
		})
	}
}

// TestSQLiteRecordsSurviveReopen reopens an existing SQLite file without
// conversion and checks records, first-registration order, push times and tag
// pointers are all intact.
func TestSQLiteRecordsSurviveReopen(t *testing.T) {
	svc, st, path := newSQLiteService(t)
	inputA := sqliteValidInput("team/app", sqliteDigestA, "latest")
	inputB := sqliteValidInput("team/app", sqliteDigestB, "latest")
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
	svc = service.New(reopened)

	list, err := svc.Query(service.Query{Kind: service.QueryByRepository, Repository: "team/app"})
	if err != nil {
		t.Fatalf("list after reopen: %v", err)
	}
	if len(list) != 2 ||
		list[0].Digest != sqliteDigestA || list[0].PushedAt != first.Artifact.PushedAt ||
		list[1].Digest != sqliteDigestB || list[1].PushedAt != second.Artifact.PushedAt {
		t.Fatalf("records/order/timestamps changed after reopen: %+v", list)
	}

	byTag, err := svc.Query(service.Query{Kind: service.QueryByTag, Repository: "team/app", Tag: "latest"})
	if err != nil {
		t.Fatalf("tag after reopen: %v", err)
	}
	if len(byTag) != 1 || byTag[0].Digest != sqliteDigestB || byTag[0].PushedAt != second.Artifact.PushedAt {
		t.Fatalf("tag pointer changed after reopen: %+v", byTag)
	}

	byDigest, err := svc.Query(service.Query{Kind: service.QueryByDigest, Repository: "team/app", Digest: sqliteDigestA})
	if err != nil {
		t.Fatalf("digest after reopen: %v", err)
	}
	if len(byDigest) != 1 || byDigest[0] != first.Artifact {
		t.Fatalf("A record changed after reopen: %+v", byDigest)
	}
}
