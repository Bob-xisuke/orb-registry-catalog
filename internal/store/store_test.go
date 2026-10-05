package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func sampleArtifact() Artifact {
	return Artifact{
		Repository:        "team/app",
		Digest:            "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Tag:               "latest",
		SignatureVerified: true,
		RetentionDays:     30,
		SizeBytes:         1024,
		PushedAt:          "2026-10-05T00:00:00Z",
	}
}

func TestOpenCreatesUsableStore(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	if err := st.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

func TestRegisterThenQueryByTagAndDigest(t *testing.T) {
	st := openTemp(t)
	want := sampleArtifact()

	stored, err := st.RegisterArtifact(want)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if stored != want {
		t.Fatalf("stored = %+v, want %+v", stored, want)
	}

	byTag, err := st.GetArtifactByTag(want.Repository, want.Tag)
	if err != nil {
		t.Fatalf("by tag: %v", err)
	}
	if byTag != want {
		t.Fatalf("by tag = %+v, want %+v", byTag, want)
	}

	byDigest, err := st.GetArtifactByDigest(want.Repository, want.Digest)
	if err != nil {
		t.Fatalf("by digest: %v", err)
	}
	if byDigest != want {
		t.Fatalf("by digest = %+v, want %+v", byDigest, want)
	}
}

func TestRegisterDuplicateReturnsOriginal(t *testing.T) {
	st := openTemp(t)
	first := sampleArtifact()
	if _, err := st.RegisterArtifact(first); err != nil {
		t.Fatalf("register: %v", err)
	}

	again := first
	again.PushedAt = "2026-10-06T00:00:00Z"
	stored, err := st.RegisterArtifact(again)
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if stored != first {
		t.Fatalf("stored = %+v, want original %+v", stored, first)
	}
}

func TestRegisterConflict(t *testing.T) {
	st := openTemp(t)
	first := sampleArtifact()
	if _, err := st.RegisterArtifact(first); err != nil {
		t.Fatalf("register: %v", err)
	}

	changed := first
	changed.SizeBytes = 2048
	if _, err := st.RegisterArtifact(changed); !errors.Is(err, ErrArtifactConflict) {
		t.Fatalf("err = %v, want ErrArtifactConflict", err)
	}
}

func TestTagMovesToNewDigestAndOldRecordSurvives(t *testing.T) {
	st := openTemp(t)
	first := sampleArtifact()
	if _, err := st.RegisterArtifact(first); err != nil {
		t.Fatalf("register first: %v", err)
	}

	second := first
	second.Digest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	second.PushedAt = "2026-10-06T00:00:00Z"
	if _, err := st.RegisterArtifact(second); err != nil {
		t.Fatalf("register second: %v", err)
	}

	current, err := st.GetArtifactByTag(first.Repository, first.Tag)
	if err != nil {
		t.Fatalf("by tag: %v", err)
	}
	if current != second {
		t.Fatalf("tag points at %+v, want %+v", current, second)
	}

	old, err := st.GetArtifactByDigest(first.Repository, first.Digest)
	if err != nil {
		t.Fatalf("old record by digest: %v", err)
	}
	if old != first {
		t.Fatalf("old record = %+v, want %+v", old, first)
	}

	// Re-registering the old record must not move the tag back.
	if _, err := st.RegisterArtifact(first); err != nil {
		t.Fatalf("re-register first: %v", err)
	}
	current, err = st.GetArtifactByTag(first.Repository, first.Tag)
	if err != nil {
		t.Fatalf("by tag after retry: %v", err)
	}
	if current != second {
		t.Fatalf("tag moved back to %+v, want %+v", current, second)
	}
}

func TestListArtifactsOrdersByFirstRegistration(t *testing.T) {
	st := openTemp(t)
	first := sampleArtifact()
	second := first
	second.Digest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	second.Tag = "stable"
	other := first
	other.Repository = "team/other"

	for _, a := range []Artifact{second, first, other} {
		if _, err := st.RegisterArtifact(a); err != nil {
			t.Fatalf("register: %v", err)
		}
	}

	list, err := st.ListArtifacts(first.Repository)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 || list[0] != second || list[1] != first {
		t.Fatalf("list = %+v", list)
	}
}

func TestMissingQueriesReturnNotFound(t *testing.T) {
	st := openTemp(t)
	if _, err := st.GetArtifactByTag("team/app", "latest"); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("tag err = %v, want ErrArtifactNotFound", err)
	}
	if _, err := st.GetArtifactByDigest("team/app", sampleArtifact().Digest); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("digest err = %v, want ErrArtifactNotFound", err)
	}
	list, err := st.ListArtifacts("team/app")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("list = %+v, want empty", list)
	}
}

func TestRecordsSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	want := sampleArtifact()
	if _, err := st.RegisterArtifact(want); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	got, err := reopened.GetArtifactByTag(want.Repository, want.Tag)
	if err != nil {
		t.Fatalf("by tag after reopen: %v", err)
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}
