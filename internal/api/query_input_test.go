package api

// These regression cases exercise only the public HTTP surface
// (GET /v1/artifacts, with POST /v1/artifacts used solely to seed committed
// state and GET /healthz to prove that entry is untouched) and back the
// conclusions in docs/query-input-decoding-analysis.md: how repeated query
// parameters, percent-decoded parameter names, case sensitivity, '+' versus
// '%2B' and the trim boundary turn a raw request URI into a dispatched
// repository/tag/digest query — or into the published 400. They deliberately
// avoid the store's unexported internals so that any refactor preserving the
// published contract keeps them green.

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/Bob-xisuke/orb-registry-catalog/internal/store"
)

// testDigestD and testDigestE extend testDigestA..C (artifacts_test.go and
// assembly_test.go) for repositories that need five distinct identities.
const (
	testDigestD = "sha256:" + "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	testDigestE = "sha256:" + "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
)

// mustRegister posts one registration and fails the test on anything but 201.
func mustRegister(t *testing.T, router *gin.Engine, repo, digest, tag string) {
	t.Helper()
	recorder := postArtifact(t, router, registration(repo, digest, tag, true, regRetention, regSizeA))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("register %s/%s/%s status = %d, want %d (%s)", repo, digest, tag,
			recorder.Code, http.StatusCreated, recorder.Body)
	}
}

// TestRepeatedQueryParameterFirstValueWins pins the first-occurrence rule for
// every recognized parameter: repository, tag and digest. Values appearing
// later are never consulted, so they can neither retarget a hit nor rescue a
// first value that names a missing repository, tag or digest.
func TestRepeatedQueryParameterFirstValueWins(t *testing.T) {
	router, _ := newTestRouter(t)
	const repo = "registry-demo/qfirst"
	mustRegister(t, router, repo, testDigestA, "alpha")
	mustRegister(t, router, repo, testDigestB, "beta")

	hitCases := map[string]struct {
		query      string
		wantDigest string
	}{
		"tag alpha before beta": {"repository=" + repo + "&tag=alpha&tag=beta", testDigestA},
		"tag beta before alpha": {"repository=" + repo + "&tag=beta&tag=alpha", testDigestB},
		"tag third occurrence":  {"repository=" + repo + "&tag=alpha&tag=beta&tag=missing", testDigestA},
		"digest A before B":     {"repository=" + repo + "&digest=" + testDigestA + "&digest=" + testDigestB, testDigestA},
		"digest B before A":     {"repository=" + repo + "&digest=" + testDigestB + "&digest=" + testDigestA, testDigestB},
		"digest third occurrence": {"repository=" + repo + "&digest=" + testDigestA + "&digest=" + testDigestB +
			"&digest=" + testDigestC, testDigestA},
	}
	for name, tc := range hitCases {
		t.Run(name, func(t *testing.T) {
			record := soleRecord(t, getArtifacts(t, router, tc.query))
			if record["digest"] != tc.wantDigest {
				t.Fatalf("target digest = %v, want %s", record["digest"], tc.wantDigest)
			}
		})
	}

	// The first repository value scopes the whole query even when a later value
	// names a real repository.
	first := getArtifacts(t, router, "repository="+repo+"&repository=registry-demo/other&tag=alpha")
	if first.Code != http.StatusOK {
		t.Fatalf("real repository first status = %d, want %d (%s)", first.Code, http.StatusOK, first.Body)
	}
	if got := soleRecord(t, first)["repository"]; got != repo {
		t.Fatalf("later repository value retargeted the query to %v", got)
	}

	// Mirrored: a first value naming a missing repository is a 404; the later
	// value that names the seeded repository cannot rescue it.
	assertErrorResponse(t,
		getArtifacts(t, router, "repository=registry-demo/other&repository="+repo+"&tag=alpha"),
		http.StatusNotFound, codeNotFound, msgNotFound)
}

