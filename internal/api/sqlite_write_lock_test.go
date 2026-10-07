package api

// This regression case pins what POST /v1/artifacts resolves to when another
// connection holds SQLite's single writer lock without itself writing either
// business table. The lock holder only reserves the writer against a fixture
// table (service_metadata); it neither inserts an artifacts row nor moves a tag
// pointer, so every committed artifact stays at A for the whole window.
//
// Together with sqlite_visibility_test.go it separates two causes of a 503
// on the bundled SQLite adapter:
//
//   - sqlite_visibility_test.go: the request's OWN transaction has written a
//     row and then fails/rolls back, or is merely uncommitted (visibility);
//   - this file: ANOTHER connection owns the writer lock, so the request never
//     reaches its identity lookup in the first place (contention).
//
// The lock holder below begins a transaction on the harness connection and
// runs one INSERT into the fixture table service_metadata (created by the
// schema at store.go:149-152, read by no production code): that INSERT
// upgrades the transaction to SQLite's reserved writer even when the table
// was empty. It performs no write against artifacts or tag_pointers, so
// every committed artifact stays at A for the whole window. The INSERTed row
// disappears on rollback and is harmless on commit because the database
// lives in t.TempDir(). waitForPendingWriteLock (sqlite_visibility_test.go:
// 56-69), issued on another pooled connection, proves from outside that the
// writer lock is really held in between.
//
// Input validation still runs before the store is touched, and the duplicate
// and conflict branches issue no write, so neither waits on the lock.

import (
	"net/http"
	"testing"
)

