package api

// These regression cases pin the input boundary of POST /v1/artifacts that
// docs/input-boundary-analysis.md describes: how the client's *raw JSON text*
// becomes an immutable record. They cover two areas the baseline leaves
// implicit:
//
//   - repeated keys inside one object — only the last occurrence survives map
//     decoding, and key names written with \uXXXX escapes are recognized after
//     decoding;
//   - integer representation — JSON numbers decode straight into int64 (never
//     through a float), so values up to 2^63-1 round-trip exactly while
//     overflow, fractional, exponential and string forms are rejected.
//
// Every request is submitted verbatim as raw text through the public HTTP
// entry with postArtifact, and every conclusion is re-checked through the
// public GET queries; the store's internals are never touched directly.

import (
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Bob-xisuke/orb-registry-catalog/internal/store"
)

const (
	dupLastRepo   = "dup/last-wins"
	dupIDRepo     = "dup/identity"
	dupDigestRepo = "dup/digests"
	dupTagRepo    = "dup/tagnames"
	dupRetryRepo  = "dup/retry"
	dupGhostRepo  = "dup/ghost"
	dupIntRepo    = "dup/integers"
	dupPrioRepo   = "dup/priority"
	dupSyntaxRepo = "dup/syntax"
	dupEscapeRepo = "dup/escapes"

	// size2Pow53Plus1 = 2^53+1: the first integer a float64 cannot represent,
	// proving the value never passes through floating point.
	size2Pow53Plus1 = "9007199254740993"
	// sizeInt64Max = math.MaxInt64: the inclusive upper edge of a signed
	// 64-bit integer.
	sizeInt64Max  = "9223372036854775807"
	sizeInt64Over = "9223372036854775808" // MaxInt64+1
)

// Escaped spellings of the required key names. "\\u00XX" is a Go-level escaped
// backslash, so each constant's VALUE is the literal JSON source text
// \u00XX…; the server's JSON decoder turns it back into the plain key name.
const (
	escRepositoryKey = "\\u0072epository" // -> repository
	escDigestKey     = "\\u0064igest"     // -> digest
	escTagKey        = "\\u0074ag"        // -> tag
	escSignatureKey  = "\\u0073ignature_verified"
	escRetentionKey  = "\\u0072etention_days"
	escSizeKey       = "\\u0073ize_bytes"
	escUnknownTagKey = "\\u0074ag2" // -> tag2, an unknown key
)

// jsonField is one ordered "key":raw-value pair. It exists so a key can be
// emitted more than once, and so key names can carry literal \uXXXX escapes.
type jsonField struct {
	key string
	raw string
}

