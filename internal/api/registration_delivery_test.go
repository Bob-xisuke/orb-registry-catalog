package api

// These regression cases pin the boundary of one POST /v1/artifacts
// registration from request cancellation to response delivery, on both
// router construction entries (NewRouter and NewRouterWithStore) backed by
// the bundled SQLite store. They keep three things the analysis in
// docs/registration-delivery-boundary-analysis.md separates:
//
//  1. the request context being cancelled before processing starts;
//  2. the request body failing to read with a non-EOF error;
//  3. the client never receiving the success response after commit.
//
// Every observable result is asserted through the public HTTP surface —
// POST /v1/artifacts and GET /v1/artifacts — and no case relies on sleeps
// or network-cut timing. The committed-state conclusions are those of the
// bundled SQLite adapter; a caller-supplied service.Store provides the same
// guarantees solely through its own implementation.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/Bob-xisuke/orb-registry-catalog/internal/store"
)

// deliveryEntries builds one router per published construction entry, each
// over its own bundled SQLite store, so every scenario runs identically
// through both assemblies.
func deliveryEntries(t *testing.T) map[string]*gin.Engine {
	t.Helper()
	entries := map[string]*gin.Engine{}
	for name, build := range map[string]func(*store.Store) *gin.Engine{
		"NewRouter":          NewRouter,
		"NewRouterWithStore": func(st *store.Store) *gin.Engine { return NewRouterWithStore(st, st.Ping) },
	} {
		st, err := store.Open(filepath.Join(t.TempDir(), "service.db"))
		if err != nil {
			t.Fatalf("open store for %s: %v", name, err)
		}
		t.Cleanup(func() { st.Close() })
		entries[name] = build(st)
	}
	return entries
}

// TestCancelledContextRegistrationCompletes cancels the request context
// before the handler runs, with a complete legal body still readable and
// storage available. The registration must still commit: the server
// generates the 201 response, and a later independent GET observes the
// single record with the tag pointing at it. A cancelled request context is
// a client-side delivery fact, not a processing signal — nothing on the
// registration path (parse, business call, transaction, response render)
// consults it.
func TestCancelledContextRegistrationCompletes(t *testing.T) {
	for name, router := range deliveryEntries(t) {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel() // cancelled before processing starts

			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/artifacts",
				strings.NewReader(registration(sequenceRepo, testDigestA, sequenceTag, true, regRetention, regSizeA)))
			router.ServeHTTP(recorder, request.WithContext(ctx))

			// The server-side result is the published 201 with the record.
			if recorder.Code != http.StatusCreated {
				t.Fatalf("cancelled-context register status = %d, want %d (%s)",
					recorder.Code, http.StatusCreated, recorder.Body)
			}
			pushedA, _ := decodeBody(t, recorder)["pushed_at"].(string)
			if pushedA == "" {
				t.Fatalf("cancelled-context register response has no pushed_at: %s", recorder.Body)
			}

			// A fresh, uncancelled GET observes exactly one record, and the
			// tag points at it with the same pushed_at.
			records := allRecords(t, queryList(t, router, sequenceRepo))
			if digests := recordDigests(records); len(digests) != 1 || digests[0] != testDigestA {
				t.Fatalf("list after cancelled-context register = %v, want [A] only", digests)
			}
			tagged := soleRecord(t, queryByTag(t, router, sequenceRepo, sequenceTag))
			if tagged["digest"] != testDigestA || tagged["pushed_at"] != pushedA {
				t.Fatalf("tag after cancelled-context register = %v, want A (%s)", tagged, pushedA)
			}
		})
	}
}

// failAfterReader yields the given bytes across reads and then fails every
// further read with err. It models a transport that delivered bytes and
// then broke: with a complete JSON object in data, the failure surfaces on
// the read that must confirm end-of-body; with a partial object, it
// surfaces in the middle of decoding.
type failAfterReader struct {
	data []byte
	err  error
}

func (f *failAfterReader) Read(p []byte) (int, error) {
	if len(f.data) == 0 {
		return 0, f.err
	}
	n := copy(p, f.data)
	f.data = f.data[n:]
	return n, nil
}

