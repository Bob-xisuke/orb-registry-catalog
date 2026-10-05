package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Bob-xisuke/orb-registry-catalog/internal/store"
)

const (
	codeInvalidInput       = "InvalidArtifactInputError"
	codeConflict           = "ArtifactConflictError"
	codeNotFound           = "ArtifactNotFoundError"
	codeStorageUnavailable = "storage_unavailable"
)

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// artifactInput mirrors the request body. Pointer fields distinguish "missing" from a zero
// value so every absent field is rejected.
type artifactInput struct {
	Repository        *string `json:"repository"`
	Digest            *string `json:"digest"`
	Tag               *string `json:"tag"`
	SignatureVerified *bool   `json:"signature_verified"`
	RetentionDays     *int64  `json:"retention_days"`
	SizeBytes         *int64  `json:"size_bytes"`
}

// artifactResponse is the public shape of one registered record.
type artifactResponse struct {
	Repository        string `json:"repository"`
	Digest            string `json:"digest"`
	Tag               string `json:"tag"`
	SignatureVerified bool   `json:"signature_verified"`
	RetentionDays     int64  `json:"retention_days"`
	SizeBytes         int64  `json:"size_bytes"`
	PushedAt          string `json:"pushed_at"`
}

func artifactResponseFrom(a store.Artifact) artifactResponse {
	return artifactResponse{
		Repository:        a.Repository,
		Digest:            a.Digest,
		Tag:               a.Tag,
		SignatureVerified: a.SignatureVerified,
		RetentionDays:     a.RetentionDays,
		SizeBytes:         a.SizeBytes,
		PushedAt:          a.PushedAt,
	}
}

func writeError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}

func writeInvalidInput(c *gin.Context) {
	writeError(c, http.StatusBadRequest, codeInvalidInput, "request does not match the artifact input contract")
}

func writeStorageUnavailable(c *gin.Context) {
	writeError(c, http.StatusServiceUnavailable, codeStorageUnavailable, "database is not available")
}

// postArtifact registers one artifact. The body must be a single JSON object with exactly the
// six documented fields required; unknown fields are ignored.
func postArtifact(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		decoder := json.NewDecoder(c.Request.Body)
		var input artifactInput
		if err := decoder.Decode(&input); err != nil || decoder.More() {
			writeInvalidInput(c)
			return
		}

		artifact, ok := normalizeArtifact(input)
		if !ok {
			writeInvalidInput(c)
			return
		}
		artifact.PushedAt = time.Now().UTC().Format(time.RFC3339)

		stored, err := st.RegisterArtifact(artifact)
		switch {
		case errors.Is(err, store.ErrArtifactConflict):
			writeError(c, http.StatusConflict, codeConflict, "an artifact with this repository and digest already exists with different content")
			return
		case err != nil:
			writeStorageUnavailable(c)
			return
		}
		c.JSON(http.StatusCreated, artifactResponseFrom(stored))
	}
}

// normalizeArtifact trims and validates the six registration fields.
func normalizeArtifact(input artifactInput) (store.Artifact, bool) {
	if input.Repository == nil || input.Digest == nil || input.Tag == nil ||
		input.SignatureVerified == nil || input.RetentionDays == nil || input.SizeBytes == nil {
		return store.Artifact{}, false
	}
	repository := strings.TrimSpace(*input.Repository)
	tag := strings.TrimSpace(*input.Tag)
	if repository == "" || tag == "" {
		return store.Artifact{}, false
	}
	if !digestPattern.MatchString(*input.Digest) {
		return store.Artifact{}, false
	}
	if *input.RetentionDays < 1 || *input.RetentionDays > 3650 {
		return store.Artifact{}, false
	}
	if *input.SizeBytes < 0 {
		return store.Artifact{}, false
	}
	return store.Artifact{
		Repository:        repository,
		Digest:            *input.Digest,
		Tag:               tag,
		SignatureVerified: *input.SignatureVerified,
		RetentionDays:     *input.RetentionDays,
		SizeBytes:         *input.SizeBytes,
	}, true
}

// listArtifacts serves GET /v1/artifacts with repository required and tag or digest optional
// (never both). Every variant answers with an artifacts array.
func listArtifacts(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		repositoryRaw, hasRepository := c.GetQuery("repository")
		tagRaw, hasTag := c.GetQuery("tag")
		digest, hasDigest := c.GetQuery("digest")

		if !hasRepository || (hasTag && hasDigest) {
			writeInvalidInput(c)
			return
		}
		repository := strings.TrimSpace(repositoryRaw)
		if repository == "" {
			writeInvalidInput(c)
			return
		}

		switch {
		case hasTag:
			tag := strings.TrimSpace(tagRaw)
			if tag == "" {
				writeInvalidInput(c)
				return
			}
			artifact, err := st.GetArtifactByTag(repository, tag)
			respondSingle(c, artifact, err)
		case hasDigest:
			if !digestPattern.MatchString(digest) {
				writeInvalidInput(c)
				return
			}
			artifact, err := st.GetArtifactByDigest(repository, digest)
			respondSingle(c, artifact, err)
		default:
			artifacts, err := st.ListArtifacts(repository)
			if err != nil {
				writeStorageUnavailable(c)
				return
			}
			if len(artifacts) == 0 {
				writeNotFound(c)
				return
			}
			response := make([]artifactResponse, 0, len(artifacts))
			for _, a := range artifacts {
				response = append(response, artifactResponseFrom(a))
			}
			c.JSON(http.StatusOK, gin.H{"artifacts": response})
		}
	}
}

func respondSingle(c *gin.Context, artifact store.Artifact, err error) {
	switch {
	case errors.Is(err, store.ErrArtifactNotFound):
		writeNotFound(c)
	case err != nil:
		writeStorageUnavailable(c)
	default:
		c.JSON(http.StatusOK, gin.H{"artifacts": []artifactResponse{artifactResponseFrom(artifact)}})
	}
}

func writeNotFound(c *gin.Context) {
	writeError(c, http.StatusNotFound, codeNotFound, "no artifact matches the query")
}
