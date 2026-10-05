package api_test

// These regression cases build the complete HTTP router through its new
// assembly entry using a store the "caller" supplies itself. The store is
// written only against the exported service.Store contract: it has no Ping
// operation, imports neither internal/store nor database/sql, and opens no
// SQLite file. The suite verifies registration/query behavior, the
// 400-before-storage priority, 503 on backend faults, and that the supplied
// health probe is fully independent of the artifact operations.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/Bob-xisuke/orb-registry-catalog/internal/api"
	"github.com/Bob-xisuke/orb-registry-catalog/internal/service"
	"github.com/Bob-xisuke/orb-registry-catalog/internal/store"
)

const (
	memDigestA = "sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	memDigestB = "sha256:" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	msgInvalidReg   = "request body is not a valid artifact registration"
	msgInvalidQuery = "query parameters are not valid"
	msgConflict     = "an artifact with this repository and digest already exists with different content"
	msgNotFound     = "no artifact matches this query"
	msgStorage      = "database is not available"
	msgNoRoute      = "no route matches this path"
)

// memoryStore is a caller-supplied backend. It models the same identity,
// ordering and tag-pointer rules as the bundled adapter while depending on
// nothing but service.Store. Each operation can be forced to fail and counts
// its invocations so tests can prove validation never reaches storage.
type memoryStore struct {
	mu      sync.Mutex
	records []service.Record
	index   map[string]int               // repository\x00digest -> records index
	tags    map[string]map[string]string // repository -> tag -> digest
	fault   map[string]bool
	calls   map[string]int
}

var (
	_             service.Store = (*memoryStore)(nil)
	errMemBackend               = errors.New("memory backend unavailable: connection reset")
)

func newMemoryStore() *memoryStore {
	return &memoryStore{
		index: make(map[string]int),
		tags:  make(map[string]map[string]string),
		fault: make(map[string]bool),
		calls: make(map[string]int),
	}
}

func memID(repo, digest string) string { return repo + "\x00" + digest }

func (m *memoryStore) Register(in service.Record) (service.Record, service.RegisterStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls["register"]++
	if m.fault["register"] {
		return service.Record{}, 0, errMemBackend
	}
	if i, ok := m.index[memID(in.Repository, in.Digest)]; ok {
		existing := m.records[i]
		if existing.Tag == in.Tag && existing.SignatureVerified == in.SignatureVerified &&
			existing.RetentionDays == in.RetentionDays && existing.SizeBytes == in.SizeBytes {
			return existing, service.StatusDuplicate, nil
		}
		return service.Record{}, service.StatusConflict, nil
	}
	m.index[memID(in.Repository, in.Digest)] = len(m.records)
	m.records = append(m.records, in)
	if m.tags[in.Repository] == nil {
		m.tags[in.Repository] = make(map[string]string)
	}
	m.tags[in.Repository][in.Tag] = in.Digest
	return in, service.StatusCreated, nil
}

func (m *memoryStore) ListRecords(repo string) ([]service.Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls["list"]++
	if m.fault["list"] {
		return nil, errMemBackend
	}
	var out []service.Record
	for _, r := range m.records {
		if r.Repository == repo {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *memoryStore) RecordByDigest(repo, digest string) (service.Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls["digest"]++
	if m.fault["digest"] {
		return service.Record{}, errMemBackend
	}
	if i, ok := m.index[memID(repo, digest)]; ok {
		return m.records[i], nil
	}
	return service.Record{}, service.ErrRecordNotFound
}

func (m *memoryStore) RecordByTag(repo, tag string) (service.Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls["tag"]++
	if m.fault["tag"] {
		return service.Record{}, errMemBackend
	}
	digest, ok := m.tags[repo][tag]
	if !ok {
		return service.Record{}, service.ErrRecordNotFound
	}
	return m.records[m.index[memID(repo, digest)]], nil
}

func (m *memoryStore) registerCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls["register"]
}

func (m *memoryStore) queryCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls["list"] + m.calls["tag"] + m.calls["digest"]
}

