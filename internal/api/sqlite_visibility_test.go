package api

// These regression cases pin the SQLite transaction-visibility conclusions in
// docs/sqlite-transaction-visibility-analysis.md. They run against a real
// SQLite file through the public HTTP surface (POST/GET /v1/artifacts).
//
// Store.Register (internal/store/store.go) keeps its write transaction
// internal, so a test cannot park that function halfway. Instead each case
// opens a SECOND, independent database/sql handle on the same file and issues
// the very two write statements Register issues (INSERT artifacts, then the
// tag_pointers upsert), leaving them in exactly the transaction stage the
// scenario names. Every read still goes through the real router, service and
// SQLite adapter on the store's own connections.

import (
	"database/sql"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	_ "modernc.org/sqlite"
)

// parkedPushedB is the pushed_at written for B by the parked transaction. It
// is a plain TEXT value the production INSERT accepts (store.go:66-69); a
// fixed literal makes "the record is byte-for-byte unchanged" assertions
// exact.
const parkedPushedB = "2026-01-02T03:04:05Z"

// sqliteVisibilityHandle opens a second, independent database/sql handle on
// the same SQLite file the router store uses. It performs no setup the
// production Open does not: a WAL database stays WAL for every connection.
func sqliteVisibilityHandle(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open visibility handle: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// parkUncommittedB runs B's registration writes — the same INSERT into
// artifacts and the same tag_pointers upsert as store.Register (store.go:66-77)
// — but stops before COMMIT, so B's row and the tag move exist only inside the
// returned, still-open transaction.
func parkUncommittedB(t *testing.T, db *sql.DB) *sql.Tx {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin parked transaction: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() }) // harmless once committed
	if _, err := tx.Exec(
		`INSERT INTO artifacts
		    (repository, digest, tag, signature_verified, retention_days, size_bytes, pushed_at)
		 VALUES (?, ?, ?, 1, ?, ?, ?)`,
		sequenceRepo, testDigestB, sequenceTag, regRetention, regSizeB, parkedPushedB); err != nil {
		t.Fatalf("insert B in parked transaction: %v", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO tag_pointers (repository, tag, digest) VALUES (?, ?, ?)
		 ON CONFLICT (repository, tag) DO UPDATE SET digest = excluded.digest`,
		sequenceRepo, sequenceTag, testDigestB); err != nil {
		t.Fatalf("move tag pointer in parked transaction: %v", err)
	}
	return tx
}

// assertOnlyAIsVisible is the committed-state expectation shared by every
// pre-commit stage: the list holds only A in registration order, the tag still
// resolves to A (with A's original pushed_at), B is a 404 miss, and A's record
// keeps every field it had when it was first registered.
func assertOnlyAIsVisible(t *testing.T, router *gin.Engine, pushedA string) {
	t.Helper()
	records := allRecords(t, queryList(t, router, sequenceRepo))
	if digests := recordDigests(records); len(digests) != 1 || digests[0] != testDigestA {
		t.Fatalf("list = %v, want only %s before commit", digests, testDigestA)
	}
	expectStoredRecord(t, records[0].(map[string]any), sequenceRepo, testDigestA, sequenceTag, true,
		float64(regRetention), float64(regSizeA), pushedA)

	tagged := soleRecord(t, queryByTag(t, router, sequenceRepo, sequenceTag))
	if tagged["digest"] != testDigestA {
		t.Fatalf("tag resolves to %v, want still-committed %s", tagged["digest"], testDigestA)
	}
	expectStoredRecord(t, tagged, sequenceRepo, testDigestA, sequenceTag, true,
		float64(regRetention), float64(regSizeA), pushedA)

	assertErrorResponse(t, queryByDigest(t, router, sequenceRepo, testDigestB),
		http.StatusNotFound, codeNotFound, msgNotFound)

	expectStoredRecord(t, soleRecord(t, queryByDigest(t, router, sequenceRepo, testDigestA)),
		sequenceRepo, testDigestA, sequenceTag, true,
		float64(regRetention), float64(regSizeA), pushedA)
}

// TestUncommittedArtifactTransactionInvisibleUntilCommit parks B's
// registration writes (record insert + tag pointer move) inside an open
// transaction on a real SQLite file and proves the HTTP queries read only
// committed state at every stage: nothing mid-transaction, both changes after
// commit, and an identical retry of A afterwards still returns the original
// record without moving the pointer back.
func TestUncommittedArtifactTransactionInvisibleUntilCommit(t *testing.T) {
	router, path := newTestRouter(t)
	db := sqliteVisibilityHandle(t, path)

	// A is registered first and committed through the public entry.
	first := postArtifact(t, router,
		registration(sequenceRepo, testDigestA, sequenceTag, true, regRetention, regSizeA))
	if first.Code != http.StatusCreated {
		t.Fatalf("register A status = %d, want 201 (%s)", first.Code, first.Body)
	}
	pushedA := decodeBody(t, first)["pushed_at"].(string)

	// Stage 1: B's row is inserted and the tag pointer moved, but the
	// transaction has NOT committed. Reads on other connections must see A
	// alone — the dangling pointer and B's digest both stay invisible.
	tx := parkUncommittedB(t, db)
	assertOnlyAIsVisible(t, router, pushedA)

	// Stage 2: the writer commits. Fresh, independent HTTP queries now observe
	// B: first-registration order is A then B, the tag JOIN resolves B, and
	// both digest lookups return their own original record.
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit parked transaction: %v", err)
	}

	records := allRecords(t, queryList(t, router, sequenceRepo))
	if digests := recordDigests(records); len(digests) != 2 ||
		digests[0] != testDigestA || digests[1] != testDigestB {
		t.Fatalf("list after commit = %v, want [A B] in registration order", digests)
	}
	expectStoredRecord(t, records[0].(map[string]any), sequenceRepo, testDigestA, sequenceTag, true,
		float64(regRetention), float64(regSizeA), pushedA)
	expectStoredRecord(t, records[1].(map[string]any), sequenceRepo, testDigestB, sequenceTag, true,
		float64(regRetention), float64(regSizeB), parkedPushedB)

	tagged := soleRecord(t, queryByTag(t, router, sequenceRepo, sequenceTag))
	expectStoredRecord(t, tagged, sequenceRepo, testDigestB, sequenceTag, true,
		float64(regRetention), float64(regSizeB), parkedPushedB)

	expectStoredRecord(t, soleRecord(t, queryByDigest(t, router, sequenceRepo, testDigestA)),
		sequenceRepo, testDigestA, sequenceTag, true,
		float64(regRetention), float64(regSizeA), pushedA)
	expectStoredRecord(t, soleRecord(t, queryByDigest(t, router, sequenceRepo, testDigestB)),
		sequenceRepo, testDigestB, sequenceTag, true,
		float64(regRetention), float64(regSizeB), parkedPushedB)

	// Stage 3: retrying A after B is committed is still 201 with A's original
	// record and pushed_at; the duplicate path performs no write and the tag
	// stays on B.
	retry := postArtifact(t, router,
		registration(sequenceRepo, testDigestA, sequenceTag, true, regRetention, regSizeA))
	if retry.Code != http.StatusCreated {
		t.Fatalf("retry A status = %d, want 201 (%s)", retry.Code, retry.Body)
	}
	expectStoredRecord(t, decodeBody(t, retry), sequenceRepo, testDigestA, sequenceTag, true,
		float64(regRetention), float64(regSizeA), pushedA)
	if got := soleRecord(t, queryByTag(t, router, sequenceRepo, sequenceTag)); got["digest"] != testDigestB {
		t.Fatalf("retrying A moved the pointer back: tag = %v, want %s", got["digest"], testDigestB)
	}
}

// TestIndependentQueriesObserveCommitBetweenThem pins the cross-request
// boundary: each GET is one autocommit SELECT with no snapshot shared with any
// other request. When B commits between two independently issued queries, the
// earlier list answering [A] and the later tag query answering B are both
// legitimate — the service never promised the two the same snapshot.
func TestIndependentQueriesObserveCommitBetweenThem(t *testing.T) {
	router, path := newTestRouter(t)
	db := sqliteVisibilityHandle(t, path)

	first := postArtifact(t, router,
		registration(sequenceRepo, testDigestA, sequenceTag, true, regRetention, regSizeA))
	if first.Code != http.StatusCreated {
		t.Fatalf("register A status = %d (%s)", first.Code, first.Body)
	}
	pushedA := decodeBody(t, first)["pushed_at"].(string)

	// Query 1 finishes while B's transaction is still open: only A.
	tx := parkUncommittedB(t, db)
	listBefore := queryList(t, router, sequenceRepo)
	if digests := recordDigests(allRecords(t, listBefore)); len(digests) != 1 || digests[0] != testDigestA {
		t.Fatalf("query 1 list = %v, want [A]", digests)
	}

	// The commit happens strictly between the two independent HTTP queries.
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit between queries: %v", err)
	}

	// Query 2 starts afterwards and is free to observe the committed move.
	tagAfter := soleRecord(t, queryByTag(t, router, sequenceRepo, sequenceTag))
	expectStoredRecord(t, tagAfter, sequenceRepo, testDigestB, sequenceTag, true,
		float64(regRetention), float64(regSizeB), parkedPushedB)

	// Query 1's already-finished answer is not retroactively changed, and A's
	// original record remains intact.
	expectStoredRecord(t, soleRecord(t, queryByDigest(t, router, sequenceRepo, testDigestA)),
		sequenceRepo, testDigestA, sequenceTag, true,
		float64(regRetention), float64(regSizeA), pushedA)
}

// TestTagPointerMoveFailureRollsBackRegistrationWith503 forces the SECOND
// write of a real POST registration to fail: after A is committed, a
// test-only trigger (installed on the temporary database file, never in the
// production schema) raises once the tag pointer moves. Register's artifact
// INSERT has already succeeded inside its transaction at that point, so the
// case proves the deferred rollback undoes the inserted row and the service
// surfaces 503 storage_unavailable with the fixed message — never 201 — with
// only A visible afterwards.
func TestTagPointerMoveFailureRollsBackRegistrationWith503(t *testing.T) {
	router, path := newTestRouter(t)
	db := sqliteVisibilityHandle(t, path)

	first := postArtifact(t, router,
		registration(sequenceRepo, testDigestA, sequenceTag, true, regRetention, regSizeA))
	if first.Code != http.StatusCreated {
		t.Fatalf("register A status = %d, want 201 (%s)", first.Code, first.Body)
	}
	pushedA := decodeBody(t, first)["pushed_at"].(string)

	// Fault injection scoped to this temporary file: any tag pointer change
	// after this point fails at the database. The upsert hits the existing
	// (repository, tag) row A created and takes its DO UPDATE branch, so the
	// BEFORE UPDATE trigger is the one that raises; BEFORE INSERT covers a
	// hypothetical fresh-tag path as well.
	for _, ddl := range []string{
		`CREATE TRIGGER fail_tag_pointer_insert
		 BEFORE INSERT ON tag_pointers
		 BEGIN SELECT RAISE(FAIL, 'forced tag pointer failure'); END;`,
		`CREATE TRIGGER fail_tag_pointer_update
		 BEFORE UPDATE ON tag_pointers
		 BEGIN SELECT RAISE(FAIL, 'forced tag pointer failure'); END;`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("install fault trigger: %v", err)
		}
	}

	// B's real registration: artifact row inserted, tag move fails, the
	// transaction rolls back (store.go:59), the service maps the error to
	// ErrStorage (service.go:170-172) and the router answers 503 with the
	// fixed storage_unavailable object (artifacts.go:177-178).
	failed := postArtifact(t, router,
		registration(sequenceRepo, testDigestB, sequenceTag, true, regRetention, regSizeB))
	assertErrorResponse(t, failed, http.StatusServiceUnavailable, codeStorage, msgStorage)

	// After the rollback the registered B row never existed as far as any
	// committed reader is concerned: only A remains, the tag still points at
	// A with its original pushed_at, and B is a 404 miss.
	assertOnlyAIsVisible(t, router, pushedA)
}
