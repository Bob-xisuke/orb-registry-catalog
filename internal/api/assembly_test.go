package api

// These regression cases exercise the storage-agnostic assembly entry
// NewRouterWithStore: the router is built from a caller-supplied
// service.Store — no Ping method, no SQLite, no database/sql — plus an
// independently provided health check. They pin the published behavior of
// POST/GET /v1/artifacts and GET /healthz on that boundary, including error
// priority and the mutual independence of the health check and the store.

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/Bob-xisuke/orb-registry-catalog/internal/service"
	"github.com/Bob-xisuke/orb-registry-catalog/internal/store"
)

// testDigestC is a third valid digest for cross-entry scenarios; testDigestA
// and testDigestB come from artifacts_test.go.
const testDigestC = "sha256:" + "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

// assemblyStore is a caller-supplied service.Store written only against the
// exported storage contract. It keeps records in memory, can be forced to
// fail like any backend, and counts calls so tests can prove that rejected
// input never reaches storage.
type assemblyStore struct {
	records []service.Record
	byID    map[string]int               // repository\x00digest -> records index
	tags    map[string]map[string]string // repository -> tag -> digest
	fault   error
	calls   map[string]int
}

func newAssemblyStore() *assemblyStore {
	return &assemblyStore{
		byID:  make(map[string]int),
		tags:  make(map[string]map[string]string),
		calls: make(map[string]int),
	}
}

func assemblyID(repository, digest string) string { return repository + "\x00" + digest }

func (a *assemblyStore) Register(in service.Record) (service.Record, service.RegisterStatus, error) {
	a.calls["register"]++
	if a.fault != nil {
		return service.Record{}, 0, a.fault
	}
	key := assemblyID(in.Repository, in.Digest)
	if idx, ok := a.byID[key]; ok {
		existing := a.records[idx]
		if existing.Tag == in.Tag &&
			existing.SignatureVerified == in.SignatureVerified &&
			existing.RetentionDays == in.RetentionDays &&
			existing.SizeBytes == in.SizeBytes {
			return existing, service.StatusDuplicate, nil
		}
		return service.Record{}, service.StatusConflict, nil
	}
	a.byID[key] = len(a.records)
	a.records = append(a.records, in)
	if a.tags[in.Repository] == nil {
		a.tags[in.Repository] = make(map[string]string)
	}
	a.tags[in.Repository][in.Tag] = in.Digest
	return in, service.StatusCreated, nil
}

func (a *assemblyStore) ListRecords(repository string) ([]service.Record, error) {
	a.calls["list"]++
	if a.fault != nil {
		return nil, a.fault
	}
	var out []service.Record
	for _, record := range a.records {
		if record.Repository == repository {
			out = append(out, record)
		}
	}
	return out, nil
}

func (a *assemblyStore) RecordByDigest(repository, digest string) (service.Record, error) {
	a.calls["digest"]++
	if a.fault != nil {
		return service.Record{}, a.fault
	}
	if idx, ok := a.byID[assemblyID(repository, digest)]; ok {
		return a.records[idx], nil
	}
	return service.Record{}, service.ErrRecordNotFound
}

func (a *assemblyStore) RecordByTag(repository, tag string) (service.Record, error) {
	a.calls["tag"]++
	if a.fault != nil {
		return service.Record{}, a.fault
	}
	digest, ok := a.tags[repository][tag]
	if !ok {
		return service.Record{}, service.ErrRecordNotFound
	}
	return a.records[a.byID[assemblyID(repository, digest)]], nil
}

func (a *assemblyStore) totalCalls() int {
	total := 0
	for _, n := range a.calls {
		total += n
	}
	return total
}

// assemblyProbe is a health check the test flips between requests; every
// /healthz call must consult it again.
type assemblyProbe struct {
	err   error
	calls int
}

func (p *assemblyProbe) check() error {
	p.calls++
	return p.err
}

func newAssemblyRouter(st *assemblyStore, probe *assemblyProbe) *gin.Engine {
	return NewRouterWithStore(st, probe.check)
}

func getHealth(t *testing.T, router *gin.Engine) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	return recorder
}

