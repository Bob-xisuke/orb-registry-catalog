package api

// These regression cases exercise only the public HTTP surface
// (POST/GET /v1/artifacts, GET /healthz) and back the conclusions in
// docs/registration-input-decoding-analysis.md: how duplicate object keys,
// Unicode-escaped key names and JSON number forms in the POST /v1/artifacts
// request body become — or fail to become — an immutable registration record.
// They deliberately avoid the store's unexported internals so that any
// refactor preserving the published contract keeps them green.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Bob-xisuke/orb-registry-catalog/internal/store"
)

// decodeBodyNumber decodes a response body keeping every JSON number as its
// original literal (json.Number), so 64-bit integers are compared exactly
// instead of through a lossy float64.
func decodeBodyNumber(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	decoder := json.NewDecoder(strings.NewReader(recorder.Body.String()))
	decoder.UseNumber()
	if err := decoder.Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return body
}

// soleRecordNumber is soleRecord with numbers preserved as json.Number.
func soleRecordNumber(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	items, ok := decodeBodyNumber(t, recorder)["artifacts"].([]any)
	if !ok {
		t.Fatalf("response has no artifacts array: %s", recorder.Body)
	}
	if len(items) != 1 {
		t.Fatalf("got %d records, want exactly 1: %s", len(items), recorder.Body)
	}
	return items[0].(map[string]any)
}

// jsonNumber returns the exact decimal literal of a numeric record field.
func jsonNumber(t *testing.T, record map[string]any, field string) string {
	t.Helper()
	number, ok := record[field].(json.Number)
	if !ok {
		t.Fatalf("field %s is %T (%v), want json.Number", field, record[field], record[field])
	}
	return number.String()
}

// duplicateFieldBody renders a valid registration for repo with every required
// field present and legal, then repeats field with lastRaw as its last
// occurrence.
func duplicateFieldBody(repo, field, lastRaw string) string {
	return `{"repository":"` + repo + `","digest":"` + testDigestA + `","tag":"release",` +
		`"signature_verified":true,"retention_days":30,"size_bytes":1024,` +
		`"` + field + `":` + lastRaw + `}`
}

// registrationRaw renders a registration body with raw JSON literals for the
// two integer fields.
func registrationRaw(repo, digest, retentionRaw, sizeRaw string) string {
	return `{"repository":"` + repo + `","digest":"` + digest + `","tag":"release",` +
		`"signature_verified":true,"retention_days":` + retentionRaw + `,"size_bytes":` + sizeRaw + `}`
}

