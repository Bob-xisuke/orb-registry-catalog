package api

// These regression cases exercise only the public HTTP surface
// (GET /v1/artifacts plus the registration entry that seeds state) and back
// the conclusions in docs/query-input-decoding-analysis.md: how a raw query
// string becomes a normalized, validated service.Query — repeated
// parameters, percent-decoded parameter names, '+' versus '%2B', and the
// difference between a missing and an empty parameter — and how the target
// of a query is decided by that normalization. They deliberately avoid the
// store's unexported internals so that any refactor preserving the published
// contract keeps them green.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/Bob-xisuke/orb-registry-catalog/internal/store"
)

const queryInputRepo = "registry-demo/qinput"

// queryPair percent-encodes one key and one value into a key=value fragment.
func queryPair(key, value string) string {
	return url.QueryEscape(key) + "=" + url.QueryEscape(value)
}

// queryString joins already-encoded key=value fragments in the exact order
// given, so tests control document order of repeated and percent-decoded
// parameter names.
func queryString(frags ...string) string { return strings.Join(frags, "&") }

// seedTwoDigestRepo registers A then B under the same "release" tag, so the
// tag pointer resolves to B while A remains reachable by digest. It returns
// the two service-generated push times for byte-for-byte comparisons.
func seedTwoDigestRepo(t *testing.T, router *gin.Engine, repo string) (pushedA, pushedB string) {
	t.Helper()
	first := postArtifact(t, router, registration(repo, testDigestA, sequenceTag, true, regRetention, regSizeA))
	if first.Code != http.StatusCreated {
		t.Fatalf("register A status = %d, want %d (%s)", first.Code, http.StatusCreated, first.Body)
	}
	pushedA = decodeBody(t, first)["pushed_at"].(string)
	second := postArtifact(t, router, registration(repo, testDigestB, sequenceTag, true, regRetention, regSizeB))
	if second.Code != http.StatusCreated {
		t.Fatalf("register B status = %d, want %d (%s)", second.Code, http.StatusCreated, second.Body)
	}
	pushedB = decodeBody(t, second)["pushed_at"].(string)
	return pushedA, pushedB
}

// expectB is the B record shape shared by the repeated-parameter cases.
func expectB(t *testing.T, recorder *httptest.ResponseRecorder, repo, pushedB string) {
	t.Helper()
	expectStoredRecord(t, soleRecord(t, recorder),
		repo, testDigestB, sequenceTag, true,
		float64(regRetention), float64(regSizeB), pushedB)
}