// TestAssemblyEntryRegisterAndQuery walks the full registration and query
// contract through the new entry on a caller-supplied store: first
// registration and identical retry share 201 and the original pushed_at, the
// tag pointer moves only on new content, conflicts are 409, lists keep
// first-registration order and misses are 404.
func TestAssemblyEntryRegisterAndQuery(t *testing.T) {
	st := newAssemblyStore()
	router := newAssemblyRouter(st, &assemblyProbe{})

	bodyA := registration(sequenceRepo, testDigestA, sequenceTag, true, regRetention, regSizeA)
	bodyB := registration(sequenceRepo, testDigestB, sequenceTag, true, regRetention, regSizeB)

	first := postArtifact(t, router, bodyA)
	if first.Code != http.StatusCreated {
		t.Fatalf("register A status = %d, want %d (%s)", first.Code, http.StatusCreated, first.Body)
	}
	pushedA, _ := decodeBody(t, first)["pushed_at"].(string)
	if pushedA == "" {
		t.Fatalf("A response has no pushed_at: %s", first.Body)
	}

	second := postArtifact(t, router, bodyB)
	if second.Code != http.StatusCreated {
		t.Fatalf("register B status = %d, want %d (%s)", second.Code, http.StatusCreated, second.Body)
	}
	pushedB, _ := decodeBody(t, second)["pushed_at"].(string)

	// The tag pointer moved to B; A survives by digest.
	if got := soleRecord(t, queryByTag(t, router, sequenceRepo, sequenceTag)); got["digest"] != testDigestB {
		t.Fatalf("tag after B points at %v, want %s", got["digest"], testDigestB)
	}
	expectStoredRecord(t, soleRecord(t, queryByDigest(t, router, sequenceRepo, testDigestA)),
		sequenceRepo, testDigestA, sequenceTag, true,
		float64(regRetention), float64(regSizeA), pushedA)

	// Identical retry of A: 201 with the original pushed_at, no new record and
	// the tag pointer does not move back.
	retry := postArtifact(t, router, bodyA)
	if retry.Code != http.StatusCreated {
		t.Fatalf("retry A status = %d, want %d (%s)", retry.Code, http.StatusCreated, retry.Body)
	}
	if got := decodeBody(t, retry)["pushed_at"]; got != pushedA {
		t.Fatalf("retry pushed_at = %v, want original %v", got, pushedA)
	}
	if got := soleRecord(t, queryByTag(t, router, sequenceRepo, sequenceTag)); got["digest"] != testDigestB {
		t.Fatalf("retry moved the tag pointer: %v", got)
	}
	if digests := recordDigests(allRecords(t, queryList(t, router, sequenceRepo))); len(digests) != 2 ||
		digests[0] != testDigestA || digests[1] != testDigestB {
		t.Fatalf("list = %v, want [%s %s] in registration order", digests, testDigestA, testDigestB)
	}

	// Same identity with different content: 409, and nothing changes.
	conflict := postArtifact(t, router,
		registration(sequenceRepo, testDigestA, sequenceTag, true, regRetention, regSizeB))
	assertErrorResponse(t, conflict, http.StatusConflict, codeConflict, msgConflict)
	expectStoredRecord(t, soleRecord(t, queryByDigest(t, router, sequenceRepo, testDigestA)),
		sequenceRepo, testDigestA, sequenceTag, true,
		float64(regRetention), float64(regSizeA), pushedA)
	if got := soleRecord(t, queryByTag(t, router, sequenceRepo, sequenceTag)); got["pushed_at"] != pushedB {
		t.Fatalf("tagged record changed after conflict: %v", got)
	}

	// Legal queries with no match are 404.
	assertErrorResponse(t, queryList(t, router, "registry-demo/missing"),
		http.StatusNotFound, codeNotFound, msgNotFound)
	assertErrorResponse(t, queryByTag(t, router, sequenceRepo, "missing-tag"),
		http.StatusNotFound, codeNotFound, msgNotFound)
	assertErrorResponse(t, queryByDigest(t, router, sequenceRepo, testDigestB[:len(testDigestB)-1]+"c"),
		http.StatusNotFound, codeNotFound, msgNotFound)
}

