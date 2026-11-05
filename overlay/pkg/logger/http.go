package logger

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gomessguii/logger"
)

// CredentialSafeHTTP records route templates, never request URLs or Gin errors.
// Install before recovery and application middleware, as with gin.Default().
func CredentialSafeHTTP() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		route := c.FullPath()
		if route == "" {
			route = "[unmatched]"
		}
		method := c.Request.Method
		// A nonstandard method is client-controlled text too; do not echo it.
		switch method {
		case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
			http.MethodPatch, http.MethodDelete, http.MethodConnect, http.MethodOptions, http.MethodTrace:
		default:
			method = "[other]"
		}
		logger.LogInfo("[HTTP] method=%s status=%d route=%q latency=%s", method, c.Writer.Status(), route, time.Since(start))
	}
}

// CredentialSafeRecovery keeps Gin's 500/abort and broken-connection handling,
// but a nil diagnostic writer prevents request, header, panic and stack dumps.
func CredentialSafeRecovery() gin.HandlerFunc {
	return gin.CustomRecoveryWithWriter(nil, func(c *gin.Context, _ any) {
		// Keep the existing Gin recovery response even for ErrAbortHandler;
		// this diagnostic change must not introduce new legacy HTTP behavior.
		logger.LogError("[HTTP] panic recovered")
		c.AbortWithStatus(http.StatusInternalServerError)
	})
}