// TestDuplicateQueryParametersFirstValueWins pins the GET-side rule, which is
// the mirror image of the POST body's last-occurrence rule: gin reads
// values[0] of the parsed query slice, so the FIRST occurrence of a repeated
// repository/tag/digest alone decides normalization, validation and dispatch.
// A later legal value never rescues an illegal first value, and a later
// illegal value never overrides a legal first one.
func TestDuplicateQueryParametersFirstValueWins(t *testing.T) {
	router, _ := newTestRouter(t)
	pushedA, pushedB := seedTwoDigestRepo(t, router, queryInputRepo)
	repoFrag := queryPair("repository", queryInputRepo)

	// tag: a missing first value picks the (failed) target even though a
	// second, present value follows; a present first value hits while the
	// later missing value is ignored.
	assertErrorResponse(t, getArtifacts(t, router, queryString(
		repoFrag, queryPair("tag", "missing-tag"), queryPair("tag", sequenceTag))),
		http.StatusNotFound, codeNotFound, msgNotFound)
	expectB(t, getArtifacts(t, router, queryString(
		repoFrag, queryPair("tag", sequenceTag), queryPair("tag", "missing-tag"))),
		queryInputRepo, pushedB)

	// A later blank or valueless tag cannot override the legal first value:
	// only values[0] is trimmed and validated.
	expectB(t, getArtifacts(t, router, queryString(
		repoFrag, queryPair("tag", sequenceTag), "tag=%20%20")), queryInputRepo, pushedB)
	expectB(t, getArtifacts(t, router, queryString(
		repoFrag, queryPair("tag", sequenceTag), "tag")), queryInputRepo, pushedB)

	// Mirror rule: a blank first value or a bare first parameter is not
	// rescued by the legal value that follows.
	assertErrorResponse(t, getArtifacts(t, router, queryString(
		repoFrag, "tag=%20%20", queryPair("tag", sequenceTag))),
		http.StatusBadRequest, codeInvalidInput, msgInvalidQuery)
	assertErrorResponse(t, getArtifacts(t, router, queryString(
		repoFrag, "tag", queryPair("tag", sequenceTag))),
		http.StatusBadRequest, codeInvalidInput, msgInvalidQuery)

	// digest: the first value selects the exact immutable record, and a later
	// different or malformed value cannot override it.
	expectStoredRecord(t, soleRecord(t, getArtifacts(t, router, queryString(
		repoFrag, queryPair("digest", testDigestA), queryPair("digest", testDigestB)))),
		queryInputRepo, testDigestA, sequenceTag, true,
		float64(regRetention), float64(regSizeA), pushedA)
	expectStoredRecord(t, soleRecord(t, getArtifacts(t, router, queryString(
		repoFrag, queryPair("digest", testDigestB), queryPair("digest", testDigestA)))),
		queryInputRepo, testDigestB, sequenceTag, true,
		float64(regRetention), float64(regSizeB), pushedB)
	expectStoredRecord(t, soleRecord(t, getArtifacts(t, router, queryString(
		repoFrag, queryPair("digest", testDigestA), "digest=sha256:xyz"))),
		queryInputRepo, testDigestA, sequenceTag, true,
		float64(regRetention), float64(regSizeA), pushedA)

	// A malformed first digest is rejected 400 despite the legal value after
	// it; the request never degrades to the second value or to a list query.
	assertErrorResponse(t, getArtifacts(t, router, queryString(
		repoFrag, "digest=sha256:xyz", queryPair("digest", testDigestA))),
		http.StatusBadRequest, codeInvalidInput, msgInvalidQuery)

	// repository follows the same first-value rule. A whitespace-only first
	// value trims to empty and is a 400 even though the second value is legal.
	assertErrorResponse(t, getArtifacts(t, router, queryString(
		"repository=%20%20", repoFrag, queryPair("tag", sequenceTag))),
		http.StatusBadRequest, codeInvalidInput, msgInvalidQuery)

	// The first repository is legal (after trimming) and the second names a
	// repository that does not exist: the list still answers for the first.
	if records := allRecords(t, getArtifacts(t, router, queryString(
		queryPair("repository", "  "+queryInputRepo+"  "),
		queryPair("repository", "registry-demo/does-not-exist")))); len(records) != 2 {
		t.Fatalf("second repository value affected the list: %v", records)
	}

	// The whole sequence was read-only: the list is still A then B and the
	// tag pointer still resolves to B with its original push time.
	if digests := recordDigests(allRecords(t, queryList(t, router, queryInputRepo))); len(digests) != 2 ||
		digests[0] != testDigestA || digests[1] != testDigestB {
		t.Fatalf("list changed after duplicate-parameter queries: %v", digests)
	}
	if tagged := soleRecord(t, queryByTag(t, router, queryInputRepo, sequenceTag)); tagged["digest"] != testDigestB ||
		tagged["pushed_at"] != pushedB {
		t.Fatalf("tag pointer changed after duplicate-parameter queries: %v", tagged)
	}
}

