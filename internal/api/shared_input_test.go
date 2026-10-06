package api

// These cases prove the HTTP handlers and a direct caller use one and the
// same input boundary: for each probe, the service.RegisterInput /
// service.Query a direct input.Parse* call returns must be byte-for-byte
// what the HTTP handler hands the service — or both reject with the shared
// sentinel / published 400 and storage is never touched. Exact int64
// forms, duplicate-key ordering and error priority get explicit rows.

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/Bob-xisuke/orb-registry-catalog/internal/input"
	"github.com/Bob-xisuke/orb-registry-catalog/internal/service"
)

// captureStore is a service.Store that records the exact business inputs
// the service dispatches: Register arrives as a service.Record (pushed_at
// aside, its six fields are the RegisterInput), and each read method lets
// the dispatched service.Query be reconstructed (kind + repository +
// selector). It keeps the same duplicate/conflict semantics as the SQLite
// adapter so retry and conflict rows behave.
type captureStore struct {
	records    []service.Record
	byID       map[string]int
	tags       map[string]map[string]string
	lastRecord service.Record
	lastQuery  service.Query
	fault      error
	regCalls   int
	readCalls  int
}

func newCaptureStore() *captureStore {
	return &captureStore{
		byID: make(map[string]int),
		tags: make(map[string]map[string]string),
	}
}

func captureID(repository, digest string) string { return repository + "\x00" + digest }

func (s *captureStore) Register(in service.Record) (service.Record, service.RegisterStatus, error) {
	s.regCalls++
	s.lastRecord = in
	if s.fault != nil {
		return service.Record{}, 0, s.fault
	}
	key := captureID(in.Repository, in.Digest)
	if idx, ok := s.byID[key]; ok {
		existing := s.records[idx]
		if existing.Tag == in.Tag && existing.SignatureVerified == in.SignatureVerified &&
			existing.RetentionDays == in.RetentionDays && existing.SizeBytes == in.SizeBytes {
			return existing, service.StatusDuplicate, nil
		}
		return service.Record{}, service.StatusConflict, nil
	}
	s.byID[key] = len(s.records)
	s.records = append(s.records, in)
	if s.tags[in.Repository] == nil {
		s.tags[in.Repository] = make(map[string]string)
	}
	s.tags[in.Repository][in.Tag] = in.Digest
	return in, service.StatusCreated, nil
}

func (s *captureStore) ListRecords(repository string) ([]service.Record, error) {
	s.readCalls++
	s.lastQuery = service.Query{Kind: service.QueryByRepository, Repository: repository}
	if s.fault != nil {
		return nil, s.fault
	}
	var out []service.Record
	for _, record := range s.records {
		if record.Repository == repository {
			out = append(out, record)
		}
	}
	return out, nil
}

func (s *captureStore) RecordByDigest(repository, digest string) (service.Record, error) {
	s.readCalls++
	s.lastQuery = service.Query{Kind: service.QueryByDigest, Repository: repository, Digest: digest}
	if s.fault != nil {
		return service.Record{}, s.fault
	}
	if idx, ok := s.byID[captureID(repository, digest)]; ok {
		return s.records[idx], nil
	}
	return service.Record{}, service.ErrRecordNotFound
}

func (s *captureStore) RecordByTag(repository, tag string) (service.Record, error) {
	s.readCalls++
	s.lastQuery = service.Query{Kind: service.QueryByTag, Repository: repository, Tag: tag}
	if s.fault != nil {
		return service.Record{}, s.fault
	}
	digest, ok := s.tags[repository][tag]
	if !ok {
		return service.Record{}, service.ErrRecordNotFound
	}
	return s.records[s.byID[captureID(repository, digest)]], nil
}

