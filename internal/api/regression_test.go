package api

// These regression cases exercise only the public HTTP surface
// (POST/GET /v1/artifacts, GET /healthz) and back the conclusions in
// docs/artifact-identity-analysis.md. They deliberately avoid the store's
// unexported internals so that any refactor preserving the published contract
// keeps them green.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/Bob-xisuke/orb-registry-catalog/internal/store"
)

const (
	sequenceRepo    = "registry-demo/control"
	otherRepoOne    = "registry-demo/one"
	otherRepoTwo    = "registry-demo/two"
	sequenceTag     = "release"
	regRetention    = int64(30)
	regSizeA        = int64(1024)
	regSizeB        = int64(2048)
	msgInvalidReg   = "request body is not a valid artifact registration"
	msgInvalidQuery = "query parameters are not valid"
	msgConflict     = "an artifact with this repository and digest already exists with different content"
	msgNotFound     = "no artifact matches this query"
	msgStorage      = "database is not available"
)

func registration(repo, digest, tag string, sig bool, retention, size int64) string {
	return fmt.Sprintf(
		`{"repository":%q,"digest":%q,"tag":%q,"signature_verified":%t,"retention_days":%d,"size_bytes":%d}`,
		repo, digest, tag, sig, retention, size)
}

func allRecords(t *testing.T, recorder *httptest.ResponseRecorder) []any {
	t.Helper()
	items, ok := decodeBody(t, recorder)["artifacts"].([]any)
	if !ok {
		t.Fatalf("response has no artifacts array: %s", recorder.Body)
	}
	return items
}

func soleRecord(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	items := allRecords(t, recorder)
	if len(items) != 1 {
		t.Fatalf("got %d records, want exactly 1: %s", len(items), recorder.Body)
	}
	return items[0].(map[string]any)
}

func recordDigests(records []any) []string {
	out := make([]string, len(records))
	for i, record := range records {
		out[i] = record.(map[string]any)["digest"].(string)
	}
	return out
}

// assertErrorResponse pins the published error contract: one top-level error
// object holding exactly the two string fields code and message, and the
// message must never carry SQL, stack frames or filesystem paths.
func assertErrorResponse(t *testing.T, recorder *httptest.ResponseRecorder, wantStatus int, wantCode, wantMessage string) {
	t.Helper()
	if recorder.Code != wantStatus {
		t.Fatalf("status = %d, want %d (%s)", recorder.Code, wantStatus, recorder.Body)
	}
	errObj, ok := decodeBody(t, recorder)["error"].(map[string]any)
	if !ok {
		t.Fatalf("response has no error object: %s", recorder.Body)
	}
	if code, _ := errObj["code"].(string); code != wantCode {
		t.Fatalf("error code = %q, want %q", code, wantCode)
	}
	message, ok := errObj["message"].(string)
	if !ok || message != wantMessage {
		t.Fatalf("error message = %v, want %q", errObj["message"], wantMessage)
	}
	if len(errObj) != 2 {
		t.Fatalf("error object has unexpected fields: %v", errObj)
	}
	lowered := strings.ToLower(message)
	for _, leaked := range []string{"sql", "sqlite", "insert", "select", "begin", ".go", "/", "goroutine", "stack"} {
		if strings.Contains(lowered, leaked) {
			t.Fatalf("error message %q leaks internal detail %q", message, leaked)
		}
	}
}

// expectStoredRecord compares every field of a record returned by the API.
func expectStoredRecord(t *testing.T, record map[string]any, repo, digest, tag string, sig bool, retention, size float64, pushedAt string) {
	t.Helper()
	want := map[string]any{
		"repository":         repo,
		"digest":             digest,
		"tag":                tag,
		"signature_verified": sig,
		"retention_days":     retention,
		"size_bytes":         size,
		"pushed_at":          pushedAt,
	}
	for key, value := range want {
		if record[key] != value {
			t.Fatalf("field %s = %v, want %v (record %v)", key, record[key], value, record)
		}
	}
}

