package input_test

// Compatibility regression cases for the shared field rules: repository/tag
// normalization and the digest format are one implementation each behind
// both entries, so the same decoded value must normalize — or reject —
// identically whether it arrives in the JSON request body or in the
// percent-encoded query string. Every probe runs through ParseRegistration
// and ParseQuery and the two outcomes are pinned side by side; the transport
// decodings (JSON \u-escapes versus percent-encoded UTF-8) stay per entry.

import (
	"errors"
	"strings"
	"testing"

	"github.com/Bob-xisuke/orb-registry-catalog/internal/input"
	"github.com/Bob-xisuke/orb-registry-catalog/internal/service"
)

// nameProbe is one decoded repository/tag value spelled both ways: jsonValue
// is the raw JSON string literal (quotes included, \u-escaped so this file
// stays ASCII) for the request body, queryValue the percent-encoded form for
// the query string. wantName is the trimmed result both entries must
// produce; blank probes set wantReject.
type nameProbe struct {
	jsonValue  string
	queryValue string
	wantName   string
	wantReject bool
}

// sharedNameProbes covers the trim alphabet (ASCII space, tab, newline and
// the Unicode spaces NBSP, em space and ideographic space), interior
// characters and case that must survive verbatim, and blank-after-trim
// rejections. The zero-width space is not Unicode whitespace, so it must be
// kept even at the edges.
var sharedNameProbes = map[string]nameProbe{
	"ascii spaces":      {`"  team/app  "`, "++team/app++", "team/app", false},
	"percent-20 spaces": {`"  team/app  "`, "%20%20team/app%20%20", "team/app", false},
	"tab and newline":   {`"\tteam/app\n"`, "%09team/app%0A", "team/app", false},
	"nbsp edges":        {`"\u00a0team/app\u00a0"`, "%C2%A0team/app%C2%A0", "team/app", false},
	"em and ideographic": {
		`"\u2003team/app\u3000"`, "%E2%80%83team/app%E3%80%80", "team/app", false},
	"interior kept with case": {`" Team/App Q "`, "+Team/App+Q+", "Team/App Q", false},
	"interior nbsp kept":      {`"team\u00a0app"`, "team%C2%A0app", "team\u00a0app", false},
	"zero-width space kept":   {`"\u200bteam"`, "%E2%80%8Bteam", "\u200bteam", false},
	"blank ascii":             {`"   "`, "+++", "", true},
	"blank unicode":           {`"\u00a0\u2003"`, "%C2%A0%E2%80%83", "", true},
}

// registrationBody builds a full legal body around the given raw JSON string
// literals for repository and tag.
func registrationBody(repositoryJSON, tagJSON string) string {
	return `{"repository":` + repositoryJSON + `,"digest":"` + digestA + `","tag":` + tagJSON +
		`,"signature_verified":false,"retention_days":1,"size_bytes":0}`
}