func memRegistration(repo, digest, tag string, sig bool, retention, size int64) string {
	return fmt.Sprintf(
		`{"repository":%q,"digest":%q,"tag":%q,"signature_verified":%t,"retention_days":%d,"size_bytes":%d}`,
		repo, digest, tag, sig, retention, size)
}

func do(router *gin.Engine, method, target, body string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
	}
	router.ServeHTTP(recorder, httptest.NewRequest(method, target, reader))
	return recorder
}

func postMem(router *gin.Engine, body string) *httptest.ResponseRecorder {
	return do(router, http.MethodPost, "/v1/artifacts", body)
}

func getMem(router *gin.Engine, query string) *httptest.ResponseRecorder {
	return do(router, http.MethodGet, "/v1/artifacts?"+query, "")
}

func decodeMem(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", recorder.Body.String(), err)
	}
	return body
}

func artifactsArray(t *testing.T, recorder *httptest.ResponseRecorder) []any {
	t.Helper()
	items, ok := decodeMem(t, recorder)["artifacts"].([]any)
	if !ok {
		t.Fatalf("response has no artifacts array: %s", recorder.Body)
	}
	return items
}

func assertErrorShape(t *testing.T, recorder *httptest.ResponseRecorder, wantStatus int, wantCode, wantMessage string) {
	t.Helper()
	if recorder.Code != wantStatus {
		t.Fatalf("status = %d, want %d (%s)", recorder.Code, wantStatus, recorder.Body)
	}
	errObj, ok := decodeMem(t, recorder)["error"].(map[string]any)
	if !ok {
		t.Fatalf("response has no top-level error object: %s", recorder.Body)
	}
	if len(errObj) != 2 {
		t.Fatalf("error object must hold exactly code and message: %v", errObj)
	}
	if code, _ := errObj["code"].(string); code != wantCode {
		t.Fatalf("error code = %q, want %q", code, wantCode)
	}
	if message, _ := errObj["message"].(string); message != wantMessage {
		t.Fatalf("error message = %q, want %q", message, wantMessage)
	}
	lowered := strings.ToLower(wantMessage)
	for _, leaked := range []string{"sql", "sqlite", "insert", "select", "begin", ".go", "connection reset", "goroutine", "stack"} {
		if strings.Contains(lowered, leaked) {
			t.Fatalf("error message %q leaks internal detail %q", wantMessage, leaked)
		}
	}
}

