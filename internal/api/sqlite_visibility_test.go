package api

// These regression cases pin the visibility of the bundled SQLite adapter's
// registration transaction while it is still open and after it rolls back.
// They run against a real SQLite file (no in-memory stand-in and no fake
// store): the production *store.Store serves every HTTP request, and a second
// database/sql connection to the same file acts only as a test harness that
// holds a registration-shaped transaction open or installs a trigger that
// makes the tag-pointer update fail. Every observable result is asserted
// through the public HTTP surface; the harness never reads results back.
//
// The visibility conclusions belong to the bundled SQLite adapter only. A
// caller-supplied service.Store provides the same guarantees solely through
// its own implementation.

import (
	"database/sql"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Bob-xisuke/orb-registry-catalog/internal/store"
	_ "modernc.org/sqlite"
)

// newVisibilityRouter opens a real SQLite file, wires the production router
// onto it, and returns a second independent connection pool to the same file
// for fault injection. Both handles are closed by test cleanup.
func newVisibilityRouter(t *testing.T) (*gin.Engine, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "service.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	harness, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open harness connection: %v", err)
	}
	t.Cleanup(func() { harness.Close() })
	return NewRouter(st), harness
}

// waitForPendingWriteLock proves, from outside the open transaction, that the
// writer connection currently holds SQLite's single writer lock. A no-op
// autocommit write issued on another connection must fail "database is
// locked"; once that has been observed the pending transaction is live across
// connections, so the HTTP reads that follow genuinely race an in-flight
// registration rather than a finished one.
func waitForPendingWriteLock(t *testing.T, db *sql.DB) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, err := db.Exec(`UPDATE service_metadata SET value = value WHERE key = 'visibility-probe'`)
		if err != nil && strings.Contains(strings.ToLower(err.Error()), "locked") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("open transaction never held the SQLite writer lock (last probe error: %v)", err)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestSQLiteUncommittedRegistrationIsInvisible registers A through the public
