package api

// These cases drive the business layer (service.Service) over the real SQLite
// file through the production composition adapter newSQLiteStore. The
// in-memory double in internal/service verifies business outcomes without a
// database; these verify the default backend actually satisfies the port —
// including reopening an existing file untouched.

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/Bob-xisuke/orb-registry-catalog/internal/service"
	"github.com/Bob-xisuke/orb-registry-catalog/internal/store"
)

func newAdaptedService(t *testing.T) (*service.Service, *store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "service.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return service.New(newSQLiteStore(st)), st, path
}

func adaptedInput(repo, digest, tag string) service.RegisterInput {
	return service.RegisterInput{
		Repository:        repo,
		Digest:            digest,
		Tag:               tag,
		SignatureVerified: true,
		RetentionDays:     30,
		SizeBytes:         1024,
	}
}

// TestSQLiteAdapterRegisterAndQuery pins the port mapping on the real engine:
// created versus identical duplicate, 409-class conflict, the tag pointer
// move/no-move rule, registration-order listing and the wrapped miss.
func TestSQLiteAdapterRegisterAndQuery(t *testing.T) {
	svc, _, _ := newAdaptedService(t)
	inputA := adaptedInput("team/app", testDigestA, "latest")
	inputB := adaptedInput("team/app", testDigestB, "latest")
	inputB.SizeBytes = 2048

	first, err := svc.Register(inputA)
	if err != nil {
		t.Fatalf("register A: %v", err)
	}
	if first.Outcome != service.OutcomeCreated {
		t.Fatalf("first outcome = %v, want OutcomeCreated", first.Outcome)
	}
	if first.Artifact.Repository != "team/app" || first.Artifact.Digest != testDigestA ||
		first.Artifact.Tag != "latest" || first.Artifact.PushedAt == "" {
		t.Fatalf("unexpected first record: %+v", first.Artifact)
	}

	second, err := svc.Register(inputB)
	if err != nil {
		t.Fatalf("register B: %v", err)
	}
	if second.Outcome != service.OutcomeCreated {
		t.Fatalf("second outcome = %v, want OutcomeCreated", second.Outcome)
	}

	// Identical retry: same record, same pushed_at, duplicate outcome.
	retry, err := svc.Register(inputA)
	if err != nil {
		t.Fatalf("retry A: %v", err)
	}
	if retry.Outcome != service.OutcomeDuplicate || retry.Artifact != first.Artifact {
		t.Fatalf("retry = %+v, want duplicate of %+v", retry, first.Artifact)
	}

	// Changed content for the existing identity is a conflict and changes
	// neither the record nor the tag pointer (still B after the move).
	conflict := inputA
	conflict.SizeBytes = 4096
	if _, err := svc.Register(conflict); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("conflict error = %v, want ErrConflict", err)
	}
	byTag, err := svc.Query(service.Query{Kind: service.QueryByTag, Repository: "team/app", Tag: "latest"})
	if err != nil || len(byTag) != 1 || byTag[0].Digest != testDigestB {
		t.Fatalf("tag pointer changed after conflict: %+v %v", byTag, err)
	}

	// Listing keeps first-registration order through the adapter.
	list, err := svc.Query(service.Query{Kind: service.QueryByRepository, Repository: "team/app"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 || list[0].Digest != testDigestA || list[1].Digest != testDigestB {
		t.Fatalf("list = %+v, want [A B] in registration order", list)
	}

	// Digest lookup and the three miss shapes all map onto service.ErrNotFound.
	unknownDigest := "sha256:" + string(repeatByte('c', 64))
	byDigest, err := svc.Query(service.Query{Kind: service.QueryByDigest, Repository: "team/app", Digest: testDigestA})
	if err != nil || len(byDigest) != 1 || byDigest[0] != first.Artifact {
		t.Fatalf("digest lookup = %+v %v, want original A", byDigest, err)
	}
	for name, query := range map[string]service.Query{
		"unknown repo":   {Kind: service.QueryByRepository, Repository: "other/repo"},
		"unknown tag":    {Kind: service.QueryByTag, Repository: "team/app", Tag: "missing"},
		"unknown digest": {Kind: service.QueryByDigest, Repository: "team/app", Digest: unknownDigest},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := svc.Query(query); !errors.Is(err, service.ErrNotFound) {
				t.Fatalf("error = %v, want ErrNotFound", err)
			}
		})
	}
}