// recordMatchesInput compares the six caller-owned fields; pushed_at is
// service-generated and never part of the parsed input.
func recordMatchesInput(t *testing.T, r service.Record, in service.RegisterInput) {
	t.Helper()
	got := service.RegisterInput{
		Repository:        r.Repository,
		Digest:            r.Digest,
		Tag:               r.Tag,
		SignatureVerified: r.SignatureVerified,
		RetentionDays:     r.RetentionDays,
		SizeBytes:         r.SizeBytes,
	}
	if got != in {
		t.Fatalf("store received %+v, want direct ParseRegistration result %+v", got, in)
	}
	if r.PushedAt == "" {
		t.Fatalf("service generated no pushed_at; the input entry must not")
	}
}

func newCaptureRouter(st *captureStore) *gin.Engine {
	return NewRouterWithStore(st, nil)
}

// TestDirectAndHTTPRegistrationShareOneEntry drives every registration
// shape through both entries and pins their identity: the HTTP handler
// passes the direct ParseRegistration result to the service, rejections are
// the shared sentinel on one side and the published 400 on the other, and a
// rejected body never reaches storage.
func TestDirectAndHTTPRegistrationShareOneEntry(t *testing.T) {
	valid := map[string]string{
		"plain":             registration("registry-demo/cap", testDigestA, "release", true, regRetention, regSizeA),
		"trimmed and zeros": `{"repository":"  registry-demo/cap  ","digest":"` + testDigestB + `","tag":"  zero  ","signature_verified":false,"retention_days":1,"size_bytes":0,"extra":"ignored"}`,
		"exact 2^53+1":      registration("registry-demo/cap", testDigestC, "big", true, 1, 9007199254740993),
		"exact max int64":   registration("registry-demo/cap", testDigestD, "max", true, 3650, 9223372036854775807),
		"duplicate keys": `{"repository":"registry-demo/decoy","repository":"registry-demo/cap",` +
			`"digest":"` + testDigestB + `","digest":"` + testDigestA + `",` +
			`"tag":"decoy","tag":"release","signature_verified":"x","signature_verified":true,` +
			`"retention_days":null,"retention_days":30,"size_bytes":"1","size_bytes":1024}`,
		"unicode escaped keys": `{` +
			`"\u0072epository":"registry-demo/cap",` +
			`"\u0064igest":"` + testDigestE + `",` +
			`"\u0074ag":"escaped",` +
			`"signature_\u0076erified":true,` +
			`"retention_\u0064ays":30,` +
			`"size_\u0062ytes":7}`,
	}
	for name, body := range valid {
		t.Run(name, func(t *testing.T) {
			st := newCaptureStore()
			router := newCaptureRouter(st)

			direct, err := input.ParseRegistration(strings.NewReader(body))
			if err != nil {
				t.Fatalf("direct ParseRegistration: %v", err)
			}
			recorder := postArtifact(t, router, body)
			if recorder.Code != http.StatusCreated {
				t.Fatalf("HTTP status = %d, want 201 (%s)", recorder.Code, recorder.Body)
			}
			if st.regCalls != 1 {
				t.Fatalf("store Register calls = %d, want exactly 1", st.regCalls)
			}
			recordMatchesInput(t, st.lastRecord, direct)

			// The identical retry keeps 201, reaches storage with the very
			// same parsed input, and changes neither record nor tag target.
			retry := postArtifact(t, router, body)
			if retry.Code != http.StatusCreated {
				t.Fatalf("retry status = %d, want 201 (%s)", retry.Code, retry.Body)
			}
			recordMatchesInput(t, st.lastRecord, direct)
			if len(st.records) != 1 {
				t.Fatalf("identical retry wrote a record: now %d", len(st.records))
			}
		})
	}

	invalid := map[string]string{
		"malformed json":      `{"repository":`,
		"json array":          `["repository"]`,
		"json null":           `null`,
		"trailing content":    valid["plain"] + ` {}`,
		"missing field":       registrationWithField("size_bytes", ""),
		"null field":          registrationWithField("signature_verified", "null"),
		"blank after trim":    registrationWithField("tag", `"  "`),
		"duplicate last null": `{"repository":"registry-demo/cap","digest":"` + testDigestA + `","tag":"t","signature_verified":true,"retention_days":30,"size_bytes":4,"size_bytes":null}`,
		"fractional integer":  `{"repository":"registry-demo/cap","digest":"` + testDigestA + `","tag":"t","signature_verified":true,"retention_days":30,"size_bytes":1.5}`,
		"exponential integer": `{"repository":"registry-demo/cap","digest":"` + testDigestA + `","tag":"t","signature_verified":true,"retention_days":3e1,"size_bytes":1}`,
		"string integer":      `{"repository":"registry-demo/cap","digest":"` + testDigestA + `","tag":"t","signature_verified":true,"retention_days":"30","size_bytes":1}`,
		"int64 overflow": `{"repository":"registry-demo/cap","digest":"` + testDigestA +
			`","tag":"t","signature_verified":true,"retention_days":30,"size_bytes":9223372036854775808}`,
		"retention out of range": registration("registry-demo/cap", testDigestA, "t", true, 3651, 1),
	}
	for name, body := range invalid {
		t.Run(name, func(t *testing.T) {
			st := newCaptureStore()
			router := newCaptureRouter(st)

			direct, err := input.ParseRegistration(strings.NewReader(body))
			if !errors.Is(err, input.ErrInvalidInput) {
				t.Fatalf("direct error = %v, want ErrInvalidInput", err)
			}
			if direct != (service.RegisterInput{}) {
				t.Fatalf("direct result = %+v, want zero", direct)
			}
			assertErrorResponse(t, postArtifact(t, router, body),
				http.StatusBadRequest, codeInvalidInput, msgInvalidReg)
			if st.regCalls != 0 {
				t.Fatalf("rejected body reached storage %d times", st.regCalls)
			}
		})
	}
}