// TestRepeatedQueryParameterInvalidFirstValue pins both halves of the rule
// around validation: an illegal first value is the same 400 when legal values
// follow it, and a legal first value stays the query target when illegal or
// merely different values follow it. A later value can neither rescue nor
// override the first occurrence.
func TestRepeatedQueryParameterInvalidFirstValue(t *testing.T) {
	router, _ := newTestRouter(t)
	const repo = "registry-demo/qinvalid"
	mustRegister(t, router, repo, testDigestA, "release")
	mustRegister(t, router, repo, testDigestB, "release") // tag release now points at B

	// Illegal first occurrence: every later legal occurrence is ignored, so the
	// request is the same 400 as a single illegal parameter.
	invalid := map[string]string{
		"repository empty first": "repository=&repository=" + repo + "&tag=release",
		"repository blank first": "repository=%20%20&repository=" + repo + "&tag=release",
		"repository bare first":  "repository&repository=" + repo + "&tag=release",
		"tag empty first":        "repository=" + repo + "&tag=&tag=release",
		"tag blank first":        "repository=" + repo + "&tag=%20%20&tag=release",
		"tag bare first":         "repository=" + repo + "&tag&tag=release",
		"digest bad first":       "repository=" + repo + "&digest=sha256:xyz&digest=" + testDigestA,
		"digest bare first":      "repository=" + repo + "&digest&digest=" + testDigestA,
		"digest spaced first":    "repository=" + repo + "&digest=+" + testDigestA + "&digest=" + testDigestA,
	}
	for name, query := range invalid {
		t.Run(name, func(t *testing.T) {
			assertErrorResponse(t, getArtifacts(t, router, query),
				http.StatusBadRequest, codeInvalidInput, msgInvalidQuery)
		})
	}

	// Legal first occurrence keeps deciding the target. A later illegal value
	// does not turn the query into a 400, and a later legal value that would
	// hit elsewhere neither retargets nor rescues a first-value miss.
	firstWins := map[string]struct {
		query      string
		wantStatus int
		wantDigest string
	}{
		"repository legal then blank": {
			"repository=" + repo + "&repository=%20&tag=release", http.StatusOK, testDigestB},
		"tag legal then empty": {
			"repository=" + repo + "&tag=release&tag=", http.StatusOK, testDigestB},
		"tag legal then blank": {
			"repository=" + repo + "&tag=release&tag=%20%20", http.StatusOK, testDigestB},
		"tag legal then malformed digest-style value": {
			"repository=" + repo + "&tag=release&tag=sha256:xyz", http.StatusOK, testDigestB},
		"tag first misses then hits": {
			"repository=" + repo + "&tag=missing&tag=release", http.StatusNotFound, ""},
		"digest legal then malformed": {
			"repository=" + repo + "&digest=" + testDigestA + "&digest=sha256:xyz", http.StatusOK, testDigestA},
		"digest hits then other legal digest": {
			"repository=" + repo + "&digest=" + testDigestA + "&digest=" + testDigestB, http.StatusOK, testDigestA},
		"digest first misses then hits": {
			"repository=" + repo + "&digest=" + testDigestC + "&digest=" + testDigestA, http.StatusNotFound, ""},
	}
	for name, tc := range firstWins {
		t.Run(name, func(t *testing.T) {
			recorder := getArtifacts(t, router, tc.query)
			if tc.wantStatus == http.StatusNotFound {
				assertErrorResponse(t, recorder, http.StatusNotFound, codeNotFound, msgNotFound)
				return
			}
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d (%s)", recorder.Code, http.StatusOK, recorder.Body)
			}
			if got := soleRecord(t, recorder)["digest"]; got != tc.wantDigest {
				t.Fatalf("target digest = %v, want %s", got, tc.wantDigest)
			}
		})
	}
}