// TestSQLiteWriteLockContentionDistinguishesRegisterOutcomes holds the writer
// lock on a second connection while it leaves both business tables untouched,
// then drives four registrations through the public entry: a different legal
// digest B (must write), an identical retry of A (no write), a same-identity
// submission that differs only in size_bytes (no write, conflict), and a
// malformed digest (rejected before any store call). Only the registration
// that must acquire the writer fails, with the fixed storage error.
func TestSQLiteWriteLockContentionDistinguishesRegisterOutcomes(t *testing.T) {
	router, harness := newVisibilityRouter(t)

	// Phase 0: A is registered and committed; the tag points at A.
	first := postArtifact(t, router,
		registration(sequenceRepo, testDigestA, sequenceTag, true, regRetention, regSizeA))
	if first.Code != http.StatusCreated {
		t.Fatalf("register A status = %d, want %d (%s)", first.Code, http.StatusCreated, first.Body)
	}
	recordA := decodeBody(t, first)
	pushedA := recordA["pushed_at"].(string)

	// Phase 1: another connection reserves the writer lock with one INSERT
	// into the fixture table, without touching the artifacts or tag_pointers
	// rows. The INSERT upgrades the transaction to SQLite's reserved writer;
	// nothing is committed.
	tx, err := harness.Begin()
	if err != nil {
		t.Fatalf("harness begin: %v", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO service_metadata (key, value) VALUES ('lock-contention-probe', 'held')`); err != nil {
		t.Fatalf("harness reserve writer lock: %v", err)
	}
	waitForPendingWriteLock(t, harness)
	t.Cleanup(func() { tx.Rollback() })

	// B is a different legal identity: Register must run the artifacts
	// INSERT and the tag-pointer upsert (store.go:66-77). It cannot take the
	// writer lock, so Begin fails before the identity lookup ever runs; the
	// service collapses every store error to ErrStorage and the handler
	// answers the fixed 503 shape.
	blocked := postArtifact(t, router,
		registration(sequenceRepo, testDigestB, sequenceTag, true, regRetention, regSizeB))
	assertErrorResponse(t, blocked, http.StatusServiceUnavailable, codeStorage, msgStorage)

	// Every failed request leaves committed state exactly at A.
	if digests := recordDigests(allRecords(t, queryList(t, router, sequenceRepo))); len(digests) != 1 ||
		digests[0] != testDigestA {
		t.Fatalf("list after blocked B = %v, want [A] only", digests)
	}

	// Re-submitting A verbatim needs no write: the identity lookup is a read
	// (allowed under WAL while one writer is active), the row compares equal
	// (store.go:86-90), and the handler returns the stored record with its
	// original pushed_at. The lock never blocks it.
	retry := postArtifact(t, router,
		registration(sequenceRepo, testDigestA, sequenceTag, true, regRetention, regSizeA))
	if retry.Code != http.StatusCreated {
		t.Fatalf("retry A while locked status = %d, want %d (%s)", retry.Code, http.StatusCreated, retry.Body)
	}
	expectStoredRecord(t, decodeBody(t, retry), sequenceRepo, testDigestA, sequenceTag, true,
		float64(regRetention), float64(regSizeA), pushedA)
	if got := soleRecord(t, queryByTag(t, router, sequenceRepo, sequenceTag)); got["digest"] != testDigestA {
		t.Fatalf("tag while locked = %v, want committed A", got["digest"])
	}
	if digests := recordDigests(allRecords(t, queryList(t, router, sequenceRepo))); len(digests) != 1 ||
		digests[0] != testDigestA {
		t.Fatalf("list after retry while locked = %v, want [A] only", digests)
	}

	// Same identity, different comparable content (size_bytes only): the
	// lookup still succeeds as a read and the mismatch is reported as conflict
	// (store.go:92) with no write attempted, so it too bypasses the lock.
	conflict := postArtifact(t, router,
		registration(sequenceRepo, testDigestA, sequenceTag, true, regRetention, regSizeB))
	assertErrorResponse(t, conflict, http.StatusConflict, codeConflict, msgConflict)
	if digests := recordDigests(allRecords(t, queryList(t, router, sequenceRepo))); len(digests) != 1 ||
		digests[0] != testDigestA {
		t.Fatalf("list after conflict while locked = %v, want [A] only", digests)
	}

	// A malformed digest is rejected by the shared input entry before the
	// service or store is reached (artifacts.go:40-44); lock or no lock the
	// answer is 400 and no state changes.
	invalid := postArtifact(t, router,
		registration(sequenceRepo, "sha256:not-hex", sequenceTag, true, regRetention, regSizeB))
	assertErrorResponse(t, invalid, http.StatusBadRequest, codeInvalidInput, msgInvalidReg)
	if digests := recordDigests(allRecords(t, queryList(t, router, sequenceRepo))); len(digests) != 1 ||
		digests[0] != testDigestA {
		t.Fatalf("list after invalid while locked = %v, want [A] only", digests)
	}

	// B's digest is still a legal identity that matches no committed record, so
	// the read it would need also answers 404 while B remains unwritten.
	assertErrorResponse(t, queryByDigest(t, router, sequenceRepo, testDigestB),
		http.StatusNotFound, codeNotFound, msgNotFound)

	// Phase 2: release the writer. The identical B request now runs its full
	// transaction: 201, B visible in list order, and the tag moves to B.
	if err := tx.Commit(); err != nil {
		t.Fatalf("harness commit: %v", err)
	}
	registered := postArtifact(t, router,
		registration(sequenceRepo, testDigestB, sequenceTag, true, regRetention, regSizeB))
	if registered.Code != http.StatusCreated {
		t.Fatalf("register B after release status = %d, want %d (%s)",
			registered.Code, http.StatusCreated, registered.Body)
	}
	if digests := recordDigests(allRecords(t, queryList(t, router, sequenceRepo))); len(digests) != 2 ||
		digests[0] != testDigestA || digests[1] != testDigestB {
		t.Fatalf("list after release = %v, want [A B] in first-registration order", digests)
	}
	if got := soleRecord(t, queryByTag(t, router, sequenceRepo, sequenceTag)); got["digest"] != testDigestB {
		t.Fatalf("tag after release = %v, want B", got["digest"])
	}

	// Retrying A after B commits is still the duplicate branch: its original
	// record and pushed_at come back and the tag pointer never moves back.
	again := postArtifact(t, router,
		registration(sequenceRepo, testDigestA, sequenceTag, true, regRetention, regSizeA))
	if again.Code != http.StatusCreated {
		t.Fatalf("retry A after B released status = %d, want %d (%s)", again.Code, http.StatusCreated, again.Body)
	}
	expectStoredRecord(t, decodeBody(t, again), sequenceRepo, testDigestA, sequenceTag, true,
		float64(regRetention), float64(regSizeA), pushedA)
	if got := soleRecord(t, queryByTag(t, router, sequenceRepo, sequenceTag)); got["digest"] != testDigestB {
		t.Fatalf("retrying A moved the tag pointer to %v, want B", got["digest"])
	}
	if digests := recordDigests(allRecords(t, queryList(t, router, sequenceRepo))); len(digests) != 2 ||
		digests[0] != testDigestA || digests[1] != testDigestB {
		t.Fatalf("list after final retry = %v, want unchanged [A B]", digests)
	}
}