func queryByTag(t *testing.T, router *gin.Engine, repo, tag string) *httptest.ResponseRecorder {
	t.Helper()
	return getArtifacts(t, router, "repository="+repo+"&tag="+tag)
}

func queryByDigest(t *testing.T, router *gin.Engine, repo, digest string) *httptest.ResponseRecorder {
	t.Helper()
	return getArtifacts(t, router, "repository="+repo+"&digest="+digest)
}

func queryList(t *testing.T, router *gin.Engine, repo string) *httptest.ResponseRecorder {
	t.Helper()
	return getArtifacts(t, router, "repository="+repo)
}

// TestRegistrationSequenceIdentityAndTagPointer walks the full scenario:
// register A, register B under the same tag, retry A verbatim, then mutate each
// comparable field of A. The immutable registration record and the mutable tag
// pointer must stay distinguishable at every step.
func TestRegistrationSequenceIdentityAndTagPointer(t *testing.T) {
	router, _ := newTestRouter(t)

	bodyA := registration(sequenceRepo, testDigestA, sequenceTag, true, regRetention, regSizeA)
	bodyB := registration(sequenceRepo, testDigestB, sequenceTag, true, regRetention, regSizeB)

	// 1. First registration of A: 201 with the service-generated pushed_at.
	first := postArtifact(t, router, bodyA)
	if first.Code != http.StatusCreated {
		t.Fatalf("register A status = %d, want %d (%s)", first.Code, http.StatusCreated, first.Body)
	}
	recordA := decodeBody(t, first)
	pushedA, _ := recordA["pushed_at"].(string)
	if pushedA == "" {
		t.Fatalf("A response has no pushed_at: %v", recordA)
	}
	expectStoredRecord(t, recordA, sequenceRepo, testDigestA, sequenceTag, true,
		float64(regRetention), float64(regSizeA), pushedA)

	// Independent query before B is registered: the tag points at A and reads
	// only committed state.
	if got := soleRecord(t, queryByTag(t, router, sequenceRepo, sequenceTag)); got["digest"] != testDigestA {
		t.Fatalf("tag before B points at %v, want %s", got["digest"], testDigestA)
	}

	// 2. Register B with the same tag: 201, tag pointer moves to B, A survives.
	second := postArtifact(t, router, bodyB)
	if second.Code != http.StatusCreated {
		t.Fatalf("register B status = %d, want %d (%s)", second.Code, http.StatusCreated, second.Body)
	}
	recordB := decodeBody(t, second)
	pushedB, _ := recordB["pushed_at"].(string)
	expectStoredRecord(t, recordB, sequenceRepo, testDigestB, sequenceTag, true,
		float64(regRetention), float64(regSizeB), pushedB)

	// A second, independent query now observes the committed pointer move.
	if got := soleRecord(t, queryByTag(t, router, sequenceRepo, sequenceTag)); got["digest"] != testDigestB {
		t.Fatalf("tag after B points at %v, want %s", got["digest"], testDigestB)
	}

	// 3. Retry A verbatim: still 201, but it is the original record: the same
	// pushed_at and the pointer does not move back.
	retry := postArtifact(t, router, bodyA)
	if retry.Code != http.StatusCreated {
		t.Fatalf("retry A status = %d, want %d (%s)", retry.Code, http.StatusCreated, retry.Body)
	}
	retryRecord := decodeBody(t, retry)
	if retryRecord["pushed_at"] != pushedA {
		t.Fatalf("retry returned pushed_at %v, want original %v", retryRecord["pushed_at"], pushedA)
	}
	expectStoredRecord(t, retryRecord, sequenceRepo, testDigestA, sequenceTag, true,
		float64(regRetention), float64(regSizeA), pushedA)

	// 4. Equivalent retry after whitespace normalization, plus an unknown field:
	// still the same identity, still the original pushed_at.
	normalized := `{"repository":" ` + sequenceRepo + ` ","digest":"` + testDigestA +
		`","tag":" ` + sequenceTag + ` ","signature_verified":true,` +
		`"retention_days":30,"size_bytes":1024,"unexpected":"ignored"}`
	normRetry := postArtifact(t, router, normalized)
	if normRetry.Code != http.StatusCreated {
		t.Fatalf("normalized retry status = %d, want %d (%s)", normRetry.Code, http.StatusCreated, normRetry.Body)
	}
	if got := decodeBody(t, normRetry)["pushed_at"]; got != pushedA {
		t.Fatalf("normalized retry pushed_at = %v, want %v", got, pushedA)
	}

	// Repository list: exactly two records in first-registration order.
	records := allRecords(t, queryList(t, router, sequenceRepo))
	if digests := recordDigests(records); len(digests) != 2 ||
		digests[0] != testDigestA || digests[1] != testDigestB {
		t.Fatalf("list = %v, want [%s %s] in registration order", digests, testDigestA, testDigestB)
	}

	// Digest lookup still fetches the original A record.
	expectStoredRecord(t, soleRecord(t, queryByDigest(t, router, sequenceRepo, testDigestA)),
		sequenceRepo, testDigestA, sequenceTag, true,
		float64(regRetention), float64(regSizeA), pushedA)

	// 5. Each comparable field changed in isolation is a 409 conflict, and
	// neither the immutable record nor the tag pointer may change afterwards.
	conflicts := map[string]string{
		"changed tag":       registration(sequenceRepo, testDigestA, "other", true, regRetention, regSizeA),
		"changed signature": registration(sequenceRepo, testDigestA, sequenceTag, false, regRetention, regSizeA),
		"changed retention": registration(sequenceRepo, testDigestA, sequenceTag, true, regRetention+1, regSizeA),
		"changed size":      registration(sequenceRepo, testDigestA, sequenceTag, true, regRetention, regSizeB),
	}
	for name, body := range conflicts {
		t.Run(name, func(t *testing.T) {
			assertErrorResponse(t, postArtifact(t, router, body),
				http.StatusConflict, codeConflict, msgConflict)

			// Original A record is byte-for-byte the same through the API.
			expectStoredRecord(t, soleRecord(t, queryByDigest(t, router, sequenceRepo, testDigestA)),
				sequenceRepo, testDigestA, sequenceTag, true,
				float64(regRetention), float64(regSizeA), pushedA)

			// Tag pointer still resolves to B; the rejected request wrote nothing.
			if got := soleRecord(t, queryByTag(t, router, sequenceRepo, sequenceTag)); got["digest"] != testDigestB {
				t.Fatalf("tag pointer moved after conflict: %v, want %s", got["digest"], testDigestB)
			}
			pointed := soleRecord(t, queryByTag(t, router, sequenceRepo, sequenceTag))
			if pointed["pushed_at"] != pushedB {
				t.Fatalf("tagged record changed after conflict: %v", pointed)
			}

			// No third record appeared and registration order is unchanged.
			if digests := recordDigests(allRecords(t, queryList(t, router, sequenceRepo))); len(digests) != 2 ||
				digests[0] != testDigestA || digests[1] != testDigestB {
				t.Fatalf("list changed after conflict: %v", digests)
			}

			// The tag carried by the rejected body was never persisted.
			missing := queryByTag(t, router, sequenceRepo, "other")
			assertErrorResponse(t, missing, http.StatusNotFound, codeNotFound, msgNotFound)
		})
	}
}

