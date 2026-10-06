package input

import (
	"errors"
	"testing"

	"github.com/Bob-xisuke/orb-registry-catalog/internal/service"
)

const repo = "registry-demo/direct"

func TestParseQueryModes(t *testing.T) {
	cases := map[string]struct {
		raw  string
		want service.Query
	}{
		"repository list": {
			"repository=" + repo,
			service.Query{Kind: service.QueryByRepository, Repository: repo},
		},
		"tag selector": {
			"repository=" + repo + "&tag=release",
			service.Query{Kind: service.QueryByTag, Repository: repo, Tag: "release"},
		},
		"digest selector": {
			"repository=" + repo + "&digest=" + digestA,
			service.Query{Kind: service.QueryByDigest, Repository: repo, Digest: digestA},
		},
		"repository and tag trimmed": {
			"repository=++" + repo + "++&tag=+release+",
			service.Query{Kind: service.QueryByTag, Repository: repo, Tag: "release"},
		},
		"plus decodes to a space in the tag": {
			"repository=" + repo + "&tag=a+b",
			service.Query{Kind: service.QueryByTag, Repository: repo, Tag: "a b"},
		},
		"percent-2B decodes to a literal plus": {
			"repository=" + repo + "&tag=a%2Bb",
			service.Query{Kind: service.QueryByTag, Repository: repo, Tag: "a+b"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := ParseQuery(tc.raw)
			if err != nil {
				t.Fatalf("ParseQuery(%q): %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("ParseQuery(%q) = %+v, want %+v", tc.raw, got, tc.want)
			}
		})
	}
}

// TestParseQueryFirstValueWins pins the repeated-parameter rule at the
// direct boundary: later values of a known name never participate, so a
// legal first value keeps targeting and an illegal first value stays
// invalid regardless of what follows.
func TestParseQueryFirstValueWins(t *testing.T) {
	hits := map[string]struct {
		raw  string
		want service.Query
	}{
		"tag first wins": {
			"repository=" + repo + "&tag=alpha&tag=beta",
			service.Query{Kind: service.QueryByTag, Repository: repo, Tag: "alpha"}},
		"digest first wins": {
			"repository=" + repo + "&digest=" + digestA + "&digest=" + digestB,
			service.Query{Kind: service.QueryByDigest, Repository: repo, Digest: digestA}},
		"repository first wins": {
			"repository=" + repo + "&repository=other&tag=alpha",
			service.Query{Kind: service.QueryByTag, Repository: repo, Tag: "alpha"}},
		"legal first survives later illegal tag": {
			"repository=" + repo + "&tag=release&tag=",
			service.Query{Kind: service.QueryByTag, Repository: repo, Tag: "release"}},
		"legal first survives later bad digest": {
			"repository=" + repo + "&digest=" + digestA + "&digest=sha256:xyz",
			service.Query{Kind: service.QueryByDigest, Repository: repo, Digest: digestA}},
	}
	for name, tc := range hits {
		t.Run(name, func(t *testing.T) {
			got, err := ParseQuery(tc.raw)
			if err != nil {
				t.Fatalf("ParseQuery(%q): %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("ParseQuery(%q) = %+v, want %+v", tc.raw, got, tc.want)
			}
		})
	}

	invalid := []string{
		"repository=&repository=" + repo + "&tag=release",
		"repository=%20%20&repository=" + repo + "&tag=release",
		"repository&repository=" + repo + "&tag=release",
		"repository=" + repo + "&tag=&tag=release",
		"repository=" + repo + "&tag=%20%20&tag=release",
		"repository=" + repo + "&tag&tag=release",
		"repository=" + repo + "&digest=sha256:xyz&digest=" + digestA,
		"repository=" + repo + "&digest&digest=" + digestA,
		"repository=" + repo + "&digest=+" + digestA + "&digest=" + digestA,
	}
	for _, raw := range invalid {
		t.Run("invalid/"+raw, func(t *testing.T) {
			got, err := ParseQuery(raw)
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("error = %v, want ErrInvalidInput", err)
			}
			if got != (service.Query{}) {
				t.Fatalf("result = %+v, want the zero value", got)
			}
		})
	}
}

// TestParseQueryCaseSensitiveAndUnknownIgnored pins name matching: it is
// byte-for-byte case sensitive after decoding, and unknown names are
// ignored rather than rejected.
func TestParseQueryCaseSensitiveAndUnknownIgnored(t *testing.T) {
	// A cased tag name is unknown: tag is absent, so this is a repository
	// list rather than a tag query.
	got, err := ParseQuery("repository=" + repo + "&Tag=release")
	if err != nil {
		t.Fatalf("ParseQuery: %v", err)
	}
	if want := (service.Query{Kind: service.QueryByRepository, Repository: repo}); got != want {
		t.Fatalf("Tag= parsed as %+v, want %+v", got, want)
	}

	// A cased repository name cannot satisfy the required repository.
	if got, err := ParseQuery("Repository=" + repo + "&tag=release"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("error = %v, want ErrInvalidInput", err)
	} else if got != (service.Query{}) {
		t.Fatalf("result = %+v, want the zero value", got)
	}

	// Unknown parameters, repeated or empty-valued, are ignored.
	got, err = ParseQuery("repository=" + repo + "&tag=release&unknown=1&unknown=2&frob=")
	if err != nil {
		t.Fatalf("ParseQuery: %v", err)
	}
	if want := (service.Query{Kind: service.QueryByTag, Repository: repo, Tag: "release"}); got != want {
		t.Fatalf("unknown params changed the query: %+v, want %+v", got, want)
	}
}

// TestParseQueryInvalidShapes covers every rejection family named in the
// contract, each returning the sentinel and the zero query.
func TestParseQueryInvalidShapes(t *testing.T) {
	invalid := map[string]string{
		"empty string":              "",
		"missing repository":        "tag=release",
		"repository empty":          "repository=",
		"repository bare":           "repository",
		"repository whitespace":     "repository=%20%20",
		"tag present but empty":     "repository=" + repo + "&tag=",
		"tag bare":                  "repository=" + repo + "&tag",
		"tag whitespace":            "repository=" + repo + "&tag=%20",
		"digest empty":              "repository=" + repo + "&digest=",
		"digest malformed":          "repository=" + repo + "&digest=sha256:xyz",
		"digest leading space":      "repository=" + repo + "&digest=+" + digestA,
		"digest trailing space":     "repository=" + repo + "&digest=" + digestA + "+",
		"tag and digest together":   "repository=" + repo + "&tag=release&digest=" + digestA,
		"both selectors both empty": "repository=" + repo + "&tag=&digest=",
	}
	for name, raw := range invalid {
		t.Run(name, func(t *testing.T) {
			got, err := ParseQuery(raw)
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("error = %v, want ErrInvalidInput", err)
			}
			if got != (service.Query{}) {
				t.Fatalf("result = %+v, want the zero value", got)
			}
		})
	}
}

