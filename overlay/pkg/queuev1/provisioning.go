package queuev1

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/evolution-foundation/evolution-go/pkg/queuepolicy"
	"github.com/gin-gonic/gin"
)

// RegisterRoutesWithAdmin also supports isolated consumers with an injected
// transport. Runtime wiring must supply the existing middleware.AuthAdmin,
// never instance Auth, and the same SQL DB used by queuepolicy.Configure.
func RegisterRoutesWithAdmin(r *gin.Engine, service *Service, auth gin.HandlerFunc, db *sql.DB, authAdmin gin.HandlerFunc) {
	available := service != nil && db != nil && authAdmin != nil
	g := r.Group("/queue/v1/instances")
	g.Use(func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Header(OptInHeader, Contract)
		if c.GetHeader(OptInHeader) != Contract {
			c.AbortWithStatusJSON(http.StatusPreconditionRequired, gin.H{"error": "queue_contract_opt_in_required"})
			return
		}
		if authAdmin == nil {
			queueHTTPError(c, ErrUnavailable)
			return
		}
		authAdmin(c)
	})
	g.POST("/:instanceId/exclusive", func(c *gin.Context) {
		if !available || !queuepolicy.Installed() {
			queueHTTPError(c, ErrUnavailable)
			return
		}
		instance := c.Param("instanceId")
		if !serviceUUID(instance) {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid_instance_id"})
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
		defer cancel()
		if _, err := queuepolicy.Exclusive(ctx, instance); err != nil {
			queueHTTPError(c, ErrUnavailable)
			return
		}
		if err := queuepolicy.Activate(ctx, db, instance); err != nil {
			switch {
			case errors.Is(err, queuepolicy.ErrInstanceNotFound):
				c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "instance_not_found"})
			case errors.Is(err, queuepolicy.ErrInvalidInstance):
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid_instance_id"})
			default:
				queueHTTPError(c, ErrUnavailable)
			}
			return
		}
		// COMMIT acknowledgment alone is insufficient: prove that the installed
		// gate for this process actually observes the durable policy/UUID.
		exclusive, err := queuepolicy.Exclusive(ctx, instance)
		if err != nil || !exclusive {
			queueHTTPError(c, ErrUnavailable)
			return
		}
		c.JSON(http.StatusOK, gin.H{"contract": Contract, "instanceId": instance, "exclusiveInstance": true})
	})
	registerRoutes(r, service, auth, available)
}