// orderedRegistration renders fields in the exact given order as one JSON
// object; duplicate keys are kept as emitted, unlike a Go map literal.
func orderedRegistration(fields ...jsonField) string {
	parts := make([]string, len(fields))
	for i, f := range fields {
		parts[i] = `"` + f.key + `":` + f.raw
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// validFields is the canonical six-field legal registration, each key once.
func validFields(repo, digest, tag, sig, retention, size string) []jsonField {
	return []jsonField{
		{"repository", `"` + repo + `"`},
		{"digest", `"` + digest + `"`},
		{"tag", `"` + tag + `"`},
		{"signature_verified", sig},
		{"retention_days", retention},
		{"size_bytes", size},
	}
}

// rawIntegerPattern extracts an integer field's exact digit sequence from a
// raw JSON response. Big values must be compared as text on both sides: the
// generic map[string]any decoder hands back a float64 and would lose the low
// digits of 9007199254740993.
var rawIntegerPattern = regexp.MustCompile(`"([a-z_]+)":(-?[0-9]+)`)

func assertRawInteger(t *testing.T, body []byte, field, wantDigits string) {
	t.Helper()
	for _, m := range rawIntegerPattern.FindAllSubmatch(body, -1) {
		if string(m[1]) == field {
			if got := string(m[2]); got != wantDigits {
				t.Fatalf("raw %s digits = %s, want %s (body %s)", field, got, wantDigits, body)
			}
			return
		}
	}
	t.Fatalf("response has no integer field %s: %s", field, body)
}

// TestDuplicateKeysFirstBadLastGood pins last-occurrence-wins for every
// required field: a first occurrence whose type cannot possibly unmarshal is
// discarded without failing the request, because only the last RawMessage for
// a decoded key name is ever validated.
func TestDuplicateKeysFirstBadLastGood(t *testing.T) {
	// The first occurrence is an un-unmarshal-able value of the wrong type;
	// the legal value below comes later in the same object.
	wrongFirst := map[string]string{
		"repository":         `7`,      // number before string
		"digest":             `9`,      // number before string
		"tag":                `false`,  // boolean before string
		"signature_verified": `"true"`, // string before boolean
		"retention_days":     `"30"`,   // string before number
		"size_bytes":         `false`,  // boolean before number
	}
	for field, bad := range wrongFirst {
		t.Run(field, func(t *testing.T) {
			router, _ := newTestRouter(t)
			fields := append([]jsonField{{field, bad}},
				validFields(dupLastRepo, testDigestA, "release", "true", "30", "7")...)
			recorder := postArtifact(t, router, orderedRegistration(fields...))
			if recorder.Code != http.StatusCreated {
				t.Fatalf("status = %d, want %d (%s)", recorder.Code, http.StatusCreated, recorder.Body)
			}

			// The stored record carries the LAST value for every field, the
			// bad first occurrence for the field under test included.
			expectStoredRecord(t, soleRecord(t, queryByDigest(t, router, dupLastRepo, testDigestA)),
				dupLastRepo, testDigestA, "release", true, float64(30), float64(7),
				decodeBody(t, recorder)["pushed_at"].(string))
		})
	}
}

// TestDuplicateKeysLastBadRejected pins the mirror image: the LAST occurrence
// decides acceptance. A null, wrong type or rule violation at the last
// position is the same 400 as a single bad field, regardless of legal values
// earlier under the same name, and leaves no record.
func TestDuplicateKeysLastBadRejected(t *testing.T) {
	wrongType := map[string]string{
		"repository":         `7`,
		"digest":             `9`,
		"tag":                `false`,
		"signature_verified": `"true"`,
		"retention_days":     `"30"`,
		"size_bytes":         `false`,
	}
	// Rule-level violations at the last position (null and wrong-type are
	// shared above; signature_verified has no rule beyond its JSON type).
	ruleViolation := map[string][]string{
		"repository":     {`"   "`}, // trims to empty
		"tag":            {`"  "`},  // trims to empty
		"digest":         {`"sha256:xyz"`},
		"retention_days": {"0", "3651"},
		"size_bytes":     {"-1"},
	}

	type tc struct {
		name  string
		field string
		bad   string
	}
	var cases []tc
	for _, field := range []string{"repository", "digest", "tag", "signature_verified", "retention_days", "size_bytes"} {
		cases = append(cases,
			tc{field + " last null", field, "null"},
			tc{field + " last wrong type", field, wrongType[field]},
		)
	}
	for field, bads := range ruleViolation {
		for _, bad := range bads {
			cases = append(cases, tc{field + " last rule violation " + bad, field, bad})
		}
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			router, _ := newTestRouter(t)
			fields := append(validFields(dupLastRepo, testDigestA, "release", "true", "30", "7"),
				jsonField{c.field, c.bad})
			assertErrorResponse(t, postArtifact(t, router, orderedRegistration(fields...)),
				http.StatusBadRequest, codeInvalidInput, msgInvalidReg)

			// Nothing was stored under the earlier legal repository.
			assertErrorResponse(t, queryList(t, router, dupLastRepo),
				http.StatusNotFound, codeNotFound, msgNotFound)
		})
	}
}