// TestParseQuerySkippedPairsStillSelectMode proves the two "pair ignored"
// rules: a pair that fails percent-decoding and a segment carrying an
// unescaped semicolon are dropped, while every surviving parameter still
// decides the query mode.
func TestParseQuerySkippedPairsStillSelectMode(t *testing.T) {
	cases := map[string]struct {
		raw  string
		want service.Query
	}{
		// An undecodable tag pair is absent, so the request is a list.
		"undecodable tag drops to list": {
			"repository=" + repo + "&tag=%zz",
			service.Query{Kind: service.QueryByRepository, Repository: repo}},
		// An undecodable digest is absent: the surviving tag selects a tag
		// query, not a both-selectors 400.
		"undecodable digest leaves tag query": {
			"repository=" + repo + "&tag=release&digest=%zz",
			service.Query{Kind: service.QueryByTag, Repository: repo, Tag: "release"}},
		// An unescaped semicolon invalidates only that segment; the rest stand.
		"semicolon segment dropped, tag survives": {
			"repository=" + repo + "&frob=a;b&tag=release",
			service.Query{Kind: service.QueryByTag, Repository: repo, Tag: "release"}},
		// A semicolon inside the tag segment drops that whole pair, leaving a list.
		"semicolon tag drops to list": {
			"repository=" + repo + "&tag=rel;ease",
			service.Query{Kind: service.QueryByRepository, Repository: repo}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := ParseQuery(tc.raw)
			if err != nil {
				t.Fatalf("ParseQuery(%q): %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("ParseQuery(%q) = %+v, want %+v", tc.raw, got, tc.want)
			}
		})
	}
}

// TestParseQueryTakesRawStringWithoutQuestionMark documents that the
// argument is the part after '?': a literal leading '?' becomes part of the
// first parameter name (matching url.ParseQuery), so the required repository
// is then missing. The HTTP entry passes c.Request.URL.RawQuery (no '?').
func TestParseQueryTakesRawStringWithoutQuestionMark(t *testing.T) {
	if got, err := ParseQuery("?repository=" + repo); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("error = %v, want ErrInvalidInput (leading '?' must be stripped by caller)", err)
	} else if got != (service.Query{}) {
		t.Fatalf("result = %+v, want the zero value", got)
	}

	got, err := ParseQuery("repository=" + repo)
	if err != nil {
		t.Fatalf("ParseQuery: %v", err)
	}
	if want := (service.Query{Kind: service.QueryByRepository, Repository: repo}); got != want {
		t.Fatalf("parsed = %+v, want %+v", got, want)
	}
}
