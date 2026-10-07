package input_test

// These cases pin the compatibility contract of the shared field rules:
// ParseRegistration and ParseQuery normalize repository and tag through one
// and the same rule (decode, then trim leading/trailing Unicode whitespace,
// keep interior characters and case, reject all-whitespace) and match the
// digest through one and the same untrimmed anchored-format rule. A value
// one entry accepts must normalize to exactly what the other entry accepts,
// so a registration is always reachable by the corresponding query.

import (
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/Bob-xisuke/orb-registry-catalog/internal/input"
	"github.com/Bob-xisuke/orb-registry-catalog/internal/service"
)

// unicodePaddings pairs a JSON string-literal fragment ( escapes kept
// literal so the test file stays ASCII) with the percent-encoded query-string
// spelling of the same decoded padding.
var unicodePaddings = map[string]struct {
	jsonPadding  string
	queryPadding string
}{
	"ascii spaces":      {`  `, "%20%20"},
	"tab and newline":   {`\t`, "%09%0A"},
	"unicode nbsp":      {`\u00a0`, "%C2%A0"},
	"unicode em space":  {`\u2003`, "%E2%80%83"},
	"mixed unicode run": {` \u00a0\u2003\t`, "%20%C2%A0%E2%80%83%09"},
}

// TestSharedNameRuleNormalizesIdenticallyAcrossEntries drives the same
// Unicode-whitespace paddings through both entries and pins identical
// normalization: the repository and tag a registration stores are exactly
// the repository and tag a query selects.
func TestSharedNameRuleNormalizesIdenticallyAcrossEntries(t *testing.T) {
	for name, pad := range unicodePaddings {
		t.Run(name, func(t *testing.T) {
			body := `{"repository":"` + pad.jsonPadding + `team/shared` + pad.jsonPadding +
				`","digest":"` + digestA + `","tag":"` + pad.jsonPadding + `release` + pad.jsonPadding +
				`","signature_verified":true,"retention_days":30,"size_bytes":1}`
			reg, err := input.ParseRegistration(strings.NewReader(body))
			if err != nil {
				t.Fatalf("ParseRegistration: %v", err)
			}
			if reg.Repository != "team/shared" || reg.Tag != "release" {
				t.Fatalf("registration not trimmed: %+v", reg)
			}

			query, err := input.ParseQuery("repository=" + pad.queryPadding + "team/shared" +
				pad.queryPadding + "&tag=" + pad.queryPadding + "release" + pad.queryPadding)
			if err != nil {
				t.Fatalf("ParseQuery: %v", err)
			}
			want := service.Query{Kind: service.QueryByTag, Repository: reg.Repository, Tag: reg.Tag}
			if query != want {
				t.Fatalf("query = %+v, want the registered identity %+v", query, want)
			}
		})
	}
}

// TestSharedNameRulePreservesInteriorAndCase pins the other half of the
// shared rule on both entries: only the edges are trimmed — interior
// whitespace, interior plus signs and letter case survive verbatim.
func TestSharedNameRulePreservesInteriorAndCase(t *testing.T) {
	body := `{"repository":" Team/App  Name ","digest":"` + digestA + `","tag":" Rel+A  B ",` +
		`"signature_verified":false,"retention_days":1,"size_bytes":0}`
	reg, err := input.ParseRegistration(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ParseRegistration: %v", err)
	}
	if reg.Repository != "Team/App  Name" || reg.Tag != "Rel+A  B" {
		t.Fatalf("registration lost interior characters or case: %+v", reg)
	}

	query, err := input.ParseQuery("repository=+Team/App++Name+&tag=+Rel%2BA++B+")
	if err != nil {
		t.Fatalf("ParseQuery: %v", err)
	}
	want := service.Query{Kind: service.QueryByTag, Repository: reg.Repository, Tag: reg.Tag}
	if query != want {
		t.Fatalf("query = %+v, want the registered identity %+v", query, want)
	}
}

