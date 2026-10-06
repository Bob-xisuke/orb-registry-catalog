package api

// These cases prove the refactor's central claim: the HTTP handlers and a
// direct caller run the exact same parsing rules. Each raw body and raw
// query string is driven through both entries — the Gin router and
// internal/input directly — and the parsed value, the dispatch result and
// the error classification must agree. Invalid input must answer 400 /
// ErrInvalidInput without ever reaching the store; the checks also run with
// the store faulted to pin error priority.

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Bob-xisuke/orb-registry-catalog/internal/input"
	"github.com/Bob-xisuke/orb-registry-catalog/internal/service"
)

// jsonEscape assembles a literal backslash-u escape from the backslash byte
// so the escape text reaches the JSON payload rather than the Go compiler.
func jsonEscape(hex string) string { return string([]byte{0x5c}) + "u" + hex }

// registerCalls reads the assembly store's Register call count.
func registerCalls(st *assemblyStore) int { return st.calls["register"] }

// TestDirectAndHTTPRegistrationShareRules posts every body through the
// router and parses the same bytes through input.ParseRegistration: a 201
// must correspond to a successful parse whose fields equal the stored
// record, and a 400 to ErrInvalidInput with the zero input and no store
// call.
func TestDirectAndHTTPRegistrationShareRules(t *testing.T) {
	st := newAssemblyStore()
	router := NewRouterWithStore(st, nil)

	cases := []struct {
		name  string
		body  string
		valid bool
	}{
		{"valid", registration("registry-demo/parity", testDigestA, "release", true, 30, 1024), true},
		{"trimmed and zero values", `{"repository":"  registry-demo/parity2  ","digest":"` + testDigestB +
			`","tag":"  latest ","signature_verified":false,"retention_days":1,"size_bytes":0}`, true},
		{"unknown fields ignored", `{"repository":"registry-demo/parity3","digest":"` + testDigestC +
			`","tag":"release","signature_verified":true,"retention_days":30,"size_bytes":7,"extra":1}`, true},
		{"exact int64 beyond float64", registration("registry-demo/parity4", testDigestD, "release", true, 1, 9007199254740993), true},
		{"duplicate keys last wins", `{"repository":"decoy","repository":"registry-demo/parity5",` +
			`"digest":"` + testDigestB + `","digest":"` + testDigestA + `",` +
			`"tag":"decoy","tag":"  release  ","signature_verified":"x","signature_verified":true,` +
			`"retention_days":null,"retention_days":30,"size_bytes":"1","size_bytes":1024}`, true},
		{"escaped key names", `{"` + jsonEscape("0072") + `epository":"registry-demo/parity6",` +
			`"` + jsonEscape("0064") + `igest":"` + testDigestA + `","` + jsonEscape("0074") + `ag":"release",` +
			`"signature_verified":true,"retention_days":30,"size_bytes":1}`, true},

		{"malformed", `{"repository":`, false},
		{"trailing content", registration("registry-demo/x", testDigestA, "t", true, 1, 1) + ` {}`, false},
		{"not an object", `[1,2,3]`, false},
		{"missing field", `{"repository":"registry-demo/x","digest":"` + testDigestA +
			`","tag":"t","signature_verified":true,"retention_days":30}`, false},
		{"null field", `{"repository":"registry-demo/x","digest":"` + testDigestA +
			`","tag":"t","signature_verified":true,"retention_days":30,"size_bytes":null}`, false},
		{"wrong type", `{"repository":7,"digest":"` + testDigestA +
			`","tag":"t","signature_verified":true,"retention_days":30,"size_bytes":1}`, false},
		{"fractional integer", `{"repository":"registry-demo/x","digest":"` + testDigestA + `","tag":"t",` +
			`"signature_verified":true,"retention_days":1.5,"size_bytes":1}`, false},
		{"exponential integer", `{"repository":"registry-demo/x","digest":"` + testDigestA +
			`","tag":"t","signature_verified":true,"retention_days":30,"size_bytes":1e3}`, false},
		{"string number", `{"repository":"registry-demo/x","digest":"` + testDigestA +
			`","tag":"t","signature_verified":true,"retention_days":30,"size_bytes":"1"}`, false},
		{"retention out of range", registration("registry-demo/x", testDigestA, "t", true, 0, 1), false},
		{"negative size", registration("registry-demo/x", testDigestA, "t", true, 30, -1), false},
		{"duplicate last invalid", `{"repository":"registry-demo/x","digest":"` + testDigestA +
			`","tag":"t","signature_verified":true,"retention_days":30,"size_bytes":1,"size_bytes":null}`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := registerCalls(st)
			recorder := postArtifact(t, router, tc.body)
			in, err := input.ParseRegistration(strings.NewReader(tc.body))

			if tc.valid {
				if recorder.Code != http.StatusCreated {
					t.Fatalf("HTTP status = %d, want 201 (%s)", recorder.Code, recorder.Body)
				}
				if err != nil {
					t.Fatalf("direct parse: %v, want nil", err)
				}
				if registerCalls(st) != before+1 {
					t.Fatalf("valid registration did not reach the store exactly once")
				}
				record := decodeBodyNumber(t, recorder)
				if record["repository"] != in.Repository || record["digest"] != in.Digest ||
					record["tag"] != in.Tag || record["signature_verified"] != in.SignatureVerified {
					t.Fatalf("HTTP record %v disagrees with parsed input %+v", record, in)
				}
				if got := jsonNumber(t, record, "retention_days"); got != itoaInput(in.RetentionDays) {
					t.Fatalf("retention_days HTTP = %s, direct = %d", got, in.RetentionDays)
				}
				if got := jsonNumber(t, record, "size_bytes"); got != itoaInput(in.SizeBytes) {
					t.Fatalf("size_bytes HTTP = %s, direct = %d", got, in.SizeBytes)
				}
			} else {
				if recorder.Code != http.StatusBadRequest {
					t.Fatalf("HTTP status = %d, want 400 (%s)", recorder.Code, recorder.Body)
				}
				if !errors.Is(err, input.ErrInvalidInput) {
					t.Fatalf("direct error = %v, want ErrInvalidInput", err)
				}
				if in != (service.RegisterInput{}) {
					t.Fatalf("direct result = %+v, want the zero value", in)
				}
				if registerCalls(st) != before {
					t.Fatalf("invalid input reached the store (calls %d -> %d)", before, registerCalls(st))
				}
			}
		})
	}
}

