package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/Bob-xisuke/orb-registry-catalog/internal/service"
	"github.com/Bob-xisuke/orb-registry-catalog/internal/store"
)

// NewRouter wires the public HTTP surface. The service contract in README.md
// describes the error shape every entry must keep.
func NewRouter(st *store.Store) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())
	svc := service.New(st)

	router.GET("/healthz", func(c *gin.Context) {
		if err := st.Ping(); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"code": "storage_unavailable", "message": "database is not available"}})
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