// TestDuplicateKeysLastValueWins proves that when a required key appears more
// than once in the same object, the last occurrence alone decides the decoded
// value: earlier occurrences are discarded even when they carry a different
// identity, a wrong type or a null, and normalization still applies to the
// last value.
func TestDuplicateKeysLastValueWins(t *testing.T) {
	router, _ := newTestRouter(t)

	// Every required field appears twice. The first occurrence is a decoy
	// identity (different repository, digest, tag) or an illegal value (wrong
	// type, null); the last occurrence is legal and must win.
	body := `{"repository":"registry-demo/decoy","repository":"registry-demo/dup",` +
		`"digest":"` + testDigestB + `","digest":"` + testDigestA + `",` +
		`"tag":"decoy-tag","tag":"release",` +
		`"signature_verified":"yes","signature_verified":true,` +
		`"retention_days":null,"retention_days":30,` +
		`"size_bytes":"1024","size_bytes":1024}`
	recorder := postArtifact(t, router, body)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d (%s)", recorder.Code, http.StatusCreated, recorder.Body)
	}
	record := decodeBody(t, recorder)
	pushedAt, _ := record["pushed_at"].(string)
	if pushedAt == "" {
		t.Fatalf("response has no pushed_at: %v", record)
	}
	expectStoredRecord(t, record, "registry-demo/dup", testDigestA, "release", true, 30, 1024, pushedAt)

	// The final identity and the tag pointer come from the last values.
	expectStoredRecord(t, soleRecord(t, queryByDigest(t, router, "registry-demo/dup", testDigestA)),
		"registry-demo/dup", testDigestA, "release", true, 30, 1024, pushedAt)
	if got := soleRecord(t, queryByTag(t, router, "registry-demo/dup", "release")); got["digest"] != testDigestA {
		t.Fatalf("tag release points at %v, want %s", got["digest"], testDigestA)
	}

	// The decoy first occurrences never became a record or a pointer.
	assertErrorResponse(t, queryList(t, router, "registry-demo/decoy"),
		http.StatusNotFound, codeNotFound, msgNotFound)
	assertErrorResponse(t, queryByTag(t, router, "registry-demo/dup", "decoy-tag"),
		http.StatusNotFound, codeNotFound, msgNotFound)
	assertErrorResponse(t, queryByDigest(t, router, "registry-demo/dup", testDigestB),
		http.StatusNotFound, codeNotFound, msgNotFound)

	// Normalization (TrimSpace) applies to the last value, not the first.
	trimmed := postArtifact(t, router, `{"repository":"registry-demo/trim","repository":"  registry-demo/trim  ",`+
		`"digest":"`+testDigestB+`","tag":"first","tag":"  release  ",`+
		`"signature_verified":false,"retention_days":1,"size_bytes":0}`)
	if trimmed.Code != http.StatusCreated {
		t.Fatalf("trimmed status = %d, want %d (%s)", trimmed.Code, http.StatusCreated, trimmed.Body)
	}
	trimmedRecord := decodeBody(t, trimmed)
	if trimmedRecord["repository"] != "registry-demo/trim" || trimmedRecord["tag"] != "release" {
		t.Fatalf("last values were not trimmed: %v", trimmedRecord)
	}
	if got := soleRecord(t, queryByTag(t, router, "registry-demo/trim", "release")); got["digest"] != testDigestB {
		t.Fatalf("trimmed tag points at %v, want %s", got["digest"], testDigestB)
	}
	assertErrorResponse(t, queryByTag(t, router, "registry-demo/trim", "first"),
		http.StatusNotFound, codeNotFound, msgNotFound)
}

