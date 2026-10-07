package api

// This regression case pins how POST /v1/artifacts behaves while another
// connection holds the bundled SQLite adapter's single writer lock — without
// touching any artifact record or tag pointer. It runs against a real SQLite
// file (no in-memory stand-in and no fake store): the production *store.Store
// serves every HTTP request, and a second database/sql connection to the same
// file acts only as a test harness that takes and releases the writer lock.
// Every observable result is asserted through the public HTTP surface; the
// harness never reads results back.
//
// The lock-contention conclusions belong to the bundled SQLite adapter only.
// A caller-supplied service.Store provides the same guarantees solely through
// its own implementation.

import (
	"net/http"
	"testing"
)

// TestSQLiteWriteLockContentionOutcomes registers A, then holds the SQLite
// writer lock from a second connection while four registration requests run
// their divergent outcomes: a brand-new artifact B is 503 storage_unavailable,
// a verbatim retry of A is 201 with the original record, A with only
// size_bytes changed is 409, and an illegal digest is 400. After every request
// the three query kinds must still see committed A alone. Once the lock is
// released, the same B request succeeds and the published ordering, tag and
// digest semantics hold.
func TestSQLiteWriteLockContentionOutcomes(t *testing.T) {
	router, harness := newVisibilityRouter(t)

	// Phase 0: A is registered and committed through POST /v1/artifacts; the
	// tag points at A.
	first := postArtifact(t, router,
		registration(sequenceRepo, testDigestA, sequenceTag, true, regRetention, regSizeA))
	if first.Code != http.StatusCreated {
		t.Fatalf("register A status = %d, want %d (%s)", first.Code, http.StatusCreated, first.Body)
	}
	pushedA := decodeBody(t, first)["pushed_at"].(string)

	// assertOnlyCommittedA runs all three query kinds through the public GET
	// entry and requires committed state to be exactly "A alone": the list
	// holds only A, the tag resolves to A with A's original pushed_at, A's
	// digest returns the record byte-for-byte as first registered, and B's
	// digest is a legal identity matching nothing. Every hit must be 200 with
	// the published {"artifacts":[...]} array shape.
	assertOnlyCommittedA := func() {
		t.Helper()

		list := queryList(t, router, sequenceRepo)
		if list.Code != http.StatusOK {
			t.Fatalf("list status = %d, want %d (%s)", list.Code, http.StatusOK, list.Body)
		}
		if digests := recordDigests(allRecords(t, list)); len(digests) != 1 || digests[0] != testDigestA {
			t.Fatalf("list = %v, want [A] only", digests)
		}

		byTag := queryByTag(t, router, sequenceRepo, sequenceTag)
		if byTag.Code != http.StatusOK {
			t.Fatalf("tag query status = %d, want %d (%s)", byTag.Code, http.StatusOK, byTag.Body)
		}
		if tagged := soleRecord(t, byTag); tagged["digest"] != testDigestA || tagged["pushed_at"] != pushedA {
			t.Fatalf("tag = %v, want committed A (%s)", tagged, pushedA)
		}

		byDigestA := queryByDigest(t, router, sequenceRepo, testDigestA)
		if byDigestA.Code != http.StatusOK {
			t.Fatalf("digest A query status = %d, want %d (%s)", byDigestA.Code, http.StatusOK, byDigestA.Body)
		}
		expectStoredRecord(t, soleRecord(t, byDigestA),
			sequenceRepo, testDigestA, sequenceTag, true,
			float64(regRetention), float64(regSizeA), pushedA)

		assertErrorResponse(t, queryByDigest(t, router, sequenceRepo, testDigestB),
			http.StatusNotFound, codeNotFound, msgNotFound)
	}

	// Phase 1: the harness connection takes SQLite's single writer lock by
	// writing one service_metadata row — no artifact record and no tag
	// pointer is touched. waitForPendingWriteLock then proves from a third
	// connection that the lock is genuinely held before any request is made.
	tx, err := harness.Begin()
	if err != nil {
		t.Fatalf("harness begin: %v", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO service_metadata (key, value) VALUES ('write-lock-holder', 'held')`); err != nil {
		t.Fatalf("harness take writer lock: %v", err)
	}
	waitForPendingWriteLock(t, harness)

	// Phase 2a: a brand-new artifact B needs the INSERT at store.go:66-71,
	// which cannot run while the writer lock is held -> 503
	// storage_unavailable with the fixed message; the lock error text never
	// reaches the body. Committed state is untouched afterwards.
	assertErrorResponse(t, postArtifact(t, router,
		registration(sequenceRepo, testDigestB, sequenceTag, true, regRetention, regSizeB)),
		http.StatusServiceUnavailable, codeStorage, msgStorage)
	assertOnlyCommittedA()

	// Phase 2b: a verbatim retry of A resolves by reading the existing row
	// and comparing (store.go:61-63, 86-90) — no write statement, so no
	// writer lock is needed even while it is held elsewhere. The response is
	// 201 with A's original record; pushed_at must not change.
	retry := postArtifact(t, router,
		registration(sequenceRepo, testDigestA, sequenceTag, true, regRetention, regSizeA))
	if retry.Code != http.StatusCreated {
		t.Fatalf("retry A during lock status = %d, want %d (%s)", retry.Code, http.StatusCreated, retry.Body)
	}
	expectStoredRecord(t, decodeBody(t, retry),
		sequenceRepo, testDigestA, sequenceTag, true,
		float64(regRetention), float64(regSizeA), pushedA)
	assertOnlyCommittedA()

	// Phase 2c: A with only size_bytes changed is still a legal input; the
	// identity lookup and field comparison are reads (store.go:86-92), so the
	// held writer lock does not degrade the conflict into a 503 — it stays
	// 409 ArtifactConflictError and writes nothing.
	assertErrorResponse(t, postArtifact(t, router,
		registration(sequenceRepo, testDigestA, sequenceTag, true, regRetention, regSizeB)),
		http.StatusConflict, codeConflict, msgConflict)
	assertOnlyCommittedA()

	// Phase 2d: an illegal digest is rejected by input validation before any
	// storage access (artifacts.go:40-44), lock or no lock -> 400
	// InvalidArtifactInputError.
	assertErrorResponse(t, postArtifact(t, router,
		registration(sequenceRepo, "sha256:not-hex", sequenceTag, true, regRetention, regSizeB)),
		http.StatusBadRequest, codeInvalidInput, msgInvalidReg)
	assertOnlyCommittedA()

	// Phase 3: release the writer lock and prove from outside that it is
	// gone — the same no-op probe write that failed while the lock was held
	// must now succeed.
	if err := tx.Rollback(); err != nil {
		t.Fatalf("harness release writer lock: %v", err)
	}
	if _, err := harness.Exec(
		`UPDATE service_metadata SET value = value WHERE key = 'visibility-probe'`); err != nil {
		t.Fatalf("writer lock still held after harness rollback: %v", err)
	}

	// Phase 4: the exact B request that was 503 under the lock now registers
	// normally: 201, the list is [A, B] in first-registration order, the tag
	// moves to B, and A is still fully retrievable by digest.
	recovered := postArtifact(t, router,
		registration(sequenceRepo, testDigestB, sequenceTag, true, regRetention, regSizeB))
	if recovered.Code != http.StatusCreated {
		t.Fatalf("register B after lock release status = %d, want %d (%s)",
			recovered.Code, http.StatusCreated, recovered.Body)
	}
	pushedB := decodeBody(t, recovered)["pushed_at"].(string)

	records := allRecords(t, queryList(t, router, sequenceRepo))
	if digests := recordDigests(records); len(digests) != 2 ||
		digests[0] != testDigestA || digests[1] != testDigestB {
		t.Fatalf("list after lock release = %v, want [A B] in first-registration order", digests)
	}
	if records[0].(map[string]any)["pushed_at"] != pushedA {
		t.Fatalf("A record changed when B registered: %v", records[0])
	}
	expectStoredRecord(t, soleRecord(t, queryByTag(t, router, sequenceRepo, sequenceTag)),
		sequenceRepo, testDigestB, sequenceTag, true,
		float64(regRetention), float64(regSizeB), pushedB)
	expectStoredRecord(t, soleRecord(t, queryByDigest(t, router, sequenceRepo, testDigestA)),
		sequenceRepo, testDigestA, sequenceTag, true,
		float64(regRetention), float64(regSizeA), pushedA)

	// Retrying A verbatim is still 201 with A's original record, and the tag
	// pointer does not move back.
	retryAfter := postArtifact(t, router,
		registration(sequenceRepo, testDigestA, sequenceTag, true, regRetention, regSizeA))
	if retryAfter.Code != http.StatusCreated {
		t.Fatalf("retry A after lock release status = %d, want %d (%s)",
			retryAfter.Code, http.StatusCreated, retryAfter.Body)
	}
	expectStoredRecord(t, decodeBody(t, retryAfter),
		sequenceRepo, testDigestA, sequenceTag, true,
		float64(regRetention), float64(regSizeA), pushedA)
	if got := soleRecord(t, queryByTag(t, router, sequenceRepo, sequenceTag)); got["digest"] != testDigestB {
		t.Fatalf("retrying A moved the tag pointer to %v, want B", got["digest"])
	}
}