// itoaInput formats an int64 the way the JSON response does, for exact
// comparison with the json.Number literal.
func itoaInput(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// TestDirectAndHTTPQueryShareRules seeds one store, then drives each raw
// query through both the router and input.ParseQuery, resolving the parsed
// query through a service over the same store. HTTP status, error
// classification and the target records must all agree.
func TestDirectAndHTTPQueryShareRules(t *testing.T) {
	st := newAssemblyStore()
	router := NewRouterWithStore(st, nil)
	svc := service.New(st)
	const repo = "registry-demo/qparity"

	mustRegister(t, router, repo, testDigestA, "alpha")
	mustRegister(t, router, repo, testDigestB, "beta")
	mustRegister(t, router, repo, testDigestD, "a b")
	mustRegister(t, router, repo, testDigestE, "a+b")

	cases := []struct {
		name string
		raw  string
	}{
		{"list", "repository=" + repo},
		{"tag hit", "repository=" + repo + "&tag=alpha"},
		{"digest hit", "repository=" + repo + "&digest=" + testDigestA},
		{"tag plus is space", "repository=" + repo + "&tag=a+b"},
		{"tag percent 2B is plus", "repository=" + repo + "&tag=a%2Bb"},
		{"tag trimmed", "repository=" + repo + "&tag=+alpha+"},
		{"repeated tag first wins", "repository=" + repo + "&tag=alpha&tag=beta"},
		{"repeated digest first wins", "repository=" + repo + "&digest=" + testDigestA + "&digest=" + testDigestB},
		{"escaped tag name", "repository=" + repo + "&%74ag=alpha"},
		{"cased tag name ignored", "repository=" + repo + "&Tag=alpha"},
		{"unknown params ignored", "repository=" + repo + "&tag=alpha&unknown=1&frob="},
		{"undecodable tag drops to list", "repository=" + repo + "&tag=%zz"},
		{"undecodable digest leaves tag", "repository=" + repo + "&tag=alpha&digest=%zz"},
		{"semicolon segment dropped", "repository=" + repo + "&frob=a;b&tag=alpha"},
		{"tag miss", "repository=" + repo + "&tag=missing"},
		{"digest miss", "repository=" + repo + "&digest=" + testDigestC},
		{"repository miss", "repository=registry-demo/other"},

		{"missing repository", "tag=alpha"},
		{"blank repository", "repository=%20%20&tag=alpha"},
		{"empty tag", "repository=" + repo + "&tag="},
		{"bad digest", "repository=" + repo + "&digest=sha256:xyz"},
		{"digest leading space", "repository=" + repo + "&digest=+" + testDigestA},
		{"both selectors", "repository=" + repo + "&tag=alpha&digest=" + testDigestA},
		{"illegal first repeated value", "repository=" + repo + "&tag=&tag=alpha"},
		{"empty string", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := getArtifacts(t, router, tc.raw)
			q, err := input.ParseQuery(tc.raw)

			if err != nil {
				if recorder.Code != http.StatusBadRequest {
					t.Fatalf("HTTP status = %d, want 400 (%s)", recorder.Code, recorder.Body)
				}
				if !errors.Is(err, input.ErrInvalidInput) {
					t.Fatalf("direct error = %v, want ErrInvalidInput", err)
				}
				if q != (service.Query{}) {
					t.Fatalf("direct result = %+v, want the zero value", q)
				}
				return
			}

			// Resolve the directly parsed query through the same store.
			records, qerr := svc.Query(q)
			switch {
			case errors.Is(qerr, service.ErrNotFound):
				if recorder.Code != http.StatusNotFound {
					t.Fatalf("HTTP status = %d, want 404 (%s)", recorder.Code, recorder.Body)
				}
			case qerr != nil:
				if recorder.Code != http.StatusServiceUnavailable {
					t.Fatalf("HTTP status = %d, want 503 (%s)", recorder.Code, recorder.Body)
				}
			default:
				if recorder.Code != http.StatusOK {
					t.Fatalf("HTTP status = %d, want 200 (%s)", recorder.Code, recorder.Body)
				}
				httpDigests := recordDigests(allRecords(t, recorder))
				directDigests := make([]string, len(records))
				for i, r := range records {
					directDigests[i] = r.Digest
				}
				if len(httpDigests) != len(directDigests) {
					t.Fatalf("HTTP returned %d records, direct service %d", len(httpDigests), len(directDigests))
				}
				for i := range httpDigests {
					if httpDigests[i] != directDigests[i] {
						t.Fatalf("record %d: HTTP %s, direct %s", i, httpDigests[i], directDigests[i])
					}
				}
			}
		})
	}
}

