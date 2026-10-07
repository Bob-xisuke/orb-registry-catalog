// Package input is the transport-independent boundary for the two artifact
// inputs the service accepts: the JSON body of POST /v1/artifacts and the
// percent-encoded query string of GET /v1/artifacts.
//
// Neither entry depends on an HTTP framework, accesses storage, or generates
// the push time: the service still owns the accepted inputs, the push time,
// transactions and the storage port. Direct callers and the HTTP handlers
// therefore share one rule set — the rules the published HTTP contract pins
// (case sensitivity, exact int64 decoding, duplicate-key and
// duplicate-parameter ordering, '+' versus '%2B', unescaped semicolon and
// undecodable pair handling) live here and nowhere else.
//
// Every deviation is reported as ErrInvalidInput with a zero result; callers
// tell the failure apart with errors.Is.
package input

import (
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"regexp"
	"strings"

	"github.com/Bob-xisuke/orb-registry-catalog/internal/service"
)

const (
	minRetentionDays int64 = 1
	maxRetentionDays int64 = 3650
)

var (
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

	// ErrInvalidInput is the single failure either entry returns. It carries
	// no internal detail, so a transport adapter may surface it to its own
	// clients; callers identify it with errors.Is.
	ErrInvalidInput = errors.New("invalid artifact input")
)

// trimmedName is the one repository/tag rule both entries share: the decoded
// value loses its leading and trailing Unicode whitespace and must stay
// non-empty; interior characters and case are preserved. ok is false when the
// value held nothing but whitespace.
func trimmedName(value string) (trimmed string, ok bool) {
	trimmed = strings.TrimSpace(value)
	return trimmed, trimmed != ""
}

// validDigest is the one digest rule both entries share: the decoded value is
// matched untrimmed against the anchored format, so an edge space keeps it
// outside the format.
func validDigest(value string) bool {
	return digestPattern.MatchString(value)
}

// ParseRegistration reads exactly one JSON object from r and validates every
// required field into a service.RegisterInput. Unknown fields are ignored;
// key names are case sensitive and matched after JSON \u-escaping is
// decoded; duplicate keys leave the last value in effect. A missing
// required field, explicit null, wrong type, JSON syntax error, trailing
// non-whitespace content or read failure all return ErrInvalidInput and a
// zero service.RegisterInput.
//
// Repository and tag follow the shared trimmedName rule; the digest follows
// the shared validDigest rule; retention days stay within the published
// range; a negative size is rejected. JSON numbers decode directly into
// int64 — fractional, exponential and string forms are rejected, while false
// and a zero size stay legal.
func ParseRegistration(r io.Reader) (service.RegisterInput, error) {
	if r == nil {
		return service.RegisterInput{}, ErrInvalidInput
	}
	decoder := json.NewDecoder(r)
	var raw map[string]json.RawMessage
	if err := decoder.Decode(&raw); err != nil || raw == nil {
		return service.RegisterInput{}, ErrInvalidInput
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return service.RegisterInput{}, ErrInvalidInput
	}

	var in service.RegisterInput

	repository, err := requiredString(raw, "repository")
	if err != nil {
		return service.RegisterInput{}, err
	}
	var ok bool
	if in.Repository, ok = trimmedName(repository); !ok {
		return service.RegisterInput{}, ErrInvalidInput
	}

	tag, err := requiredString(raw, "tag")
	if err != nil {
		return service.RegisterInput{}, err
	}
	if in.Tag, ok = trimmedName(tag); !ok {
		return service.RegisterInput{}, ErrInvalidInput
	}

	digest, err := requiredString(raw, "digest")
	if err != nil {
		return service.RegisterInput{}, err
	}
	if !validDigest(digest) {
		return service.RegisterInput{}, ErrInvalidInput
	}
	in.Digest = digest

	if in.SignatureVerified, err = requiredBool(raw, "signature_verified"); err != nil {
		return service.RegisterInput{}, err
	}

	if in.RetentionDays, err = requiredInt(raw, "retention_days"); err != nil {
		return service.RegisterInput{}, err
	}
	if in.RetentionDays < minRetentionDays || in.RetentionDays > maxRetentionDays {
		return service.RegisterInput{}, ErrInvalidInput
	}

	if in.SizeBytes, err = requiredInt(raw, "size_bytes"); err != nil {
		return service.RegisterInput{}, err
	}
	if in.SizeBytes < 0 {
		return service.RegisterInput{}, ErrInvalidInput
	}
	return in, nil
}