// TestSharedNameRuleRejectsAllWhitespaceAcrossEntries pins that a value
// holding nothing but Unicode whitespace — ASCII or not — is the same
// rejection on both entries: ErrInvalidInput with a zero result.
func TestSharedNameRuleRejectsAllWhitespaceAcrossEntries(t *testing.T) {
	for name, pad := range unicodePaddings {
		t.Run(name, func(t *testing.T) {
			for _, field := range []string{"repository", "tag"} {
				body := validBody("team/app", digestA, "release")
				replacement := `"` + field + `":"` + pad.jsonPadding + `"`
				if field == "repository" {
					body = strings.Replace(body, `"repository":"team/app"`, replacement, 1)
				} else {
					body = strings.Replace(body, `"tag":"release"`, replacement, 1)
				}
				got, err := input.ParseRegistration(strings.NewReader(body))
				if !errors.Is(err, input.ErrInvalidInput) || got != (service.RegisterInput{}) {
					t.Fatalf("blank %s registration = (%+v, %v), want zero + ErrInvalidInput", field, got, err)
				}
			}

			for _, raw := range []string{
				"repository=" + pad.queryPadding + "&tag=release",
				"repository=team/app&tag=" + pad.queryPadding,
			} {
				got, err := input.ParseQuery(raw)
				if !errors.Is(err, input.ErrInvalidInput) || got != (service.Query{}) {
					t.Fatalf("ParseQuery(%q) = (%+v, %v), want zero + ErrInvalidInput", raw, got, err)
				}
			}
		})
	}
}

// TestSharedDigestRuleUntrimmedAcrossEntries pins the shared digest rule:
// both entries match the decoded value untrimmed against the anchored
// format, so an edge whitespace is a rejection on both sides while the clean
// digest passes both with the value kept verbatim.
func TestSharedDigestRuleUntrimmedAcrossEntries(t *testing.T) {
	reg, err := input.ParseRegistration(strings.NewReader(validBody("team/app", digestA, "release")))
	if err != nil {
		t.Fatalf("clean registration: %v", err)
	}
	query, err := input.ParseQuery("repository=team/app&digest=" + digestA)
	if err != nil {
		t.Fatalf("clean query: %v", err)
	}
	if reg.Digest != digestA || query.Digest != digestA {
		t.Fatalf("clean digest not kept verbatim: %q / %q", reg.Digest, query.Digest)
	}

	// Edge whitespace — ASCII or Unicode, encoded either way — rejects both.
	paddings := map[string]struct {
		jsonPadding  string
		queryPadding string
	}{
		"ascii space":  {` `, "%20"},
		"query plus":   {"", "+"},
		"unicode nbsp": {`\u00a0`, "%C2%A0"},
	}
	for name, pad := range paddings {
		t.Run(name, func(t *testing.T) {
			if pad.jsonPadding != "" {
				body := strings.Replace(validBody("team/app", digestA, "release"),
					`"digest":"`+digestA+`"`, `"digest":"`+pad.jsonPadding+digestA+`"`, 1)
				if got, err := input.ParseRegistration(strings.NewReader(body)); !errors.Is(err, input.ErrInvalidInput) ||
					got != (service.RegisterInput{}) {
					t.Fatalf("padded digest registration = (%+v, %v), want zero + ErrInvalidInput", got, err)
				}
			}
			for _, raw := range []string{
				"repository=team/app&digest=" + pad.queryPadding + digestA,
				"repository=team/app&digest=" + digestA + pad.queryPadding,
			} {
				if got, err := input.ParseQuery(raw); !errors.Is(err, input.ErrInvalidInput) ||
					got != (service.Query{}) {
					t.Fatalf("ParseQuery(%q) = (%+v, %v), want zero + ErrInvalidInput", raw, got, err)
				}
			}
		})
	}

	// The format neighbors reject identically on both entries: wrong prefix,
	// uppercase hex, wrong length.
	for _, digest := range []string{
		"sha512:" + strings.Repeat("a", 64),
		"sha256:" + strings.Repeat("A", 64),
		"sha256:" + strings.Repeat("a", 63),
	} {
		body := strings.Replace(validBody("team/app", digestA, "release"),
			`"digest":"`+digestA+`"`, `"digest":"`+digest+`"`, 1)
		if _, err := input.ParseRegistration(strings.NewReader(body)); !errors.Is(err, input.ErrInvalidInput) {
			t.Fatalf("registration digest %q error = %v, want ErrInvalidInput", digest, err)
		}
		if _, err := input.ParseQuery("repository=team/app&digest=" + url.QueryEscape(digest)); !errors.Is(err, input.ErrInvalidInput) {
			t.Fatalf("query digest %q error = %v, want ErrInvalidInput", digest, err)
		}
	}
}
