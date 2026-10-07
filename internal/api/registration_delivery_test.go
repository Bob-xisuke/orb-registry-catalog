package api

// These regression cases back docs/registration-delivery-boundary-analysis.md:
// the boundary of one POST /v1/artifacts registration from a cancelled request
// context, through a failing request-body read, to a response that never
// reaches the client. Every scenario is reproduced deterministically — an
// already-cancelled context, a reader that fails after delivering a complete
// JSON object, and a ResponseWriter whose Write always fails — so no case
// depends on sleeps or network teardown timing. All routers are assembled over
// the bundled SQLite store through both public construction entries
// (NewRouter and NewRouterWithStore), and every post-condition is checked
// through the public GET /v1/artifacts responses, never through store
// internals or a caller-supplied service.Store's contract.

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

// eachSQLiteEntry runs the same scenario against both public router
// construction entries, each backed by its own bundled SQLite store.
func eachSQLiteEntry(t *testing.T, run func(t *testing.T, router *gin.Engine)) {
	t.Helper()
	entries := []struct {
		name  string
		build func(st *store.Store) *gin.Engine
	}{
		{"NewRouter", func(st *store.Store) *gin.Engine { return NewRouter(st) }},
		{"NewRouterWithStore", func(st *store.Store) *gin.Engine { return NewRouterWithStore(st, st.Ping) }},
	}
	for _, entry := range entries {
		t.Run(entry.name, func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "service.db"))
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			t.Cleanup(func() { st.Close() })
			run(t, entry.build(st))
		})
	}
}

// readThenFailReader delivers the complete body first and then fails every
// read with a non-EOF error, reproducing a transport failure that strikes
// after the client has sent a full JSON object.
type readThenFailReader struct {
	body *strings.Reader
	err  error
}

func (r *readThenFailReader) Read(p []byte) (int, error) {
	if r.body.Len() > 0 {
		return r.body.Read(p)
	}
	return 0, r.err
}

// failOnWriteResponseWriter accepts headers and the status line but fails the
// body write, reproducing a client that is gone by the time the committed
// response is delivered. The server-side status it recorded is what the
// handler generated; nothing here models what the client received.
type failOnWriteResponseWriter struct {
	header http.Header
	status int
	err    error
}

func (w *failOnWriteResponseWriter) Header() http.Header { return w.header }
func (w *failOnWriteResponseWriter) WriteHeader(status int) {
	w.status = status
}
func (w *failOnWriteResponseWriter) Write([]byte) (int, error) {
	return 0, w.err
}

// TestCancelledRequestContextStillCompletesRegistration pins that the
// registration path never consults the request context: with a complete,
// valid body and a healthy store, a request whose context is already
// cancelled before handling still registers, still gets the 201 response
// generated server-side, and is visible to later queries.
func TestCancelledRequestContextStillCompletesRegistration(t *testing.T) {
	eachSQLiteEntry(t, func(t *testing.T, router *gin.Engine) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		request := httptest.NewRequest(http.MethodPost, "/v1/artifacts",
			strings.NewReader(registration(sequenceRepo, testDigestA, sequenceTag, true, regRetention, regSizeA))).
			WithContext(ctx)
		if request.Context().Err() == nil {
			t.Fatal("request context is not cancelled before handling")
		}

		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusCreated {
			t.Fatalf("cancelled-context register status = %d, want %d (%s)",
				recorder.Code, http.StatusCreated, recorder.Body)
		}
		recordA := decodeBody(t, recorder)
		pushedA, _ := recordA["pushed_at"].(string)
		if pushedA == "" {
			t.Fatalf("response has no pushed_at: %v", recordA)
		}
		expectStoredRecord(t, recordA, sequenceRepo, testDigestA, sequenceTag, true,
			float64(regRetention), float64(regSizeA), pushedA)

		// The cancelled-context registration is the one committed record: the
		// repository list holds exactly it, the tag resolves to it and the
		// digest lookup returns it field-for-field.
		if records := allRecords(t, queryList(t, router, sequenceRepo)); len(records) != 1 {
			t.Fatalf("list = %v, want exactly the cancelled-context record", records)
		}
		if got := soleRecord(t, queryByTag(t, router, sequenceRepo, sequenceTag)); got["digest"] != testDigestA {
			t.Fatalf("tag points at %v, want %s", got["digest"], testDigestA)
		}
		expectStoredRecord(t, soleRecord(t, queryByDigest(t, router, sequenceRepo, testDigestA)),
			sequenceRepo, testDigestA, sequenceTag, true,
			float64(regRetention), float64(regSizeA), pushedA)
	})
}