// TestNewEntryRegistrationDuplicateConflictAndQueries drives the complete
// created -> tag move -> identical retry -> conflict sequence through a
// router built solely from the supplied store, plus all three query kinds and
// cross-repository isolation.
func TestNewEntryRegistrationDuplicateConflictAndQueries(t *testing.T) {
	mem := newMemoryStore()
	router := api.NewRouterWithHealth(mem, func() error { return nil })

	bodyA := memRegistration("team/app", memDigestA, "latest", true, 30, 1024)
	bodyB := memRegistration("team/app", memDigestB, "latest", true, 30, 2048)

	// First registration: 201 with a service-generated RFC3339 pushed_at.
	first := postMem(router, bodyA)
	if first.Code != http.StatusCreated {
		t.Fatalf("register A status = %d, want 201 (%s)", first.Code, first.Body)
	}
	recordA := decodeMem(t, first)
	pushedA, _ := recordA["pushed_at"].(string)
	if pushedA == "" {
		t.Fatalf("A response has no pushed_at: %v", recordA)
	}
	if recordA["repository"] != "team/app" || recordA["digest"] != memDigestA || recordA["tag"] != "latest" ||
		recordA["signature_verified"] != true || recordA["retention_days"] != float64(30) ||
		recordA["size_bytes"] != float64(1024) {
		t.Fatalf("A record has unexpected fields: %v", recordA)
	}

	// B under the same tag: 201 and the pointer moves; A survives.
	second := postMem(router, bodyB)
	if second.Code != http.StatusCreated {
		t.Fatalf("register B status = %d, want 201 (%s)", second.Code, second.Body)
	}
	pushedB := decodeMem(t, second)["pushed_at"].(string)
	tagged := artifactsArray(t, getMem(router, "repository=team/app&tag=latest"))
	if len(tagged) != 1 || tagged[0].(map[string]any)["digest"] != memDigestB {
		t.Fatalf("tag does not point at B: %v", tagged)
	}

	// Identical retry: still 201, original pushed_at, no new record and the
	// tag pointer must not move back to A.
	retry := postMem(router, bodyA)
	if retry.Code != http.StatusCreated {
		t.Fatalf("retry A status = %d, want 201 (%s)", retry.Code, retry.Body)
	}
	if got := decodeMem(t, retry)["pushed_at"]; got != pushedA {
		t.Fatalf("retry pushed_at = %v, want original %v", got, pushedA)
	}
	if items := artifactsArray(t, getMem(router, "repository=team/app")); len(items) != 2 {
		t.Fatalf("retry added a record: list = %v", items)
	}
	if items := artifactsArray(t, getMem(router, "repository=team/app&tag=latest")); items[0].(map[string]any)["digest"] != memDigestB {
		t.Fatalf("retry moved the tag pointer back: %v", items)
	}

	// Whitespace-normalized equivalent retry with an unknown field also keeps
	// the original pushed_at.
	normalized := `{"repository":" team/app ","digest":"` + memDigestA + `","tag":" latest ",` +
		`"signature_verified":true,"retention_days":30,"size_bytes":1024,"extra":1}`
	if got := postMem(router, normalized); got.Code != http.StatusCreated ||
		decodeMem(t, got)["pushed_at"] != pushedA {
		t.Fatalf("normalized retry = %d %s, want 201 with original pushed_at", got.Code, got.Body)
	}

	// List keeps first-registration order A then B.
	list := artifactsArray(t, getMem(router, "repository=team/app"))
	digests := []string{list[0].(map[string]any)["digest"].(string), list[1].(map[string]any)["digest"].(string)}
	if digests[0] != memDigestA || digests[1] != memDigestB {
		t.Fatalf("list order = %v, want [A B]", digests)
	}

	// Digest lookup fetches the immutable A, including its pushed_at.
	byDigest := artifactsArray(t, getMem(router, "repository=team/app&digest="+memDigestA))
	if len(byDigest) != 1 || byDigest[0].(map[string]any)["pushed_at"] != pushedA {
		t.Fatalf("digest lookup changed A: %v", byDigest)
	}

	// Different content for A's identity: 409 ArtifactConflictError and
	// nothing moves — record A, tag B, list size and B's pushed_at.
	conflict := postMem(router, memRegistration("team/app", memDigestA, "other", true, 30, 2048))
	assertErrorShape(t, conflict, http.StatusConflict, "ArtifactConflictError", msgConflict)
	if items := artifactsArray(t, getMem(router, "repository=team/app")); len(items) != 2 {
		t.Fatalf("conflict added or removed a record: %v", items)
	}
	stillA := artifactsArray(t, getMem(router, "repository=team/app&digest="+memDigestA))
	if stillA[0].(map[string]any)["tag"] != "latest" || stillA[0].(map[string]any)["size_bytes"] != float64(1024) ||
		stillA[0].(map[string]any)["pushed_at"] != pushedA {
		t.Fatalf("A record changed after conflict: %v", stillA)
	}
	stillTagged := artifactsArray(t, getMem(router, "repository=team/app&tag=latest"))
	if stillTagged[0].(map[string]any)["digest"] != memDigestB || stillTagged[0].(map[string]any)["pushed_at"] != pushedB {
		t.Fatalf("tag pointer changed after conflict: %v", stillTagged)
	}

	// Repositories do not interfere: same digest and tag name in another repo
	// is an independent identity and pointer.
	other := postMem(router, memRegistration("team/other", memDigestA, "latest", true, 30, 1024))
	if other.Code != http.StatusCreated {
		t.Fatalf("register other repo status = %d (%s)", other.Code, other.Body)
	}
	otherTag := artifactsArray(t, getMem(router, "repository=team/other&tag=latest"))
	if otherTag[0].(map[string]any)["repository"] != "team/other" {
		t.Fatalf("tag lookup crossed repositories: %v", otherTag)
	}
	if items := artifactsArray(t, getMem(router, "repository=team/other")); len(items) != 1 {
		t.Fatalf("other repo list leaked records: %v", items)
	}

	// Legal misses stay 404 ArtifactNotFoundError.
	assertErrorShape(t, getMem(router, "repository=team/missing"),
		http.StatusNotFound, "ArtifactNotFoundError", msgNotFound)
	assertErrorShape(t, getMem(router, "repository=team/app&tag=missing"),
		http.StatusNotFound, "ArtifactNotFoundError", msgNotFound)
	assertErrorShape(t, getMem(router, "repository=team/app&digest="+memDigestB[:len(memDigestB)-1]+"c"),
		http.StatusNotFound, "ArtifactNotFoundError", msgNotFound)
}