// TestAssemblyEntryErrorPriority pins the error ordering on the new entry:
// invalid input is a 400 decided before any storage call — even while the
// store is down — and a legal request that hits a failing store is a 503.
func TestAssemblyEntryErrorPriority(t *testing.T) {
	st := newAssemblyStore()
	router := newAssemblyRouter(st, &assemblyProbe{})

	// Invalid registration and query bodies are rejected without touching the
	// store, while it is still healthy.
	assertErrorResponse(t, postArtifact(t, router, `{"repository":`),
		http.StatusBadRequest, codeInvalidInput, msgInvalidReg)
	assertErrorResponse(t, postArtifact(t, router, registrationWithField("size_bytes", "null")),
		http.StatusBadRequest, codeInvalidInput, msgInvalidReg)
	assertErrorResponse(t, getArtifacts(t, router, "tag="+sequenceTag),
		http.StatusBadRequest, codeInvalidInput, msgInvalidQuery)
	assertErrorResponse(t, getArtifacts(t, router, "repository="+sequenceRepo+"&tag=&digest="),
		http.StatusBadRequest, codeInvalidInput, msgInvalidQuery)
	if got := st.totalCalls(); got != 0 {
		t.Fatalf("invalid input reached the store %d times, want 0", got)
	}

	// With the store faulted, invalid input still wins its 400 over the 503.
	st.fault = errors.New("assembly backend down")
	assertErrorResponse(t, postArtifact(t, router, registrationWithField("signature_verified", "null")),
		http.StatusBadRequest, codeInvalidInput, msgInvalidReg)
	assertErrorResponse(t, getArtifacts(t, router, "repository="+sequenceRepo+"&digest=sha256:xyz"),
		http.StatusBadRequest, codeInvalidInput, msgInvalidQuery)

	// Legal requests degrade to 503 storage_unavailable on the same faulted
	// store, register and query alike.
	assertErrorResponse(t, postArtifact(t, router,
		registration(sequenceRepo, testDigestA, sequenceTag, true, regRetention, regSizeA)),
		http.StatusServiceUnavailable, codeStorage, msgStorage)
	assertErrorResponse(t, queryList(t, router, sequenceRepo),
		http.StatusServiceUnavailable, codeStorage, msgStorage)
	assertErrorResponse(t, queryByTag(t, router, sequenceRepo, sequenceTag),
		http.StatusServiceUnavailable, codeStorage, msgStorage)
	assertErrorResponse(t, queryByDigest(t, router, sequenceRepo, testDigestA),
		http.StatusServiceUnavailable, codeStorage, msgStorage)
}

// TestAssemblyHealthCheckIndependent proves the health check and the store
// answer for themselves alone: a failing probe degrades only /healthz, a
// failing store degrades only the artifact entries, and /healthz reflects the
// probe result on every single call.
func TestAssemblyHealthCheckIndependent(t *testing.T) {
	st := newAssemblyStore()
	probe := &assemblyProbe{}
	router := newAssemblyRouter(st, probe)

	bodyA := registration(sequenceRepo, testDigestA, sequenceTag, true, regRetention, regSizeA)

	// Healthy probe: the published 200 body.
	recorder := getHealth(t, router)
	if recorder.Code != http.StatusOK || recorder.Body.String() != `{"database":"ok","status":"ok"}` {
		t.Fatalf("healthy /healthz = %d %s", recorder.Code, recorder.Body)
	}

	// Failing probe: /healthz is 503 with the published shape, but
	// registration and queries are not gated on the probe and still succeed.
	probe.err = errors.New("probe: dependency unreachable")
	assertErrorResponse(t, getHealth(t, router),
		http.StatusServiceUnavailable, codeStorage, msgStorage)
	if got := postArtifact(t, router, bodyA); got.Code != http.StatusCreated {
		t.Fatalf("register with failing probe status = %d (%s)", got.Code, got.Body)
	}
	if got := soleRecord(t, queryByTag(t, router, sequenceRepo, sequenceTag)); got["digest"] != testDigestA {
		t.Fatalf("query with failing probe = %v, want the registered record", got)
	}

	// The probe is consulted on every call: clearing the fault restores 200.
	probe.err = nil
	if recorder := getHealth(t, router); recorder.Code != http.StatusOK {
		t.Fatalf("/healthz after probe recovery = %d, want %d", recorder.Code, http.StatusOK)
	}
	if probe.calls != 3 {
		t.Fatalf("probe consulted %d times for 3 /healthz calls", probe.calls)
	}

	// A failing store does not stand in for the health check: /healthz stays
	// 200 on the healthy probe while legal artifact requests are 503.
	st.fault = errors.New("assembly backend down")
	if recorder := getHealth(t, router); recorder.Code != http.StatusOK {
		t.Fatalf("/healthz with failing store = %d, want %d", recorder.Code, http.StatusOK)
	}
	assertErrorResponse(t, postArtifact(t, router,
		registration(sequenceRepo, testDigestB, sequenceTag, true, regRetention, regSizeB)),
		http.StatusServiceUnavailable, codeStorage, msgStorage)
	assertErrorResponse(t, queryList(t, router, sequenceRepo),
		http.StatusServiceUnavailable, codeStorage, msgStorage)
}