// repeatByte returns n copies of b without pulling in strings at call sites
// that already store the hex elsewhere.
func repeatByte(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

// TestSQLiteAdapterStorageFailureClassified closes the underlying file and
// checks every business call reports ErrStorage — including a lookup that
// would be a miss while open, proving faults never degrade to ErrNotFound.
func TestSQLiteAdapterStorageFailureClassified(t *testing.T) {
	svc, st, _ := newAdaptedService(t)
	if _, err := svc.Register(adaptedInput("team/app", testDigestA, "latest")); err != nil {
		t.Fatalf("seed register: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if _, err := svc.Register(adaptedInput("team/app", testDigestB, "latest")); !errors.Is(err, service.ErrStorage) {
		t.Fatalf("register error = %v, want ErrStorage", err)
	}
	for name, query := range map[string]service.Query{
		"list":   {Kind: service.QueryByRepository, Repository: "team/app"},
		"tag":    {Kind: service.QueryByTag, Repository: "team/app", Tag: "latest"},
		"digest": {Kind: service.QueryByDigest, Repository: "team/app", Digest: testDigestA},
		"miss":   {Kind: service.QueryByDigest, Repository: "team/app", Digest: "sha256:" + string(repeatByte('c', 64))},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.Query(query)
			if !errors.Is(err, service.ErrStorage) {
				t.Fatalf("query error = %v, want ErrStorage", err)
			}
			if errors.Is(err, service.ErrNotFound) {
				t.Fatalf("closed database must not look like a miss: %v", err)
			}
		})
	}
}

// TestSQLiteAdapterReopenPreservesState reopens an existing file through a
// fresh adapter/service with no conversion: records, first-registration
// order, service-generated push times and the tag pointer all survive.
func TestSQLiteAdapterReopenPreservesState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")

	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	svc := service.New(newSQLiteStore(st))
	inputA := adaptedInput("team/app", testDigestA, "latest")
	inputB := adaptedInput("team/app", testDigestB, "latest")
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
		t.Fatalf("close before reopen: %v", err)
	}

	reopened, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { reopened.Close() })
	svc = service.New(newSQLiteStore(reopened))

	list, err := svc.Query(service.Query{Kind: service.QueryByRepository, Repository: "team/app"})
	if err != nil {
		t.Fatalf("list after reopen: %v", err)
	}
	if len(list) != 2 ||
		list[0].Digest != testDigestA || list[0].PushedAt != first.Artifact.PushedAt ||
		list[1].Digest != testDigestB || list[1].PushedAt != second.Artifact.PushedAt {
		t.Fatalf("records/order/timestamps changed after reopen: %+v", list)
	}

	byTag, err := svc.Query(service.Query{Kind: service.QueryByTag, Repository: "team/app", Tag: "latest"})
	if err != nil {
		t.Fatalf("tag after reopen: %v", err)
	}
	if len(byTag) != 1 || byTag[0].Digest != testDigestB || byTag[0].PushedAt != second.Artifact.PushedAt {
		t.Fatalf("tag pointer changed after reopen: %+v", byTag)
	}

	byDigest, err := svc.Query(service.Query{Kind: service.QueryByDigest, Repository: "team/app", Digest: testDigestA})
	if err != nil {
		t.Fatalf("digest after reopen: %v", err)
	}
	if len(byDigest) != 1 || byDigest[0] != first.Artifact {
		t.Fatalf("A record changed after reopen: %+v", byDigest)
	}
}