// TestNewEntryInvalidInputNeverTouchesStorage forces every backend operation
// to fail, then proves malformed input still answers 400
// InvalidArtifactInputError, that the store is never called for it, and that a
// failing health probe does not change the priority either.
func TestNewEntryInvalidInputNeverTouchesStorage(t *testing.T) {
	mem := newMemoryStore()
	for _, op := range []string{"register", "list", "tag", "digest"} {
		mem.fault[op] = true
	}
	probeHealthy := false
	router := api.NewRouterWithHealth(mem, func() error {
		if !probeHealthy {
			return errors.New("probe down")
		}
		return nil
	})

	invalidPosts := map[string]string{
		"malformed json":  `{"repository":`,
		"missing size":    `{"repository":"team/app","digest":"` + memDigestA + `","tag":"latest","signature_verified":true,"retention_days":30}`,
		"null retention":  `{"repository":"team/app","digest":"` + memDigestA + `","tag":"latest","signature_verified":true,"retention_days":null,"size_bytes":1}`,
		"bad digest":      `{"repository":"team/app","digest":"sha256:xyz","tag":"latest","signature_verified":true,"retention_days":30,"size_bytes":1}`,
		"blank tag":       `{"repository":"team/app","digest":"` + memDigestA + `","tag":"  ","signature_verified":true,"retention_days":30,"size_bytes":1}`,
		"retention zero":  `{"repository":"team/app","digest":"` + memDigestA + `","tag":"latest","signature_verified":true,"retention_days":0,"size_bytes":1}`,
		"negative size":   `{"repository":"team/app","digest":"` + memDigestA + `","tag":"latest","signature_verified":true,"retention_days":30,"size_bytes":-1}`,
		"trailing object": memRegistration("team/app", memDigestA, "latest", true, 30, 1) + ` {}`,
	}
	for name, body := range invalidPosts {
		t.Run("post "+name, func(t *testing.T) {
			before := mem.registerCalls()
			assertErrorShape(t, postMem(router, body),
				http.StatusBadRequest, "InvalidArtifactInputError", msgInvalidReg)
			if got := mem.registerCalls(); got != before {
				t.Fatalf("invalid POST reached Register: calls %d -> %d", before, got)
			}
		})
	}

	invalidGets := map[string]string{
		"missing repository":    "tag=latest",
		"blank repository":      "repository=%20%20",
		"tag and digest":        "repository=team/app&tag=latest&digest=" + memDigestA,
		"empty tag":             "repository=team/app&tag=",
		"malformed digest":      "repository=team/app&digest=sha256:xyz",
		"empty tag with digest": "repository=team/app&tag=&digest=" + memDigestA,
	}
	for name, query := range invalidGets {
		t.Run("get "+name, func(t *testing.T) {
			before := mem.queryCalls()
			assertErrorShape(t, getMem(router, query),
				http.StatusBadRequest, "InvalidArtifactInputError", msgInvalidQuery)
			if after := mem.queryCalls(); after != before {
				t.Fatalf("invalid GET reached a query method: calls %d -> %d", before, after)
			}
		})
	}

	// The failing probe does not promote anything to 503: validation still
	// wins while /healthz independently reports the probe failure.
	assertErrorShape(t, postMem(router, `{"repository":`),
		http.StatusBadRequest, "InvalidArtifactInputError", msgInvalidReg)
	assertErrorShape(t, do(router, http.MethodGet, "/healthz", ""),
		http.StatusServiceUnavailable, "storage_unavailable", msgStorage)

	// And legal requests against the faulting store still degrade to 503.
	assertErrorShape(t, postMem(router, memRegistration("team/app", memDigestA, "latest", true, 30, 1)),
		http.StatusServiceUnavailable, "storage_unavailable", msgStorage)
	assertErrorShape(t, getMem(router, "repository=team/app"),
		http.StatusServiceUnavailable, "storage_unavailable", msgStorage)
}