// entry, then holds B's record insert and tag-pointer move inside an
// uncommitted transaction mirroring store.go:66-77. Until commit, every query
// kind must answer as if B did not exist and A's record were untouched; after
// commit, fresh independent queries observe B atomically, in first-
// registration order, and retrying A still returns A's original record while
// the tag stays on B.
func TestSQLiteUncommittedRegistrationIsInvisible(t *testing.T) {
	router, harness := newVisibilityRouter(t)

	// Phase 0: A is registered and committed through POST /v1/artifacts.
	first := postArtifact(t, router,
		registration(sequenceRepo, testDigestA, sequenceTag, true, regRetention, regSizeA))
	if first.Code != http.StatusCreated {
		t.Fatalf("register A status = %d, want %d (%s)", first.Code, http.StatusCreated, first.Body)
	}
	recordA := decodeBody(t, first)
	pushedA := recordA["pushed_at"].(string)

	// Phase 1: a second connection performs the exact two writes from
	// Store.Register's created branch (store.go:66-77) but stops short of the
	// Commit at store.go:78. B's row and the tag move are both pending.
	tx, err := harness.Begin()
	if err != nil {
		t.Fatalf("harness begin: %v", err)
	}
	pushedB := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.Exec(
		`INSERT INTO artifacts (repository, digest, tag, signature_verified, retention_days, size_bytes, pushed_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		sequenceRepo, testDigestB, sequenceTag, 1, regRetention, regSizeB, pushedB); err != nil {
		t.Fatalf("harness insert B: %v", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO tag_pointers (repository, tag, digest) VALUES (?, ?, ?)
		 ON CONFLICT (repository, tag) DO UPDATE SET digest = excluded.digest`,
		sequenceRepo, sequenceTag, testDigestB); err != nil {
		t.Fatalf("harness move tag pointer: %v", err)
	}
	waitForPendingWriteLock(t, harness)

	// While the transaction is open the repository list is 200 with only A.
	preCommitList := queryList(t, router, sequenceRepo)
	if preCommitList.Code != http.StatusOK {
		t.Fatalf("list during open transaction status = %d (%s)", preCommitList.Code, preCommitList.Body)
	}
	if digests := recordDigests(allRecords(t, preCommitList)); len(digests) != 1 || digests[0] != testDigestA {
		t.Fatalf("list during open transaction = %v, want [A] only", digests)
	}

	// The tag still resolves to committed A with A's original pushed_at.
	tagged := soleRecord(t, queryByTag(t, router, sequenceRepo, sequenceTag))
	if tagged["digest"] != testDigestA || tagged["pushed_at"] != pushedA {
		t.Fatalf("tag during open transaction = %v, want committed A (%s)", tagged, pushedA)
	}

	// A's record is byte-for-byte the first registration at every phase.
	expectStoredRecord(t, soleRecord(t, queryByDigest(t, router, sequenceRepo, testDigestA)),
		sequenceRepo, testDigestA, sequenceTag, true,
		float64(regRetention), float64(regSizeA), pushedA)

	// B's digest is a legal identity that simply matches no committed record.
	assertErrorResponse(t, queryByDigest(t, router, sequenceRepo, testDigestB),
		http.StatusNotFound, codeNotFound, msgNotFound)

	// Phase 2: the registration transaction commits. Fresh independent queries
	// now observe both changes together: record visible and pointer moved.
	if err := tx.Commit(); err != nil {
		t.Fatalf("harness commit: %v", err)
	}

	records := allRecords(t, queryList(t, router, sequenceRepo))
	if digests := recordDigests(records); len(digests) != 2 ||
		digests[0] != testDigestA || digests[1] != testDigestB {
		t.Fatalf("list after commit = %v, want [A B] in first-registration order", digests)
	}
	if records[0].(map[string]any)["pushed_at"] != pushedA {
		t.Fatalf("A record changed when B committed: %v", records[0])
	}

	pointedB := soleRecord(t, queryByTag(t, router, sequenceRepo, sequenceTag))
	expectStoredRecord(t, pointedB, sequenceRepo, testDigestB, sequenceTag, true,
		float64(regRetention), float64(regSizeB), pushedB)

	expectStoredRecord(t, soleRecord(t, queryByDigest(t, router, sequenceRepo, testDigestA)),
		sequenceRepo, testDigestA, sequenceTag, true,
		float64(regRetention), float64(regSizeA), pushedA)
	expectStoredRecord(t, soleRecord(t, queryByDigest(t, router, sequenceRepo, testDigestB)),
		sequenceRepo, testDigestB, sequenceTag, true,
		float64(regRetention), float64(regSizeB), pushedB)

	// Cross-request boundary: the list response captured BEFORE commit contains
	// only A, while a SEPARATE tag query issued AFTER commit hits B. Each query
	// is its own autocommit read; nothing promises one snapshot spanning the two
	// requests, so this pairing is a legal result rather than a contradiction.
	if digests := recordDigests(allRecords(t, preCommitList)); len(digests) != 1 || digests[0] != testDigestA {
		t.Fatalf("earlier list response changed after commit: %v", digests)
	}
	if laterTag := soleRecord(t, queryByTag(t, router, sequenceRepo, sequenceTag)); laterTag["digest"] != testDigestB {
		t.Fatalf("later tag query = %v, want committed B", laterTag["digest"])
	}

	// Phase 3: after B commits, retrying A verbatim is still 201 with A's
	// original record (pushed_at included) and never moves the pointer back.
	retry := postArtifact(t, router,
		registration(sequenceRepo, testDigestA, sequenceTag, true, regRetention, regSizeA))
	if retry.Code != http.StatusCreated {
		t.Fatalf("retry A after B committed status = %d, want %d (%s)", retry.Code, http.StatusCreated, retry.Body)
	}
	expectStoredRecord(t, decodeBody(t, retry), sequenceRepo, testDigestA, sequenceTag, true,
		float64(regRetention), float64(regSizeA), pushedA)
	if got := soleRecord(t, queryByTag(t, router, sequenceRepo, sequenceTag)); got["digest"] != testDigestB {
		t.Fatalf("retrying A moved the tag pointer to %v, want B", got["digest"])
	}
	if digests := recordDigests(allRecords(t, queryList(t, router, sequenceRepo))); len(digests) != 2 ||
		digests[0] != testDigestA || digests[1] != testDigestB {
		t.Fatalf("list after retry = %v, want unchanged [A B]", digests)
	}
}

