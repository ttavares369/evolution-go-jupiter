package queuev1

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"time"

	instance_model "github.com/evolution-foundation/evolution-go/pkg/instance/model"
	"github.com/evolution-foundation/evolution-go/pkg/queuepolicy"
	whatsmeow_service "github.com/evolution-foundation/evolution-go/pkg/whatsmeow/service"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// RegisterOptional is inert unless explicitly enabled. Ledger initialization
// failures close only the new contract, never the legacy router/startup.
func RegisterOptional(r *gin.Engine, db *gorm.DB, wa whatsmeow_service.WhatsmeowService, auth gin.HandlerFunc, enabled bool, admin ...gin.HandlerFunc) {
	if !enabled {
		return
	}
	var service *Service
	var sqlDB *sql.DB
	if db != nil {
		var err error
		sqlDB, err = db.DB()
		if err == nil {
			store, storeErr := NewPostgresLedger(sqlDB)
			if storeErr == nil {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				err = store.Migrate(ctx)
				cancel()
				if err == nil {
					service = NewService(store, NewTransport(wa))
				}
			}
		}
	}
	var authAdmin gin.HandlerFunc
	if len(admin) == 1 {
		authAdmin = admin[0]
	}
	RegisterRoutesWithAdmin(r, service, auth, sqlDB, authAdmin)
}

// RegisterRoutes does not change or wrap any legacy endpoint. A nil service
// deliberately publishes an unavailable contract, not a legacy-send fallback.
func RegisterRoutes(r *gin.Engine, service *Service, auth gin.HandlerFunc) {
	registerRoutes(r, service, auth, false)
}

func registerRoutes(r *gin.Engine, service *Service, auth gin.HandlerFunc, provisioning bool) {
	g := r.Group("/queue/v1")
	g.Use(func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Header(OptInHeader, Contract)
		if c.GetHeader(OptInHeader) != Contract {
			c.AbortWithStatusJSON(http.StatusPreconditionRequired, gin.H{"error": "queue_contract_opt_in_required"})
			return
		}
		if auth == nil {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "queue_unavailable"})
			return
		}
		auth(c)
	})
	g.Use(func(c *gin.Context) {
		instance, ok := c.Get("instance")
		account, valid := instance.(*instance_model.Instance)
		if !ok || !valid || account == nil || !serviceUUID(account.Id) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "not_authorized"})
			return
		}
		if service == nil {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "queue_unavailable"})
			return
		}
		c.Set("queue-v1-instance", account.Id)
		c.Next()
	})
	g.GET("/capabilities", func(c *gin.Context) {
		exclusive, err := queuepolicy.Exclusive(c.Request.Context(), c.GetString("queue-v1-instance"))
		if err != nil {
			queueHTTPError(c, ErrUnavailable)
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"contract": Contract, "instanceId": c.GetString("queue-v1-instance"),
			"durableLedger": true, "singleCommercialCall": true, "technicalRetransmissions": true,
			"validUntil": true, "exclusiveInstance": exclusive,
			"exclusiveProvisioning": provisioning && queuepolicy.Installed(),
			"acceptedMeans":         "transport_ack_not_delivery", "uncertainReplay": false,
			"presence": gin.H{"minDurationMs": 8000, "maxDurationMs": 26000, "refreshMs": 5000},
			"kinds":    []string{"text", "image"}, "imageEncoding": "base64",
			"imageMimes":    []string{"image/jpeg", "image/png", "image/webp"},
			"maxImageBytes": MaxImageBytes, "maxBodyBytes": MaxBodyBytes,
			"maxTextBytes": serviceMaxTextBytes, "maxCaptionBytes": serviceMaxCaptionBytes,
			"serializationScope": "queue-v1-only",
		})
	})
	g.POST("/presence", func(c *gin.Context) {
		var req PresenceRequest
		if !decodeQueueBody(c, &req) {
			return
		}
		if err := service.Presence(c.Request.Context(), c.GetString("queue-v1-instance"), req); err != nil {
			queueHTTPError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"contract": Contract, "state": "presence_completed", "durationMs": req.DurationMS})
	})
	for _, kind := range []string{"text", "image"} {
		kind := kind
		g.POST("/"+kind, func(c *gin.Context) {
			var req SendRequest
			if !decodeQueueBody(c, &req) {
				return
			}
			if req.Kind != "" && req.Kind != kind {
				queueHTTPError(c, ErrInvalid)
				return
			}
			req.Kind = kind
			// Bound admission/row-lock waits; Service detaches this deadline only
			// after a durable admission to finish the owned effect/bookkeeping.
			ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
			defer cancel()
			attempt, err := service.Send(ctx, c.GetString("queue-v1-instance"), req)
			if err != nil {
				queueHTTPError(c, err)
				return
			}
			status := http.StatusAccepted
			switch attempt.State {
			case Accepted:
				status = http.StatusOK
			case Rejected:
				status = http.StatusUnprocessableEntity
			}
			c.JSON(status, gin.H{"contract": Contract, "attempt": attempt})
		})
	}
	g.GET("/attempts/:attemptId", func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
		defer cancel()
		attempt, err := service.Lookup(ctx, c.GetString("queue-v1-instance"), c.Param("attemptId"))
		if err != nil {
			queueHTTPError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"contract": Contract, "attempt": attempt})
	})
}

func decodeQueueBody(c *gin.Context, dst any) bool {
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || mediaType != "application/json" {
		c.AbortWithStatusJSON(http.StatusUnsupportedMediaType, gin.H{"error": "json_required"})
		return false
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaxBodyBytes)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	err = decoder.Decode(dst)
	if err == nil {
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			err = ErrInvalid
		}
	}
	if err != nil {
		var tooLarge *http.MaxBytesError
		status := http.StatusBadRequest
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		c.AbortWithStatusJSON(status, gin.H{"error": "invalid_queue_request"})
		return false
	}
	return true
}

func queueHTTPError(c *gin.Context, err error) {
	status, code := http.StatusServiceUnavailable, "queue_unavailable"
	switch {
	case errors.Is(err, ErrInvalid):
		status, code = http.StatusBadRequest, "invalid_queue_request"
	case errors.Is(err, ErrConflict):
		status, code = http.StatusConflict, "identity_conflict"
	case errors.Is(err, ErrBusy):
		status, code = http.StatusConflict, "instance_busy"
	case errors.Is(err, ErrNotFound):
		status, code = http.StatusNotFound, "attempt_not_found"
	}
	// No SQL/provider/request/error strings can reach this boundary.
	c.AbortWithStatusJSON(status, gin.H{"error": code})
}
