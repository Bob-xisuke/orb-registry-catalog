// Package input parses and validates the artifact registration and query
// requests into service-layer values. It is the single input boundary shared
// by the HTTP handlers and by direct callers: it depends on neither a web
// framework nor a storage backend, it performs no persistence, and it never
// generates a push time — that stays with the service.
package input

import (
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"

	"github.com/Bob-xisuke/orb-registry-catalog/internal/service"
)

// ErrInvalidInput is the sole failure either parser returns. Every deviation
// from the published input contract — a missing, null or mistyped field, a
// syntax error, trailing content, an unreadable body, or an illegal query
// shape — maps to it, and the returned value is the zero value. Callers
// identify it with errors.Is.
var ErrInvalidInput = errors.New("invalid artifact input")

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// ParseRegistration reads exactly one JSON object from body and validates
// every required field, returning the normalized service.RegisterInput.
// Unknown fields are ignored; key matching is case sensitive and happens on
// the decoded key name, so a \u-escaped spelling of a required key is that
// key and duplicate keys keep the last value. Repository and tag are trimmed
// after decoding; the digest is matched untrimmed. Integer fields decode
// directly into int64, so fractional, exponential and string-number forms
// are rejected. Any failure returns ErrInvalidInput and the zero input; the
// parser never touches storage and assigns no push time.
func ParseRegistration(body io.Reader) (service.RegisterInput, error) {
	decoder := json.NewDecoder(body)
	var raw map[string]json.RawMessage
	if err := decoder.Decode(&raw); err != nil || raw == nil {
		return service.RegisterInput{}, ErrInvalidInput
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return service.RegisterInput{}, ErrInvalidInput
	}

	// Validate into locals first: every error path must hand back the zero
	// RegisterInput, never a partially populated one.
	repository, err := requiredString(raw, "repository")
	if err != nil {
		return service.RegisterInput{}, err
	}
	if repository = strings.TrimSpace(repository); repository == "" {
		return service.RegisterInput{}, ErrInvalidInput
	}

	tag, err := requiredString(raw, "tag")
	if err != nil {
		return service.RegisterInput{}, err
	}
	if tag = strings.TrimSpace(tag); tag == "" {
		return service.RegisterInput{}, ErrInvalidInput
	}

	digest, err := requiredString(raw, "digest")
	if err != nil {
		return service.RegisterInput{}, err
	}
	if !digestPattern.MatchString(digest) {
		return service.RegisterInput{}, ErrInvalidInput
	}

	signatureVerified, err := requiredBool(raw, "signature_verified")
	if err != nil {
		return service.RegisterInput{}, err
	}

	retentionDays, err := requiredInt(raw, "retention_days")
	if err != nil {
		return service.RegisterInput{}, err
	}
	if retentionDays < 1 || retentionDays > 3650 {
		return service.RegisterInput{}, ErrInvalidInput
	}

	sizeBytes, err := requiredInt(raw, "size_bytes")
	if err != nil {
		return service.RegisterInput{}, err
	}
	if sizeBytes < 0 {
		return service.RegisterInput{}, ErrInvalidInput
	}

	return service.RegisterInput{
		Repository:        repository,
		Digest:            digest,
		Tag:               tag,
		SignatureVerified: signatureVerified,
		RetentionDays:     retentionDays,
		SizeBytes:         sizeBytes,
	}, nil
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