// TestPercentDecodedParameterNamesCollide proves parameter matching happens on
// the percent-decoded name: "%74ag" decodes to "tag" and is therefore the
// same parameter as a literal "tag", with document order deciding the first
// value. Unknown names — including percent-decoded ones — stay ignored, and
// matching is case-sensitive, so "Tag"/"Repository" are different, unknown
// parameters rather than duplicates.
func TestPercentDecodedParameterNamesCollide(t *testing.T) {
	router, _ := newTestRouter(t)
	pushedA, pushedB := seedTwoDigestRepo(t, router, queryInputRepo)
	repoFrag := queryPair("repository", queryInputRepo)

	// "%74ag" decodes to "tag". The decoded name collides with a literal tag:
	// first occurrence wins, regardless of which spelling came first.
	assertErrorResponse(t, getArtifacts(t, router, queryString(
		repoFrag, "%74ag="+url.QueryEscape("missing-tag"), queryPair("tag", sequenceTag))),
		http.StatusNotFound, codeNotFound, msgNotFound)
	expectB(t, getArtifacts(t, router, queryString(
		repoFrag, queryPair("tag", sequenceTag), "%74ag="+url.QueryEscape("missing-tag"))),
		queryInputRepo, pushedB)

	// Unknown parameters — a plain name and a percent-decoded one (%65xtra ->
	// "extra") — are ignored; omitting tag and digest lists the repository.
	if records := allRecords(t, getArtifacts(t, router, queryString(
		repoFrag, queryPair("unknown", "x"), "%65xtra=y"))); len(records) != 2 {
		t.Fatalf("unknown parameters were not ignored: %v", records)
	}

	// Parameter names are case-sensitive. A capital "Tag" is unknown and
	// ignored, so the request is a repository list (200), not a tag query.
	if records := allRecords(t, getArtifacts(t, router,
		queryString(repoFrag, "Tag="+sequenceTag))); len(records) != 2 {
		t.Fatalf("capital Tag was treated as the tag parameter: %v", records)
	}

	// "Repository" and "repository" are two distinct names, not a repeated
	// parameter: the capital one names a missing repository and the real one
	// lists successfully. Had matching been case-insensitive the capital
	// value (a missing repository) would have collided with the list.
	if records := allRecords(t, getArtifacts(t, router, queryString(
		"Repository="+url.QueryEscape("registry-demo/does-not-exist"), repoFrag))); len(records) != 2 {
		t.Fatalf("capital Repository collided with repository: %v", records)
	}

	// A capital "Repository" cannot stand in for the missing lowercase one.
	assertErrorResponse(t, getArtifacts(t, router,
		queryString("Repository="+url.QueryEscape(queryInputRepo), queryPair("tag", sequenceTag))),
		http.StatusBadRequest, codeInvalidInput, msgInvalidQuery)

	// Read-only: the immutable A record and the tag pointer are untouched.
	expectStoredRecord(t, soleRecord(t, queryByDigest(t, router, queryInputRepo, testDigestA)),
		queryInputRepo, testDigestA, sequenceTag, true,
		float64(regRetention), float64(regSizeA), pushedA)
	if tagged := soleRecord(t, queryByTag(t, router, queryInputRepo, sequenceTag)); tagged["digest"] != testDigestB {
		t.Fatalf("tag pointer changed: %v", tagged)
	}
}

// TestPlusAndPercent2BTagDistinctRecords proves the query-string decoding
// rule that lets two visually close tags coexist: '+' decodes to a space
// while '%2B' decodes to an actual '+', so "tag=a+b" and "tag=a%2Bb" target
// different tag pointers and therefore different records.
func TestPlusAndPercent2BTagDistinctRecords(t *testing.T) {
	router, _ := newTestRouter(t)
	const repo = "registry-demo/plus"

	// Two records with tags that differ only in space versus '+'. JSON bodies
	// carry the literal characters; the tag_pointers keys are "a b" and "a+b".
	spaceRec := postArtifact(t, router, registration(repo, testDigestA, "a b", true, regRetention, regSizeA))
	if spaceRec.Code != http.StatusCreated {
		t.Fatalf("register space tag status = %d (%s)", spaceRec.Code, spaceRec.Body)
	}
	pushedSpace := decodeBody(t, spaceRec)["pushed_at"].(string)
	plusRec := postArtifact(t, router, registration(repo, testDigestC, "a+b", true, regRetention, regSizeB))
	if plusRec.Code != http.StatusCreated {
		t.Fatalf("register plus tag status = %d (%s)", plusRec.Code, plusRec.Body)
	}
	pushedPlus := decodeBody(t, plusRec)["pushed_at"].(string)

	repoFrag := queryPair("repository", repo)

	// '+' is a space in the query component: "a+b" decodes to the "a b" tag,
	// as does the explicit "%20" spelling; a leading '+' decodes to a leading
	// space that TrimSpace then removes, reaching the same pointer.
	expectStoredRecord(t, soleRecord(t, getArtifacts(t, router, repoFrag+"&tag=a+b")),
		repo, testDigestA, "a b", true,
		float64(regRetention), float64(regSizeA), pushedSpace)
	expectStoredRecord(t, soleRecord(t, getArtifacts(t, router, repoFrag+"&tag=a%20b")),
		repo, testDigestA, "a b", true,
		float64(regRetention), float64(regSizeA), pushedSpace)
	expectStoredRecord(t, soleRecord(t, getArtifacts(t, router, repoFrag+"&tag=+a+b")),
		repo, testDigestA, "a b", true,
		float64(regRetention), float64(regSizeA), pushedSpace)

	// '%2B' decodes to a literal '+': "a%2Bb" is the other tag and record.
	// (url.QueryEscape("a+b") itself renders "a%2Bb", the canonical client
	// spelling for a tag containing a plus.)
	expectStoredRecord(t, soleRecord(t, getArtifacts(t, router, repoFrag+"&tag=a%2Bb")),
		repo, testDigestC, "a+b", true,
		float64(regRetention), float64(regSizeB), pushedPlus)
	expectStoredRecord(t, soleRecord(t, getArtifacts(t, router,
		repoFrag+"&tag="+url.QueryEscape("a+b"))),
		repo, testDigestC, "a+b", true,
		float64(regRetention), float64(regSizeB), pushedPlus)

	// The two encodings cannot reach each other's pointer: an internal space
	// before a literal plus matches neither key.
	assertErrorResponse(t, getArtifacts(t, router, repoFrag+"&tag=a%20%2Bb"),
		http.StatusNotFound, codeNotFound, msgNotFound)
	// But surrounding spaces around a real plus are removed by TrimSpace, so
	// "%20a%2Bb%20" normalizes back to the "a+b" pointer and hits C.
	expectStoredRecord(t, soleRecord(t, getArtifacts(t, router, repoFrag+"&tag=%20a%2Bb%20")),
		repo, testDigestC, "a+b", true,
		float64(regRetention), float64(regSizeB), pushedPlus)

	// Both records coexist in first-registration order.
	if digests := recordDigests(allRecords(t, queryList(t, router, repo))); len(digests) != 2 ||
		digests[0] != testDigestA || digests[1] != testDigestC {
		t.Fatalf("plus list = %v, want A then C in registration order", digests)
	}
}