// TestDuplicateKeysLastValueInvalid pins the mirror image: a legal earlier
// occurrence never rescues an illegal last one. Null, a rule-violating value
// or a wrong type in the last position is the same 400 as if it were the only
// occurrence, and no record is left behind.
func TestDuplicateKeysLastValueInvalid(t *testing.T) {
	cases := map[string]struct{ field, lastRaw string }{
		"repository null":       {"repository", `null`},
		"repository blank":      {"repository", `"   "`},
		"repository wrong type": {"repository", `7`},
		"digest null":           {"digest", `null`},
		"digest malformed":      {"digest", `"sha256:xyz"`},
		"digest wrong type":     {"digest", `7`},
		"tag null":              {"tag", `null`},
		"tag blank":             {"tag", `"  "`},
		"tag wrong type":        {"tag", `7`},
		"signature null":        {"signature_verified", `null`},
		"signature wrong type":  {"signature_verified", `"true"`},
		"retention null":        {"retention_days", `null`},
		"retention zero":        {"retention_days", `0`},
		"retention too large":   {"retention_days", `3651`},
		"retention fractional":  {"retention_days", `1.5`},
		"size null":             {"size_bytes", `null`},
		"size negative":         {"size_bytes", `-1`},
		"size string":           {"size_bytes", `"5"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			router, _ := newTestRouter(t)
			assertErrorResponse(t, postArtifact(t, router,
				duplicateFieldBody("registry-demo/duplast", tc.field, tc.lastRaw)),
				http.StatusBadRequest, codeInvalidInput, msgInvalidReg)
			assertErrorResponse(t, queryList(t, router, "registry-demo/duplast"),
				http.StatusNotFound, codeNotFound, msgNotFound)
		})
	}
}

// TestUnicodeEscapedKeyNamesDecodeBeforeMatching proves that key matching
// happens on the decoded key name: a \u-escaped spelling of a required field
// is the required field, an escaped and a plain spelling of the same name
// collide like any duplicate, and an unknown key — however similar — can
// never stand in for a missing required one.
func TestUnicodeEscapedKeyNamesDecodeBeforeMatching(t *testing.T) {
	router, _ := newTestRouter(t)

	// Every required key spelled with a \u escape registers normally: the
	// decoder matches the decoded name, not the raw bytes. (Backtick strings
	// keep the backslash literal, so the JSON text really carries the
	// escape sequences.)
	body := `{` +
		`"\u0072epository":"registry-demo/uni",` +
		`"\u0064igest":"` + testDigestA + `",` +
		`"\u0074ag":"release",` +
		`"signature_\u0076erified":true,` +
		`"retention_\u0064ays":30,` +
		`"size_\u0062ytes":1024}`
	recorder := postArtifact(t, router, body)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("escaped-keys status = %d, want %d (%s)", recorder.Code, http.StatusCreated, recorder.Body)
	}
	record := decodeBody(t, recorder)
	pushedAt, _ := record["pushed_at"].(string)
	expectStoredRecord(t, record, "registry-demo/uni", testDigestA, "release", true, 30, 1024, pushedAt)

	// An escaped and a plain spelling of the same decoded name collide like
	// any duplicate key: document order decides which value wins.
	plainLast := postArtifact(t, router, `{"repository":"registry-demo/uni2","digest":"`+testDigestB+`",`+
		`"\u0074ag":"escaped","tag":"plain","signature_verified":true,"retention_days":30,"size_bytes":1}`)
	if plainLast.Code != http.StatusCreated {
		t.Fatalf("plain-last status = %d (%s)", plainLast.Code, plainLast.Body)
	}
	if got := decodeBody(t, plainLast)["tag"]; got != "plain" {
		t.Fatalf("plain spelling last, stored tag = %v, want plain", got)
	}
	escapedLast := postArtifact(t, router, `{"repository":"registry-demo/uni3","digest":"`+testDigestC+`",`+
		`"tag":"plain","\u0074ag":"escaped","signature_verified":true,"retention_days":30,"size_bytes":1}`)
	if escapedLast.Code != http.StatusCreated {
		t.Fatalf("escaped-last status = %d (%s)", escapedLast.Code, escapedLast.Body)
	}
	if got := decodeBody(t, escapedLast)["tag"]; got != "escaped" {
		t.Fatalf("escaped spelling last, stored tag = %v, want escaped", got)
	}
	if got := soleRecord(t, queryByTag(t, router, "registry-demo/uni3", "escaped")); got["digest"] != testDigestC {
		t.Fatalf("escaped tag points at %v, want %s", got["digest"], testDigestC)
	}
	assertErrorResponse(t, queryByTag(t, router, "registry-demo/uni3", "plain"),
		http.StatusNotFound, codeNotFound, msgNotFound)

	// An unknown key is ignored even when it merely resembles a required one:
	// "repo" never satisfies the missing "repository".
	missing := postArtifact(t, router, `{"repo":"registry-demo/uni4","digest":"`+testDigestA+`",`+
		`"tag":"release","signature_verified":true,"retention_days":30,"size_bytes":1}`)
	assertErrorResponse(t, missing, http.StatusBadRequest, codeInvalidInput, msgInvalidReg)
	assertErrorResponse(t, queryList(t, router, "registry-demo/uni4"),
		http.StatusNotFound, codeNotFound, msgNotFound)
}

// TestDuplicateKeysSyntaxErrorStill400 proves that a JSON syntax error
// anywhere in the body is a 400 no matter what the duplicate keys around it
// look like: a later legal occurrence of a required field cannot rescue a
// body that does not parse, and trailing content after the object is
// rejected just the same.
func TestDuplicateKeysSyntaxErrorStill400(t *testing.T) {
	valid := registration("registry-demo/syn", testDigestA, "release", true, 30, 1024)
	cases := map[string]string{
		"truncated after duplicate key": `{"repository":"registry-demo/syn","repository":`,
		"double comma between keys": `{"repository":"registry-demo/syn",,"repository":"registry-demo/syn",` +
			`"digest":"` + testDigestA + `","tag":"release","signature_verified":true,"retention_days":30,"size_bytes":1}`,
		// The later "size_bytes":5 would be legal, but the unquoted token
		// before it is a syntax error, so the whole body is a 400.
		"unquoted token before valid duplicate": `{"repository":"registry-demo/syn","digest":"` + testDigestA + `",` +
			`"tag":"release","signature_verified":true,"retention_days":30,"size_bytes":oops,"size_bytes":5}`,
		"trailing garbage after object": valid + ` garbage`,
		"trailing brace after object":   valid + `}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			router, _ := newTestRouter(t)
			assertErrorResponse(t, postArtifact(t, router, body),
				http.StatusBadRequest, codeInvalidInput, msgInvalidReg)
			assertErrorResponse(t, queryList(t, router, "registry-demo/syn"),
				http.StatusNotFound, codeNotFound, msgNotFound)
		})
	}
}