// TestAssemblyEntryUnknownRoute keeps the published 404 shape on the new
// entry, independent of store and probe state.
func TestAssemblyEntryUnknownRoute(t *testing.T) {
	router := newAssemblyRouter(newAssemblyStore(), &assemblyProbe{})

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/missing", nil))
	assertErrorResponse(t, recorder, http.StatusNotFound, "route_not_found", "no route matches this path")
}

// scriptedStore is a minimal service.Store whose register outcome and lookup
// errors are set directly by the boundary tests, so they can drive storage
// outcomes the in-memory assemblyStore does not produce: a wrapped miss and
// a register status returned together with an error.
type scriptedStore struct {
	registerStatus service.RegisterStatus
	registerErr    error
	lookupErr      error
}

func (s *scriptedStore) Register(service.Record) (service.Record, service.RegisterStatus, error) {
	return service.Record{}, s.registerStatus, s.registerErr
}

func (s *scriptedStore) ListRecords(string) ([]service.Record, error) {
	return nil, s.lookupErr
}

func (s *scriptedStore) RecordByDigest(string, string) (service.Record, error) {
	return service.Record{}, s.lookupErr
}

func (s *scriptedStore) RecordByTag(string, string) (service.Record, error) {
	return service.Record{}, s.lookupErr
}

// TestAssemblyWrappedMissIsNotFound proves the not-found classification
// survives error wrapping: a store that reports a tag or digest miss as
// fmt.Errorf("...: %w", service.ErrRecordNotFound) still maps to 404
// ArtifactNotFoundError, because the service classifies with errors.Is
// rather than comparing the error value.
func TestAssemblyWrappedMissIsNotFound(t *testing.T) {
	st := &scriptedStore{lookupErr: fmt.Errorf("backend lookup: %w", service.ErrRecordNotFound)}
	router := NewRouterWithStore(st, nil)

	assertErrorResponse(t, queryByTag(t, router, sequenceRepo, "missing"),
		http.StatusNotFound, codeNotFound, msgNotFound)
	assertErrorResponse(t, queryByDigest(t, router, sequenceRepo, testDigestA),
		http.StatusNotFound, codeNotFound, msgNotFound)
}

// TestAssemblyRegisterStatusWithErrorIsStorage pins the error-first rule on
// the register path: a store that returns a duplicate or conflict status
// together with a non-nil error is reported as a storage failure — 503
// storage_unavailable, never 201 or 409 — because the service checks the
// error before it looks at the status.
func TestAssemblyRegisterStatusWithErrorIsStorage(t *testing.T) {
	statuses := map[string]service.RegisterStatus{
		"duplicate with error": service.StatusDuplicate,
		"conflict with error":  service.StatusConflict,
	}
	for name, status := range statuses {
		t.Run(name, func(t *testing.T) {
			st := &scriptedStore{
				registerStatus: status,
				registerErr:    errors.New("scripted backend failure"),
			}
			router := NewRouterWithStore(st, nil)
			assertErrorResponse(t, postArtifact(t, router,
				registration(sequenceRepo, testDigestA, sequenceTag, true, regRetention, regSizeA)),
				http.StatusServiceUnavailable, codeStorage, msgStorage)
		})
	}
}