// TestBodyReadFailureAfterCompleteObjectReturns400 pins that a non-EOF read
// failure after a complete JSON object is still a 400 decided before any
// storage call: the seeded record, its push time and the tag pointer are
// untouched, and the well-formed object inside the failed body never becomes
// a record.
func TestBodyReadFailureAfterCompleteObjectReturns400(t *testing.T) {
	eachSQLiteEntry(t, func(t *testing.T, router *gin.Engine) {
		seed := postArtifact(t, router, registration(sequenceRepo, testDigestA, sequenceTag, true, regRetention, regSizeA))
		if seed.Code != http.StatusCreated {
			t.Fatalf("seed register status = %d (%s)", seed.Code, seed.Body)
		}
		pushedA := decodeBody(t, seed)["pushed_at"].(string)

		// The body carries a complete, valid registration for digest B; only
		// the read that follows the object fails.
		body := &readThenFailReader{
			body: strings.NewReader(registration(sequenceRepo, testDigestB, sequenceTag, true, regRetention, regSizeB)),
			err:  errors.New("injected request body read failure"),
		}
		request := httptest.NewRequest(http.MethodPost, "/v1/artifacts", body)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		assertErrorResponse(t, recorder, http.StatusBadRequest, codeInvalidInput, msgInvalidReg)

		// Nothing was registered: the list still holds only A, A's record is
		// byte-for-byte intact, the tag still resolves to A, and B — a legal
		// digest identity — matches no committed record.
		if digests := recordDigests(allRecords(t, queryList(t, router, sequenceRepo))); len(digests) != 1 ||
			digests[0] != testDigestA {
			t.Fatalf("list after failed read = %v, want only %s", digests, testDigestA)
		}
		expectStoredRecord(t, soleRecord(t, queryByDigest(t, router, sequenceRepo, testDigestA)),
			sequenceRepo, testDigestA, sequenceTag, true,
			float64(regRetention), float64(regSizeA), pushedA)
		tagged := soleRecord(t, queryByTag(t, router, sequenceRepo, sequenceTag))
		if tagged["digest"] != testDigestA || tagged["pushed_at"] != pushedA {
			t.Fatalf("tag pointer changed after failed read: %v", tagged)
		}
		assertErrorResponse(t, queryByDigest(t, router, sequenceRepo, testDigestB),
			http.StatusNotFound, codeNotFound, msgNotFound)
	})
}

