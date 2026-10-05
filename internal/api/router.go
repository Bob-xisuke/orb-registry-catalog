package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/Bob-xisuke/orb-registry-catalog/internal/service"
	"github.com/Bob-xisuke/orb-registry-catalog/internal/store"
)

// HealthCheck is the caller-supplied probe behind GET /healthz. It is invoked
// on every health request and its result alone decides the response: nil
// error keeps the published 200 body, any error the published 503. Artifact
// registration and queries never consult it.
type HealthCheck func() error

// NewRouter keeps the original SQLite-backed assembly for existing callers:
// the bundled store serves the storage contract and its own Ping becomes the
// health check.
func NewRouter(st *store.Store) *gin.Engine {
	return NewRouterWithStore(st, st.Ping)
}

// NewRouterWithStore wires the public HTTP surface around any service.Store
// plus an independent health check. The store may be a caller-supplied
// implementation of the storage contract; it needs no Ping method, no SQLite
// and no database/sql. The router never opens a database itself and never
// closes the store it is given: ownership and lifecycle stay with the caller.
// A nil health check reports the service as healthy. The service contract in
// README.md describes the error shape every entry must keep.
func NewRouterWithStore(st service.Store, health HealthCheck) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())

	svc := service.New(st)

	router.GET("/healthz", func(c *gin.Context) {
		if health != nil && health() != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"code": codeStorage, "message": "database is not available"}})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok", "database": "ok"})
	})

	router.POST("/v1/artifacts", registerArtifact(svc))
	router.GET("/v1/artifacts", queryArtifacts(svc))

	router.NoRoute(func(c *gin.Context) {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"code": "route_not_found", "message": "no route matches this path"}})
	})
	return router
}