// TestMissingVersusEmptyQueryParameters pins the distinction between a
// parameter that is absent and one whose value is empty after decoding or
// trimming: only total absence of both tag and digest is a repository list.
// Every other form — missing/blank repository, blank or bare tag, malformed
// digest, bare digest, or tag and digest together — is the same 400 and can
// never degrade into a list or a single-condition query.
func TestMissingVersusEmptyQueryParameters(t *testing.T) {
	router, _ := newTestRouter(t)
	seedTwoDigestRepo(t, router, queryInputRepo) // non-empty list: degradation to a list would be visible

	invalid := map[string]string{
		"no parameters":            "",
		"only unknown parameter":   "unknown=1",
		"repository missing":       "tag=" + sequenceTag,
		"repository empty":         "repository=&tag=" + sequenceTag,
		"repository whitespace":    "repository=%20%20&tag=" + sequenceTag,
		"repository bare":          "repository&tag=" + sequenceTag,
		"tag empty value":          "repository=" + queryInputRepo + "&tag=",
		"tag bare parameter":       "repository=" + queryInputRepo + "&tag",
		"tag whitespace only":      "repository=" + queryInputRepo + "&tag=%20%20",
		"digest wrong shape":       "repository=" + queryInputRepo + "&digest=sha256:xyz",
		"digest uppercase hex":     "repository=" + queryInputRepo + "&digest=" + strings.ToUpper(testDigestA),
		"digest bare parameter":    "repository=" + queryInputRepo + "&digest",
		"tag and digest present":   "repository=" + queryInputRepo + "&tag=" + sequenceTag + "&digest=" + testDigestA,
		"tag and digest both bare": "repository=" + queryInputRepo + "&tag&digest",
	}
	for name, query := range invalid {
		t.Run(name, func(t *testing.T) {
			assertErrorResponse(t, getArtifacts(t, router, query),
				http.StatusBadRequest, codeInvalidInput, msgInvalidQuery)
		})
	}

	// The control case: omitting tag and digest entirely is the one list
	// query, while an explicit empty value is the 400 just asserted.
	if records := allRecords(t, queryList(t, router, queryInputRepo)); len(records) != 2 {
		t.Fatalf("repository list = %d records, want 2", len(records))
	}

	// A legal single-condition query hits; a legal miss stays 404.
	if got := soleRecord(t, queryByTag(t, router, queryInputRepo, sequenceTag)); got["digest"] != testDigestB {
		t.Fatalf("tag query = %v, want B", got)
	}
	assertErrorResponse(t, queryByDigest(t, router, queryInputRepo, testDigestC),
		http.StatusNotFound, codeNotFound, msgNotFound)
}