// TestPercentDecodedParameterNamesAndCaseSensitivity proves that parameter
// matching happens on the percent-decoded name ('%74ag' is 'tag') while
// matching stays byte-for-byte case sensitive ('Tag' is an unknown parameter):
// unknown parameters, repeated or not, are ignored and never satisfy a
// required one.
func TestPercentDecodedParameterNamesAndCaseSensitivity(t *testing.T) {
	router, _ := newTestRouter(t)
	const repo = "registry-demo/qnames"
	mustRegister(t, router, repo, testDigestA, "release") // tag release -> A
	mustRegister(t, router, repo, testDigestB, "other")   // two records total

	// Escaped spellings of the known names decode to the same parameter, so the
	// escaped and plain spellings collide and document order still picks the
	// first value. %72='r', %74='t', %64='d'.
	collisions := map[string]struct {
		query      string
		wantStatus int
		wantDigest string
	}{
		"escaped repository first hits": {
			"%72epository=" + repo + "&repository=registry-demo/other&tag=release", http.StatusOK, testDigestA},
		"plain repository first misses": {
			"repository=registry-demo/other&%72epository=" + repo + "&tag=release", http.StatusNotFound, ""},
		"escaped tag first misses": {
			"repository=" + repo + "&%74ag=missing&tag=release", http.StatusNotFound, ""},
		"plain tag first hits": {
			"repository=" + repo + "&tag=release&%74ag=missing", http.StatusOK, testDigestA},
		"escaped tag alone is the tag query": {
			"repository=" + repo + "&%74ag=release", http.StatusOK, testDigestA},
		"escaped digest alone is the digest query": {
			"repository=" + repo + "&%64igest=" + testDigestB, http.StatusOK, testDigestB},
	}
	for name, tc := range collisions {
		t.Run(name, func(t *testing.T) {
			recorder := getArtifacts(t, router, tc.query)
			if tc.wantStatus == http.StatusNotFound {
				assertErrorResponse(t, recorder, http.StatusNotFound, codeNotFound, msgNotFound)
				return
			}
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d (%s)", recorder.Code, http.StatusOK, recorder.Body)
			}
			if got := soleRecord(t, recorder)["digest"]; got != tc.wantDigest {
				t.Fatalf("target digest = %v, want %s", got, tc.wantDigest)
			}
		})
	}

	// Case-sensitive names: a differently cased spelling is unknown, not the
	// known parameter. With tag absent the request legally dispatches as the
	// repository list (both records), which distinguishes ignoring from a tag
	// query (one record).
	uppercaseTag := getArtifacts(t, router, "repository="+repo+"&Tag=release")
	if uppercaseTag.Code != http.StatusOK {
		t.Fatalf("Tag= status = %d, want %d (%s)", uppercaseTag.Code, http.StatusOK, uppercaseTag.Body)
	}
	if records := allRecords(t, uppercaseTag); len(records) != 2 {
		t.Fatalf("Tag= was treated as the tag query: got %d records, want the 2-record list", len(records))
	}

	// A cased repository spelling cannot satisfy the required repository.
	assertErrorResponse(t, getArtifacts(t, router, "Repository="+repo+"&tag=release"),
		http.StatusBadRequest, codeInvalidInput, msgInvalidQuery)

	// Case variants arriving after the known name are just unknown repeats.
	one := soleRecord(t, getArtifacts(t, router, "repository="+repo+"&tag=release&TAG=other"))
	if one["digest"] != testDigestA {
		t.Fatalf("TAG= retargeted the tag query: %v", one)
	}
	one = soleRecord(t, getArtifacts(t, router, "repository="+repo+"&digest="+testDigestA+"&DIGEST="+testDigestB))
	if one["digest"] != testDigestA {
		t.Fatalf("DIGEST= retargeted the digest query: %v", one)
	}

	// Unknown parameters, even repeated, are ignored and never block the query.
	unknown := getArtifacts(t, router, "repository="+repo+"&tag=release&unknown=1&unknown=2&frob=")
	if unknown.Code != http.StatusOK {
		t.Fatalf("unknown parameters status = %d, want %d (%s)", unknown.Code, http.StatusOK, unknown.Body)
	}
	if records := allRecords(t, unknown); len(records) != 1 {
		t.Fatalf("unknown parameters changed the query shape: %v", records)
	}
}

