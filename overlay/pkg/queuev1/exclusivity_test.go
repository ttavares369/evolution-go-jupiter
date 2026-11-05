package queuev1

import (
	"context"
	"errors"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	call_service "github.com/evolution-foundation/evolution-go/pkg/call/service"
	community_service "github.com/evolution-foundation/evolution-go/pkg/community/service"
	"github.com/evolution-foundation/evolution-go/pkg/config"
	group_service "github.com/evolution-foundation/evolution-go/pkg/group/service"
	instance_model "github.com/evolution-foundation/evolution-go/pkg/instance/model"
	message_service "github.com/evolution-foundation/evolution-go/pkg/message/service"
	auth_middleware "github.com/evolution-foundation/evolution-go/pkg/middleware"
	newsletter_service "github.com/evolution-foundation/evolution-go/pkg/newsletter/service"
	"github.com/evolution-foundation/evolution-go/pkg/queuepolicy"
	send_service "github.com/evolution-foundation/evolution-go/pkg/sendMessage/service"
	user_service "github.com/evolution-foundation/evolution-go/pkg/user/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func exclusiveTestPolicy(t *testing.T) {
	t.Helper()
	queuepolicy.Install(func(_ context.Context, id string) (bool, error) { return id == serviceTestInstance, nil })
	t.Cleanup(func() { queuepolicy.Install(func(context.Context, string) (bool, error) { return false, nil }) })
}

// Nil DTOs/dependencies are intentional: reaching validation/client/storage or
// downloader code before the gate would panic. No Whatsmeow runtime is started.
func TestExclusiveServiceGuardsBeforeAnyPreparation(t *testing.T) {
	exclusiveTestPolicy(t)
	for _, tc := range []struct {
		service any
		methods []string
	}{
		{send_service.NewSendService(nil, nil, nil, nil), []string{"SendText", "SendLink", "SendMediaFile", "SendMediaUrl", "SendPoll", "SendSticker", "SendLocation", "SendContact", "SendButton", "SendList", "SendMessage", "SendCarousel", "SendStatusText", "SendStatusMediaUrl", "SendStatusMediaFile"}},
		{call_service.NewCallService(nil, nil, nil), []string{"RejectCall"}},
		{message_service.NewMessageService(nil, nil, nil, nil), []string{"React", "ChatPresence", "DeleteMessageEveryone", "EditMessage"}},
		{group_service.NewGroupService(nil, nil, nil), []string{"GetGroupInviteLink", "SetGroupPhoto", "SetGroupName", "SetGroupDescription", "CreateGroup", "UpdateParticipant", "JoinGroupLink", "LeaveGroup", "UpdateGroupSettings", "UpdateGroupRequestParticipants"}},
		{community_service.NewCommunityService(nil, nil, nil), []string{"CreateCommunity", "CommunityAdd", "CommunityRemove"}},
		{user_service.NewUserService(nil, nil, nil), []string{"SetProfilePicture", "SetProfileName", "SetProfileStatus"}},
		{newsletter_service.NewNewsletterService(nil, nil, nil), []string{"CreateNewsletter", "SubscribeNewsletter"}},
	} {
		for _, method := range tc.methods {
			t.Run(method, func(t *testing.T) {
				f := reflect.ValueOf(tc.service).MethodByName(method)
				if !f.IsValid() {
					t.Fatal("inventory method missing")
				}
				args := make([]reflect.Value, f.Type().NumIn())
				for i := range args {
					typ := f.Type().In(i)
					if typ == reflect.TypeOf((*instance_model.Instance)(nil)) {
						args[i] = reflect.ValueOf(&instance_model.Instance{Id: serviceTestInstance})
					} else {
						args[i] = reflect.Zero(typ)
					}
				}
				result := f.Call(args)
				err, ok := result[len(result)-1].Interface().(error)
				if !ok || !errors.Is(err, queuepolicy.ErrExclusive) {
					t.Fatalf("legacy service not gated: %v", err)
				}
			})
		}
	}
}

func TestExclusiveHTTPRejectsBeforeHandlersAndKeepsOtherUUIDs(t *testing.T) {
	exclusiveTestPolicy(t)
	paths := []string{"/call/reject", "/send/text", "/send/media", "/send/status/media", "/message/react", "/message/presence", "/message/delete", "/message/edit", "/group/create", "/group/photo", "/community/add", "/user/profilePicture", "/newsletter/create"}
	for _, path := range paths {
		for _, id := range []string{serviceTestInstance, serviceTestOther} {
			calls := 0
			r := gin.New()
			r.Use(func(c *gin.Context) { c.Set("instance", &instance_model.Instance{Id: id}) }, queuepolicy.HTTP())
			r.POST(path, func(c *gin.Context) { calls++; c.Status(200) })
			req := httptest.NewRequest("POST", path, strings.NewReader(`{"url":"http://127.0.0.1/never-fetch"`))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if id == serviceTestInstance && (w.Code != 403 || calls != 0) {
				t.Fatal("exclusive route reached handler")
			}
			if id == serviceTestOther && (w.Code != 200 || calls != 1) {
				t.Fatal("nonexclusive route changed")
			}
		}
	}
}

func TestExclusiveCapabilitiesUsesAuthenticatedUUIDAndRealGate(t *testing.T) {
	exclusiveTestPolicy(t)
	r := gin.New()
	auth := auth_middleware.NewMiddleware(&config.Config{}, queueAuthFixture{}).Auth
	RegisterRoutes(r, NewService(serviceNewLedger(), &serviceTransportFake{}), auth)
	for _, tc := range []struct {
		token     string
		exclusive string
	}{{"fixture-token-one", "true"}, {"fixture-token-two", "false"}} {
		w := queueHTTPRequest(r, "GET", "/queue/v1/capabilities", "", tc.token, Contract)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"validUntil":true`) || !strings.Contains(w.Body.String(), `"exclusiveInstance":`+tc.exclusive) {
			t.Fatalf("capabilities not scoped: %d %s", w.Code, w.Body.String())
		}
	}
	queuepolicy.Install(nil)
	w := queueHTTPRequest(r, "GET", "/queue/v1/capabilities", "", "fixture-token-one", Contract)
	if w.Code != 503 || strings.Contains(w.Body.String(), `"exclusiveInstance":true`) {
		t.Fatal("failed gate claimed exclusivity")
	}
}

func TestExclusiveRealPolicyStickyAcrossRestartAndQueueDisabled(t *testing.T) {
	_, db, _ := postgresReal(t)
	ctx := context.Background()
	id := uuid.NewString()
	other := uuid.NewString()
	check, err := queuepolicy.Initialize(ctx, db, id)
	if err != nil {
		t.Fatal(err)
	}
	if active, err := check(ctx, id); err != nil || !active {
		t.Fatal("allowlist not persisted")
	}
	// No queue service/routes are involved; removal from env is not deactivation.
	restarted, err := queuepolicy.Initialize(ctx, db, "")
	if err != nil {
		t.Fatal(err)
	}
	queuepolicy.Install(restarted)
	t.Cleanup(func() { queuepolicy.Install(func(context.Context, string) (bool, error) { return false, nil }) })
	if !errors.Is(queuepolicy.Legacy(id), queuepolicy.ErrExclusive) || queuepolicy.Legacy(other) != nil {
		t.Fatal("persisted policy lost or leaked to n8n UUID")
	}
	again, err := queuepolicy.Initialize(ctx, db, id+","+id)
	if err != nil {
		t.Fatal("idempotent activation failed")
	}
	if active, err := again(ctx, id); err != nil || !active {
		t.Fatal("repeated activation lost")
	}
}