// TestQueriesDoNotMutateState runs hits and misses of every dispatch shape
// and then proves reads change nothing: registration order, both immutable
// records (push times included) and the current tag pointer are identical to
// before the queries.
func TestQueriesDoNotMutateState(t *testing.T) {
	router, _ := newTestRouter(t)
	pushedA, pushedB := seedTwoDigestRepo(t, router, queryInputRepo)
	repoFrag := queryPair("repository", queryInputRepo)

	// A mixture of hits, legal misses and the repeated/escaped forms.
	getArtifacts(t, router, queryString(repoFrag))
	getArtifacts(t, router, queryString(repoFrag, queryPair("tag", sequenceTag)))
	getArtifacts(t, router, queryString(repoFrag, queryPair("digest", testDigestA)))
	getArtifacts(t, router, queryString(repoFrag, queryPair("digest", testDigestB)))
	getArtifacts(t, router, queryString(repoFrag, queryPair("tag", sequenceTag), queryPair("tag", "missing")))
	getArtifacts(t, router, queryString(repoFrag, "%74ag="+url.QueryEscape(sequenceTag)))
	getArtifacts(t, router, "repository=registry-demo/missing")
	getArtifacts(t, router, queryString(repoFrag, queryPair("tag", "missing")))
	getArtifacts(t, router, queryString(repoFrag, queryPair("digest", testDigestC)))

	// Records, order and push times are byte-for-byte the first registrations.
	records := allRecords(t, queryList(t, router, queryInputRepo))
	if digests := recordDigests(records); len(digests) != 2 ||
		digests[0] != testDigestA || digests[1] != testDigestB {
		t.Fatalf("list after queries = %v, want [A B] in registration order", digests)
	}
	expectStoredRecord(t, records[0].(map[string]any),
		queryInputRepo, testDigestA, sequenceTag, true,
		float64(regRetention), float64(regSizeA), pushedA)
	expectStoredRecord(t, records[1].(map[string]any),
		queryInputRepo, testDigestB, sequenceTag, true,
		float64(regRetention), float64(regSizeB), pushedB)

	// The tag still resolves to B with B's original push time.
	expectB(t, queryByTag(t, router, queryInputRepo, sequenceTag), queryInputRepo, pushedB)
}

// TestMalformedPercentEscapeBaseline records the net/url behavior the handler
// inherits: URL.Query() discards ParseQuery's error, and a pair that fails
// percent-decoding is omitted from the parsed map (it occupies no slot),
// while the remaining pairs parse normally. A legal client that builds its
// query with url.QueryEscape can never produce a malformed escape; this pins
// what malformed raw text does rather than endorsing it.
func TestMalformedPercentEscapeBaseline(t *testing.T) {
	router, _ := newTestRouter(t)
	_, pushedB := seedTwoDigestRepo(t, router, queryInputRepo)
	repoFrag := queryPair("repository", queryInputRepo)

	// The malformed tag/digest pair is dropped: neither parameter is present,
	// so the request is the repository list (200), not a 400.
	if records := allRecords(t, getArtifacts(t, router, repoFrag+"&tag=%zz")); len(records) != 2 {
		t.Fatalf("malformed tag did not degrade to the parsed list: %v", records)
	}
	if records := allRecords(t, getArtifacts(t, router, repoFrag+"&digest=%zz")); len(records) != 2 {
		t.Fatalf("malformed digest did not degrade to the parsed list: %v", records)
	}

	// A malformed repository pair disappears, leaving no repository at all.
	assertErrorResponse(t, getArtifacts(t, router, "repository=%zz&tag="+sequenceTag),
		http.StatusBadRequest, codeInvalidInput, msgInvalidQuery)

	// The dropped pair occupies no slot: the surviving literal occurrence is
	// values[0], so both orderings resolve "release" and hit B.
	expectB(t, getArtifacts(t, router, repoFrag+"&tag="+sequenceTag+"&tag=%zz"),
		queryInputRepo, pushedB)
	expectB(t, getArtifacts(t, router, repoFrag+"&tag=%zz&tag="+sequenceTag),
		queryInputRepo, pushedB)
}