// TestBodyReadFailureRejectsWithoutStoreWrite seeds committed A, then posts
// B through a body whose reads fail with a non-EOF error — once after the
// complete JSON object has already been delivered (the end-of-body check
// fails), once in the middle of the object. Both must be 400
// InvalidArtifactInputError, decided before any store write: no new record
// appears, and A's record and the tag pointer are byte-for-byte unchanged.
func TestBodyReadFailureRejectsWithoutStoreWrite(t *testing.T) {
	for name, router := range deliveryEntries(t) {
		t.Run(name, func(t *testing.T) {
			first := postArtifact(t, router,
				registration(sequenceRepo, testDigestA, sequenceTag, true, regRetention, regSizeA))
			if first.Code != http.StatusCreated {
				t.Fatalf("register A status = %d (%s)", first.Code, first.Body)
			}
			pushedA := decodeBody(t, first)["pushed_at"].(string)

			// assertCommittedAOnly requires the public read surface to show
			// exactly the state before the rejected request: the list holds
			// only A, the tag resolves to A with A's original pushed_at, A's
			// record is intact, and B's digest is a legal identity matching
			// nothing.
			assertCommittedAOnly := func() {
				t.Helper()
				if digests := recordDigests(allRecords(t, queryList(t, router, sequenceRepo))); len(digests) != 1 ||
					digests[0] != testDigestA {
					t.Fatalf("list after rejected body = %v, want [A] only", digests)
				}
				tagged := soleRecord(t, queryByTag(t, router, sequenceRepo, sequenceTag))
				if tagged["digest"] != testDigestA || tagged["pushed_at"] != pushedA {
					t.Fatalf("tag after rejected body = %v, want committed A (%s)", tagged, pushedA)
				}
				expectStoredRecord(t, soleRecord(t, queryByDigest(t, router, sequenceRepo, testDigestA)),
					sequenceRepo, testDigestA, sequenceTag, true,
					float64(regRetention), float64(regSizeA), pushedA)
				assertErrorResponse(t, queryByDigest(t, router, sequenceRepo, testDigestB),
					http.StatusNotFound, codeNotFound, msgNotFound)
			}

			bodyB := registration(sequenceRepo, testDigestB, sequenceTag, true, regRetention, regSizeB)

			// The complete JSON object for B is delivered, then the read
			// that must confirm end-of-body fails with a non-EOF error.
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/artifacts",
				&failAfterReader{data: []byte(bodyB), err: errors.New("injected body read failure")})
			router.ServeHTTP(recorder, request)
			assertErrorResponse(t, recorder, http.StatusBadRequest, codeInvalidInput, msgInvalidReg)
			assertCommittedAOnly()

			// A read failure in the middle of the object takes the same 400
			// path and likewise leaves nothing behind.
			recorder = httptest.NewRecorder()
			request = httptest.NewRequest(http.MethodPost, "/v1/artifacts",
				&failAfterReader{data: []byte(bodyB[:len(bodyB)/2]), err: errors.New("injected body read failure")})
			router.ServeHTTP(recorder, request)
			assertErrorResponse(t, recorder, http.StatusBadRequest, codeInvalidInput, msgInvalidReg)
			assertCommittedAOnly()
		})
	}
}

// failingWriter is an http.ResponseWriter whose Write always fails, standing
// in for a client connection that breaks while the response body is being
// written. The status line may already have left the server; the body never
// arrives.
type failingWriter struct {
	http.ResponseWriter
	err error
}

func (w failingWriter) Write(p []byte) (int, error) { return 0, w.err }