// TestNewEntryStorageFaultsReturn503 exercises every legal operation against
// a backend fault one at a time; each must answer 503 storage_unavailable
// with the fixed, leak-free error shape.
func TestNewEntryStorageFaultsReturn503(t *testing.T) {
	mem := newMemoryStore()
	router := api.NewRouterWithHealth(mem, func() error { return nil })
	if r := postMem(router, memRegistration("team/app", memDigestA, "latest", true, 30, 1024)); r.Code != http.StatusCreated {
		t.Fatalf("seed register: %d %s", r.Code, r.Body)
	}

	legalRequests := map[string]func(){
		"register fault": func() {
			mem.fault["register"] = true
			assertErrorShape(t, postMem(router, memRegistration("team/app", memDigestB, "latest", true, 30, 2048)),
				http.StatusServiceUnavailable, "storage_unavailable", msgStorage)
		},
		"list fault": func() {
			mem.fault["list"] = true
			assertErrorShape(t, getMem(router, "repository=team/app"),
				http.StatusServiceUnavailable, "storage_unavailable", msgStorage)
		},
		"tag fault": func() {
			mem.fault["tag"] = true
			assertErrorShape(t, getMem(router, "repository=team/app&tag=latest"),
				http.StatusServiceUnavailable, "storage_unavailable", msgStorage)
		},
		"digest fault": func() {
			mem.fault["digest"] = true
			assertErrorShape(t, getMem(router, "repository=team/app&digest="+memDigestA),
				http.StatusServiceUnavailable, "storage_unavailable", msgStorage)
		},
	}
	for name, fault := range legalRequests {
		t.Run(name, func(t *testing.T) { fault() })
	}

	// A fault must not poison later successful requests once it clears: the
	// same router keeps serving 201/200 through the same supplied store.
	for op := range mem.fault {
		mem.fault[op] = false
	}
	if r := postMem(router, memRegistration("team/app", memDigestB, "latest", true, 30, 2048)); r.Code != http.StatusCreated {
		t.Fatalf("register after faults cleared = %d (%s)", r.Code, r.Body)
	}
	if r := getMem(router, "repository=team/app"); r.Code != http.StatusOK {
		t.Fatalf("list after faults cleared = %d (%s)", r.Code, r.Body)
	}
}