// TestInvalidQueryWinsOverStorageFailure closes the SQLite store and checks
// that query validation happens before any storage access: an illegal query
// keeps its 400 InvalidArtifactInputError while a legal one degrades to 503
// storage_unavailable.
func TestInvalidQueryWinsOverStorageFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	router := NewRouter(st)
	seedTwoDigestRepo(t, router, queryInputRepo)
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	repoFrag := queryPair("repository", queryInputRepo)

	// Invalid queries keep their 400 even though the store is down.
	assertErrorResponse(t, getArtifacts(t, router, ""),
		http.StatusBadRequest, codeInvalidInput, msgInvalidQuery)
	assertErrorResponse(t, getArtifacts(t, router, repoFrag+"&tag="),
		http.StatusBadRequest, codeInvalidInput, msgInvalidQuery)
	assertErrorResponse(t, getArtifacts(t, router, repoFrag+"&digest=sha256:xyz"),
		http.StatusBadRequest, codeInvalidInput, msgInvalidQuery)
	assertErrorResponse(t, getArtifacts(t, router, queryString(
		repoFrag, "digest=sha256:xyz", queryPair("digest", testDigestA))),
		http.StatusBadRequest, codeInvalidInput, msgInvalidQuery)

	// Legal queries reach the failing store and degrade to 503.
	assertErrorResponse(t, queryList(t, router, queryInputRepo),
		http.StatusServiceUnavailable, codeStorage, msgStorage)
	assertErrorResponse(t, queryByTag(t, router, queryInputRepo, sequenceTag),
		http.StatusServiceUnavailable, codeStorage, msgStorage)
	assertErrorResponse(t, queryByDigest(t, router, queryInputRepo, testDigestA),
		http.StatusServiceUnavailable, codeStorage, msgStorage)
}

// TestQueryInputOnAssemblyEntry proves the query normalization boundary lives
// in the shared handler chain, not in the SQLite assembly: the
// caller-supplied store entry NewRouterWithStore shows the same
// first-occurrence duplicate rule, percent-decoded name collision and '+' /
// '%2B' distinction, and /healthz is independent and untouched.
func TestQueryInputOnAssemblyEntry(t *testing.T) {
	st := newAssemblyStore()
	router := newAssemblyRouter(st, &assemblyProbe{})
	pushedA, pushedB := seedTwoDigestRepo(t, router, queryInputRepo)
	repoFrag := queryPair("repository", queryInputRepo)

	// Repeated tag: the first occurrence selects the target on this entry too.
	assertErrorResponse(t, getArtifacts(t, router, queryString(
		repoFrag, queryPair("tag", "missing-tag"), queryPair("tag", sequenceTag))),
		http.StatusNotFound, codeNotFound, msgNotFound)
	expectB(t, getArtifacts(t, router, queryString(
		repoFrag, queryPair("tag", sequenceTag), queryPair("tag", "missing-tag"))),
		queryInputRepo, pushedB)

	// A percent-decoded name collides with the literal one and resolves B.
	expectB(t, getArtifacts(t, router, queryString(
		repoFrag, "%74ag="+url.QueryEscape(sequenceTag))),
		queryInputRepo, pushedB)

	// '+' / '%2B' keep their distinct pointers through this entry: register a
	// third record under the literal "a+b" tag, then prove "a+b" in the query
	// decodes to the absent "a b" tag (404) while "a%2Bb" hits the new record.
	plusRec := postArtifact(t, router, registration(queryInputRepo, testDigestC, "a+b", true, regRetention, regSizeB))
	if plusRec.Code != http.StatusCreated {
		t.Fatalf("register plus tag status = %d (%s)", plusRec.Code, plusRec.Body)
	}
	pushedPlus := decodeBody(t, plusRec)["pushed_at"].(string)
	assertErrorResponse(t, getArtifacts(t, router, repoFrag+"&tag=a+b"),
		http.StatusNotFound, codeNotFound, msgNotFound)
	expectStoredRecord(t, soleRecord(t, getArtifacts(t, router, repoFrag+"&tag=a%2Bb")),
		queryInputRepo, testDigestC, "a+b", true,
		float64(regRetention), float64(regSizeB), pushedPlus)

	// Illegal input is a 400; with the store faulted it stays 400, while a
	// legal query degrades to 503 — validation precedes storage on this entry.
	st.fault = errors.New("assembly backend down")
	assertErrorResponse(t, getArtifacts(t, router, repoFrag+"&tag="),
		http.StatusBadRequest, codeInvalidInput, msgInvalidQuery)
	assertErrorResponse(t, queryList(t, router, queryInputRepo),
		http.StatusServiceUnavailable, codeStorage, msgStorage)
	st.fault = nil

	// The immutable A record reads back unchanged, and /healthz is independent.
	expectStoredRecord(t, soleRecord(t, queryByDigest(t, router, queryInputRepo, testDigestA)),
		queryInputRepo, testDigestA, sequenceTag, true,
		float64(regRetention), float64(regSizeA), pushedA)
	if recorder := getHealth(t, router); recorder.Code != http.StatusOK {
		t.Fatalf("/healthz = %d, want %d", recorder.Code, http.StatusOK)
	}
}