// TestDirectAndHTTPRegistrationReadFailure pins the reader-failure row on
// both entries with the same failing stream.
func TestDirectAndHTTPRegistrationReadFailure(t *testing.T) {
	newFail := func() io.Reader {
		return &registrationFailReader{prefix: []byte(`{"repository":"x"`), err: errors.New("read boom")}
	}
	st := newCaptureStore()
	router := newCaptureRouter(st)

	direct, err := input.ParseRegistration(newFail())
	if !errors.Is(err, input.ErrInvalidInput) || direct != (service.RegisterInput{}) {
		t.Fatalf("direct read failure = (%+v, %v)", direct, err)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/artifacts", newFail())
	router.ServeHTTP(recorder, request)
	assertErrorResponse(t, recorder, http.StatusBadRequest, codeInvalidInput, msgInvalidReg)
	if st.regCalls != 0 {
		t.Fatalf("failed read reached storage %d times", st.regCalls)
	}
}

// registrationFailReader delivers prefix once, then err on every Read.
type registrationFailReader struct {
	prefix []byte
	err    error
}

func (r *registrationFailReader) Read(p []byte) (int, error) {
	if len(r.prefix) > 0 {
		n := copy(p, r.prefix)
		r.prefix = r.prefix[n:]
		return n, nil
	}
	return 0, r.err
}

// TestDirectAndHTTPQueryShareOneEntry drives each query shape through both
// entries: the dispatched store call reconstructs exactly the service.Query
// the direct call returned; invalid rows are the sentinel plus 400 with no
// storage call.
func TestDirectAndHTTPQueryShareOneEntry(t *testing.T) {
	st := newCaptureStore()
	router := newCaptureRouter(st)
	const repo = "registry-demo/qcap"
	// Seed so every legal selector resolves: alpha, beta, a space tag and a
	// plus tag.
	mustRegister(t, router, repo, testDigestA, "alpha")
	mustRegister(t, router, repo, testDigestB, "beta")
	postArtifact(t, router, registration(repo, testDigestD, "a b", true, regRetention, regSizeA))
	postArtifact(t, router, registration(repo, testDigestE, "a+b", true, regRetention, regSizeA))

	cases := map[string]string{
		"repository list":           "repository=" + repo,
		"repository padded":         "repository=++" + repo + "++",
		"tag first value":           "repository=" + repo + "&tag=alpha&tag=beta",
		"tag later illegal ignored": "repository=" + repo + "&tag=alpha&tag=",
		"tag later missing ignored": "repository=" + repo + "&tag=beta&tag=missing",
		"digest first value":        "repository=" + repo + "&digest=" + testDigestA + "&digest=" + testDigestB,
		"digest later malformed":    "repository=" + repo + "&digest=" + testDigestA + "&digest=sha256:xyz",
		"plus decodes to space":     "repository=" + repo + "&tag=a+b",
		"percent-20 decodes space":  "repository=" + repo + "&tag=a%20b",
		"percent-2B decodes plus":   "repository=" + repo + "&tag=a%2Bb",
		"escaped tag name":          "repository=" + repo + "&%74ag=alpha",
		"cased name is unknown":     "repository=" + repo + "&Tag=alpha",
		"unknown repeats ignored":   "repository=" + repo + "&tag=alpha&frob=1&frob=2",
		"undecodable pair skipped":  "repository=" + repo + "&tag=%zz",
		"bad pair before good tag":  "repository=" + repo + "&tag=%zz&tag=alpha",
		"semicolon segment skipped": "repository=" + repo + "&tag=alpha;evil=1",
		"selector after semicolon":  "repository=" + repo + "&digest=" + testDigestA + ";x=1&tag=alpha",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			direct, err := input.ParseQuery(raw)
			if err != nil {
				t.Fatalf("direct ParseQuery(%q): %v", raw, err)
			}
			recorder := getArtifacts(t, router, raw)
			if recorder.Code == http.StatusBadRequest {
				t.Fatalf("HTTP 400 for a query direct parsing accepted: %s", recorder.Body)
			}
			if !reflect.DeepEqual(st.lastQuery, direct) {
				t.Fatalf("dispatched query = %+v, want direct result %+v", st.lastQuery, direct)
			}
		})
	}

	// A literal %3B in a tag value reaches storage decoded (no segment is
	// skipped); it simply matches no tag, which is the 404 path.
	raw := "repository=" + repo + "&tag=a%3Bb"
	direct, err := input.ParseQuery(raw)
	if err != nil {
		t.Fatalf("direct %%3B query: %v", err)
	}
	if direct.Tag != "a;b" {
		t.Fatalf("%%3B tag = %q, want a;b", direct.Tag)
	}
	assertErrorResponse(t, getArtifacts(t, router, raw), http.StatusNotFound, codeNotFound, msgNotFound)
	if !reflect.DeepEqual(st.lastQuery, direct) {
		t.Fatalf("%%3B dispatched query = %+v, want %+v", st.lastQuery, direct)
	}

	invalid := []string{
		"",
		"repository=&tag=alpha",
		"repository=%20%20",
		"repository&tag=alpha",
		"repository=" + repo + "&tag=",
		"repository=" + repo + "&tag=%20%20",
		"repository=" + repo + "&digest=sha256:xyz",
		"repository=" + repo + "&digest=" + testDigestA + "+",
		"repository=" + repo + "&tag=alpha&digest=" + testDigestA,
		"repository=" + repo + "&digest=" + testDigestA + "&tag=alpha",
		"?repository=" + repo,
	}
	for _, raw := range invalid {
		t.Run("invalid/"+raw, func(t *testing.T) {
			before := st.readCalls
			direct, err := input.ParseQuery(raw)
			if !errors.Is(err, input.ErrInvalidInput) || direct != (service.Query{}) {
				t.Fatalf("direct ParseQuery(%q) = (%+v, %v), want zero + ErrInvalidInput", raw, direct, err)
			}
			assertErrorResponse(t, getArtifacts(t, router, raw),
				http.StatusBadRequest, codeInvalidInput, msgInvalidQuery)
			if st.readCalls != before {
				t.Fatalf("invalid query %q reached storage", raw)
			}
		})
	}
}

