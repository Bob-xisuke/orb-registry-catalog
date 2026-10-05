package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/Bob-xisuke/orb-registry-catalog/internal/service"
)

// HealthChecker is the independently supplied health probe. The router never
// derives health from the storage backend: a caller-provided store does not
// have to implement Ping, and each GET /healthz response reflects only the
// result of this call. Returning nil answers 200; any error answers 503.
type HealthChecker func() error

// NewRouter wires the public HTTP surface over any store satisfying the
// business-layer contract in service.Store. The router opens no database and
// never closes the supplied store; lifecycle ownership stays with the caller.
//
// It registers only the artifact entries, so callers that bring their own
// storage without a Ping-style probe are not forced into a health route. Use
// NewRouterWithHealth to additionally serve GET /healthz.
func NewRouter(st service.Store) *gin.Engine {
	return NewRouterWithHealth(st, nil)
}

// NewRouterWithHealth wires the same artifact surface as NewRouter and, when
// check is non-nil, additionally serves GET /healthz entirely from the
// caller-supplied probe. A nil check leaves /healthz unregistered like every
// other unknown route, so stores without a Ping operation impose no health
// dependency on artifact registration or queries.
func NewRouterWithHealth(st service.Store, check HealthChecker) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())

	svc := service.New(st)

	if check != nil {
		router.GET("/healthz", healthz(check))
	}

	router.POST("/v1/artifacts", registerArtifact(svc))
	router.GET("/v1/artifacts", queryArtifacts(svc))

	router.NoRoute(func(c *gin.Context) {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"code": "route_not_found", "message": "no route matches this path"}})
	})
	return router
}

// healthz answers only from the caller-supplied probe result; it never calls
// into the store, so a failing probe cannot be overridden by a working
// registration and a failing registration cannot replace the probe result.
func healthz(check HealthChecker) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := check(); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"code": "storage_unavailable", "message": "database is not available"}})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok", "database": "ok"})
	}
}