// TestPlusVersusPercent2BTagDecoding proves the query decoder is not
// field-specific: QueryUnescape turns '+' into a space but '%2B' into a literal
// plus sign, so the tags "a b" and "a+b" are different records. It also pins
// the per-field normalization boundary: repository/tag are trimmed after
// decoding while digest is matched by the anchored regex untrimmed.
func TestPlusVersusPercent2BTagDecoding(t *testing.T) {
	router, _ := newTestRouter(t)
	const repo = "registry-demo/qplus"
	mustRegister(t, router, repo, testDigestA, "a b") // tag with a literal space
	mustRegister(t, router, repo, testDigestB, "a+b") // tag with a literal plus
	mustRegister(t, router, repo, testDigestC, "latest")

	// '+' and '%20' both decode to a space: both reach the space-tagged record.
	for _, value := range []string{"a+b", "a%20b"} {
		recorder := getArtifacts(t, router, "repository="+repo+"&tag="+value)
		if recorder.Code != http.StatusOK {
			t.Fatalf("tag %q status = %d, want %d (%s)", value, recorder.Code, http.StatusOK, recorder.Body)
		}
		if got := soleRecord(t, recorder)["digest"]; got != testDigestA {
			t.Fatalf("tag %q resolved to %v, want space-tagged %s", value, got, testDigestA)
		}
	}

	// '%2B' decodes to a literal plus sign: a different tag and record.
	plus := getArtifacts(t, router, "repository="+repo+"&tag=a%2Bb")
	if plus.Code != http.StatusOK {
		t.Fatalf("tag a%%2Bb status = %d, want %d (%s)", plus.Code, http.StatusOK, plus.Body)
	}
	if got := soleRecord(t, plus)["digest"]; got != testDigestB {
		t.Fatalf("tag a%%2Bb resolved to %v, want plus-tagged %s", got, testDigestB)
	}

	// TrimSpace runs after decoding: '+' edges become spaces and are trimmed,
	// but '%2B' edges stay literal plus signs, which are not whitespace.
	trimmed := getArtifacts(t, router, "repository="+repo+"&tag=+latest+")
	if trimmed.Code != http.StatusOK {
		t.Fatalf("tag +latest+ status = %d, want %d (%s)", trimmed.Code, http.StatusOK, trimmed.Body)
	}
	if got := soleRecord(t, trimmed)["digest"]; got != testDigestC {
		t.Fatalf("+latest+ resolved to %v, want trimmed latest %s", got, testDigestC)
	}
	assertErrorResponse(t, getArtifacts(t, router, "repository="+repo+"&tag=%2Blatest%2B"),
		http.StatusNotFound, codeNotFound, msgNotFound)

	// Repository gets the same decode-then-trim treatment: '+' padding becomes
	// spaces and is trimmed away.
	list := getArtifacts(t, router, "repository=++"+repo+"++")
	if list.Code != http.StatusOK {
		t.Fatalf("padded repository status = %d, want %d (%s)", list.Code, http.StatusOK, list.Body)
	}
	if digests := recordDigests(allRecords(t, list)); len(digests) != 3 ||
		digests[0] != testDigestA || digests[1] != testDigestB || digests[2] != testDigestC {
		t.Fatalf("list order = %v, want [A B C] in registration order", digests)
	}

	// Digest is never trimmed: an encoded space before or after the value keeps
	// it outside the anchored digest format, so the answer is 400 (not 404 and
	// not a hit after trimming).
	for _, value := range []string{"+" + testDigestA, testDigestA + "+", "%20" + testDigestA} {
		assertErrorResponse(t, getArtifacts(t, router, "repository="+repo+"&digest="+value),
			http.StatusBadRequest, codeInvalidInput, msgInvalidQuery)
	}
}