// TestResponseWriteFailureKeepsCommittedRecord drives the first registration
// of A through a writer that fails while the 201 body is being written. The
// registration transaction has already committed at that point, so the
// failed delivery is not a rollback: a later independent GET returns A. The
// scenario a real client in this position would run then follows the
// published rules unchanged — B registers under the same tag, a verbatim
// retry of A returns 201 with A's original pushed_at, changed content is
// 409, a legal miss is 404, and none of them disturbs committed state.
func TestResponseWriteFailureKeepsCommittedRecord(t *testing.T) {
	for name, router := range deliveryEntries(t) {
		t.Run(name, func(t *testing.T) {
			bodyA := registration(sequenceRepo, testDigestA, sequenceTag, true, regRetention, regSizeA)

			// Phase 1: A commits server-side, but the 201 response cannot be
			// delivered — the body write fails. The server generated a 201
			// status line; what the disconnected client actually received is
			// not something the server can promise.
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/artifacts", strings.NewReader(bodyA))
			router.ServeHTTP(failingWriter{recorder, errors.New("injected response write failure")}, request)
			if recorder.Code != http.StatusCreated {
				t.Fatalf("server-side status for undelivered register = %d, want %d",
					recorder.Code, http.StatusCreated)
			}
			if recorder.Body.Len() != 0 {
				t.Fatalf("response body reached the broken client: %s", recorder.Body)
			}

			// The committed record is observable through a fresh GET. A's
			// original pushed_at is taken from the query response, since the
			// 201 body never arrived anywhere.
			records := allRecords(t, queryList(t, router, sequenceRepo))
			if digests := recordDigests(records); len(digests) != 1 || digests[0] != testDigestA {
				t.Fatalf("list after undelivered 201 = %v, want [A] only", digests)
			}
			pushedA, _ := records[0].(map[string]any)["pushed_at"].(string)
			if pushedA == "" {
				t.Fatalf("committed A has no pushed_at: %v", records[0])
			}
			if tagged := soleRecord(t, queryByTag(t, router, sequenceRepo, sequenceTag)); tagged["digest"] != testDigestA {
				t.Fatalf("tag after undelivered 201 = %v, want A", tagged)
			}

			// Phase 2: B registers under the same repository and tag through
			// a healthy connection; the tag pointer moves to B.
			second := postArtifact(t, router,
				registration(sequenceRepo, testDigestB, sequenceTag, true, regRetention, regSizeB))
			if second.Code != http.StatusCreated {
				t.Fatalf("register B status = %d (%s)", second.Code, second.Body)
			}
			pushedB := decodeBody(t, second)["pushed_at"].(string)

			// Phase 3: the client retries the original A request verbatim —
			// exactly what a client that never saw the 201 would do. It gets
			// 201 with A's original pushed_at; the list keeps [A, B] in
			// first-registration order, the tag stays on B, and both digests
			// resolve.
			retry := postArtifact(t, router, bodyA)
			if retry.Code != http.StatusCreated {
				t.Fatalf("retry A status = %d (%s)", retry.Code, retry.Body)
			}
			if got := decodeBody(t, retry)["pushed_at"]; got != pushedA {
				t.Fatalf("retry pushed_at = %v, want original %v", got, pushedA)
			}
			if digests := recordDigests(allRecords(t, queryList(t, router, sequenceRepo))); len(digests) != 2 ||
				digests[0] != testDigestA || digests[1] != testDigestB {
				t.Fatalf("list after retry = %v, want [A B] in first-registration order", digests)
			}
			expectStoredRecord(t, soleRecord(t, queryByTag(t, router, sequenceRepo, sequenceTag)),
				sequenceRepo, testDigestB, sequenceTag, true,
				float64(regRetention), float64(regSizeB), pushedB)
			expectStoredRecord(t, soleRecord(t, queryByDigest(t, router, sequenceRepo, testDigestA)),
				sequenceRepo, testDigestA, sequenceTag, true,
				float64(regRetention), float64(regSizeA), pushedA)
			expectStoredRecord(t, soleRecord(t, queryByDigest(t, router, sequenceRepo, testDigestB)),
				sequenceRepo, testDigestB, sequenceTag, true,
				float64(regRetention), float64(regSizeB), pushedB)

			// Phase 4: A with only size_bytes changed is 409
			// ArtifactConflictError and writes nothing.
			assertErrorResponse(t, postArtifact(t, router,
				registration(sequenceRepo, testDigestA, sequenceTag, true, regRetention, regSizeB)),
				http.StatusConflict, codeConflict, msgConflict)

			// Phase 5: a legal query for an unknown digest is 404
			// ArtifactNotFoundError and changes nothing.
			assertErrorResponse(t, queryByDigest(t, router, sequenceRepo, testDigestC),
				http.StatusNotFound, codeNotFound, msgNotFound)

			// Committed state after both rejections is byte-for-byte the
			// state after phase 3.
			if digests := recordDigests(allRecords(t, queryList(t, router, sequenceRepo))); len(digests) != 2 ||
				digests[0] != testDigestA || digests[1] != testDigestB {
				t.Fatalf("list after 409/404 = %v, want unchanged [A B]", digests)
			}
			expectStoredRecord(t, soleRecord(t, queryByTag(t, router, sequenceRepo, sequenceTag)),
				sequenceRepo, testDigestB, sequenceTag, true,
				float64(regRetention), float64(regSizeB), pushedB)
			expectStoredRecord(t, soleRecord(t, queryByDigest(t, router, sequenceRepo, testDigestA)),
				sequenceRepo, testDigestA, sequenceTag, true,
				float64(regRetention), float64(regSizeA), pushedA)
		})
	}
}