// TestRejectedDuplicateBodyLeavesCommittedStateUntouched registers a record,
// then submits a body that carries the same identity with different content
// but whose last size_bytes occurrence is null. Validation must answer 400
// before any identity lookup — never 409 — and neither the committed record
// nor the tag pointer may change.
func TestRejectedDuplicateBodyLeavesCommittedStateUntouched(t *testing.T) {
	router, _ := newTestRouter(t)

	first := postArtifact(t, router, registration("registry-demo/rej", testDigestA, "release", true, 30, 1024))
	if first.Code != http.StatusCreated {
		t.Fatalf("register status = %d (%s)", first.Code, first.Body)
	}
	pushedA := decodeBody(t, first)["pushed_at"].(string)

	// Existing (repository, digest) identity, different tag and size — but the
	// body's last size_bytes is null: 400 must win over the would-be 409.
	body := `{"repository":"registry-demo/rej","digest":"` + testDigestA + `","tag":"moved",` +
		`"signature_verified":true,"retention_days":30,"size_bytes":4096,"size_bytes":null}`
	assertErrorResponse(t, postArtifact(t, router, body),
		http.StatusBadRequest, codeInvalidInput, msgInvalidReg)

	// The committed record is byte-for-byte unchanged, push time included.
	expectStoredRecord(t, soleRecord(t, queryByDigest(t, router, "registry-demo/rej", testDigestA)),
		"registry-demo/rej", testDigestA, "release", true, 30, 1024, pushedA)

	// The tag pointer still resolves to the original record, no record was
	// added, and the tag carried by the rejected body was never persisted.
	if got := soleRecord(t, queryByTag(t, router, "registry-demo/rej", "release")); got["pushed_at"] != pushedA {
		t.Fatalf("tag pointer record changed: %v", got)
	}
	if records := allRecords(t, queryList(t, router, "registry-demo/rej")); len(records) != 1 {
		t.Fatalf("rejected body added a record: %v", records)
	}
	assertErrorResponse(t, queryByTag(t, router, "registry-demo/rej", "moved"),
		http.StatusNotFound, codeNotFound, msgNotFound)
}