// TestRepositoriesDoNotInterfere checks that the same digest and the same tag
// name in a different repository are independent identities and pointers.
func TestRepositoriesDoNotInterfere(t *testing.T) {
	router, _ := newTestRouter(t)

	first := postArtifact(t, router, registration(otherRepoOne, testDigestA, sequenceTag, true, regRetention, regSizeA))
	if first.Code != http.StatusCreated {
		t.Fatalf("repo one register status = %d (%s)", first.Code, first.Body)
	}
	second := postArtifact(t, router, registration(otherRepoTwo, testDigestA, sequenceTag, true, regRetention, regSizeA))
	if second.Code != http.StatusCreated {
		t.Fatalf("repo two register status = %d (%s)", second.Code, second.Body)
	}

	// Each repository resolves the shared tag name within its own namespace.
	if got := soleRecord(t, queryByTag(t, router, otherRepoOne, sequenceTag)); got["repository"] != otherRepoOne {
		t.Fatalf("repo one tag resolves cross-repository: %v", got)
	}
	if got := soleRecord(t, queryByTag(t, router, otherRepoTwo, sequenceTag)); got["repository"] != otherRepoTwo {
		t.Fatalf("repo two tag resolves cross-repository: %v", got)
	}

	// A conflict inside repo two must not touch repo one's record.
	crossConflict := postArtifact(t, router,
		registration(otherRepoTwo, testDigestA, sequenceTag, true, regRetention, regSizeB))
	assertErrorResponse(t, crossConflict, http.StatusConflict, codeConflict, msgConflict)
	if got := soleRecord(t, queryByDigest(t, router, otherRepoOne, testDigestA)); got["size_bytes"] != float64(regSizeA) {
		t.Fatalf("repo one record changed after repo two conflict: %v", got)
	}

	// Moving the tag in repo two (new digest B) leaves repo one's pointer at A.
	if recorder := postArtifact(t, router,
		registration(otherRepoTwo, testDigestB, sequenceTag, true, regRetention, regSizeB)); recorder.Code != http.StatusCreated {
		t.Fatalf("repo two register B status = %d (%s)", recorder.Code, recorder.Body)
	}
	if got := soleRecord(t, queryByTag(t, router, otherRepoTwo, sequenceTag)); got["digest"] != testDigestB {
		t.Fatalf("repo two tag = %v, want %s", got["digest"], testDigestB)
	}
	if got := soleRecord(t, queryByTag(t, router, otherRepoOne, sequenceTag)); got["digest"] != testDigestA {
		t.Fatalf("repo one tag = %v, want %s", got["digest"], testDigestA)
	}

	if records := allRecords(t, queryList(t, router, otherRepoOne)); len(records) != 1 {
		t.Fatalf("repo one list leaked other-repository records: %v", records)
	}
	if records := allRecords(t, queryList(t, router, otherRepoTwo)); len(records) != 2 {
		t.Fatalf("repo two list = %d records, want 2", len(records))
	}
}

