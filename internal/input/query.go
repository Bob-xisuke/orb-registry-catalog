package input

import (
	"net/url"
	"strings"

	"github.com/Bob-xisuke/orb-registry-catalog/internal/service"
)

// ParseQuery turns the raw query string — the part after '?', without the
// leading '?' — into a validated service.Query. Parameter names and values
// are percent-decoded before use and matching is case sensitive: a literal
// '+' decodes to a space while '%2B' decodes to a literal '+'. When a name
// repeats, only its first value participates; later values are never
// consulted. Unknown parameters are ignored, as are pairs that fail to
// decode and segments carrying an unescaped semicolon, while the remaining
// parameters still decide the query mode. Repository and tag are trimmed
// after decoding; the digest is matched untrimmed. A missing or blank
// repository, a tag or digest present but empty or malformed, or both
// selectors present at once return ErrInvalidInput and the zero query.
func ParseQuery(rawQuery string) (service.Query, error) {
	// url.ParseQuery drops pairs it cannot decode and skips segments with an
	// unescaped semicolon, appending every surviving pair in document order.
	// Its error only describes the first skipped pair; (*url.URL).Query
	// discards that error and so do we, so those pairs behave as absent while
	// the surviving parameters still select the query mode.
	values, _ := url.ParseQuery(rawQuery)

	repository := strings.TrimSpace(values.Get("repository"))
	_, hasTag := values["tag"]
	_, hasDigest := values["digest"]
	tag := strings.TrimSpace(values.Get("tag"))
	digest := values.Get("digest")

	if repository == "" || (hasTag && hasDigest) ||
		(hasTag && tag == "") || (hasDigest && !digestPattern.MatchString(digest)) {
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
