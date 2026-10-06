package input

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Bob-xisuke/orb-registry-catalog/internal/service"
)

const (
	digestA = "sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// failingReader returns its fixed error on the first read so the parser's
// read-failure path can be exercised without any network or HTTP machinery.
type failingReader struct{ err error }

func (f failingReader) Read([]byte) (int, error) { return 0, f.err }

// validBody is the canonical legal registration; individual tests edit it.
func validBody(repo, digest, tag string) string {
	return `{"repository":"` + repo + `","digest":"` + digest + `","tag":"` + tag +
		`","signature_verified":true,"retention_days":30,"size_bytes":1024}`
}

func wantRegisterInput(repo, digest, tag string, sig bool, retention, size int64) service.RegisterInput {
	return service.RegisterInput{
		Repository:        repo,
		Digest:            digest,
		Tag:               tag,
		SignatureVerified: sig,
		RetentionDays:     retention,
		SizeBytes:         size,
	}
}

// withField renders a valid registration with one field replaced by the raw
// JSON value, or omitted entirely when rawValue is empty.
func withField(field, rawValue string) string {
	values := []struct{ key, value string }{
		{"repository", `"team/app"`},
		{"digest", `"` + digestA + `"`},
		{"tag", `"latest"`},
		{"signature_verified", `true`},
		{"retention_days", `30`},
		{"size_bytes", `1`},
	}
	parts := make([]string, 0, len(values))
	for _, pair := range values {
		value := pair.value
		if pair.key == field {
			if rawValue == "" {
				continue
			}
			value = rawValue
		}
		parts = append(parts, `"`+pair.key+`":`+value)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// TestParseRegistrationValidNormalizes exercises the direct entry the same
// way the HTTP handler uses it: one JSON object in, a fully validated
// service.RegisterInput out. Repository and tag are trimmed; false and a
// zero size are legal values, not nulls; unknown fields are ignored.
func TestParseRegistrationValidNormalizes(t *testing.T) {
	body := `{"repository":"  team/app  ","digest":"` + digestA + `","tag":"  latest ",` +
		`"signature_verified":false,"retention_days":1,"size_bytes":0,"extra":"ignored","unknown":null}`
	got, err := ParseRegistration(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ParseRegistration: %v", err)
	}
	if want := wantRegisterInput("team/app", digestA, "latest", false, 1, 0); got != want {
		t.Fatalf("parsed input = %+v, want %+v", got, want)
	}
}

// TestParseRegistrationExactInt64 pins precise 64-bit integer handling at
// the direct boundary: 2^53+1 (not exactly representable as float64) and
// the int64 maximum decode exactly, with retention endpoints 1 and 3650.
func TestParseRegistrationExactInt64(t *testing.T) {
	cases := map[string]struct {
		retention, size int64
	}{
		"beyond float64 exact range": {1, 9007199254740993},
		"maximum int64 size":         {3650, 9223372036854775807},
		"retention endpoints":        {1, 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			body := `{"repository":"r","digest":"` + digestA + `","tag":"t",` +
				`"signature_verified":true,"retention_days":` + itoa(tc.retention) +
				`,"size_bytes":` + itoa(tc.size) + `}`
			got, err := ParseRegistration(strings.NewReader(body))
			if err != nil {
				t.Fatalf("ParseRegistration: %v", err)
			}
			if got.RetentionDays != tc.retention || got.SizeBytes != tc.size {
				t.Fatalf("integers = retention %d size %d, want %d %d",
					got.RetentionDays, got.SizeBytes, tc.retention, tc.size)
			}
		})
	}
}

func itoa(v int64) string {
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

// TestParseRegistrationInvalidIsSentinelAndZero covers every failure family:
// each must be errors.Is-identifiable as ErrInvalidInput and come back with
// the zero RegisterInput so a caller can never mistake a rejected parse for
// partially populated input.
func TestParseRegistrationInvalidIsSentinelAndZero(t *testing.T) {
	cases := map[string]string{
		"empty body":            ``,
		"json array":            `[{"repository":"team/app"}]`,
		"json scalar":           `"text"`,
		"json null":             `null`,
		"malformed json":        `{"repository":`,
		"trailing garbage":      validBody("team/app", digestA, "latest") + ` garbage`,
		"trailing object":       validBody("team/app", digestA, "latest") + ` {}`,
		"blank repository":      withField("repository", `"   "`),
		"blank tag":             withField("tag", `"  "`),
		"repository wrong type": withField("repository", `7`),
		"digest malformed":      withField("digest", `"sha256:xyz"`),
		"digest wrong type":     withField("digest", `7`),
		"digest leading space":  withField("digest", `" `+digestA+`"`),
		"signature string":      withField("signature_verified", `"true"`),
		"retention zero":        withField("retention_days", `0`),
		"retention too large":   withField("retention_days", `3651`),
		"retention fractional":  withField("retention_days", `1.5`),
		"retention exponential": withField("retention_days", `3e1`),
		"retention string":      withField("retention_days", `"30"`),
		"retention overflows":   withField("retention_days", `9223372036854775808`),
		"size negative":         withField("size_bytes", `-1`),
		"size fractional":       withField("size_bytes", `1.5`),
		"size exponential":      withField("size_bytes", `1e3`),
		"size string":           withField("size_bytes", `"5"`),
		"size overflows":        withField("size_bytes", `9223372036854775808`),
		"missing repository":    withField("repository", ""),
		"missing digest":        withField("digest", ""),
		"missing tag":           withField("tag", ""),
		"missing signature":     withField("signature_verified", ""),
		"missing retention":     withField("retention_days", ""),
		"missing size":          withField("size_bytes", ""),
		"null repository":       withField("repository", "null"),
		"null digest":           withField("digest", "null"),
		"null tag":              withField("tag", "null"),
		"null signature":        withField("signature_verified", "null"),
		"null retention":        withField("retention_days", "null"),
		"null size":             withField("size_bytes", "null"),
		"unknown key cannot stand": `{"repo":"team/app","digest":"` + digestA + `","tag":"latest",` +
			`"signature_verified":true,"retention_days":30,"size_bytes":1}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := ParseRegistration(strings.NewReader(body))
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("error = %v, want ErrInvalidInput", err)
			}
			if got != (service.RegisterInput{}) {
				t.Fatalf("result = %+v, want the zero value", got)
			}
		})
	}
}

// TestParseRegistrationReadFailure proves a reader that fails is the same
// sentinel and zero result as a syntactically invalid body.
func TestParseRegistrationReadFailure(t *testing.T) {
	got, err := ParseRegistration(failingReader{err: io.ErrUnexpectedEOF})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("error = %v, want ErrInvalidInput", err)
	}
	if got != (service.RegisterInput{}) {
		t.Fatalf("result = %+v, want the zero value", got)
	}
}

// TestParseRegistrationDuplicateKeysLastValueWins is the direct-boundary
// counterpart of the HTTP duplicate-key regression: earlier occurrences,
// even a decoy identity or an illegal value, are discarded by map decoding;
// the last occurrence alone is validated and normalized.
func TestParseRegistrationDuplicateKeysLastValueWins(t *testing.T) {
	body := `{"repository":"decoy","repository":"team/dup",` +
		`"digest":"` + digestB + `","digest":"` + digestA + `",` +
		`"tag":"decoy-tag","tag":"  release  ",` +
		`"signature_verified":"yes","signature_verified":false,` +
		`"retention_days":null,"retention_days":7,` +
		`"size_bytes":"1024","size_bytes":0}`
	got, err := ParseRegistration(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ParseRegistration: %v", err)
	}
	if want := wantRegisterInput("team/dup", digestA, "release", false, 7, 0); got != want {
		t.Fatalf("parsed input = %+v, want %+v", got, want)
	}
}

// TestParseRegistrationDuplicateKeysLastInvalid proves a legal earlier
// value never rescues an illegal last one; the answer is the same sentinel
// and zero value as a single bad occurrence.
func TestParseRegistrationDuplicateKeysLastInvalid(t *testing.T) {
	base := func(field, lastRaw string) string {
		return `{"repository":"team/dup","digest":"` + digestA + `","tag":"release",` +
			`"signature_verified":true,"retention_days":30,"size_bytes":1,` +
			`"` + field + `":` + lastRaw + `}`
	}
	cases := map[string]string{
		"repository null":      base("repository", `null`),
		"repository blank":     base("repository", `" "`),
		"digest malformed":     base("digest", `"sha256:xyz"`),
		"tag wrong type":       base("tag", `7`),
		"signature null":       base("signature_verified", `null`),
		"retention fractional": base("retention_days", `1.5`),
		"size negative":        base("size_bytes", `-1`),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := ParseRegistration(strings.NewReader(body))
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("error = %v, want ErrInvalidInput", err)
			}
			if got != (service.RegisterInput{}) {
				t.Fatalf("result = %+v, want the zero value", got)
			}
		})
	}
}

// TestParseRegistrationEscapedKeyNames proves key matching uses the decoded
// name: every required key spelled with a unicode escape is still
// recognized, and escaped/plain spellings of the same decoded name collide
// as duplicates whose document order decides the value.
func TestParseRegistrationEscapedKeyNames(t *testing.T) {
	// Every required key is escaped; matching uses the decoded name.
	allEscaped := `{"` + escR + `epository":"team/uni",` +
		`"` + escD + `igest":"` + digestA + `",` +
		`"` + escT + `ag":"release",` +
		`"signature_` + escV + `erified":true,` +
		`"retention_` + escD + `ays":30,` +
		`"size_` + escB + `ytes":1}`
	got, err := ParseRegistration(strings.NewReader(allEscaped))
	if err != nil {
		t.Fatalf("all-escaped keys: %v", err)
	}
	if want := wantRegisterInput("team/uni", digestA, "release", true, 30, 1); got != want {
		t.Fatalf("parsed input = %+v, want %+v", got, want)
	}

	// Escaped spelling first, plain spelling last: the plain value wins.
	plainLast := `{"repository":"r","digest":"` + digestB + `",` +
		`"` + escT + `ag":"escaped","tag":"plain",` +
		`"signature_verified":true,"retention_days":30,"size_bytes":1}`
	if got, err = ParseRegistration(strings.NewReader(plainLast)); err != nil {
		t.Fatalf("plain-last: %v", err)
	} else if got.Tag != "plain" {
		t.Fatalf("tag = %q, want plain", got.Tag)
	}

	// Plain spelling first, escaped spelling last: the escaped value wins.
	escapedLast := `{"repository":"r","digest":"` + digestB + `",` +
		`"tag":"plain","` + escT + `ag":"escaped",` +
		`"signature_verified":true,"retention_days":30,"size_bytes":1}`
	if got, err = ParseRegistration(strings.NewReader(escapedLast)); err != nil {
		t.Fatalf("escaped-last: %v", err)
	} else if got.Tag != "escaped" {
		t.Fatalf("tag = %q, want escaped", got.Tag)
	}

	// An unknown key, however similar, never satisfies a missing required one.
	missing := `{"repo":"team/uni","digest":"` + digestA + `","tag":"release",` +
		`"signature_verified":true,"retention_days":30,"size_bytes":1}`
	if got, err := ParseRegistration(strings.NewReader(missing)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("error = %v, want ErrInvalidInput", err)
	} else if got != (service.RegisterInput{}) {
		t.Fatalf("result = %+v, want the zero value", got)
	}
}