// TestStorageUnavailableReturns503 closes the underlying SQLite file so every
// entry must answer 503 storage_unavailable through the public error shape.
func TestStorageUnavailableReturns503(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	router := NewRouter(st)
	if recorder := postArtifact(t, router,
		registration(sequenceRepo, testDigestA, sequenceTag, true, regRetention, regSizeA)); recorder.Code != http.StatusCreated {
		t.Fatalf("seed register status = %d (%s)", recorder.Code, recorder.Body)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	assertErrorResponse(t,
		postArtifact(t, router, registration(sequenceRepo, testDigestB, sequenceTag, true, regRetention, regSizeB)),
		http.StatusServiceUnavailable, codeStorage, msgStorage)
	assertErrorResponse(t, queryList(t, router, sequenceRepo),
		http.StatusServiceUnavailable, codeStorage, msgStorage)
	assertErrorResponse(t, queryByTag(t, router, sequenceRepo, sequenceTag),
		http.StatusServiceUnavailable, codeStorage, msgStorage)
	assertErrorResponse(t, queryByDigest(t, router, sequenceRepo, testDigestA),
		http.StatusServiceUnavailable, codeStorage, msgStorage)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	assertErrorResponse(t, recorder, http.StatusServiceUnavailable, codeStorage, msgStorage)
}

// TestClientErrorShapeAndMessages pins the 400 and 404 messages for the query
// and registration entries; conflict messages are covered in the sequence and
// cross-repository tests above.
func TestClientErrorShapeAndMessages(t *testing.T) {
	router, _ := newTestRouter(t)
	postArtifact(t, router, registration(sequenceRepo, testDigestA, sequenceTag, true, regRetention, regSizeA))

	// Malformed registration body -> 400; decoding fails before any store write.
	assertErrorResponse(t, postArtifact(t, router, `{"repository":`),
		http.StatusBadRequest, codeInvalidInput, msgInvalidReg)

	// digest is matched by an anchored regex and is never trimmed, so leading
	// whitespace is a rejection rather than a normalization.
	assertErrorResponse(t, postArtifact(t, router,
		`{"repository":"`+sequenceRepo+`","digest":" `+testDigestA+`","tag":"`+sequenceTag+
			`","signature_verified":true,"retention_days":30,"size_bytes":1}`),
		http.StatusBadRequest, codeInvalidInput, msgInvalidReg)

	// Invalid query parameters -> 400 with the query-specific message.
	assertErrorResponse(t, getArtifacts(t, router, "tag="+sequenceTag),
		http.StatusBadRequest, codeInvalidInput, msgInvalidQuery)
	assertErrorResponse(t, getArtifacts(t, router, "repository="+sequenceRepo+"&tag="+sequenceTag+"&digest="+testDigestA),
		http.StatusBadRequest, codeInvalidInput, msgInvalidQuery)

	// Legal query with no match -> 404.
	assertErrorResponse(t, queryList(t, router, "registry-demo/missing"),
		http.StatusNotFound, codeNotFound, msgNotFound)
	assertErrorResponse(t, queryByTag(t, router, sequenceRepo, "missing-tag"),
		http.StatusNotFound, codeNotFound, msgNotFound)
	assertErrorResponse(t, queryByDigest(t, router, sequenceRepo, testDigestB),
		http.StatusNotFound, codeNotFound, msgNotFound)
}

// TestRestartPreservesRecordsOrderAndPointers reopens the same database file
// and checks that immutable records, service-generated timestamps, list order
// and the current tag pointer all survive.
func TestRestartPreservesRecordsOrderAndPointers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")

	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	router := NewRouter(st)
	first := postArtifact(t, router, registration(sequenceRepo, testDigestA, sequenceTag, true, regRetention, regSizeA))
	pushedA := decodeBody(t, first)["pushed_at"].(string)
	second := postArtifact(t, router, registration(sequenceRepo, testDigestB, sequenceTag, true, regRetention, regSizeB))
	pushedB := decodeBody(t, second)["pushed_at"].(string)
	if err := st.Close(); err != nil {
		t.Fatalf("close before reopen: %v", err)
	}

	reopened, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { reopened.Close() })
	router = NewRouter(reopened)

	records := allRecords(t, queryList(t, router, sequenceRepo))
	if digests := recordDigests(records); len(digests) != 2 ||
		digests[0] != testDigestA || digests[1] != testDigestB {
		t.Fatalf("list after reopen = %v, want A then B", digests)
	}
	if records[0].(map[string]any)["pushed_at"] != pushedA ||
		records[1].(map[string]any)["pushed_at"] != pushedB {
		t.Fatalf("pushed_at values did not survive reopen: %v", records)
	}

	tagged := soleRecord(t, queryByTag(t, router, sequenceRepo, sequenceTag))
	if tagged["digest"] != testDigestB || tagged["pushed_at"] != pushedB {
		t.Fatalf("tag pointer did not survive reopen: %v", tagged)
	}
	expectStoredRecord(t, soleRecord(t, queryByDigest(t, router, sequenceRepo, testDigestA)),
		sequenceRepo, testDigestA, sequenceTag, true,
		float64(regRetention), float64(regSizeA), pushedA)
}