// TestDuplicateKeysIdentityAndTagFromLastValue proves that the last
// repository/digest pair is the immutable identity and the last tag is both
// the record's frozen tag field and the pointer that moves. Earlier values
// leave no trace at all: no record, no pointer.
func TestDuplicateKeysIdentityAndTagFromLastValue(t *testing.T) {
	// (a) Duplicate repository: the first repository name gets no record;
	// trimming is still applied to the LAST value.
	t.Run("duplicate repository", func(t *testing.T) {
		router, _ := newTestRouter(t)
		fields := []jsonField{
			{"repository", `"` + dupGhostRepo + `"`},
			{"repository", `"  ` + dupIDRepo + `  "`},
			{"digest", `"` + testDigestA + `"`},
			{"tag", `"release"`},
			{"signature_verified", "true"},
			{"retention_days", "30"},
			{"size_bytes", "7"},
		}
		recorder := postArtifact(t, router, orderedRegistration(fields...))
		if recorder.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (%s)", recorder.Code, recorder.Body)
		}
		if got := soleRecord(t, queryByDigest(t, router, dupIDRepo, testDigestA)); got["repository"] != dupIDRepo {
			t.Fatalf("record landed under %v, want last repository %s", got["repository"], dupIDRepo)
		}
		assertErrorResponse(t, queryList(t, router, dupGhostRepo),
			http.StatusNotFound, codeNotFound, msgNotFound)
	})

	// (b) Duplicate digest: the record's identity is the last digest.
	t.Run("duplicate digest", func(t *testing.T) {
		router, _ := newTestRouter(t)
		fields := []jsonField{
			{"repository", `"` + dupDigestRepo + `"`},
			{"digest", `"` + testDigestB + `"`},
			{"digest", `"` + testDigestA + `"`},
			{"tag", `"release"`},
			{"signature_verified", "true"},
			{"retention_days", "30"},
			{"size_bytes", "7"},
		}
		if recorder := postArtifact(t, router, orderedRegistration(fields...)); recorder.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (%s)", recorder.Code, recorder.Body)
		}
		if got := soleRecord(t, queryByTag(t, router, dupDigestRepo, "release")); got["digest"] != testDigestA {
			t.Fatalf("tag resolves %v, want last digest %s", got["digest"], testDigestA)
		}
		if recorder := queryByDigest(t, router, dupDigestRepo, testDigestA); recorder.Code != http.StatusOK {
			t.Fatalf("last digest lookup = %d, want 200", recorder.Code)
		}
		assertErrorResponse(t, queryByDigest(t, router, dupDigestRepo, testDigestB),
			http.StatusNotFound, codeNotFound, msgNotFound)
	})

	// (c) Duplicate tag: the last tag is frozen on the record AND moves that
	// tag's pointer; the first tag's pointer is left on the earlier record.
	// Whitespace on the last value is normalized with the existing rule.
	t.Run("duplicate tag moves only the last pointer", func(t *testing.T) {
		router, _ := newTestRouter(t)
		first := postArtifact(t, router,
			orderedRegistration(validFields(dupTagRepo, testDigestA, "old", "true", "30", "7")...))
		if first.Code != http.StatusCreated {
			t.Fatalf("register A status = %d (%s)", first.Code, first.Body)
		}
		pushedA := decodeBody(t, first)["pushed_at"].(string)

		second := postArtifact(t, router, orderedRegistration(
			jsonField{"repository", `"` + dupTagRepo + `"`},
			jsonField{"digest", `"` + testDigestB + `"`},
			jsonField{"tag", `"old"`}, // would move "old" if an earlier value won
			jsonField{"tag", `"  new  "`},
			jsonField{"signature_verified", "true"},
			jsonField{"retention_days", "30"},
			jsonField{"size_bytes", "9"},
		))
		if second.Code != http.StatusCreated {
			t.Fatalf("register B status = %d (%s)", second.Code, second.Body)
		}
		pushedB := decodeBody(t, second)["pushed_at"].(string)

		// B's immutable tag field is the trimmed LAST value.
		expectStoredRecord(t, soleRecord(t, queryByDigest(t, router, dupTagRepo, testDigestB)),
			dupTagRepo, testDigestB, "new", true, float64(30), float64(9), pushedB)
		// The "new" pointer moved to B; "old" still points at A.
		if got := soleRecord(t, queryByTag(t, router, dupTagRepo, "new")); got["digest"] != testDigestB {
			t.Fatalf(`"new" points at %v, want %s`, got["digest"], testDigestB)
		}
		expectStoredRecord(t, soleRecord(t, queryByTag(t, router, dupTagRepo, "old")),
			dupTagRepo, testDigestA, "old", true, float64(30), float64(7), pushedA)
		if digests := recordDigests(allRecords(t, queryList(t, router, dupTagRepo))); len(digests) != 2 ||
			digests[0] != testDigestA || digests[1] != testDigestB {
			t.Fatalf("list = %v, want [A B]", digests)
		}
	})

	// (d) Normalized last values still drive dedup: an earlier repository/tag
	// that would be a conflict are discarded, the trimmed last pair equals the
	// stored identity and content, so the answer is 201 with the ORIGINAL
	// pushed_at — duplicate, not conflict, not created.
	t.Run("normalized last values are a duplicate", func(t *testing.T) {
		router, _ := newTestRouter(t)
		first := postArtifact(t, router,
			orderedRegistration(validFields(dupRetryRepo, testDigestA, "release", "true", "30", "11")...))
		if first.Code != http.StatusCreated {
			t.Fatalf("seed status = %d (%s)", first.Code, first.Body)
		}
		pushedA := decodeBody(t, first)["pushed_at"].(string)

		retry := postArtifact(t, router, orderedRegistration(
			jsonField{"repository", `"` + dupGhostRepo + `"`},
			jsonField{"repository", `"  ` + dupRetryRepo + `  "`},
			jsonField{"digest", `"` + testDigestA + `"`},
			jsonField{"tag", `"other"`}, // would conflict if an earlier value won
			jsonField{"tag", `" release "`},
			jsonField{"signature_verified", "true"},
			jsonField{"retention_days", "30"},
			jsonField{"size_bytes", "11"},
		))
		if retry.Code != http.StatusCreated {
			t.Fatalf("normalized duplicate status = %d, want 201 (%s)", retry.Code, retry.Body)
		}
		if got := decodeBody(t, retry)["pushed_at"]; got != pushedA {
			t.Fatalf("duplicate pushed_at = %v, want original %v", got, pushedA)
		}
		if records := allRecords(t, queryList(t, router, dupRetryRepo)); len(records) != 1 {
			t.Fatalf("list = %v, want exactly one record", records)
		}
		assertErrorResponse(t, queryList(t, router, dupGhostRepo),
			http.StatusNotFound, codeNotFound, msgNotFound)
		assertErrorResponse(t, queryByTag(t, router, dupRetryRepo, "other"),
			http.StatusNotFound, codeNotFound, msgNotFound)
	})
}

