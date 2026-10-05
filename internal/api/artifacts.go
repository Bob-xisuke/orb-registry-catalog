package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/Bob-xisuke/orb-registry-catalog/internal/service"
)

const (
	codeInvalidInput = "InvalidArtifactInputError"
	codeConflict     = "ArtifactConflictError"
	codeNotFound     = "ArtifactNotFoundError"
	codeStorage      = "storage_unavailable"
)

var (
	digestPattern   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	errInvalidInput = errors.New("invalid artifact input")
)

// artifactInput is a registration request after normalization and validation.
type artifactInput struct {
	repository        string
	digest            string
	tag               string
	signatureVerified bool
	retentionDays     int64
	sizeBytes         int64
}

func writeError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}

func artifactResponse(a service.Artifact) gin.H {
	return gin.H{
		"repository":         a.Repository,
		"digest":             a.Digest,
		"tag":                a.Tag,
		"signature_verified": a.SignatureVerified,
		"retention_days":     a.RetentionDays,
		"size_bytes":         a.SizeBytes,
		"pushed_at":          a.PushedAt,
	}
}

// decodeArtifactInput reads exactly one JSON object and validates every required
// field. Unknown fields are ignored; any deviation yields errInvalidInput.
func decodeArtifactInput(body io.Reader) (artifactInput, error) {
	var input artifactInput
	decoder := json.NewDecoder(body)
	var raw map[string]json.RawMessage
	if err := decoder.Decode(&raw); err != nil || raw == nil {
		return input, errInvalidInput
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return input, errInvalidInput
	}

	repository, err := requiredString(raw, "repository")
	if err != nil {
		return input, err
	}
	if input.repository = strings.TrimSpace(repository); input.repository == "" {
		return input, errInvalidInput
	}

	tag, err := requiredString(raw, "tag")
	if err != nil {
		return input, err
	}
	if input.tag = strings.TrimSpace(tag); input.tag == "" {
		return input, errInvalidInput
	}

	digest, err := requiredString(raw, "digest")
	if err != nil {
		return input, err
	}
	if !digestPattern.MatchString(digest) {
		return input, errInvalidInput
	}
	input.digest = digest

	if input.signatureVerified, err = requiredBool(raw, "signature_verified"); err != nil {
		return input, err
	}

	if input.retentionDays, err = requiredInt(raw, "retention_days"); err != nil {
		return input, err
	}
	if input.retentionDays < 1 || input.retentionDays > 3650 {
		return input, errInvalidInput
	}

	if input.sizeBytes, err = requiredInt(raw, "size_bytes"); err != nil {
		return input, err
	}
	if input.sizeBytes < 0 {
		return input, errInvalidInput
	}
	return input, nil
}

// The required* helpers decode into pointers so an explicit JSON null lands on
// a nil pointer and is rejected like a missing field, instead of silently
// becoming the zero value ("", false, 0).
func requiredString(raw map[string]json.RawMessage, name string) (string, error) {
	field, ok := raw[name]
	if !ok {
		return "", errInvalidInput
	}
	var value *string
	if err := json.Unmarshal(field, &value); err != nil || value == nil {
		return "", errInvalidInput
	}
	return *value, nil
}

func requiredBool(raw map[string]json.RawMessage, name string) (bool, error) {
	field, ok := raw[name]
	if !ok {
		return false, errInvalidInput
	}
	var value *bool
	if err := json.Unmarshal(field, &value); err != nil || value == nil {
		return false, errInvalidInput
	}
	return *value, nil
}

// requiredInt rejects fractional, exponential and string forms because
// encoding/json refuses to unmarshal them into an integer.
func requiredInt(raw map[string]json.RawMessage, name string) (int64, error) {
	field, ok := raw[name]
	if !ok {
		return 0, errInvalidInput
	}
	var value *int64
	if err := json.Unmarshal(field, &value); err != nil || value == nil {
		return 0, errInvalidInput
	}
	return *value, nil
}

func registerArtifact(svc *service.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		input, err := decodeArtifactInput(c.Request.Body)
		if err != nil {
			writeError(c, http.StatusBadRequest, codeInvalidInput, "request body is not a valid artifact registration")
			return
		}

		result, err := svc.Register(service.RegisterInput{
			Repository:        input.repository,
			Digest:            input.digest,
			Tag:               input.tag,
			SignatureVerified: input.signatureVerified,
			RetentionDays:     input.retentionDays,
			SizeBytes:         input.sizeBytes,
		})
		switch {
		case errors.Is(err, service.ErrConflict):
			writeError(c, http.StatusConflict, codeConflict, "an artifact with this repository and digest already exists with different content")
		case err != nil:
			writeError(c, http.StatusServiceUnavailable, codeStorage, "database is not available")
		default:
			// First registrations and identical retries share the public 201;
			// the created/duplicate distinction stays inside the service result.
			c.JSON(http.StatusCreated, artifactResponse(result.Artifact))
		}
	}
}

func queryArtifacts(svc *service.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		repository := strings.TrimSpace(c.Query("repository"))
		tag, hasTag := c.GetQuery("tag")
		tag = strings.TrimSpace(tag)
		digest, hasDigest := c.GetQuery("digest")

		if repository == "" || (hasTag && hasDigest) ||
			(hasTag && tag == "") || (hasDigest && !digestPattern.MatchString(digest)) {
			writeError(c, http.StatusBadRequest, codeInvalidInput, "query parameters are not valid")
			return
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

		artifacts, err := svc.Query(query)
		switch {
		case errors.Is(err, service.ErrNotFound):
			writeError(c, http.StatusNotFound, codeNotFound, "no artifact matches this query")
		case err != nil:
			writeError(c, http.StatusServiceUnavailable, codeStorage, "database is not available")
		default:
			response := make([]gin.H, 0, len(artifacts))
			for _, record := range artifacts {
				response = append(response, artifactResponse(record))
			}
			c.JSON(http.StatusOK, gin.H{"artifacts": response})
		}
	}
}
