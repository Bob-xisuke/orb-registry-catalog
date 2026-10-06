package input_test

// These cases call the two shared entries directly, without any HTTP
// machinery: they pin what a non-HTTP caller of ParseRegistration and
// ParseQuery receives — an exact service.RegisterInput / service.Query on
// success, or ErrInvalidInput together with a zero result. The HTTP-side
// mirror lives in internal/api.

import (
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/Bob-xisuke/orb-registry-catalog/internal/input"
	"github.com/Bob-xisuke/orb-registry-catalog/internal/service"
)

const (
	digestA = "sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// validBody is a registration every field of which is legal.
func validBody(repo, digest, tag string) string {
	return `{"repository":"` + repo + `","digest":"` + digest + `","tag":"` + tag +
		`","signature_verified":true,"retention_days":30,"size_bytes":1024}`
}

// failingReader returns err on every Read, optionally delivering prefix
// bytes first so both immediate and mid-body failures can be exercised.
type failingReader struct {
	prefix []byte
	err    error
}

func (f *failingReader) Read(p []byte) (int, error) {
	if len(f.prefix) > 0 {
		n := copy(p, f.prefix)
		f.prefix = f.prefix[n:]
		return n, nil
	}
	return 0, f.err
}

// TestParseRegistrationSuccessExactFields proves the success path returns a
// fully populated service.RegisterInput: trim normalization for repository
// and tag, the digest kept verbatim, and the two integers kept as exact
// int64 values. Unknown fields never enter the result.
func TestParseRegistrationSuccessExactFields(t *testing.T) {
	body := `{"repository":"  team/app  ","digest":"` + digestA + `","tag":"  latest  ",` +
		`"signature_verified":false,"retention_days":1,"size_bytes":0,"extra":"ignored"}`
	got, err := input.ParseRegistration(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ParseRegistration: %v", err)
	}
	want := service.RegisterInput{
		Repository:        "team/app",
		Digest:            digestA,
		Tag:               "latest",
		SignatureVerified: false,
		RetentionDays:     1,
		SizeBytes:         0,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RegisterInput = %+v, want %+v", got, want)
	}
}

// TestParseRegistrationExactInt64 pins the precise integer representation:
// 2^53+1 (not representable as float64) and max int64 survive exactly,
// retention accepts its 3650 maximum, and the fractional/exponential/string
// neighbors are all refused.
func TestParseRegistrationExactInt64(t *testing.T) {
	cases := map[string]struct {
		retention, size string
		wantRetention   int64
		wantSize        int64
	}{
		"size 2^53+1":    {"1", "9007199254740993", 1, 9007199254740993},
		"size max int64": {"3650", "9223372036854775807", 3650, 9223372036854775807},
		"retention 3650": {"3650", "1", 3650, 1},
		"false and zero": {"1", "0", 1, 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			body := `{"repository":"team/exact","digest":"` + digestA + `","tag":"t",` +
				`"signature_verified":true,"retention_days":` + tc.retention +
				`,"size_bytes":` + tc.size + `}`
			got, err := input.ParseRegistration(strings.NewReader(body))
			if err != nil {
				t.Fatalf("ParseRegistration: %v", err)
			}
			if got.RetentionDays != tc.wantRetention || got.SizeBytes != tc.wantSize {
				t.Fatalf("integers = (%d, %d), want (%d, %d)",
					got.RetentionDays, got.SizeBytes, tc.wantRetention, tc.wantSize)
			}
		})
	}

	rejected := map[string]string{
		"size overflows int64": "9223372036854775808",
		"size string digits":   `"1024"`,
		"size fractional":      "1024.0",
		"size exponential":     "1e3",
		"size boolean":         "true",
	}
	for name, raw := range rejected {
		t.Run(name, func(t *testing.T) {
			body := `{"repository":"team/exact","digest":"` + digestA + `","tag":"t",` +
				`"signature_verified":true,"retention_days":30,"size_bytes":` + raw + `}`
			if _, err := input.ParseRegistration(strings.NewReader(body)); !errors.Is(err, input.ErrInvalidInput) {
				t.Fatalf("size %s error = %v, want ErrInvalidInput", raw, err)
			}
		})
	}
	for name, raw := range map[string]string{
		"retention overflows":   "9223372036854775808",
		"retention fractional":  "30.0",
		"retention exponential": "3e1",
		"retention string":      `"30"`,
	} {
		t.Run(name, func(t *testing.T) {
			body := `{"repository":"team/exact","digest":"` + digestA + `","tag":"t",` +
				`"signature_verified":true,"retention_days":` + raw + `,"size_bytes":1}`
			if _, err := input.ParseRegistration(strings.NewReader(body)); !errors.Is(err, input.ErrInvalidInput) {
				t.Fatalf("retention %s error = %v, want ErrInvalidInput", raw, err)
			}
		})
	}
}

