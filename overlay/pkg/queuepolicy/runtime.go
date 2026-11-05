package queuepolicy

import (
	"context"
	"net/http"
	"strings"
	"time"

	instance_model "github.com/evolution-foundation/evolution-go/pkg/instance/model"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Configure runs before service/runtime construction, independently of whether
// the queue HTTP contract is enabled. Initialization failure keeps emissions shut.
func Configure(db *gorm.DB, allowlist string) error {
	Install(nil)
	if db == nil {
		return ErrUnavailable
	}
	sqlDB, err := db.DB()
	if err != nil {
		return ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	check, err := Initialize(ctx, sqlDB, allowlist)
	if err != nil {
		return ErrUnavailable
	}
	Install(check)
	return nil
}

// HTTP is defense in depth; service guards also protect non-HTTP producers.
// It runs after Auth and before DTO parsing, downloads, uploads or admin effects.
func HTTP() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.FullPath()
		blocked := strings.HasPrefix(path, "/send/")
		switch path {
		case "/call/reject", "/message/react", "/message/presence", "/message/delete", "/message/edit",
			"/group/invitelink", "/group/photo", "/group/name", "/group/description", "/group/create", "/group/participant", "/group/join", "/group/leave", "/group/settings",
			"/community/create", "/community/add", "/community/remove",
			"/newsletter/create", "/newsletter/subscribe",
			"/user/profilePicture", "/user/profileName", "/user/profileStatus":
			blocked = true
		}
		if blocked {
			value, ok := c.Get("instance")
			instance, valid := value.(*instance_model.Instance)
			if !ok || !valid || instance == nil {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "not_authorized"})
				return
			}
			if err := Legacy(instance.Id); err != nil {
				status, code := http.StatusServiceUnavailable, "queue_policy_unavailable"
				if err == ErrExclusive {
					status, code = http.StatusForbidden, "queue_v1_exclusive_instance"
				}
				c.AbortWithStatusJSON(status, gin.H{"error": code})
				return
			}
		}
		c.Next()
	}
}