// TestIntegerBoundaryExactInt64 proves that JSON numbers decode directly into
// signed 64-bit integers: 2^53+1 (not representable as a float64) and the
// maximum int64 register and read back exactly, an identical resubmission is
// a duplicate with the original pushed_at, and the neighboring integer —
// identical to 2^53+1 when compared as float64 — is a 409 conflict, which
// only an exact integer comparison can produce.
func TestIntegerBoundaryExactInt64(t *testing.T) {
	router, _ := newTestRouter(t)
	const repo = "registry-demo/int"

	// 2^53+1, one more than the largest exactly representable float64 integer.
	bigBody := registration(repo, testDigestA, "release", true, 1, 9007199254740993)
	big := postArtifact(t, router, bigBody)
	if big.Code != http.StatusCreated {
		t.Fatalf("register 2^53+1 status = %d (%s)", big.Code, big.Body)
	}
	if !strings.Contains(big.Body.String(), `"size_bytes":9007199254740993`) {
		t.Fatalf("response rounded the integer: %s", big.Body)
	}
	bigRecord := decodeBodyNumber(t, big)
	if got := jsonNumber(t, bigRecord, "size_bytes"); got != "9007199254740993" {
		t.Fatalf("size_bytes = %s, want exact 9007199254740993", got)
	}
	if got := jsonNumber(t, bigRecord, "retention_days"); got != "1" {
		t.Fatalf("retention_days = %s, want 1", got)
	}
	pushedBig, _ := bigRecord["pushed_at"].(string)

	// The largest signed 64-bit integer, with the largest legal retention.
	maxRec := postArtifact(t, router, registration(repo, testDigestB, "release", true, 3650, 9223372036854775807))
	if maxRec.Code != http.StatusCreated {
		t.Fatalf("register max int64 status = %d (%s)", maxRec.Code, maxRec.Body)
	}
	maxRecord := decodeBodyNumber(t, maxRec)
	if got := jsonNumber(t, maxRecord, "size_bytes"); got != "9223372036854775807" {
		t.Fatalf("size_bytes = %s, want exact 9223372036854775807", got)
	}
	if got := jsonNumber(t, maxRecord, "retention_days"); got != "3650" {
		t.Fatalf("retention_days = %s, want 3650", got)
	}

	// Queries return the stored integers exactly — the check never goes
	// through a float64.
	if got := jsonNumber(t, soleRecordNumber(t, queryByDigest(t, router, repo, testDigestA)), "size_bytes"); got != "9007199254740993" {
		t.Fatalf("queried size_bytes = %s, want exact 9007199254740993", got)
	}
	if got := jsonNumber(t, soleRecordNumber(t, queryByTag(t, router, repo, "release")), "size_bytes"); got != "9223372036854775807" {
		t.Fatalf("tagged size_bytes = %s, want exact 9223372036854775807", got)
	}
	if records := allRecords(t, queryList(t, router, repo)); len(records) != 2 {
		t.Fatalf("list = %d records, want 2", len(records))
	}

	// Identical resubmission is a duplicate: 201 with the original pushed_at.
	retry := postArtifact(t, router, bigBody)
	if retry.Code != http.StatusCreated {
		t.Fatalf("retry status = %d (%s)", retry.Code, retry.Body)
	}
	if got := decodeBodyNumber(t, retry)["pushed_at"]; got != pushedBig {
		t.Fatalf("retry pushed_at = %v, want original %v", got, pushedBig)
	}

	// 9007199254740992 and 9007199254740993 are the SAME float64 (both round
	// to 2^53) but different int64 values: the same identity with the
	// neighboring size must be a 409 conflict, which only an exact integer
	// comparison can produce.
	assertErrorResponse(t, postArtifact(t, router,
		registration(repo, testDigestA, "release", true, 1, 9007199254740992)),
		http.StatusConflict, codeConflict, msgConflict)

	// The rejected body changed nothing: the stored record still reads back
	// exactly, push time included.
	after := soleRecordNumber(t, queryByDigest(t, router, repo, testDigestA))
	if got := jsonNumber(t, after, "size_bytes"); got != "9007199254740993" {
		t.Fatalf("size_bytes after conflict = %s, want 9007199254740993", got)
	}
	if after["pushed_at"] != pushedBig {
		t.Fatalf("pushed_at changed after conflict: %v", after)
	}
}