// TestSharedEntryErrorPriorityOverStorageFailure proves on the real wiring
// that validation happens wholly inside the store-free input entry: with
// the store faulted, every invalid row keeps its 400 and never calls the
// store, while legal rows degrade to 503.
func TestSharedEntryErrorPriorityOverStorageFailure(t *testing.T) {
	st := newCaptureStore()
	router := newCaptureRouter(st)
	body := registration("registry-demo/prio", testDigestA, "release", true, regRetention, regSizeA)
	if recorder := postArtifact(t, router, body); recorder.Code != http.StatusCreated {
		t.Fatalf("seed register status = %d (%s)", recorder.Code, recorder.Body)
	}
	st.fault = errors.New("capture backend down")

	// Invalid registration and query rows: 400, and the store counters do
	// not move, even though every store method is faulted.
	regBefore := st.regCalls
	assertErrorResponse(t, postArtifact(t, router, registrationWithField("size_bytes", "null")),
		http.StatusBadRequest, codeInvalidInput, msgInvalidReg)
	assertErrorResponse(t, postArtifact(t, router, `{"repository":`),
		http.StatusBadRequest, codeInvalidInput, msgInvalidReg)
	assertErrorResponse(t, getArtifacts(t, router, "repository=registry-demo/prio&tag="),
		http.StatusBadRequest, codeInvalidInput, msgInvalidQuery)
	assertErrorResponse(t, getArtifacts(t, router, "repository=x&tag=r&digest="+testDigestA),
		http.StatusBadRequest, codeInvalidInput, msgInvalidQuery)
	if st.regCalls != regBefore {
		t.Fatalf("invalid registration reached a faulted store: %d calls", st.regCalls-regBefore)
	}

	// Direct parsing of the same rows rejects without any store existing in
	// its call path — the input package simply cannot reach one.
	if _, err := input.ParseRegistration(strings.NewReader(registrationWithField("size_bytes", "null"))); !errors.Is(err, input.ErrInvalidInput) {
		t.Fatalf("direct invalid registration error = %v", err)
	}
	if _, err := input.ParseQuery("repository=x&tag=r&digest=" + testDigestA); !errors.Is(err, input.ErrInvalidInput) {
		t.Fatalf("direct invalid query error = %v", err)
	}

	// Legal rows pass the shared entry and reach the faulted store: 503.
	assertErrorResponse(t, postArtifact(t, router,
		registration("registry-demo/prio", testDigestB, "release", true, regRetention, regSizeB)),
		http.StatusServiceUnavailable, codeStorage, msgStorage)
	assertErrorResponse(t, getArtifacts(t, router, "repository=registry-demo/prio"),
		http.StatusServiceUnavailable, codeStorage, msgStorage)
}

// TestSharedEntryConflictIsServiceLayerNotParse shows parsing a body that
// conflicts with committed state still succeeds directly with a non-zero
// input; the 409 is the service's decision after parsing.
func TestSharedEntryConflictIsServiceLayerNotParse(t *testing.T) {
	st := newCaptureStore()
	router := newCaptureRouter(st)
	first := registration("registry-demo/conf", testDigestA, "release", true, regRetention, regSizeA)
	if recorder := postArtifact(t, router, first); recorder.Code != http.StatusCreated {
		t.Fatalf("seed status = %d (%s)", recorder.Code, recorder.Body)
	}

	changed := registration("registry-demo/conf", testDigestA, "moved", true, regRetention, regSizeB)
	direct, err := input.ParseRegistration(strings.NewReader(changed))
	if err != nil {
		t.Fatalf("direct parse of a conflicting body rejected at the input boundary: %v", err)
	}
	if direct.Tag != "moved" || direct.SizeBytes != regSizeB {
		t.Fatalf("direct parsed input = %+v", direct)
	}
	assertErrorResponse(t, postArtifact(t, router, changed),
		http.StatusConflict, codeConflict, msgConflict)
}
