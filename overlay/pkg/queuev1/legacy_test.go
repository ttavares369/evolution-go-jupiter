package queuev1

import (
	"testing"

	"github.com/evolution-foundation/evolution-go/pkg/config"
	instance_model "github.com/evolution-foundation/evolution-go/pkg/instance/model"
	auth_middleware "github.com/evolution-foundation/evolution-go/pkg/middleware"
	send_handler "github.com/evolution-foundation/evolution-go/pkg/sendMessage/handler"
	send_service "github.com/evolution-foundation/evolution-go/pkg/sendMessage/service"
	"github.com/gin-gonic/gin"
)

// The unmodified legacy handlers still dispatch to their own service with their
// original DTOs; registering queue-v1 must not intercept calls or require opt-in.
type queueLegacyService struct {
	send_service.SendService
	text, media int
	t           *testing.T
}

func (f *queueLegacyService) SendText(data *send_service.TextStruct, inst *instance_model.Instance) (*send_service.MessageSendStruct, error) {
	f.text++
	if data.Number != "5511999999999" || data.Text != "legacy text" || data.Delay != 123 || data.Id != "LEGACYID" || inst.Id != serviceTestInstance {
		f.t.Error("legacy text DTO changed")
	}
	return &send_service.MessageSendStruct{}, nil
}
func (f *queueLegacyService) SendMediaUrl(data *send_service.MediaStruct, inst *instance_model.Instance) (*send_service.MessageSendStruct, error) {
	f.media++
	if data.Url != "https://example.invalid/image.png" || data.Type != "image" || data.Caption != "legacy caption" || data.Delay != 456 || inst.Id != serviceTestInstance {
		f.t.Error("legacy media DTO changed")
	}
	// No URL is fetched; this is the fake boundary of the legacy handler.
	return &send_service.MessageSendStruct{}, nil
}
func TestQueueLegacyHandlersRemainIndependent(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		r := gin.New()
		fake := &queueLegacyService{t: t}
		handler := send_handler.NewSendHandler(fake)
		auth := auth_middleware.NewMiddleware(&config.Config{}, queueAuthFixture{}).Auth
		legacy := r.Group("/send")
		legacy.Use(auth)
		legacy.POST("/text", handler.SendText)
		legacy.POST("/media", handler.SendMedia)
		if enabled {
			RegisterRoutes(r, NewService(serviceNewLedger(), &serviceTransportFake{}), auth)
		} else {
			RegisterOptional(r, nil, nil, nil, false)
		}
		for _, tc := range []struct{ path, body string }{
			{"/send/text", `{"number":"5511999999999","text":"legacy text","delay":123,"id":"LEGACYID"}`},
			{"/send/media", `{"number":"5511999999999","url":"https://example.invalid/image.png","type":"image","caption":"legacy caption","delay":456}`},
		} {
			w := queueHTTPRequest(r, "POST", tc.path, tc.body, "fixture-token-one", "")
			if w.Code != 200 || w.Header().Get(OptInHeader) != "" {
				t.Fatalf("legacy response changed: %d", w.Code)
			}
			w = queueHTTPRequest(r, "POST", tc.path, tc.body, "invalid", "")
			if w.Code != 401 {
				t.Fatal("legacy auth changed")
			}
		}
		if fake.text != 1 || fake.media != 1 {
			t.Fatal("legacy routing intercepted")
		}
	}
}