// isJSONNull reports an explicit JSON null. encoding/json unmarshals null
// into any Go value without an error and leaves the zero value behind, which
// would silently turn a null signature_verified into false and a null
// size_bytes into 0; a required field must reject null like a missing key.
func isJSONNull(field json.RawMessage) bool {
	return string(field) == "null"
}

func requiredString(raw map[string]json.RawMessage, name string) (string, error) {
	field, ok := raw[name]
	if !ok || isJSONNull(field) {
		return "", ErrInvalidInput
	}
	var value string
	if err := json.Unmarshal(field, &value); err != nil {
		return "", ErrInvalidInput
	}
	return value, nil
}

func requiredBool(raw map[string]json.RawMessage, name string) (bool, error) {
	field, ok := raw[name]
	if !ok || isJSONNull(field) {
		return false, ErrInvalidInput
	}
	var value bool
	if err := json.Unmarshal(field, &value); err != nil {
		return false, ErrInvalidInput
	}
	return value, nil
}

// requiredInt rejects fractional, exponential and string forms because
// encoding/json refuses to unmarshal them into an integer.
func requiredInt(raw map[string]json.RawMessage, name string) (int64, error) {
	field, ok := raw[name]
	if !ok || isJSONNull(field) {
		return 0, ErrInvalidInput
	}
	var value int64
	if err := json.Unmarshal(field, &value); err != nil {
		return 0, ErrInvalidInput
	}
	return value, nil
}

// ParseQuery parses rawQuery — the query string without its leading '?' —
// into a validated service.Query. Parameter names are matched after
// percent-decoding, byte-for-byte case sensitively; the first occurrence of
// each recognized parameter decides and later occurrences never
// participate; unknown parameters are ignored. A key/value pair that cannot
// be percent-decoded and a segment carrying an unescaped ';' are skipped,
// exactly as url.ParseQuery skips them; the remaining pairs still select the
// query mode.
//
// '+' decodes to a space and '%2B' to a literal plus. Repository and tag
// follow the shared trimmedName rule; the digest follows the shared
// validDigest rule. A missing or blank repository, a present-but-empty tag
// or digest, an ill-formed digest, or both selectors present together all
// return ErrInvalidInput and a zero service.Query.
func ParseQuery(rawQuery string) (service.Query, error) {
	// ParseQuery always returns the map of the pairs it could decode and
	// drops pairs it could not (bad escape, unescaped ';'); the error it
	// also returns is deliberately discarded, as the dropped pairs are
	// simply absent from the map the same way unknown parameters are.
	values, _ := url.ParseQuery(rawQuery)

	repository, repositoryOK := trimmedName(values.Get("repository"))
	tagValues, hasTag := values["tag"]
	digestValues, hasDigest := values["digest"]
	tag := ""
	tagOK := true
	if hasTag {
		tag, tagOK = trimmedName(tagValues[0])
	}
	// The digest is the first decoded value, untrimmed: an encoded edge
	// space must keep it outside the anchored format.
	digest := ""
	if hasDigest {
		digest = digestValues[0]
	}

	if !repositoryOK || (hasTag && hasDigest) ||
		!tagOK || (hasDigest && !validDigest(digest)) {
		return service.Query{}, ErrInvalidInput
	}

	query := service.Query{Repository: repository}
	switch {
	case hasTag:
		query.Kind = service.QueryByTag
		query.Tag = tag
	case hasDigest:
		query.Kind = service.QueryByDigest
		query.Digest = digest
	default:
		query.Kind = service.QueryByRepository
	}
	return query, nil
}