// TestParseRegistrationInvalidReturnsZeroAndSentinel enumerates every
// rejection class and pins both halves of the contract: errors.Is recognizes
// ErrInvalidInput and the returned input is the zero value.
func TestParseRegistrationInvalidReturnsZeroAndSentinel(t *testing.T) {
	cases := map[string]string{
		"empty body":            ``,
		"json array":            `[{"repository":"team/app"}]`,
		"json scalar":           `"text"`,
		"json number":           `42`,
		"json null":             `null`,
		"trailing object":       validBody("team/app", digestA, "latest") + ` {}`,
		"trailing token":        validBody("team/app", digestA, "latest") + ` garbage`,
		"malformed json":        `{"repository":`,
		"double comma":          `{"repository":"team/app",,"digest":"` + digestA + `"}`,
		"missing repository":    `{"digest":"` + digestA + `","tag":"t","signature_verified":true,"retention_days":30,"size_bytes":1}`,
		"missing digest":        `{"repository":"team/app","tag":"t","signature_verified":true,"retention_days":30,"size_bytes":1}`,
		"missing tag":           `{"repository":"team/app","digest":"` + digestA + `","signature_verified":true,"retention_days":30,"size_bytes":1}`,
		"missing signature":     `{"repository":"team/app","digest":"` + digestA + `","tag":"t","retention_days":30,"size_bytes":1}`,
		"missing retention":     `{"repository":"team/app","digest":"` + digestA + `","tag":"t","signature_verified":true,"size_bytes":1}`,
		"missing size":          `{"repository":"team/app","digest":"` + digestA + `","tag":"t","signature_verified":true,"retention_days":30}`,
		"null repository":       `{"repository":null,"digest":"` + digestA + `","tag":"t","signature_verified":true,"retention_days":30,"size_bytes":1}`,
		"null tag":              `{"repository":"team/app","digest":"` + digestA + `","tag":null,"signature_verified":true,"retention_days":30,"size_bytes":1}`,
		"null signature":        `{"repository":"team/app","digest":"` + digestA + `","tag":"t","signature_verified":null,"retention_days":30,"size_bytes":1}`,
		"null retention":        `{"repository":"team/app","digest":"` + digestA + `","tag":"t","signature_verified":true,"retention_days":null,"size_bytes":1}`,
		"null size":             `{"repository":"team/app","digest":"` + digestA + `","tag":"t","signature_verified":true,"retention_days":30,"size_bytes":null}`,
		"blank repository":      `{"repository":"   ","digest":"` + digestA + `","tag":"t","signature_verified":true,"retention_days":30,"size_bytes":1}`,
		"blank tag":             `{"repository":"team/app","digest":"` + digestA + `","tag":"  ","signature_verified":true,"retention_days":30,"size_bytes":1}`,
		"repository wrong type": `{"repository":7,"digest":"` + digestA + `","tag":"t","signature_verified":true,"retention_days":30,"size_bytes":1}`,
		"tag wrong type":        `{"repository":"team/app","digest":"` + digestA + `","tag":7,"signature_verified":true,"retention_days":30,"size_bytes":1}`,
		"digest wrong type":     `{"repository":"team/app","digest":7,"tag":"t","signature_verified":true,"retention_days":30,"size_bytes":1}`,
		"digest wrong prefix":   `{"repository":"team/app","digest":"sha512:` + strings.Repeat("a", 64) + `","tag":"t","signature_verified":true,"retention_days":30,"size_bytes":1}`,
		"digest uppercase hex":  `{"repository":"team/app","digest":"sha256:` + strings.Repeat("A", 64) + `","tag":"t","signature_verified":true,"retention_days":30,"size_bytes":1}`,
		"digest too short":      `{"repository":"team/app","digest":"sha256:` + strings.Repeat("a", 63) + `","tag":"t","signature_verified":true,"retention_days":30,"size_bytes":1}`,
		"digest leading space":  `{"repository":"team/app","digest":" ` + digestA + `","tag":"t","signature_verified":true,"retention_days":30,"size_bytes":1}`,
		"signature string":      `{"repository":"team/app","digest":"` + digestA + `","tag":"t","signature_verified":"true","retention_days":30,"size_bytes":1}`,
		"retention zero":        `{"repository":"team/app","digest":"` + digestA + `","tag":"t","signature_verified":true,"retention_days":0,"size_bytes":1}`,
		"retention too large":   `{"repository":"team/app","digest":"` + digestA + `","tag":"t","signature_verified":true,"retention_days":3651,"size_bytes":1}`,
		"size negative":         `{"repository":"team/app","digest":"` + digestA + `","tag":"t","signature_verified":true,"retention_days":30,"size_bytes":-1}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := input.ParseRegistration(strings.NewReader(body))
			if !errors.Is(err, input.ErrInvalidInput) {
				t.Fatalf("error = %v, want ErrInvalidInput", err)
			}
			if got != (service.RegisterInput{}) {
				t.Fatalf("result = %+v, want zero RegisterInput", got)
			}
		})
	}
}

// TestParseRegistrationReadFailure proves a reader failure is the same
// published failure as a syntax error: ErrInvalidInput plus a zero result,
// whether the failure arrives before any byte or mid-body.
func TestParseRegistrationReadFailure(t *testing.T) {
	boom := errors.New("read boom")
	for name, reader := range map[string]io.Reader{
		"immediate": &failingReader{err: boom},
		"mid body":  &failingReader{prefix: []byte(`{"repository":"team/app"`), err: boom},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := input.ParseRegistration(reader)
			if !errors.Is(err, input.ErrInvalidInput) {
				t.Fatalf("error = %v, want ErrInvalidInput", err)
			}
			if got != (service.RegisterInput{}) {
				t.Fatalf("result = %+v, want zero RegisterInput", got)
			}
		})
	}

	// A nil reader is the same failure class.
	if got, err := input.ParseRegistration(nil); !errors.Is(err, input.ErrInvalidInput) || got != (service.RegisterInput{}) {
		t.Fatalf("nil reader = (%+v, %v), want zero result and ErrInvalidInput", got, err)
	}
}

// TestParseRegistrationDuplicateKeysLastValueWins proves the last
// occurrence of every key is the only one decoded: a legal last value wins
// over a decoy or illegal earlier one, while an illegal last value fails
// even though every earlier value was legal.
func TestParseRegistrationDuplicateKeysLastValueWins(t *testing.T) {
	body := `{"repository":"team/decoy","repository":"team/dup",` +
		`"digest":"` + digestB + `","digest":"` + digestA + `",` +
		`"tag":"decoy","tag":"release",` +
		`"signature_verified":"yes","signature_verified":true,` +
		`"retention_days":null,"retention_days":30,` +
		`"size_bytes":"1024","size_bytes":9007199254740993}`
	got, err := input.ParseRegistration(strings.NewReader(body))
	if err != nil {
		t.Fatalf("legal-last ParseRegistration: %v", err)
	}
	want := service.RegisterInput{
		Repository: "team/dup", Digest: digestA, Tag: "release",
		SignatureVerified: true, RetentionDays: 30, SizeBytes: 9007199254740993,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("last-wins result = %+v, want %+v", got, want)
	}

	// Normalization applies to the last value: an earlier untrimmed value
	// loses to a trimmed last one.
	trimmed := `{"repository":"team/decoy","repository":"  team/trim  ","digest":"` + digestA +
		`","tag":"first","tag":"  release  ","signature_verified":false,"retention_days":1,"size_bytes":0}`
	got, err = input.ParseRegistration(strings.NewReader(trimmed))
	if err != nil {
		t.Fatalf("trim-last ParseRegistration: %v", err)
	}
	if got.Repository != "team/trim" || got.Tag != "release" {
		t.Fatalf("last values not trimmed: %+v", got)
	}

	// Illegal last values: the earlier legal occurrence cannot rescue them.
	for name, tail := range map[string]string{
		"repository null":  `{"repository":"team/app","repository":null,"digest":"` + digestA + `","tag":"t","signature_verified":true,"retention_days":30,"size_bytes":1}`,
		"tag blank":        `{"repository":"team/app","digest":"` + digestA + `","tag":"ok","tag":"  ","signature_verified":true,"retention_days":30,"size_bytes":1}`,
		"signature string": `{"repository":"team/app","digest":"` + digestA + `","tag":"t","signature_verified":true,"signature_verified":"yes","retention_days":30,"size_bytes":1}`,
		"retention null":   `{"repository":"team/app","digest":"` + digestA + `","tag":"t","signature_verified":true,"retention_days":30,"retention_days":null,"size_bytes":1}`,
		"size fractional":  `{"repository":"team/app","digest":"` + digestA + `","tag":"t","signature_verified":true,"retention_days":30,"size_bytes":1,"size_bytes":1.5}`,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := input.ParseRegistration(strings.NewReader(tail))
			if !errors.Is(err, input.ErrInvalidInput) || got != (service.RegisterInput{}) {
				t.Fatalf("illegal last = (%+v, %v), want zero result and ErrInvalidInput", got, err)
			}
		})
	}
}

// TestParseRegistrationEscapedKeyNames proves key matching runs on the
// decoded name, and plain/escaped spellings of the same name collide with
// document order — the same last-wins rule as ordinary duplicates.
func TestParseRegistrationEscapedKeyNames(t *testing.T) {
	body := `{` +
		`"\u0072epository":"team/uni",` +
		`"\u0064igest":"` + digestA + `",` +
		`"\u0074ag":"release",` +
		`"signature_\u0076erified":true,` +
		`"retention_\u0064ays":30,` +
		`"size_\u0062ytes":1024}`
	if _, err := input.ParseRegistration(strings.NewReader(body)); err != nil {
		t.Fatalf("escaped keys rejected: %v", err)
	}

	// A literal backslash-u escape, built so the test file itself stays
	// ASCII: decoded, "\u0074ag" is the same key as "tag".
	literalEsc := "{\"repository\":\"team/uni2\",\"digest\":\"" + digestB + "\"," +
		"\"tag\":\"plain\",\"\\u0074ag\":\"escaped\",\"signature_verified\":true,\"retention_days\":30,\"size_bytes\":1}"
	got, err := input.ParseRegistration(strings.NewReader(literalEsc))
	if err != nil {
		t.Fatalf("literal \\u escape: %v", err)
	}
	if got.Tag != "escaped" {
		t.Fatalf("escaped spelling last: tag = %q, want escaped", got.Tag)
	}

	// A merely similar unknown key never stands in for the required one.
	missing := `{"repo":"team/uni3","digest":"` + digestA + `","tag":"t",` +
		`"signature_verified":true,"retention_days":30,"size_bytes":1}`
	if _, err := input.ParseRegistration(strings.NewReader(missing)); !errors.Is(err, input.ErrInvalidInput) {
		t.Fatalf("unknown key satisfied a required one: %v", err)
	}
}

// TestParseQueryModesExactQueries pins the three dispatched shapes and the
// normalization boundary: repository/tag trimmed after decoding, the digest
// kept verbatim.
func TestParseQueryModesExactQueries(t *testing.T) {
	cases := map[string]struct {
		raw  string
		want service.Query
	}{
		"repository list": {
			"repository=++team/app++",
			service.Query{Kind: service.QueryByRepository, Repository: "team/app"},
		},
		"by tag trimmed": {
			"repository=team/app&tag=+release+",
			service.Query{Kind: service.QueryByTag, Repository: "team/app", Tag: "release"},
		},
		"by tag with literal plus": {
			"repository=team/app&tag=a%2Bb",
			service.Query{Kind: service.QueryByTag, Repository: "team/app", Tag: "a+b"},
		},
		"by tag with decoded space": {
			"repository=team/app&tag=a+b",
			service.Query{Kind: service.QueryByTag, Repository: "team/app", Tag: "a b"},
		},
		"by digest untrimmed": {
			"repository=team/app&digest=" + digestA,
			service.Query{Kind: service.QueryByDigest, Repository: "team/app", Digest: digestA},
		},
		"unknown parameters ignored": {
			"repository=team/app&unknown=1&frob=&tag=release",
			service.Query{Kind: service.QueryByTag, Repository: "team/app", Tag: "release"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := input.ParseQuery(tc.raw)
			if err != nil {
				t.Fatalf("ParseQuery(%q): %v", tc.raw, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ParseQuery(%q) = %+v, want %+v", tc.raw, got, tc.want)
			}
		})
	}
}

// TestParseQueryFirstValueWins proves later occurrences of any recognized
// parameter never participate: neither retargeting a legal first value nor
// rescuing an illegal one.
func TestParseQueryFirstValueWins(t *testing.T) {
	want := service.Query{Kind: service.QueryByTag, Repository: "team/app", Tag: "alpha"}
	for _, raw := range []string{
		"repository=team/app&tag=alpha&tag=beta",
		"repository=team/app&tag=alpha&tag=",
		"repository=team/app&tag=alpha&tag=sha256:xyz",
		"repository=team/app&repository=other/repo&tag=alpha",
	} {
		got, err := input.ParseQuery(raw)
		if err != nil {
			t.Fatalf("ParseQuery(%q): %v", raw, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("ParseQuery(%q) = %+v, want %+v", raw, got, want)
		}
	}

	// An illegal first value is rejected even when every later value is legal.
	for _, raw := range []string{
		"repository=&repository=team/app&tag=release",
		"repository=%20%20&repository=team/app&tag=release",
		"repository&repository=team/app&tag=release",
		"repository=team/app&tag=&tag=release",
		"repository=team/app&tag=%20%20&tag=release",
		"repository=team/app&tag&tag=release",
		"repository=team/app&digest=sha256:xyz&digest=" + digestA,
		"repository=team/app&digest&digest=" + digestA,
		"repository=team/app&digest=+" + digestA + "&digest=" + digestA,
	} {
		if _, err := input.ParseQuery(raw); !errors.Is(err, input.ErrInvalidInput) {
			t.Fatalf("ParseQuery(%q) error = %v, want ErrInvalidInput", raw, err)
		}
	}
}

// TestParseQueryNamesDecodedCaseSensitive proves names match after
// percent-decoding but stay case sensitive, and that the raw string is used
// verbatim — a leading '?' is not stripped.
func TestParseQueryNamesDecodedCaseSensitive(t *testing.T) {
	// Decoded-name collisions obey the same first-value ordering.
	got, err := input.ParseQuery("repository=team/app&%74ag=release")
	if err != nil {
		t.Fatalf("escaped tag name: %v", err)
	}
	if got.Kind != service.QueryByTag || got.Tag != "release" {
		t.Fatalf("%%74ag did not decode to tag: %+v", got)
	}
	if _, err := input.ParseQuery("repository=team/app&%64igest=" + digestB); err != nil {
		t.Fatalf("escaped digest name: %v", err)
	}

	// A cased spelling is unknown: absent both selectors, the request is a
	// legal repository list, not a tag query.
	list, err := input.ParseQuery("repository=team/app&Tag=release&TAG=x")
	if err != nil {
		t.Fatalf("cased names: %v", err)
	}
	if list.Kind != service.QueryByRepository {
		t.Fatalf("Tag= was treated as the tag query: %+v", list)
	}

	// A cased repository spelling cannot satisfy the required name.
	if _, err := input.ParseQuery("Repository=team/app&tag=release"); !errors.Is(err, input.ErrInvalidInput) {
		t.Fatalf("Repository= error = %v, want ErrInvalidInput", err)
	}

	// The contract takes the string without its '?'; passing one makes the
	// leading '?' part of the first key name, so repository is missing.
	if _, err := input.ParseQuery("?repository=team/app"); !errors.Is(err, input.ErrInvalidInput) {
		t.Fatalf("leading '?' error = %v, want ErrInvalidInput", err)
	}
}

// TestParseQueryInvalidReturnsZeroAndSentinel enumerates the invalid query
// shapes and pins the zero result alongside the sentinel, including the
// both-selectors-present rule in either order.
func TestParseQueryInvalidReturnsZeroAndSentinel(t *testing.T) {
	for _, raw := range []string{
		"",
		"unknown=1",
		"repository=",
		"repository=%20%20",
		"repository&tag=release",
		"tag=release",
		"repository=team/app&tag=",
		"repository=team/app&tag",
		"repository=team/app&tag=%20%20",
		"repository=team/app&digest=",
		"repository=team/app&digest",
		"repository=team/app&digest=sha256:xyz",
		"repository=team/app&digest=" + digestA + "+",
		"repository=team/app&digest=%20" + digestA,
		"repository=team/app&tag=release&digest=" + digestA,
		"repository=team/app&digest=" + digestA + "&tag=release",
		"repository=team/app&tag=&digest=",
	} {
		got, err := input.ParseQuery(raw)
		if !errors.Is(err, input.ErrInvalidInput) {
			t.Fatalf("ParseQuery(%q) error = %v, want ErrInvalidInput", raw, err)
		}
		if got != (service.Query{}) {
			t.Fatalf("ParseQuery(%q) = %+v, want zero Query", raw, got)
		}
	}
}

// TestParseQuerySkippedPairsStillSelectMode proves the two "pair ignored"
// cases — an undecodable escape and an unescaped semicolon segment — leave
// the remaining pairs in control of the mode, exactly as url.ParseQuery
// orders them. A skipped recognized parameter is simply absent.
func TestParseQuerySkippedPairsStillSelectMode(t *testing.T) {
	// Undecodable pairs vanish: the bad tag is absent, so this is a list.
	got, err := input.ParseQuery("repository=team/app&tag=%zz")
	if err != nil {
		t.Fatalf("bad-escape pair: %v", err)
	}
	if got.Kind != service.QueryByRepository || got.Repository != "team/app" {
		t.Fatalf("bad-escape pair not skipped: %+v", got)
	}

	// A decodable pair after the bad one still counts; first-value ordering
	// holds across the gap.
	first := "repository=team/app&tag=%zz&tag=release"
	if got, err := input.ParseQuery(first); err != nil || got.Kind != service.QueryByTag || got.Tag != "release" {
		t.Fatalf("ParseQuery(%q) = (%+v, %v), want a tag query for release", first, got, err)
	}

	// An unescaped ';' invalidates only the segment carrying it: the tag
	// segment is dropped wholesale, leaving repository alone as a legal list.
	tagSegment := "repository=team/app&tag=release;evil=1"
	got, err = input.ParseQuery(tagSegment)
	if err != nil {
		t.Fatalf("ParseQuery(%q): %v", tagSegment, err)
	}
	if got.Kind != service.QueryByRepository || got.Repository != "team/app" {
		t.Fatalf("semicolon tag segment not skipped: %+v", got)
	}

	// A semicolon in the repository segment drops the repository itself, so
	// the same input is now the missing-repository failure.
	if _, err := input.ParseQuery("repository=team/app;a=b&tag=release"); !errors.Is(err, input.ErrInvalidInput) {
		t.Fatalf("semicolon repository segment error = %v, want ErrInvalidInput", err)
	}

	// The selector segment carrying the ';' is dropped while a clean selector
	// in a later segment survives and picks the mode by itself.
	tagSurvives, err := input.ParseQuery("repository=team/app&digest=" + digestA + ";x=1&tag=release")
	if err != nil {
		t.Fatalf("semicolon digest segment: %v", err)
	}
	if tagSurvives.Kind != service.QueryByTag || tagSurvives.Tag != "release" {
		t.Fatalf("valid pair after semicolon segment lost: %+v", tagSurvives)
	}

	// A properly percent-encoded semicolon is an ordinary value character:
	// the segment is not skipped.
	encoded, err := input.ParseQuery("repository=team/app&tag=a%3Bb")
	if err != nil {
		t.Fatalf("encoded semicolon: %v", err)
	}
	if encoded.Tag != "a;b" {
		t.Fatalf("%%3B decoded tag = %q, want a;b", encoded.Tag)
	}
}

// TestParseQueryPlusAndPercent2BReachDifferentTags pins the decode rule the
// tag side normalization is built on.
func TestParseQueryPlusAndPercent2BReachDifferentTags(t *testing.T) {
	space, err := input.ParseQuery("repository=team/app&tag=a+b")
	if err != nil {
		t.Fatalf("plus: %v", err)
	}
	space2, err := input.ParseQuery("repository=team/app&tag=a%20b")
	if err != nil {
		t.Fatalf("percent-20: %v", err)
	}
	plus, err := input.ParseQuery("repository=team/app&tag=a%2Bb")
	if err != nil {
		t.Fatalf("percent-2B: %v", err)
	}
	if space.Tag != "a b" || space2.Tag != "a b" {
		t.Fatalf("'+' / %%20 decoded as %q / %q, want a b both", space.Tag, space2.Tag)
	}
	if plus.Tag != "a+b" {
		t.Fatalf("%%2B decoded as %q, want a+b", plus.Tag)
	}
	if reflect.DeepEqual(space, plus) {
		t.Fatalf("space and plus queries collapsed to the same value: %+v", plus)
	}
}
