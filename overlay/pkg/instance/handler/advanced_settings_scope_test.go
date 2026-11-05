package instance_handler

import (
	"net/http/httptest"
	"strings"
	"testing"

	instance_model "github.com/evolution-foundation/evolution-go/pkg/instance/model"
	instance_service "github.com/evolution-foundation/evolution-go/pkg/instance/service"
	"github.com/gin-gonic/gin"
)

type settingsScopeService struct {
	instance_service.InstanceService
	calls int
}

func (s *settingsScopeService) GetAdvancedSettings(string) (*instance_model.AdvancedSettings, error) {
	s.calls++
	return &instance_model.AdvancedSettings{}, nil
}
func (s *settingsScopeService) UpdateAdvancedSettings(string, *instance_model.AdvancedSettings) error {
	s.calls++
	return nil
}

func TestAdvancedSettingsAuthenticatedUUIDScope(t *testing.T) {
	const own = "10000000-0000-4000-8000-000000000001"
	const other = "10000000-0000-4000-8000-000000000002"
	for _, method := range []string{"GET", "PUT"} {
		for _, tc := range []struct {
			name, url     string
			context       any
			status, calls int
		}{
			{"own", own, &instance_model.Instance{Id: own}, 200, 1},
			{"foreign", other, &instance_model.Instance{Id: own}, 403, 0},
			{"missing-context", own, nil, 403, 0},
			{"wrong-context", own, "not-an-instance", 403, 0},
			{"nil-instance", own, (*instance_model.Instance)(nil), 403, 0},
		} {
			t.Run(method+"/"+tc.name, func(t *testing.T) {
				service := &settingsScopeService{}
				h := NewInstanceHandler(service, nil)
				r := gin.New()
				r.Use(func(c *gin.Context) {
					if tc.context != nil {
						c.Set("instance", tc.context)
					}
				})
				r.GET("/instance/:instanceId/advanced-settings", h.GetAdvancedSettings)
				r.PUT("/instance/:instanceId/advanced-settings", h.UpdateAdvancedSettings)
				req := httptest.NewRequest(method, "/instance/"+tc.url+"/advanced-settings", strings.NewReader(`{"rejectCall":true,"msgRejectCall":"fixture"}`))
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				r.ServeHTTP(w, req)
				if w.Code != tc.status || service.calls != tc.calls {
					t.Fatalf("status=%d calls=%d", w.Code, service.calls)
				}
			})
		}
	}
}