// TestDuplicateKeysDedupUsesLastValue closes the dedup loop: against an
// existing identity, the LAST value of a duplicated content field is what is
// compared. A different last value is a 409 even if an earlier value matched;
// a matching last value is an identical retry even if an earlier one differed.
func TestDuplicateKeysDedupUsesLastValue(t *testing.T) {
	router, _ := newTestRouter(t)
	first := postArtifact(t, router,
		orderedRegistration(validFields(dupIDRepo, testDigestA, "release", "true", "30", "1024")...))
	if first.Code != http.StatusCreated {
		t.Fatalf("seed status = %d (%s)", first.Code, first.Body)
	}
	pushedA := decodeBody(t, first)["pushed_at"].(string)

	// Last size differs -> conflict, despite the first size matching the
	// stored record.
	conflict := postArtifact(t, router, orderedRegistration(
		jsonField{"repository", `"` + dupIDRepo + `"`},
		jsonField{"digest", `"` + testDigestA + `"`},
		jsonField{"tag", `"release"`},
		jsonField{"signature_verified", "true"},
		jsonField{"retention_days", "30"},
		jsonField{"size_bytes", "1024"},
		jsonField{"size_bytes", "2048"},
	))
	assertErrorResponse(t, conflict, http.StatusConflict, codeConflict, msgConflict)

	// The same for tag: first matches, last differs -> 409.
	conflictTag := postArtifact(t, router, orderedRegistration(
		jsonField{"repository", `"` + dupIDRepo + `"`},
		jsonField{"digest", `"` + testDigestA + `"`},
		jsonField{"tag", `"release"`},
		jsonField{"tag", `"other"`},
		jsonField{"signature_verified", "true"},
		jsonField{"retention_days", "30"},
		jsonField{"size_bytes", "1024"},
	))
	assertErrorResponse(t, conflictTag, http.StatusConflict, codeConflict, msgConflict)

	// First size differs, last matches -> identical retry, original pushed_at.
	duplicate := postArtifact(t, router, orderedRegistration(
		jsonField{"repository", `"` + dupIDRepo + `"`},
		jsonField{"digest", `"` + testDigestA + `"`},
		jsonField{"tag", `"release"`},
		jsonField{"signature_verified", "true"},
		jsonField{"retention_days", "30"},
		jsonField{"size_bytes", "2048"},
		jsonField{"size_bytes", "1024"},
	))
	if duplicate.Code != http.StatusCreated {
		t.Fatalf("last-matches duplicate status = %d, want 201 (%s)", duplicate.Code, duplicate.Body)
	}
	if got := decodeBody(t, duplicate)["pushed_at"]; got != pushedA {
		t.Fatalf("duplicate pushed_at = %v, want original %v", got, pushedA)
	}

	// Neither rejection nor retry changed the committed record.
	expectStoredRecord(t, soleRecord(t, queryByDigest(t, router, dupIDRepo, testDigestA)),
		dupIDRepo, testDigestA, "release", true, float64(30), float64(1024), pushedA)
	assertErrorResponse(t, queryByTag(t, router, dupIDRepo, "other"),
		http.StatusNotFound, codeNotFound, msgNotFound)
}