// TestParameterArrangementSelectsTargetAndQueriesAreReadOnly walks two records
// of different digests in one repository to show how parameter presence and
// order select the target — ordered list, current tag pointer, immutable
// digest record, and the 400 when both selectors are present — and then proves
// that queries never rewrite records, push times or tag pointers.
func TestParameterArrangementSelectsTargetAndQueriesAreReadOnly(t *testing.T) {
	router, _ := newTestRouter(t)
	const repo = "registry-demo/qarrange"

	first := postArtifact(t, router, registration(repo, testDigestA, "release", true, regRetention, regSizeA))
	if first.Code != http.StatusCreated {
		t.Fatalf("register A status = %d (%s)", first.Code, first.Body)
	}
	pushedA := decodeBody(t, first)["pushed_at"].(string)
	second := postArtifact(t, router, registration(repo, testDigestB, "release", true, regRetention, regSizeB))
	if second.Code != http.StatusCreated {
		t.Fatalf("register B status = %d (%s)", second.Code, second.Body)
	}
	pushedB := decodeBody(t, second)["pushed_at"].(string)

	// Repository list: both records, in first-registration order.
	if digests := recordDigests(allRecords(t, queryList(t, router, repo))); len(digests) != 2 ||
		digests[0] != testDigestA || digests[1] != testDigestB {
		t.Fatalf("list = %v, want [A B] in registration order", digests)
	}

	// Tag follows the current pointer (B); digest returns the immutable
	// original record A, push time and all, even though the tag moved.
	if tagged := soleRecord(t, queryByTag(t, router, repo, "release")); tagged["digest"] != testDigestB ||
		tagged["pushed_at"] != pushedB {
		t.Fatalf("tag release = %v, want current pointer B/%s", tagged, pushedB)
	}
	expectStoredRecord(t, soleRecord(t, queryByDigest(t, router, repo, testDigestA)),
		repo, testDigestA, "release", true, float64(regRetention), float64(regSizeA), pushedA)

	// Tag and digest together never degrade into a list or a single-condition
	// query, in either parameter order.
	assertErrorResponse(t,
		getArtifacts(t, router, "repository="+repo+"&tag=release&digest="+testDigestB),
		http.StatusBadRequest, codeInvalidInput, msgInvalidQuery)
	assertErrorResponse(t,
		getArtifacts(t, router, "repository="+repo+"&digest="+testDigestB+"&tag=release"),
		http.StatusBadRequest, codeInvalidInput, msgInvalidQuery)

	// snapshot captures every successful view of committed state.
	snapshot := func() string {
		views := []string{
			queryList(t, router, repo).Body.String(),
			queryByTag(t, router, repo, "release").Body.String(),
			queryByDigest(t, router, repo, testDigestA).Body.String(),
			queryByDigest(t, router, repo, testDigestB).Body.String(),
		}
		return strings.Join(views, "\n")
	}
	before := snapshot()

	// A barrage of reads: hits, first-value misses, rejected shapes and
	// differently decoded tags — none of which may write anything.
	barrage := []string{
		"repository=" + repo + "&tag=release&tag=missing",
		"repository=" + repo + "&digest=" + testDigestA + "&digest=" + testDigestB,
		"repository=" + repo + "&tag=release&digest=" + testDigestB,
		"repository=" + repo + "&digest=" + testDigestC,
		"repository=" + repo + "&tag=a%2Bb",
		"repository=" + repo + "&tag=+release+",
		"repository=registry-demo/other",
		"repository=" + repo,
	}
	for _, query := range barrage {
		getArtifacts(t, router, query)
	}

	if after := snapshot(); after != before {
		t.Fatalf("committed state changed through queries:\nbefore: %s\nafter:  %s", before, after)
	}
	// Explicit re-check of the pointer and the original record after the reads.
	if tagged := soleRecord(t, queryByTag(t, router, repo, "release")); tagged["digest"] != testDigestB ||
		tagged["pushed_at"] != pushedB {
		t.Fatalf("tag pointer changed after queries: %v", tagged)
	}
	expectStoredRecord(t, soleRecord(t, queryByDigest(t, router, repo, testDigestA)),
		repo, testDigestA, "release", true, float64(regRetention), float64(regSizeA), pushedA)
}

// TestInvalidQueryFirstValueWinsOverStorageFailure closes the underlying
// SQLite file and checks the ordering on the query entry: an illegal first
// occurrence keeps its 400 before any storage access, while legal queries
// degrade to 503 storage_unavailable.
func TestInvalidQueryFirstValueWinsOverStorageFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	router := NewRouter(st)
	const repo = "registry-demo/qdown"
	mustRegister(t, router, repo, testDigestA, "release")
	mustRegister(t, router, repo, testDigestB, "release")
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	// Illegal first occurrences keep their 400 even though the store is down.
	invalid := []string{
		"repository=&repository=" + repo + "&tag=release",
		"repository=" + repo + "&tag=&tag=release",
		"repository=" + repo + "&tag=%20%20&tag=release",
		"repository=" + repo + "&digest=sha256:xyz&digest=" + testDigestA,
		"repository=" + repo + "&digest&digest=" + testDigestA,
	}
	for _, query := range invalid {
		assertErrorResponse(t, getArtifacts(t, router, query),
			http.StatusBadRequest, codeInvalidInput, msgInvalidQuery)
	}

	// Legal queries keep the published 503 degradation.
	assertErrorResponse(t, queryList(t, router, repo),
		http.StatusServiceUnavailable, codeStorage, msgStorage)
	assertErrorResponse(t, queryByTag(t, router, repo, "release"),
		http.StatusServiceUnavailable, codeStorage, msgStorage)
	assertErrorResponse(t, queryByDigest(t, router, repo, testDigestA),
		http.StatusServiceUnavailable, codeStorage, msgStorage)
}