// TestHealthProbeIndependentOfArtifacts proves the two independence rules in
// both directions: a failing probe cannot block artifact operations that
// still succeed, and storage faults cannot replace the probe result. The
// probe is also the only thing /healthz consults, once per call.
func TestHealthProbeIndependentOfArtifacts(t *testing.T) {
	mem := newMemoryStore()
	probeCalls := 0
	probeHealthy := true
	probe := api.HealthChecker(func() error {
		probeCalls++
		if !probeHealthy {
			return errors.New("external dependency down")
		}
		return nil
	})
	router := api.NewRouterWithHealth(mem, probe)

	// 1. Probe failing, storage healthy: 503 from /healthz, but registration
	// and every query still succeed — health is not a precondition.
	probeHealthy = false
	assertErrorShape(t, do(router, http.MethodGet, "/healthz", ""),
		http.StatusServiceUnavailable, "storage_unavailable", msgStorage)
	if r := postMem(router, memRegistration("team/app", memDigestA, "latest", true, 30, 1024)); r.Code != http.StatusCreated {
		t.Fatalf("register while probe down = %d (%s)", r.Code, r.Body)
	}
	if r := postMem(router, memRegistration("team/app", memDigestB, "latest", true, 30, 2048)); r.Code != http.StatusCreated {
		t.Fatalf("register B while probe down = %d (%s)", r.Code, r.Body)
	}
	if r := getMem(router, "repository=team/app"); r.Code != http.StatusOK {
		t.Fatalf("list while probe down = %d (%s)", r.Code, r.Body)
	}
	if items := artifactsArray(t, getMem(router, "repository=team/app&tag=latest")); items[0].(map[string]any)["digest"] != memDigestB {
		t.Fatalf("tag query while probe down returned %v", items)
	}
	if r := getMem(router, "repository=team/app&digest="+memDigestA); r.Code != http.StatusOK {
		t.Fatalf("digest query while probe down = %d (%s)", r.Code, r.Body)
	}
	if probeCalls != 1 {
		t.Fatalf("artifact requests invoked the probe: calls = %d, want 1", probeCalls)
	}

	// 2. Probe healthy, storage faulting: /healthz keeps answering 200 with
	// the existing JSON while legal artifact requests answer 503.
	probeHealthy = true
	for _, op := range []string{"register", "list", "tag", "digest"} {
		mem.fault[op] = true
	}
	healthOK := do(router, http.MethodGet, "/healthz", "")
	if healthOK.Code != http.StatusOK {
		t.Fatalf("healthz with faulting store = %d (%s)", healthOK.Code, healthOK.Body)
	}
	if got := healthOK.Body.String(); got != `{"database":"ok","status":"ok"}` {
		t.Fatalf("healthz body = %s", got)
	}
	assertErrorShape(t, postMem(router, memRegistration("team/app", memDigestA, "latest", true, 30, 1)),
		http.StatusServiceUnavailable, "storage_unavailable", msgStorage)
	assertErrorShape(t, getMem(router, "repository=team/app"),
		http.StatusServiceUnavailable, "storage_unavailable", msgStorage)

	// 3. Each /healthz call consults the probe afresh and follows its current
	// result immediately; artifact traffic never invokes it.
	for _, op := range []string{"register", "list", "tag", "digest"} {
		mem.fault[op] = false
	}
	callsBefore := probeCalls
	probeHealthy = true
	if r := do(router, http.MethodGet, "/healthz", ""); r.Code != http.StatusOK {
		t.Fatalf("probe flipped healthy but status = %d", r.Code)
	}
	probeHealthy = false
	assertErrorShape(t, do(router, http.MethodGet, "/healthz", ""),
		http.StatusServiceUnavailable, "storage_unavailable", msgStorage)
	probeHealthy = true
	if r := do(router, http.MethodGet, "/healthz", ""); r.Code != http.StatusOK {
		t.Fatalf("probe flipped healthy again but status = %d", r.Code)
	}
	if probeCalls != callsBefore+3 {
		t.Fatalf("probe calls = %d, want exactly %d (one per healthz)", probeCalls, callsBefore+3)
	}
}

// TestNewRouterWithoutHealthCheckSuppliesArtifactsOnly builds the surface with
// the store-only entry: no Ping is required or available, /healthz is an
// unknown route like any other, and the artifact entries are fully usable.
func TestNewRouterWithoutHealthCheckSuppliesArtifactsOnly(t *testing.T) {
	mem := newMemoryStore()
	router := api.NewRouter(mem)

	// No Ping on the supplied type: the store-only entry must not demand one.
	type pinger interface{ Ping() error }
	if _, ok := any(mem).(pinger); ok {
		t.Fatal("self-supplied store unexpectedly exposes Ping")
	}

	if r := postMem(router, memRegistration("team/app", memDigestA, "latest", true, 30, 1024)); r.Code != http.StatusCreated {
		t.Fatalf("register via store-only router = %d (%s)", r.Code, r.Body)
	}
	if r := getMem(router, "repository=team/app&tag=latest"); r.Code != http.StatusOK {
		t.Fatalf("query via store-only router = %d (%s)", r.Code, r.Body)
	}

	// /healthz was never registered, so it keeps the standard unknown-route
	// shape rather than a fabricated health result.
	assertErrorShape(t, do(router, http.MethodGet, "/healthz", ""),
		http.StatusNotFound, "route_not_found", msgNoRoute)
	assertErrorShape(t, do(router, http.MethodGet, "/v1/nothing", ""),
		http.StatusNotFound, "route_not_found", msgNoRoute)
}