// TestUnicodeEscapedKeyNames checks that key matching happens on the DECODED
// name: \uXXXX spellings of a required key provide that key, and a duplicate
// written half-literal half-escaped is still one key with last-wins. The key
// source text is carried by the esc*Key constants (whose Go values are the
// literal backslash-u sequences), so orderedRegistration emits the escapes
// verbatim and the server's JSON decoder is what interprets them.
func TestUnicodeEscapedKeyNames(t *testing.T) {
	// Every required key is supplied entirely through \uXXXX escapes and is
	// recognized after decoding (repository, digest, tag, signature/retention/
	// size all begin with r/d/t/s).
	t.Run("all keys escaped", func(t *testing.T) {
		router, _ := newTestRouter(t)
		body := orderedRegistration(
			jsonField{escRepositoryKey, `"` + dupEscapeRepo + `"`},
			jsonField{escDigestKey, `"` + testDigestA + `"`},
			jsonField{escTagKey, `"release"`},
			jsonField{escSignatureKey, "true"},
			jsonField{escRetentionKey, "30"},
			jsonField{escSizeKey, "7"},
		)
		recorder := postArtifact(t, router, body)
		if recorder.Code != http.StatusCreated {
			t.Fatalf("escaped-keys status = %d, want %d (%s)", recorder.Code, http.StatusCreated, recorder.Body)
		}
		if got := soleRecord(t, queryByDigest(t, router, dupEscapeRepo, testDigestA)); got["tag"] != "release" {
			t.Fatalf("record not assembled from escaped key names: %v", got)
		}
	})

	// The same key written literal first and escaped second is one key; the
	// last occurrence wins regardless of which spelling it used.
	for name, fields := range map[string][]jsonField{
		"literal then escaped": {
			{"repository", `"` + dupEscapeRepo + `"`},
			{"digest", `"` + testDigestB + `"`},
			{"tag", `"one"`},
			{escTagKey, `"two"`}, // escaped tag, last
			{"signature_verified", "true"},
			{"retention_days", "30"},
			{"size_bytes", "7"},
		},
		"escaped then literal": {
			{"repository", `"` + dupEscapeRepo + `"`},
			{"digest", `"` + testDigestB + `"`},
			{escTagKey, `"one"`}, // escaped tag, first
			{"tag", `"two"`},
			{"signature_verified", "true"},
			{"retention_days", "30"},
			{"size_bytes", "7"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			router, _ := newTestRouter(t)
			if recorder := postArtifact(t, router, orderedRegistration(fields...)); recorder.Code != http.StatusCreated {
				t.Fatalf("status = %d, want %d (%s)", recorder.Code, http.StatusCreated, recorder.Body)
			}
			if got := soleRecord(t, queryByDigest(t, router, dupEscapeRepo, testDigestB)); got["tag"] != "two" {
				t.Fatalf("tag = %v, want last value %q", got["tag"], "two")
			}
			assertErrorResponse(t, queryByTag(t, router, dupEscapeRepo, "one"),
				http.StatusNotFound, codeNotFound, msgNotFound)
		})
	}

	// A differently-decoding escaped name is still an unknown field: it is
	// ignored, but cannot stand in for the required key. tag2 != tag, so the
	// required tag is missing and the body is a 400.
	t.Run("escaped unknown cannot substitute", func(t *testing.T) {
		router, _ := newTestRouter(t)
		body := orderedRegistration(
			jsonField{"repository", `"` + dupEscapeRepo + `"`},
			jsonField{"digest", `"` + testDigestA + `"`},
			jsonField{escUnknownTagKey, `"release"`}, // decodes to "tag2"
			jsonField{"signature_verified", "true"},
			jsonField{"retention_days", "30"},
			jsonField{"size_bytes", "7"},
		)
		assertErrorResponse(t, postArtifact(t, router, body),
			http.StatusBadRequest, codeInvalidInput, msgInvalidReg)
		assertErrorResponse(t, queryList(t, router, dupEscapeRepo),
			http.StatusNotFound, codeNotFound, msgNotFound)
	})
}