// TestAssemblyNoHealthCheckStoreFailure builds the router without a health
// check: GET /healthz keeps the published 200 body, while the faulted store
// alone degrades legal artifact requests to 503 storage_unavailable.
func TestAssemblyNoHealthCheckStoreFailure(t *testing.T) {
	st := newAssemblyStore()
	st.fault = errors.New("assembly backend down")
	router := NewRouterWithStore(st, nil)

	recorder := getHealth(t, router)
	if recorder.Code != http.StatusOK || recorder.Body.String() != `{"database":"ok","status":"ok"}` {
		t.Fatalf("/healthz without a health check = %d %s, want the 200 ok body",
			recorder.Code, recorder.Body)
	}

	assertErrorResponse(t, postArtifact(t, router,
		registration(sequenceRepo, testDigestA, sequenceTag, true, regRetention, regSizeA)),
		http.StatusServiceUnavailable, codeStorage, msgStorage)
	assertErrorResponse(t, queryList(t, router, sequenceRepo),
		http.StatusServiceUnavailable, codeStorage, msgStorage)
}

// TestSQLiteEntryStaysCompatibleWithExistingData writes records through the
// original NewRouter assembly, reopens the same database file behind the new
// NewRouterWithStore entry and confirms the committed data — push times, list
// order, unique identity and the current tag pointer — reads back unchanged,
// and that the new entry keeps registering into the same store.
func TestSQLiteEntryStaysCompatibleWithExistingData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")

	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	legacy := NewRouter(st)
	first := postArtifact(t, legacy, registration(sequenceRepo, testDigestA, sequenceTag, true, regRetention, regSizeA))
	if first.Code != http.StatusCreated {
		t.Fatalf("legacy register A status = %d (%s)", first.Code, first.Body)
	}
	pushedA := decodeBody(t, first)["pushed_at"].(string)
	second := postArtifact(t, legacy, registration(sequenceRepo, testDigestB, sequenceTag, true, regRetention, regSizeB))
	if second.Code != http.StatusCreated {
		t.Fatalf("legacy register B status = %d (%s)", second.Code, second.Body)
	}
	pushedB := decodeBody(t, second)["pushed_at"].(string)
	if err := st.Close(); err != nil {
		t.Fatalf("close before reopen: %v", err)
	}

	// Reopen the same file behind the new entry; the store's Ping is passed
	// explicitly as the health check, like NewRouter does.
	reopened, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { reopened.Close() })
	router := NewRouterWithStore(reopened, reopened.Ping)

	// Existing data survives: order, push times and the tag pointer.
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

	// The unique identity still holds across the boundary: a verbatim retry of
	// A through the new entry is a duplicate, not a new record.
	retry := postArtifact(t, router, registration(sequenceRepo, testDigestA, sequenceTag, true, regRetention, regSizeA))
	if retry.Code != http.StatusCreated {
		t.Fatalf("retry A status = %d (%s)", retry.Code, retry.Body)
	}
	if got := decodeBody(t, retry)["pushed_at"]; got != pushedA {
		t.Fatalf("retry pushed_at = %v, want original %v", got, pushedA)
	}
	if digests := recordDigests(allRecords(t, queryList(t, router, sequenceRepo))); len(digests) != 2 {
		t.Fatalf("retry added a record across the boundary: %v", digests)
	}

	// New registrations through the new entry land in the same store and are
	// visible to a fresh legacy router on the same file.
	third := postArtifact(t, router,
		registration(sequenceRepo, testDigestC, sequenceTag, true, regRetention, regSizeB))
	if third.Code != http.StatusCreated {
		t.Fatalf("register C status = %d (%s)", third.Code, third.Body)
	}
	pushedC := decodeBody(t, third)["pushed_at"].(string)
	if got := soleRecord(t, queryByTag(t, router, sequenceRepo, sequenceTag)); got["digest"] != testDigestC {
		t.Fatalf("tag after C points at %v, want %s", got["digest"], testDigestC)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("close before legacy reopen: %v", err)
	}

	legacyStore, err := store.Open(path)
	if err != nil {
		t.Fatalf("legacy reopen: %v", err)
	}
	t.Cleanup(func() { legacyStore.Close() })
	legacy = NewRouter(legacyStore)
	records = allRecords(t, queryList(t, legacy, sequenceRepo))
	if digests := recordDigests(records); len(digests) != 3 ||
		digests[0] != testDigestA || digests[1] != testDigestB || digests[2] != testDigestC {
		t.Fatalf("legacy list after cross-entry writes = %v, want A B C", digests)
	}
	expectStoredRecord(t, soleRecord(t, queryByTag(t, legacy, sequenceRepo, sequenceTag)),
		sequenceRepo, testDigestC, sequenceTag, true,
		float64(regRetention), float64(regSizeB), pushedC)
}