// TestSQLiteStoreRemainsCompatibleAcrossEntries confirms the original SQLite
// backend and existing data keep working through the new assembly: records,
// first-registration order, pushed_at values and tag pointers survive a
// close/reopen performed with different router constructors, and the bundled
// Ping continues to serve as the independently wired health check.
func TestSQLiteStoreRemainsCompatibleAcrossEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")

	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	router := api.NewRouterWithHealth(st, st.Ping) // main.go-style wiring
	first := postMem(router, memRegistration("team/app", memDigestA, "latest", true, 30, 1024))
	if first.Code != http.StatusCreated {
		t.Fatalf("register A = %d (%s)", first.Code, first.Body)
	}
	pushedA := decodeMem(t, first)["pushed_at"].(string)
	second := postMem(router, memRegistration("team/app", memDigestB, "latest", true, 30, 2048))
	if second.Code != http.StatusCreated {
		t.Fatalf("register B = %d (%s)", second.Code, second.Body)
	}
	pushedB := decodeMem(t, second)["pushed_at"].(string)
	if r := do(router, http.MethodGet, "/healthz", ""); r.Code != http.StatusOK ||
		r.Body.String() != `{"database":"ok","status":"ok"}` {
		t.Fatalf("healthz before close = %d %s", r.Code, r.Body)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen the existing file and wire it through the store-only entry: the
	// router neither reopened nor closed it, and committed data is intact.
	reopened, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { reopened.Close() })
	router = api.NewRouter(reopened)

	items := artifactsArray(t, getMem(router, "repository=team/app"))
	if len(items) != 2 ||
		items[0].(map[string]any)["digest"] != memDigestA || items[0].(map[string]any)["pushed_at"] != pushedA ||
		items[1].(map[string]any)["digest"] != memDigestB || items[1].(map[string]any)["pushed_at"] != pushedB {
		t.Fatalf("records, order or pushed_at changed after reopen: %v", items)
	}
	tagged := artifactsArray(t, getMem(router, "repository=team/app&tag=latest"))
	if tagged[0].(map[string]any)["digest"] != memDigestB || tagged[0].(map[string]any)["pushed_at"] != pushedB {
		t.Fatalf("tag pointer did not survive reopen: %v", tagged)
	}
	old := artifactsArray(t, getMem(router, "repository=team/app&digest="+memDigestA))
	if old[0].(map[string]any)["pushed_at"] != pushedA || old[0].(map[string]any)["size_bytes"] != float64(1024) {
		t.Fatalf("A record did not survive reopen: %v", old)
	}

	// An identical retry after reopen is still a duplicate with the original
	// pushed_at; a content change is still 409 — single-transaction semantics
	// over existing data are unchanged.
	if r := postMem(router, memRegistration("team/app", memDigestA, "latest", true, 30, 1024)); r.Code != http.StatusCreated ||
		decodeMem(t, r)["pushed_at"] != pushedA {
		t.Fatalf("duplicate after reopen = %d %s", r.Code, r.Body)
	}
	assertErrorShape(t, postMem(router, memRegistration("team/app", memDigestA, "latest", true, 31, 1024)),
		http.StatusConflict, "ArtifactConflictError", msgConflict)

	// Adding the health route later against the same reopened handle restores
	// the /healthz surface without touching artifact state.
	router = api.NewRouterWithHealth(reopened, reopened.Ping)
	if r := do(router, http.MethodGet, "/healthz", ""); r.Code != http.StatusOK {
		t.Fatalf("healthz after rewiring = %d (%s)", r.Code, r.Body)
	}
	if items := artifactsArray(t, getMem(router, "repository=team/app")); len(items) != 2 {
		t.Fatalf("rewiring health changed stored records: %v", items)
	}
}