// TestSQLiteRolledBackRegistrationIsInvisible models "B's row is written but
// the tag-pointer update fails". A test-only trigger rejects the upsert's
// UPDATE path, so the genuine POST /v1/artifacts request runs Store.Register's
// real failure branch: insert succeeds, tag move errors, defer tx.Rollback()
// (store.go:59) undoes the insert, and the client receives 503
// storage_unavailable. After the rollback only committed A must remain
// visible; once the trigger is dropped, the same B registers normally, which
// proves the connection pool and schema are still usable.
func TestSQLiteRolledBackRegistrationIsInvisible(t *testing.T) {
	router, harness := newVisibilityRouter(t)

	// Phase 0: A is committed and the tag points at A.
	first := postArtifact(t, router,
		registration(sequenceRepo, testDigestA, sequenceTag, true, regRetention, regSizeA))
	if first.Code != http.StatusCreated {
		t.Fatalf("register A status = %d (%s)", first.Code, first.Body)
	}
	pushedA := decodeBody(t, first)["pushed_at"].(string)

	// Phase 1: install a trigger that aborts only the tag-pointer move. The
	// artifacts insert is unaffected, so Register reaches "row written, tag
	// update failed" exactly as the failure scenario describes.
	if _, err := harness.Exec(`
		CREATE TRIGGER fail_tag_move BEFORE UPDATE ON tag_pointers
		BEGIN
			SELECT RAISE(ABORT, 'injected tag pointer update failure');
		END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	// Phase 2: the real registration request fails inside its transaction and
	// answers the fixed 503 shape; the injected SQL text never reaches the body.
	failed := postArtifact(t, router,
		registration(sequenceRepo, testDigestB, sequenceTag, true, regRetention, regSizeB))
	assertErrorResponse(t, failed, http.StatusServiceUnavailable, codeStorage, msgStorage)

	// Phase 3: the rolled-back transaction is indistinguishable from nothing:
	// the list still holds only A, the pointer still resolves to A with A's
	// pushed_at, A's record is intact, and B's digest misses with 404.
	if digests := recordDigests(allRecords(t, queryList(t, router, sequenceRepo))); len(digests) != 1 ||
		digests[0] != testDigestA {
		t.Fatalf("list after rollback = %v, want [A] only", digests)
	}
	tagged := soleRecord(t, queryByTag(t, router, sequenceRepo, sequenceTag))
	if tagged["digest"] != testDigestA || tagged["pushed_at"] != pushedA {
		t.Fatalf("tag after rollback = %v, want committed A (%s)", tagged, pushedA)
	}
	expectStoredRecord(t, soleRecord(t, queryByDigest(t, router, sequenceRepo, testDigestA)),
		sequenceRepo, testDigestA, sequenceTag, true,
		float64(regRetention), float64(regSizeA), pushedA)
	assertErrorResponse(t, queryByDigest(t, router, sequenceRepo, testDigestB),
		http.StatusNotFound, codeNotFound, msgNotFound)

	// Phase 4: remove the injected fault and register B again. The store is
	// fully usable post-rollback: B commits, the order is [A B] and the tag
	// moves to B.
	if _, err := harness.Exec(`DROP TRIGGER fail_tag_move`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	recovered := postArtifact(t, router,
		registration(sequenceRepo, testDigestB, sequenceTag, true, regRetention, regSizeB))
	if recovered.Code != http.StatusCreated {
		t.Fatalf("register B after recovery status = %d, want %d (%s)",
			recovered.Code, http.StatusCreated, recovered.Body)
	}
	if digests := recordDigests(allRecords(t, queryList(t, router, sequenceRepo))); len(digests) != 2 ||
		digests[0] != testDigestA || digests[1] != testDigestB {
		t.Fatalf("list after recovery = %v, want [A B]", digests)
	}
	if got := soleRecord(t, queryByTag(t, router, sequenceRepo, sequenceTag)); got["digest"] != testDigestB {
		t.Fatalf("tag after recovery = %v, want B", got["digest"])
	}
}
