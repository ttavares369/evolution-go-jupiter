package queuev1

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/evolution-foundation/evolution-go/pkg/config"
	instance_model "github.com/evolution-foundation/evolution-go/pkg/instance/model"
	instance_service "github.com/evolution-foundation/evolution-go/pkg/instance/service"
	auth_middleware "github.com/evolution-foundation/evolution-go/pkg/middleware"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Embed unrelated legacy methods: a queue request must never call them.
type queueAuthFixture struct {
	instance_service.InstanceService
}

func (queueAuthFixture) GetInstanceByToken(token string) (*instance_model.Instance, error) {
	switch token {
	case "fixture-token-one":
		return &instance_model.Instance{Id: serviceTestInstance, Token: token, Name: "legacy-name"}, nil
	case "fixture-token-two":
		return &instance_model.Instance{Id: serviceTestOther, Token: token}, nil
	default:
		return nil, errors.New("fixture-secret-driver-error")
	}
}
func queueHTTPRouter(service *Service) *gin.Engine {
	r := gin.New()
	// A representative pre-existing route must not inherit contract requirements.
	r.GET("/legacy", func(c *gin.Context) { c.String(200, "legacy unchanged") })
	RegisterRoutes(r, service, auth_middleware.NewMiddleware(&config.Config{}, queueAuthFixture{}).Auth)
	return r
}
func queueHTTPRequest(r http.Handler, method, path, body, token, contract string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("apikey", token)
	req.Header.Set(OptInHeader, contract)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}
func queueJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
func queueHTTPAttempt(t *testing.T, w *httptest.ResponseRecorder) Attempt {
	t.Helper()
	var payload struct {
		Attempt Attempt `json:"attempt"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	return payload.Attempt
}
func TestQueueHTTPOptInAuthAndLegacy(t *testing.T) {
	r := queueHTTPRouter(NewService(serviceNewLedger(), &serviceTransportFake{}))
	for _, tc := range []struct {
		path, token, contract string
		status                int
	}{
		{"/legacy", "", "", 200},
		{"/queue/v1/capabilities", "", "", 428},
		{"/queue/v1/capabilities", "", Contract, 401},
		{"/queue/v1/capabilities", "invalid", Contract, 401},
		{"/queue/v1/capabilities", "fixture-token-one", Contract, 200},
	} {
		w := queueHTTPRequest(r, "GET", tc.path, "", tc.token, tc.contract)
		if w.Code != tc.status {
			t.Fatalf("%s: got %d", tc.path, w.Code)
		}
		if strings.Contains(w.Body.String(), "fixture-secret") || strings.Contains(w.Body.String(), "fixture-token") {
			t.Fatal("credential in response")
		}
	}
	w := queueHTTPRequest(r, "GET", "/queue/v1/capabilities", "", "fixture-token-one", Contract)
	if w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), `"durableLedger":true`) {
		t.Fatal("capability/cache contract")
	}
	// Disabled must be inert even with nil dependencies, including no DDL.
	disabled := gin.New()
	RegisterOptional(disabled, nil, nil, nil, false)
	if len(disabled.Routes()) != 0 {
		t.Fatal("disabled routes registered")
	}
	// Failed opt-in initialization must not kill legacy service or permit sends.
	unavailable := gin.New()
	RegisterOptional(unavailable, nil, nil, auth_middleware.NewMiddleware(&config.Config{}, queueAuthFixture{}).Auth, true)
	w = queueHTTPRequest(unavailable, "GET", "/queue/v1/capabilities", "", "fixture-token-one", Contract)
	if w.Code != 503 {
		t.Fatal("failed initialization was not closed")
	}
}
func TestQueueHTTPTextImageReplayAndIsolation(t *testing.T) {
	for _, kind := range []string{"text", "image"} {
		t.Run(kind, func(t *testing.T) {
			tr := &serviceTransportFake{}
			r := queueHTTPRouter(NewService(serviceNewLedger(), tr))
			req := serviceTestRequest()
			if kind == "image" {
				req = serviceTestImage(t, "png")
			}
			req.Kind = "" // Endpoint establishes the kind, not caller-controlled dispatch.
			body := queueJSON(req)
			path := "/queue/v1/" + kind
			for i := 0; i < 3; i++ {
				w := queueHTTPRequest(r, "POST", path, body, "fixture-token-one", Contract)
				a := queueHTTPAttempt(t, w)
				if w.Code != 200 || a.State != Accepted || a.Receipt == nil || a.Receipt.MessageID != req.MessageID {
					t.Fatalf("receipt %d: %s", w.Code, w.Body.String())
				}
			}
			if tr.sends.Load() != 1 || tr.prepares.Load() != 1 {
				t.Fatal("replayed transport")
			}
			w := queueHTTPRequest(r, "GET", "/queue/v1/attempts/"+req.AttemptID, "", "fixture-token-two", Contract)
			if w.Code != 404 {
				t.Fatal("cross-instance lookup")
			}
			changed := req
			changed.To = "5511888888888@s.whatsapp.net"
			w = queueHTTPRequest(r, "POST", path, queueJSON(changed), "fixture-token-one", Contract)
			if w.Code != 409 || tr.sends.Load() != 1 {
				t.Fatal("conflict sent")
			}
			// The same identities in another authenticated instance are isolated.
			w = queueHTTPRequest(r, "POST", path, body, "fixture-token-two", Contract)
			if w.Code != 200 || tr.sends.Load() != 2 {
				t.Fatal("instance isolation")
			}
		})
	}
}
func TestQueueHTTPInvalidBodiesAndSafeErrors(t *testing.T) {
	tr := &serviceTransportFake{}
	ledger := serviceNewLedger()
	r := queueHTTPRouter(NewService(ledger, tr))
	body := queueJSON(serviceTestRequest())
	for _, bad := range []string{"null", "[]", "{}", body + "{}", strings.TrimSuffix(body, "}") + `,"ownerId":"client-secret"}`, strings.TrimSuffix(body, "}") + `,"instanceId":"client-secret"}`} {
		w := queueHTTPRequest(r, "POST", "/queue/v1/text", bad, "fixture-token-one", Contract)
		if w.Code != 400 {
			t.Fatalf("invalid status %d", w.Code)
		}
	}
	req := serviceTestRequest()
	req.Kind = "image"
	w := queueHTTPRequest(r, "POST", "/queue/v1/text", queueJSON(req), "fixture-token-one", Contract)
	if w.Code != 400 {
		t.Fatal("kind mismatch")
	}
	huge := strings.Repeat(" ", MaxBodyBytes+1)
	w = queueHTTPRequest(r, "POST", "/queue/v1/text", huge, "fixture-token-one", Contract)
	if w.Code != 413 {
		t.Fatalf("size status %d", w.Code)
	}
	ledger.d.lookupErr = errors.New("password=fixture-sensitive-provider-output")
	w = queueHTTPRequest(r, "POST", "/queue/v1/text", body, "fixture-token-one", Contract)
	if w.Code != 503 || strings.Contains(w.Body.String(), "password") || tr.sends.Load() != 0 {
		t.Fatal("unsafe error boundary")
	}
}
func TestQueueHTTPUncertainAndLostClientResponse(t *testing.T) {
	tr := &serviceTransportFake{send: func(context.Context, string) (Receipt, error) { return Receipt{}, errors.New("lost ACK") }}
	ledger := serviceNewLedger()
	r := queueHTTPRouter(NewService(ledger, tr))
	req := serviceTestRequest()
	w := queueHTTPRequest(r, "POST", "/queue/v1/text", queueJSON(req), "fixture-token-one", Contract)
	if w.Code != 202 || queueHTTPAttempt(t, w).State != Uncertain {
		t.Fatal("false success")
	}
	for i := 0; i < 3; i++ {
		w = queueHTTPRequest(r, "POST", "/queue/v1/text", queueJSON(req), "fixture-token-one", Contract)
		if w.Code != 202 {
			t.Fatal("uncertain replay")
		}
	}
	// An HTTP response lost after a committed acceptance must query/replay, not send.
	req.AttemptID = uuid.NewString()
	req.LogicalMessageID = uuid.NewString()
	req.MessageID = "3EB222"
	tr2 := &serviceTransportFake{}
	r2 := queueHTTPRouter(NewService(serviceNewLedger(), tr2))
	ctx, cancel := context.WithCancel(context.Background())
	tr2.send = func(context.Context, string) (Receipt, error) {
		cancel()
		return Receipt{MessageID: req.MessageID, AcceptedAt: serviceTestTime}, nil
	}
	request := httptest.NewRequest("POST", "/queue/v1/text", strings.NewReader(queueJSON(req))).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("apikey", "fixture-token-one")
	request.Header.Set(OptInHeader, Contract)
	r2.ServeHTTP(httptest.NewRecorder(), request) // Intentionally discard client response.
	w = queueHTTPRequest(r2, "POST", "/queue/v1/text", queueJSON(req), "fixture-token-one", Contract)
	if w.Code != 200 || tr2.sends.Load() != 1 || tr.sends.Load() != 1 {
		t.Fatal("lost response replayed")
	}
}
func TestQueueHTTPPresenceBounds(t *testing.T) {
	tr := &serviceTransportFake{}
	r := queueHTTPRouter(NewService(serviceNewLedger(), tr))
	for _, ms := range []int{7999, 8000, 26000, 26001} {
		w := queueHTTPRequest(r, "POST", "/queue/v1/presence", queueJSON(PresenceRequest{To: serviceTestRequest().To, DurationMS: ms}), "fixture-token-one", Contract)
		want := 200
		if ms < 8000 || ms > 26000 {
			want = 400
		}
		if w.Code != want {
			t.Fatalf("presence %d: %d", ms, w.Code)
		}
	}
	if tr.presence.Load() != 2 {
		t.Fatal("presence calls")
	}
}

func queueRealRequest() SendRequest {
	r := serviceTestRequest()
	r.AttemptID = uuid.NewString()
	r.LogicalMessageID = uuid.NewString()
	r.MessageID = strings.ReplaceAll(uuid.NewString(), "-", "")
	return r
}
func queueReopen(t *testing.T, dsn string) *PostgresLedger {
	t.Helper()
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(8)
	t.Cleanup(func() { db.Close() })
	p, err := NewPostgresLedger(db)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func TestQueueHTTPPostgresRealEndToEnd(t *testing.T) {
	p, db, dsn := postgresReal(t)
	instance := uuid.NewString()
	auth := func(c *gin.Context) { c.Set("instance", &instance_model.Instance{Id: instance}); c.Next() }
	router := func(p Ledger, tr Transport) *gin.Engine {
		r := gin.New()
		RegisterRoutes(r, NewService(p, tr), auth)
		return r
	}
	t.Run("concurrent_exact_replay_and_restart", func(t *testing.T) {
		tr := &serviceTransportFake{}
		r := router(p, tr)
		req := queueRealRequest()
		body := queueJSON(req)
		var wg sync.WaitGroup
		results := make(chan int, 12)
		for i := 0; i < 12; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				w := queueHTTPRequest(r, "POST", "/queue/v1/text", body, "", Contract)
				results <- w.Code
			}()
		}
		wg.Wait()
		close(results)
		for status := range results {
			if status != 200 && status != 202 && status != 422 {
				t.Fatalf("concurrent status %d", status)
			}
		}
		// A busy admission may win the durable identity before any send. Retry is
		// never automatic: only an explicit new attempt after proven rejection.
		w := queueHTTPRequest(r, "GET", "/queue/v1/attempts/"+req.AttemptID, "", "", Contract)
		a := queueHTTPAttempt(t, w)
		if a.State == Rejected {
			req.AttemptID = uuid.NewString()
			w = queueHTTPRequest(r, "POST", "/queue/v1/text", queueJSON(req), "", Contract)
			a = queueHTTPAttempt(t, w)
		}
		if a.State != Accepted || tr.sends.Load() != 1 {
			t.Fatalf("effect count/state %d %s", tr.sends.Load(), a.State)
		}
		restarted := router(queueReopen(t, dsn), tr)
		w = queueHTTPRequest(restarted, "POST", "/queue/v1/text", queueJSON(req), "", Contract)
		if w.Code != 200 || tr.sends.Load() != 1 {
			t.Fatal("restart re-sent")
		}
	})
	t.Run("no_open_transaction_during_transport", func(t *testing.T) {
		tr := &serviceTransportFake{send: func(_ context.Context, id string) (Receipt, error) {
			var n int
			if err := db.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND state='idle in transaction'`).Scan(&n); err != nil || n != 0 {
				t.Error("transaction held during external call")
			}
			return Receipt{MessageID: id, AcceptedAt: time.Now().UTC()}, nil
		}}
		w := queueHTTPRequest(router(p, tr), "POST", "/queue/v1/text", queueJSON(queueRealRequest()), "", Contract)
		if w.Code != 200 {
			t.Fatalf("send %d", w.Code)
		}
	})
	t.Run("crash_after_authorization_keeps_barrier", func(t *testing.T) {
		req := queueRealRequest()
		_, hash, err := serviceMessage(req)
		if err != nil {
			t.Fatal(err)
		}
		other := uuid.NewString()
		b := Binding{InstanceID: other, AttemptID: req.AttemptID, LogicalMessageID: req.LogicalMessageID, MessageID: req.MessageID, ContentHash: hash, OwnerID: uuid.NewString()}
		if _, run, err := p.Admit(context.Background(), b, ""); err != nil || !run {
			t.Fatal("admit")
		}
		if err := p.MarkSending(context.Background(), b); err != nil {
			t.Fatal(err)
		}
		tr := &serviceTransportFake{}
		s := NewService(queueReopen(t, dsn), tr)
		a, err := s.Send(context.Background(), other, req)
		if err != nil || a.State != Sending || tr.sends.Load() != 0 {
			t.Fatal("crash replay")
		}
		next := queueRealRequest()
		a, err = s.Send(context.Background(), other, next)
		if err != nil || a.State != Rejected || a.Code != "instance_busy" || tr.sends.Load() != 0 {
			t.Fatal("crash barrier released")
		}
	})
	t.Run("different_instances_parallel_same_instance_busy", func(t *testing.T) {
		began := make(chan struct{}, 2)
		release := make(chan struct{})
		var active, max atomic.Int32
		tr := &serviceTransportFake{send: func(_ context.Context, id string) (Receipt, error) {
			n := active.Add(1)
			for old := max.Load(); n > old && !max.CompareAndSwap(old, n); old = max.Load() {
			}
			began <- struct{}{}
			<-release
			active.Add(-1)
			return Receipt{MessageID: id, AcceptedAt: time.Now().UTC()}, nil
		}}
		s := NewService(p, tr)
		one, two := uuid.NewString(), uuid.NewString()
		results := make(chan serviceTestResult, 2)
		for _, id := range []string{one, two} {
			go func(id string) {
				a, e := s.Send(context.Background(), id, queueRealRequest())
				results <- serviceTestResult{a, e}
			}(id)
		}
		for i := 0; i < 2; i++ {
			select {
			case <-began:
			case <-time.After(5 * time.Second):
				close(release)
				t.Fatal("instances serialized")
			}
		}
		a, err := s.Send(context.Background(), one, queueRealRequest())
		if err != nil || a.State != Rejected {
			t.Error("same instance sent concurrently")
		}
		close(release)
		for i := 0; i < 2; i++ {
			result := serviceWait(t, results)
			if result.err != nil || result.a.State != Accepted {
				t.Fatal("parallel send failed")
			}
		}
		if max.Load() != 2 {
			t.Fatal("not independent")
		}
	})
	// No HTTP server or WhatsApp client is instantiated by any of these tests.
}
