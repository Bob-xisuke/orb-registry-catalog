package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/Bob-xisuke/orb-registry-catalog/internal/input"
	"github.com/Bob-xisuke/orb-registry-catalog/internal/service"
)

const (
	codeInvalidInput = "InvalidArtifactInputError"
	codeConflict     = "ArtifactConflictError"
	codeNotFound     = "ArtifactNotFoundError"
	codeStorage      = "storage_unavailable"
)

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

func registerArtifact(svc *service.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		// The request body is parsed and validated by the shared,
		// framework-free input entry; the handler never inspects JSON itself.
		in, err := input.ParseRegistration(c.Request.Body)
		if err != nil {
			writeError(c, http.StatusBadRequest, codeInvalidInput, "request body is not a valid artifact registration")
			return
		}

		result, err := svc.Register(in)
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
		// The raw query string (without '?') goes through the same shared
		// entry a direct caller uses; the handler only dispatches its result.
		query, err := input.ParseQuery(c.Request.URL.RawQuery)
		if err != nil {
			writeError(c, http.StatusBadRequest, codeInvalidInput, "query parameters are not valid")
			return
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