// TestResponseWriteFailureAfterCommitKeepsCommittedRecord pins that the
// response write happens after the transaction commit, so a delivery failure
// is not a rollback: the record stays committed and later registrations,
// retries, conflicts and misses behave exactly as after a delivered 201.
// Every post-condition is observed through the public query responses; the
// test asserts nothing about what the disconnected client received.
func TestResponseWriteFailureAfterCommitKeepsCommittedRecord(t *testing.T) {
	eachSQLiteEntry(t, func(t *testing.T, router *gin.Engine) {
		bodyA := registration(sequenceRepo, testDigestA, sequenceTag, true, regRetention, regSizeA)
		bodyB := registration(sequenceRepo, testDigestB, sequenceTag, true, regRetention, regSizeB)

		// First registration of A: the response body write fails, so the
		// client never receives it. The server generated 201 before the
		// failed write; the committed state is checked only via later GETs.
		writer := &failOnWriteResponseWriter{
			header: make(http.Header),
			err:    errors.New("injected response delivery failure"),
		}
		router.ServeHTTP(writer, httptest.NewRequest(http.MethodPost, "/v1/artifacts", strings.NewReader(bodyA)))
		if writer.status != http.StatusCreated {
			t.Fatalf("server-side status = %d, want %d", writer.status, http.StatusCreated)
		}

		// The undelivered registration is committed: the digest lookup
		// returns it, and the original pushed_at is recovered from the
		// public query response (the 201 body never reached anyone).
		recordA := soleRecord(t, queryByDigest(t, router, sequenceRepo, testDigestA))
		pushedA, _ := recordA["pushed_at"].(string)
		if pushedA == "" {
			t.Fatalf("committed record has no pushed_at: %v", recordA)
		}
		expectStoredRecord(t, recordA, sequenceRepo, testDigestA, sequenceTag, true,
			float64(regRetention), float64(regSizeA), pushedA)

		// Register B under the same tag: 201, the pointer moves to B.
		second := postArtifact(t, router, bodyB)
		if second.Code != http.StatusCreated {
			t.Fatalf("register B status = %d, want %d (%s)", second.Code, http.StatusCreated, second.Body)
		}
		pushedB := decodeBody(t, second)["pushed_at"].(string)
		if got := soleRecord(t, queryByTag(t, router, sequenceRepo, sequenceTag)); got["digest"] != testDigestB {
			t.Fatalf("tag after B points at %v, want %s", got["digest"], testDigestB)
		}

		// Retry the undelivered first request verbatim: 201 with the original
		// pushed_at, no new record, and the pointer does not move back.
		retry := postArtifact(t, router, bodyA)
		if retry.Code != http.StatusCreated {
			t.Fatalf("retry A status = %d, want %d (%s)", retry.Code, http.StatusCreated, retry.Body)
		}
		if got := decodeBody(t, retry)["pushed_at"]; got != pushedA {
			t.Fatalf("retry pushed_at = %v, want original %v", got, pushedA)
		}
		if digests := recordDigests(allRecords(t, queryList(t, router, sequenceRepo))); len(digests) != 2 ||
			digests[0] != testDigestA || digests[1] != testDigestB {
			t.Fatalf("list = %v, want [%s %s] in registration order", digests, testDigestA, testDigestB)
		}
		tagged := soleRecord(t, queryByTag(t, router, sequenceRepo, sequenceTag))
		if tagged["digest"] != testDigestB || tagged["pushed_at"] != pushedB {
			t.Fatalf("tag pointer moved after retry: %v", tagged)
		}
		expectStoredRecord(t, soleRecord(t, queryByDigest(t, router, sequenceRepo, testDigestA)),
			sequenceRepo, testDigestA, sequenceTag, true,
			float64(regRetention), float64(regSizeA), pushedA)
		expectStoredRecord(t, soleRecord(t, queryByDigest(t, router, sequenceRepo, testDigestB)),
			sequenceRepo, testDigestB, sequenceTag, true,
			float64(regRetention), float64(regSizeB), pushedB)

		// Same identity with a changed size: 409 ArtifactConflictError, and
		// neither record, push time nor pointer changes.
		assertErrorResponse(t, postArtifact(t, router,
			registration(sequenceRepo, testDigestA, sequenceTag, true, regRetention, regSizeB)),
			http.StatusConflict, codeConflict, msgConflict)

		// A legal query for a digest that was never registered: 404
		// ArtifactNotFoundError, and it changes nothing either.
		assertErrorResponse(t, queryByDigest(t, router, sequenceRepo, testDigestC),
			http.StatusNotFound, codeNotFound, msgNotFound)

		expectStoredRecord(t, soleRecord(t, queryByDigest(t, router, sequenceRepo, testDigestA)),
			sequenceRepo, testDigestA, sequenceTag, true,
			float64(regRetention), float64(regSizeA), pushedA)
		if got := soleRecord(t, queryByTag(t, router, sequenceRepo, sequenceTag)); got["digest"] != testDigestB {
			t.Fatalf("tag pointer moved after conflict/miss: %v", got)
		}
		if digests := recordDigests(allRecords(t, queryList(t, router, sequenceRepo))); len(digests) != 2 ||
			digests[0] != testDigestA || digests[1] != testDigestB {
			t.Fatalf("list changed after conflict/miss: %v", digests)
		}
	})
}