// TestInvalidInputWinsOverFaultedStoreOnBothEntries faults the store and
// confirms invalid bodies/queries keep their 400 through the HTTP entry
// without a store call, while legal input degrades to 503. The direct
// parsing entry, by contract, never consults the store at all.
func TestInvalidInputWinsOverFaultedStoreOnBothEntries(t *testing.T) {
	st := newAssemblyStore()
	router := NewRouterWithStore(st, nil)
	const repo = "registry-demo/qfault"

	st.fault = errors.New("backend down")

	invalidBodies := []string{
		`{"repository":`,
		`{"repository":"` + repo + `","digest":"` + testDigestA + `","tag":"t",` +
			`"signature_verified":true,"retention_days":30,"size_bytes":null}`,
	}
	for _, body := range invalidBodies {
		before := registerCalls(st)
		assertErrorResponse(t, postArtifact(t, router, body),
			http.StatusBadRequest, codeInvalidInput, msgInvalidReg)
		if registerCalls(st) != before {
			t.Fatalf("invalid registration reached the faulted store")
		}
		in, err := input.ParseRegistration(strings.NewReader(body))
		if !errors.Is(err, input.ErrInvalidInput) || in != (service.RegisterInput{}) {
			t.Fatalf("direct parse: err=%v in=%+v, want ErrInvalidInput and zero", err, in)
		}
	}

	invalidQueries := []string{
		"tag=alpha",
		"repository=" + repo + "&tag=",
		"repository=" + repo + "&digest=sha256:xyz",
		"repository=" + repo + "&tag=alpha&digest=" + testDigestA,
	}
	for _, raw := range invalidQueries {
		assertErrorResponse(t, getArtifacts(t, router, raw),
			http.StatusBadRequest, codeInvalidInput, msgInvalidQuery)
		if q, err := input.ParseQuery(raw); !errors.Is(err, input.ErrInvalidInput) || q != (service.Query{}) {
			t.Fatalf("direct ParseQuery(%q): err=%v q=%+v, want ErrInvalidInput and zero", raw, err, q)
		}
	}

	// Legal input passes the shared parser and only then hits the fault.
	assertErrorResponse(t, postArtifact(t, router,
		registration(repo, testDigestA, "release", true, 30, 1024)),
		http.StatusServiceUnavailable, codeStorage, msgStorage)
	assertErrorResponse(t, getArtifacts(t, router, "repository="+repo),
		http.StatusServiceUnavailable, codeStorage, msgStorage)
}