// TestSyntaxErrorAnywhereRejects pins that decoding is all-or-nothing: a JSON
// syntax error is a 400 regardless of a perfectly legal same-named field on
// either side of it. There is no partial-object acceptance.
func TestSyntaxErrorAnywhereRejects(t *testing.T) {
	legalTail := `"signature_verified":true,"retention_days":30,"size_bytes":1}`
	truncated := registration(dupSyntaxRepo, testDigestA, "v1", true, 30, 1)
	cases := map[string]string{
		"legal tag then syntax error": `{"repository":"` + dupSyntaxRepo + `","digest":"` + testDigestA +
			`","tag":"a","tag":,` + legalTail,
		"syntax error then legal tag": `{"repository":"` + dupSyntaxRepo + `","digest":"` + testDigestA +
			`","tag":,"tag":"a",` + legalTail,
		"truncated object": truncated[:len(truncated)-1], // drop the closing brace
		"trailing comma after legal duplicate": `{"repository":"` + dupSyntaxRepo +
			`","size_bytes":1,"size_bytes":2,}`,
		"leading zero number": `{"repository":"` + dupSyntaxRepo + `","size_bytes":01}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			router, _ := newTestRouter(t)
			assertErrorResponse(t, postArtifact(t, router, body),
				http.StatusBadRequest, codeInvalidInput, msgInvalidReg)
			assertErrorResponse(t, queryList(t, router, dupSyntaxRepo),
				http.StatusNotFound, codeNotFound, msgNotFound)
		})
	}
}

// TestIntegerExactBoundaries pins the direct JSON-number -> int64 path. The
// two values above 2^53 must return with every digit intact, compared as raw
// text so the test itself never converts through float64.
func TestIntegerExactBoundaries(t *testing.T) {
	router, _ := newTestRouter(t)

	type exactCase struct {
		digest string
		size   string
	}
	for i, c := range []exactCase{
		{testDigestA, size2Pow53Plus1}, // 2^53+1
		{testDigestB, sizeInt64Max},    // 2^63-1
	} {
		body := orderedRegistration(validFields(dupIntRepo, c.digest, "release", "true", "30", c.size)...)
		created := postArtifact(t, router, body)
		if created.Code != http.StatusCreated {
			t.Fatalf("case %d register status = %d, want 201 (%s)", i, created.Code, created.Body)
		}
		// The 201 body itself carries the exact integer literal.
		assertRawInteger(t, created.Body.Bytes(), "size_bytes", c.size)

		// And an independent GET returns 200 with the same digits.
		query := queryByDigest(t, router, dupIntRepo, c.digest)
		if query.Code != http.StatusOK {
			t.Fatalf("case %d query status = %d, want 200 (%s)", i, query.Code, query.Body)
		}
		assertRawInteger(t, query.Body.Bytes(), "size_bytes", c.size)
	}

	// Both large-integer records exist and are independently retrievable.
	if digests := recordDigests(allRecords(t, queryList(t, router, dupIntRepo))); len(digests) != 2 {
		t.Fatalf("list = %v, want both large-integer records", digests)
	}
}

// TestIntegerRejectedForms pins every non-integral or out-of-range JSON
// representation for size_bytes and the retention_days range edges.
func TestIntegerRejectedForms(t *testing.T) {
	badSizes := map[string]string{
		"above int64 max":       sizeInt64Over,
		"negative":              "-1",
		"string number":         `"4096"`,
		"fraction equal to int": "1.0",
		"lowercase exponent":    "1e3",
		"uppercase exponent":    "1E3",
		"below int64 min":       "-9223372036854775809",
	}
	for name, raw := range badSizes {
		t.Run("size_bytes "+name, func(t *testing.T) {
			router, _ := newTestRouter(t)
			assertErrorResponse(t, postArtifact(t, router, registrationWithField("size_bytes", raw)),
				http.StatusBadRequest, codeInvalidInput, msgInvalidReg)
			assertErrorResponse(t, queryList(t, router, "registry-demo/nulls"),
				http.StatusNotFound, codeNotFound, msgNotFound)
		})
	}

	// retention_days: 1 and 3650 are the inclusive legal edges.
	for _, days := range []string{"1", "3650"} {
		t.Run("retention_days legal "+days, func(t *testing.T) {
			router, _ := newTestRouter(t)
			recorder := postArtifact(t, router, registrationWithField("retention_days", days))
			if recorder.Code != http.StatusCreated {
				t.Fatalf("retention %s status = %d, want 201 (%s)", days, recorder.Code, recorder.Body)
			}
			assertRawInteger(t, recorder.Body.Bytes(), "retention_days", days)
		})
	}
	for _, days := range []string{"0", "3651"} {
		t.Run("retention_days rejected "+days, func(t *testing.T) {
			router, _ := newTestRouter(t)
			assertErrorResponse(t, postArtifact(t, router, registrationWithField("retention_days", days)),
				http.StatusBadRequest, codeInvalidInput, msgInvalidReg)
			assertErrorResponse(t, queryList(t, router, "registry-demo/nulls"),
				http.StatusNotFound, codeNotFound, msgNotFound)
		})
	}

	// Exponential/fractional/string forms are rejected for retention as well;
	// they must never be silently truncated or coerced.
	for name, raw := range map[string]string{
		"fraction": "1.0",
		"exponent": "1e3",
		"string":   `"30"`,
	} {
		t.Run("retention_days rejected "+name, func(t *testing.T) {
			router, _ := newTestRouter(t)
			assertErrorResponse(t, postArtifact(t, router, registrationWithField("retention_days", raw)),
				http.StatusBadRequest, codeInvalidInput, msgInvalidReg)
		})
	}
}

// TestFalseSignatureAndZeroSizeStayLegal records the in-range zero values the
// null check must not conflate: false is a legal boolean and 0 a legal
// non-negative size; both register and read back exactly.
func TestFalseSignatureAndZeroSizeStayLegal(t *testing.T) {
	router, _ := newTestRouter(t)

	unsigned := postArtifact(t, router, registrationWithField("signature_verified", "false"))
	if unsigned.Code != http.StatusCreated {
		t.Fatalf("signature false status = %d, want 201 (%s)", unsigned.Code, unsigned.Body)
	}
	if got := decodeBody(t, unsigned)["signature_verified"]; got != false {
		t.Fatalf("false signature read back as %v", got)
	}

	zeroBody := strings.Replace(registrationWithField("size_bytes", "0"), testDigestA, testDigestB, 1)
	zero := postArtifact(t, router, zeroBody)
	if zero.Code != http.StatusCreated {
		t.Fatalf("zero size status = %d, want 201 (%s)", zero.Code, zero.Body)
	}
	assertRawInteger(t, zero.Body.Bytes(), "size_bytes", "0")
	if got := soleRecord(t, queryByDigest(t, router, "registry-demo/nulls", testDigestB)); got["size_bytes"] != float64(0) {
		t.Fatalf("zero size read back as %v", got["size_bytes"])
	}
}

// TestInvalidInputBeatsConflictAndStorage establishes the decision order on
// POST: decode and field validation run before the store is consulted, so an
// invalid body is a 400 even when its identity already exists with different
// content (which would be a 409) and even when the database is down (which
// would be a 503).
func TestInvalidInputBeatsConflictAndStorage(t *testing.T) {
	t.Run("invalid beats 409 against existing identity", func(t *testing.T) {
		router, _ := newTestRouter(t)
		seed := postArtifact(t, router,
			orderedRegistration(validFields(dupPrioRepo, testDigestA, "release", "true", "30", "1024")...))
		if seed.Code != http.StatusCreated {
			t.Fatalf("seed status = %d (%s)", seed.Code, seed.Body)
		}
		pushedA := decodeBody(t, seed)["pushed_at"].(string)

		// Every body WOULD conflict (existing identity A, different content)
		// but is invalid, so the answer is 400 and the state is untouched.
		invalid := map[string]string{
			"last size null": orderedRegistration(
				jsonField{"repository", `"` + dupPrioRepo + `"`},
				jsonField{"digest", `"` + testDigestA + `"`},
				jsonField{"tag", `"moved"`},
				jsonField{"signature_verified", "true"},
				jsonField{"retention_days", "30"},
				jsonField{"size_bytes", "4096"},
				jsonField{"size_bytes", "null"},
			),
			"last size over int64": orderedRegistration(
				jsonField{"repository", `"` + dupPrioRepo + `"`},
				jsonField{"digest", `"` + testDigestA + `"`},
				jsonField{"tag", `"moved"`},
				jsonField{"signature_verified", "true"},
				jsonField{"retention_days", "30"},
				jsonField{"size_bytes", sizeInt64Over},
			),
			"last size exponent": orderedRegistration(
				jsonField{"repository", `"` + dupPrioRepo + `"`},
				jsonField{"digest", `"` + testDigestA + `"`},
				jsonField{"tag", `"moved"`},
				jsonField{"signature_verified", "true"},
				jsonField{"retention_days", "30"},
				jsonField{"size_bytes", "1e3"},
			),
			"last tag null despite matching first": orderedRegistration(
				jsonField{"repository", `"` + dupPrioRepo + `"`},
				jsonField{"digest", `"` + testDigestA + `"`},
				jsonField{"tag", `"release"`},
				jsonField{"tag", "null"},
				jsonField{"signature_verified", "true"},
				jsonField{"retention_days", "30"},
				jsonField{"size_bytes", "1024"},
			),
			"retention as string": orderedRegistration(
				jsonField{"repository", `"` + dupPrioRepo + `"`},
				jsonField{"digest", `"` + testDigestA + `"`},
				jsonField{"tag", `"moved"`},
				jsonField{"signature_verified", "true"},
				jsonField{"retention_days", `"30"`},
				jsonField{"size_bytes", "1024"},
			),
		}
		for name, body := range invalid {
			t.Run(name, func(t *testing.T) {
				assertErrorResponse(t, postArtifact(t, router, body),
					http.StatusBadRequest, codeInvalidInput, msgInvalidReg)

				// The committed record and the tag pointer are byte-for-byte
				// unchanged; the "moved" tag was never persisted.
				expectStoredRecord(t, soleRecord(t, queryByDigest(t, router, dupPrioRepo, testDigestA)),
					dupPrioRepo, testDigestA, "release", true, float64(30), float64(1024), pushedA)
				if got := soleRecord(t, queryByTag(t, router, dupPrioRepo, "release")); got["pushed_at"] != pushedA {
					t.Fatalf("tag pointer changed after rejection: %v", got)
				}
				if records := allRecords(t, queryList(t, router, dupPrioRepo)); len(records) != 1 {
					t.Fatalf("list = %v, want the one seeded record", records)
				}
				assertErrorResponse(t, queryByTag(t, router, dupPrioRepo, "moved"),
					http.StatusNotFound, codeNotFound, msgNotFound)
			})
		}
	})

	// With the SQLite file closed, an invalid body is still the 400 decided in
	// the handler — never the 503 a legal body gets on the same closed store.
	t.Run("invalid beats 503 with store closed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "service.db")
		st, err := store.Open(path)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		router := NewRouter(st)
		if recorder := postArtifact(t, router,
			orderedRegistration(validFields(dupPrioRepo, testDigestA, "release", "true", "30", "1024")...)); recorder.Code != http.StatusCreated {
			t.Fatalf("seed status = %d (%s)", recorder.Code, recorder.Body)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("close store: %v", err)
		}

		assertErrorResponse(t, postArtifact(t, router, orderedRegistration(
			jsonField{"repository", `"` + dupPrioRepo + `"`},
			jsonField{"digest", `"` + testDigestA + `"`},
			jsonField{"tag", `"release"`},
			jsonField{"signature_verified", "true"},
			jsonField{"retention_days", "30"},
			jsonField{"size_bytes", "4096"},
			jsonField{"size_bytes", "null"},
		)), http.StatusBadRequest, codeInvalidInput, msgInvalidReg)
	})
}