// TestIntegerFormsRejected pins the number forms that never decode into an
// int64: overflow beyond the signed 64-bit range, digits in a string, and
// fractional or exponential spellings even when their numeric value is an
// integer. Each is the same 400 and leaves no record behind.
func TestIntegerFormsRejected(t *testing.T) {
	cases := map[string]struct{ retention, size string }{
		"size overflows int64":      {"30", "9223372036854775808"},
		"size string digits":        {"30", `"1024"`},
		"size integral float":       {"30", "1024.0"},
		"size exponential":          {"30", "1e3"},
		"retention overflows int64": {"9223372036854775808", "1"},
		"retention integral float":  {"30.0", "1"},
		"retention exponential":     {"3e1", "1"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			router, _ := newTestRouter(t)
			assertErrorResponse(t, postArtifact(t, router,
				registrationRaw("registry-demo/intrej", testDigestA, tc.retention, tc.size)),
				http.StatusBadRequest, codeInvalidInput, msgInvalidReg)
			assertErrorResponse(t, queryList(t, router, "registry-demo/intrej"),
				http.StatusNotFound, codeNotFound, msgNotFound)
		})
	}
}

// TestIntegerBoundaryInvalidWinsOverStorageFailure closes the store and
// checks that the integer validation happens before any storage access:
// illegal number forms keep their 400 while a legal boundary value degrades
// to 503 storage_unavailable.
func TestIntegerBoundaryInvalidWinsOverStorageFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	router := NewRouter(st)
	if recorder := postArtifact(t, router,
		registration("registry-demo/down", testDigestA, "release", true, 30, 1024)); recorder.Code != http.StatusCreated {
		t.Fatalf("seed register status = %d (%s)", recorder.Code, recorder.Body)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	// Illegal integer forms keep their 400 even though the store is down.
	assertErrorResponse(t, postArtifact(t, router,
		registrationRaw("registry-demo/down", testDigestB, "30", "9223372036854775808")),
		http.StatusBadRequest, codeInvalidInput, msgInvalidReg)
	assertErrorResponse(t, postArtifact(t, router,
		registrationRaw("registry-demo/down", testDigestB, "30.0", "1")),
		http.StatusBadRequest, codeInvalidInput, msgInvalidReg)

	// A legal boundary value passes validation and reaches the failing store.
	assertErrorResponse(t, postArtifact(t, router,
		registration("registry-demo/down", testDigestB, "release", true, 30, 9223372036854775807)),
		http.StatusServiceUnavailable, codeStorage, msgStorage)
}

// TestDuplicateAndIntegerBoundaryOnAssemblyEntry proves the decoding boundary
// lives in the shared handler chain, not in the SQLite assembly: the
// caller-supplied store entry NewRouterWithStore shows the same
// last-occurrence-wins duplicate handling and the same exact 64-bit integer
// round-trip, and /healthz is untouched.
func TestDuplicateAndIntegerBoundaryOnAssemblyEntry(t *testing.T) {
	st := newAssemblyStore()
	router := newAssemblyRouter(st, &assemblyProbe{})

	body := `{"repository":"registry-demo/asm-decoy","repository":"registry-demo/asm",` +
		`"digest":"` + testDigestA + `","tag":"release","signature_verified":true,` +
		`"retention_days":30,"size_bytes":9007199254740993}`
	recorder := postArtifact(t, router, body)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d (%s)", recorder.Code, http.StatusCreated, recorder.Body)
	}
	if got := jsonNumber(t, decodeBodyNumber(t, recorder), "size_bytes"); got != "9007199254740993" {
		t.Fatalf("size_bytes = %s, want exact 9007199254740993", got)
	}
	if got := jsonNumber(t, soleRecordNumber(t, queryByDigest(t, router, "registry-demo/asm", testDigestA)), "size_bytes"); got != "9007199254740993" {
		t.Fatalf("queried size_bytes = %s, want exact 9007199254740993", got)
	}
	assertErrorResponse(t, queryList(t, router, "registry-demo/asm-decoy"),
		http.StatusNotFound, codeNotFound, msgNotFound)

	// The health entry is untouched by the decoding boundary.
	if recorder := getHealth(t, router); recorder.Code != http.StatusOK {
		t.Fatalf("/healthz = %d, want %d", recorder.Code, http.StatusOK)
	}
}