// TestQueryInputRulesOnAssemblyEntry proves the query input boundary lives in
// the shared handler chain, not in the SQLite assembly: on
// NewRouterWithStore with a caller-supplied store, repeated parameters still
// take their first value, an illegal first value still wins its 400,
// percent-decoded names still collide, case variants and unknown parameters
// are still ignored, '+' and '%2B' still reach different tags, and /healthz
// is untouched by any of it.
func TestQueryInputRulesOnAssemblyEntry(t *testing.T) {
	st := newAssemblyStore()
	router := newAssemblyRouter(st, &assemblyProbe{})
	const repo = "registry-demo/qasm"
	mustRegister(t, router, repo, testDigestA, "alpha")
	mustRegister(t, router, repo, testDigestB, "beta")
	mustRegister(t, router, repo, testDigestD, "a b")
	mustRegister(t, router, repo, testDigestE, "a+b")

	// First occurrence wins for every recognized parameter.
	firstWins := map[string]struct {
		query      string
		wantStatus int
		wantDigest string
	}{
		"tag alpha first":          {"repository=" + repo + "&tag=alpha&tag=beta", http.StatusOK, testDigestA},
		"tag beta first":           {"repository=" + repo + "&tag=beta&tag=alpha", http.StatusOK, testDigestB},
		"digest A first":           {"repository=" + repo + "&digest=" + testDigestA + "&digest=" + testDigestB, http.StatusOK, testDigestA},
		"digest B first":           {"repository=" + repo + "&digest=" + testDigestB + "&digest=" + testDigestA, http.StatusOK, testDigestB},
		"repository first missing": {"repository=registry-demo/other&repository=" + repo + "&tag=alpha", http.StatusNotFound, ""},
		"escaped tag first misses": {"repository=" + repo + "&%74ag=missing&tag=alpha", http.StatusNotFound, ""},
		"plain tag first hits":     {"repository=" + repo + "&tag=alpha&%74ag=missing", http.StatusOK, testDigestA},
		"plus reaches space tag":   {"repository=" + repo + "&tag=a+b", http.StatusOK, testDigestD},
		"percent-2B reaches plus":  {"repository=" + repo + "&tag=a%2Bb", http.StatusOK, testDigestE},
	}
	for name, tc := range firstWins {
		t.Run(name, func(t *testing.T) {
			recorder := getArtifacts(t, router, tc.query)
			if tc.wantStatus == http.StatusNotFound {
				assertErrorResponse(t, recorder, http.StatusNotFound, codeNotFound, msgNotFound)
				return
			}
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d (%s)", recorder.Code, http.StatusOK, recorder.Body)
			}
			if got := soleRecord(t, recorder)["digest"]; got != tc.wantDigest {
				t.Fatalf("target digest = %v, want %s", got, tc.wantDigest)
			}
		})
	}

	// Illegal first value: later legal values cannot rescue it.
	for _, query := range []string{
		"repository=&repository=" + repo + "&tag=alpha",
		"repository=" + repo + "&tag=&tag=alpha",
		"repository=" + repo + "&digest=sha256:xyz&digest=" + testDigestA,
	} {
		assertErrorResponse(t, getArtifacts(t, router, query),
			http.StatusBadRequest, codeInvalidInput, msgInvalidQuery)
	}

	// Legal first value cannot be overridden by a later illegal one.
	if recorder := getArtifacts(t, router,
		"repository="+repo+"&digest="+testDigestA+"&digest=sha256:xyz"); recorder.Code != http.StatusOK {
		t.Fatalf("legal first digest status = %d, want %d (%s)", recorder.Code, http.StatusOK, recorder.Body)
	}

	// A cased name is unknown: Tag= leaves the repository list, while tag and
	// digest together stay a 400; unknown repeats are ignored.
	if records := allRecords(t, getArtifacts(t, router, "repository="+repo+"&Tag=alpha")); len(records) != 4 {
		t.Fatalf("Tag= was treated as the tag query: got %d records, want the 4-record list", len(records))
	}
	assertErrorResponse(t,
		getArtifacts(t, router, "repository="+repo+"&tag=alpha&digest="+testDigestA),
		http.StatusBadRequest, codeInvalidInput, msgInvalidQuery)
	if recorder := getArtifacts(t, router,
		"repository="+repo+"&tag=alpha&frob=1&frob=2"); recorder.Code != http.StatusOK {
		t.Fatalf("unknown repeats status = %d, want %d (%s)", recorder.Code, http.StatusOK, recorder.Body)
	}

	// The health entry is untouched by the query input boundary.
	if recorder := getHealth(t, router); recorder.Code != http.StatusOK {
		t.Fatalf("/healthz = %d, want %d (%s)", recorder.Code, http.StatusOK, recorder.Body)
	}
}