// TestSharedNameRuleIdenticalOnBothEntries drives every probe as the
// repository and as the tag through both entries and pins identical
// outcomes: the same trimmed name, or the same zero result plus
// ErrInvalidInput.
func TestSharedNameRuleIdenticalOnBothEntries(t *testing.T) {
	for name, probe := range sharedNameProbes {
		t.Run(name+"/repository", func(t *testing.T) {
			got, err := input.ParseRegistration(strings.NewReader(registrationBody(probe.jsonValue, `"release"`)))
			query, queryErr := input.ParseQuery("repository=" + probe.queryValue)
			if probe.wantReject {
				if !errors.Is(err, input.ErrInvalidInput) || got != (service.RegisterInput{}) {
					t.Fatalf("body repository = (%+v, %v), want zero + ErrInvalidInput", got, err)
				}
				if !errors.Is(queryErr, input.ErrInvalidInput) || query != (service.Query{}) {
					t.Fatalf("query repository = (%+v, %v), want zero + ErrInvalidInput", query, queryErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("body repository rejected: %v", err)
			}
			if queryErr != nil {
				t.Fatalf("query repository rejected: %v", queryErr)
			}
			if got.Repository != probe.wantName || query.Repository != probe.wantName {
				t.Fatalf("trimmed repository: body %q, query %q, want %q",
					got.Repository, query.Repository, probe.wantName)
			}
			if query.Kind != service.QueryByRepository {
				t.Fatalf("query kind = %v, want a repository list", query.Kind)
			}
		})

		t.Run(name+"/tag", func(t *testing.T) {
			got, err := input.ParseRegistration(strings.NewReader(registrationBody(`"team/app"`, probe.jsonValue)))
			query, queryErr := input.ParseQuery("repository=team/app&tag=" + probe.queryValue)
			if probe.wantReject {
				if !errors.Is(err, input.ErrInvalidInput) || got != (service.RegisterInput{}) {
					t.Fatalf("body tag = (%+v, %v), want zero + ErrInvalidInput", got, err)
				}
				if !errors.Is(queryErr, input.ErrInvalidInput) || query != (service.Query{}) {
					t.Fatalf("query tag = (%+v, %v), want zero + ErrInvalidInput", query, queryErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("body tag rejected: %v", err)
			}
			if queryErr != nil {
				t.Fatalf("query tag rejected: %v", queryErr)
			}
			if got.Tag != probe.wantName || query.Tag != probe.wantName {
				t.Fatalf("trimmed tag: body %q, query %q, want %q", got.Tag, query.Tag, probe.wantName)
			}
			if query.Kind != service.QueryByTag {
				t.Fatalf("query kind = %v, want a tag query", query.Kind)
			}
		})
	}
}

// TestSharedDigestRuleIdenticalOnBothEntries pins the digest half of the
// shared rules: never trimmed, anchored format, same accept/reject on both
// entries.
func TestSharedDigestRuleIdenticalOnBothEntries(t *testing.T) {
	// A clean digest keeps its exact bytes on both entries.
	body := registrationBody(`"team/app"`, `"release"`)
	got, err := input.ParseRegistration(strings.NewReader(body))
	if err != nil || got.Digest != digestA {
		t.Fatalf("body digest = (%q, %v), want %q", got.Digest, err, digestA)
	}
	query, err := input.ParseQuery("repository=team/app&digest=" + digestA)
	if err != nil || query.Digest != digestA || query.Kind != service.QueryByDigest {
		t.Fatalf("query digest = (%+v, %v), want a digest query for %q", query, err, digestA)
	}

	// Edge whitespace, a wrong prefix, uppercase hex and a short hex part
	// reject identically on both entries — the digest is never trimmed.
	rejected := map[string]struct {
		jsonValue  string
		queryValue string
	}{
		"leading space":  {`" ` + digestA + `"`, "+" + digestA},
		"trailing space": {`"` + digestA + ` "`, digestA + "+"},
		"wrong prefix":   {`"sha512:` + strings.Repeat("a", 64) + `"`, "sha512:" + strings.Repeat("a", 64)},
		"uppercase hex":  {`"sha256:` + strings.Repeat("A", 64) + `"`, "sha256:" + strings.Repeat("A", 64)},
		"too short":      {`"sha256:` + strings.Repeat("a", 63) + `"`, "sha256:" + strings.Repeat("a", 63)},
	}
	for name, probe := range rejected {
		t.Run(name, func(t *testing.T) {
			badBody := `{"repository":"team/app","digest":` + probe.jsonValue +
				`,"tag":"release","signature_verified":false,"retention_days":1,"size_bytes":0}`
			got, err := input.ParseRegistration(strings.NewReader(badBody))
			if !errors.Is(err, input.ErrInvalidInput) || got != (service.RegisterInput{}) {
				t.Fatalf("body digest = (%+v, %v), want zero + ErrInvalidInput", got, err)
			}
			query, err := input.ParseQuery("repository=team/app&digest=" + probe.queryValue)
			if !errors.Is(err, input.ErrInvalidInput) || query != (service.Query{}) {
				t.Fatalf("query digest = (%+v, %v), want zero + ErrInvalidInput", query, err)
			}
		})
	}
}
