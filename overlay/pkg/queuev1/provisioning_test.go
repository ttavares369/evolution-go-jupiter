package queuev1

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/evolution-foundation/evolution-go/pkg/config"
	instance_model "github.com/evolution-foundation/evolution-go/pkg/instance/model"
	instance_service "github.com/evolution-foundation/evolution-go/pkg/instance/service"
	auth_middleware "github.com/evolution-foundation/evolution-go/pkg/middleware"
	"github.com/evolution-foundation/evolution-go/pkg/queuepolicy"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

const provisioningAdminKey = "fixture-existing-global-key"

func provisioningReset(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { queuepolicy.Install(func(context.Context, string) (bool, error) { return false, nil }) })
}
func provisioningAuth() auth_middleware.Middleware {
	return auth_middleware.NewMiddleware(&config.Config{GlobalApiKey: provisioningAdminKey}, queueAuthFixture{})
}
func provisioningPath(id string) string { return "/queue/v1/instances/" + id + "/exclusive" }
func provisioningMock(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
		db.Close()
	})
	return db, mock
}
func provisioningCommit(mock sqlmock.Sqlmock, id string, commitErr error) {
	mock.ExpectBegin()
	mock.ExpectExec("SELECT pg_advisory_xact_lock\\(719628120025100002\\)").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT id FROM instances.*FOR KEY SHARE").WithArgs(id).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(id))
	mock.ExpectExec("CREATE TABLE IF NOT EXISTS queue_v1_exclusive_instances").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("INSERT INTO queue_v1_exclusive_instances.*ON CONFLICT DO NOTHING").WithArgs(id).WillReturnResult(sqlmock.NewResult(0, 1))
	if commitErr != nil {
		mock.ExpectCommit().WillReturnError(commitErr)
	} else {
		mock.ExpectCommit()
	}
}

func TestProvisioningRealAuthAdminAndContractBeforeSQL(t *testing.T) {
	provisioningReset(t)
	queuepolicy.Install(func(context.Context, string) (bool, error) { return false, nil })
	db, _ := provisioningMock(t)
	r := gin.New()
	auth := provisioningAuth()
	RegisterRoutesWithAdmin(r, NewService(serviceNewLedger(), &serviceTransportFake{}), auth.Auth, db, auth.AuthAdmin)
	for _, tc := range []struct {
		token, contract string
		status          int
	}{
		{provisioningAdminKey, "", 428}, {provisioningAdminKey, "wrong", 428}, {"", Contract, 401}, {"invalid", Contract, 401}, {"fixture-token-one", Contract, 401}, {"fixture-token-two", Contract, 401},
	} {
		w := queueHTTPRequest(r, "POST", provisioningPath(serviceTestInstance), "", tc.token, tc.contract)
		if w.Code != tc.status || strings.Contains(w.Body.String(), `"exclusiveInstance":true`) {
			t.Fatalf("auth bypass: %d %s", w.Code, w.Body.String())
		}
		if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get(OptInHeader) != Contract {
			t.Fatal("missing contract/cache headers")
		}
	}
	for _, id := range []string{"invalid", "00000000-0000-0000-0000-000000000000", "10000000-0000-4000-8000-00000000000A"} {
		w := queueHTTPRequest(r, "POST", provisioningPath(id), "", provisioningAdminKey, Contract)
		if w.Code != 400 || w.Body.String() != `{"error":"invalid_instance_id"}` {
			t.Fatalf("invalid UUID accepted: %d %s", w.Code, w.Body.String())
		}
	}
	w := queueHTTPRequest(r, "GET", "/queue/v1/capabilities", "", provisioningAdminKey, Contract)
	if w.Code != 401 {
		t.Fatal("admin key substituted for per-instance auth")
	}
}

func TestProvisioningCommitAndGateProofBoundaries(t *testing.T) {
	for _, phase := range []string{"pre-gate", "missing", "commit-ambiguous", "proof-false", "proof-error", "success"} {
		t.Run(phase, func(t *testing.T) {
			provisioningReset(t)
			var checks int
			queuepolicy.Install(func(context.Context, string) (bool, error) {
				checks++
				if phase == "pre-gate" || checks > 1 && phase == "proof-error" {
					return false, queuepolicy.ErrUnavailable
				}
				return checks > 1 && phase != "proof-false", nil
			})
			db, mock := provisioningMock(t)
			auth := provisioningAuth()
			r := gin.New()
			RegisterRoutesWithAdmin(r, NewService(serviceNewLedger(), &serviceTransportFake{}), auth.Auth, db, auth.AuthAdmin)
			want := 503
			if phase == "missing" {
				mock.ExpectBegin()
				mock.ExpectExec("SELECT pg_advisory_xact_lock").WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectQuery("SELECT id FROM instances.*FOR KEY SHARE").WillReturnRows(sqlmock.NewRows([]string{"id"}))
				mock.ExpectRollback()
				want = 404
			} else if phase != "pre-gate" {
				var err error
				if phase == "commit-ambiguous" {
					err = errors.New("private-commit-token-sentinel")
				}
				provisioningCommit(mock, serviceTestInstance, err)
			}
			if phase == "success" {
				want = 200
			}
			w := queueHTTPRequest(r, "POST", provisioningPath(serviceTestInstance), "", provisioningAdminKey, Contract)
			if w.Code != want {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if want == 200 {
				if w.Body.String() != `{"contract":"queue-v1","exclusiveInstance":true,"instanceId":"`+serviceTestInstance+`"}` || checks != 2 {
					t.Fatalf("success without scoped proof: %s", w.Body.String())
				}
			} else if strings.Contains(w.Body.String(), "exclusiveInstance") || strings.Contains(w.Body.String(), "sentinel") {
				t.Fatal("failure declared true or exposed private error")
			}
			if phase == "commit-ambiguous" && checks != 1 {
				t.Fatal("ambiguous commit was converted to a success path")
			}
		})
	}
}

func TestProvisioningOptInAndAvailability(t *testing.T) {
	provisioningReset(t)
	auth := provisioningAuth()
	disabled := gin.New()
	RegisterOptional(disabled, nil, nil, auth.Auth, false, auth.AuthAdmin)
	if len(disabled.Routes()) != 0 || queueHTTPRequest(disabled, "POST", provisioningPath(serviceTestInstance), "", provisioningAdminKey, Contract).Code != 404 {
		t.Fatal("disabled contract registered provisioning")
	}
	queuepolicy.Install(func(context.Context, string) (bool, error) { return false, nil })
	db, _ := provisioningMock(t)
	for _, which := range []string{"without-admin", "without-db", "without-service", "standalone"} {
		r := gin.New()
		svc := NewService(serviceNewLedger(), &serviceTransportFake{})
		switch which {
		case "without-admin":
			RegisterRoutesWithAdmin(r, svc, auth.Auth, db, nil)
		case "without-db":
			RegisterRoutesWithAdmin(r, svc, auth.Auth, nil, auth.AuthAdmin)
		case "without-service":
			RegisterRoutesWithAdmin(r, nil, auth.Auth, db, auth.AuthAdmin)
		case "standalone":
			RegisterRoutes(r, svc, auth.Auth)
		}
		w := queueHTTPRequest(r, "POST", provisioningPath(serviceTestInstance), "", provisioningAdminKey, Contract)
		if which == "standalone" {
			if w.Code != 404 {
				t.Fatal("standalone routes claimed provisioning")
			}
		} else if w.Code != 503 {
			t.Fatal("incomplete provisioning was not closed")
		}
		w = queueHTTPRequest(r, "GET", "/queue/v1/capabilities", "", "fixture-token-one", Contract)
		if strings.Contains(w.Body.String(), `"exclusiveProvisioning":true`) {
			t.Fatal("unavailable route advertised provisioning")
		}
	}
	r := gin.New()
	RegisterRoutesWithAdmin(r, NewService(serviceNewLedger(), &serviceTransportFake{}), auth.Auth, db, auth.AuthAdmin)
	queuepolicy.Install(nil)
	w := queueHTTPRequest(r, "GET", "/queue/v1/capabilities", "", "fixture-token-one", Contract)
	if w.Code != 503 || strings.Contains(w.Body.String(), `"exclusiveProvisioning":true`) {
		t.Fatal("failed gate advertised provisioning")
	}
	if w = queueHTTPRequest(r, "POST", provisioningPath(serviceTestInstance), "", provisioningAdminKey, Contract); w.Code != 503 {
		t.Fatal("failed gate allowed provisioning")
	}
}

type provisioningInstanceAuth struct {
	instance_service.InstanceService
	id, other string
}

func (f provisioningInstanceAuth) GetInstanceByToken(token string) (*instance_model.Instance, error) {
	id := f.id
	if token == "fixture-other-token" {
		id = f.other
	} else if token != "fixture-new-token" {
		return nil, errors.New("not authorized")
	}
	return &instance_model.Instance{Id: id, Token: token}, nil
}

func TestProvisioningRealPostgresIdempotentStickyAndIsolated(t *testing.T) {
	p, db, _ := postgresReal(t)
	provisioningReset(t)
	ctx := context.Background()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS instances(id UUID PRIMARY KEY,name TEXT,token TEXT,connected BOOLEAN,qrcode TEXT)`); err != nil {
		t.Fatal(err)
	}
	id, other := uuid.NewString(), uuid.NewString()
	if _, err := db.Exec(`INSERT INTO instances(id,name,token,connected,qrcode) VALUES($1,'existing-name','existing-token',FALSE,'existing-qr')`, id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM instances WHERE id=$1`, id) })
	check, err := queuepolicy.Initialize(ctx, db, "")
	if err != nil {
		t.Fatal(err)
	}
	queuepolicy.Install(check)
	auth := auth_middleware.NewMiddleware(&config.Config{GlobalApiKey: provisioningAdminKey}, provisioningInstanceAuth{id: id, other: other})
	tr := &serviceTransportFake{}
	r := gin.New()
	RegisterRoutesWithAdmin(r, NewService(p, tr), auth.Auth, db, auth.AuthAdmin)
	cap := queueHTTPRequest(r, "GET", "/queue/v1/capabilities", "", "fixture-new-token", Contract)
	if cap.Code != 200 || !strings.Contains(cap.Body.String(), `"exclusiveProvisioning":true`) || !strings.Contains(cap.Body.String(), `"exclusiveInstance":false`) {
		t.Fatal("provisioning capability before activation", cap.Body.String())
	}
	w := queueHTTPRequest(r, "POST", provisioningPath(id), "", provisioningAdminKey, Contract)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"instanceId":"`+id+`"`) {
		t.Fatal("activation failed", w.Code, w.Body.String())
	}
	var activated string
	if err = db.QueryRow(`SELECT activated_at::text FROM queue_v1_exclusive_instances WHERE instance_id=$1`, id).Scan(&activated); err != nil {
		t.Fatal(err)
	}
	var failures atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := queueHTTPRequest(r, "POST", provisioningPath(id), "", provisioningAdminKey, Contract)
			if w.Code != 200 {
				failures.Add(1)
			}
		}()
	}
	wg.Wait()
	if failures.Load() != 0 {
		t.Fatal("concurrent activation not idempotent")
	}
	var name, token, qr, again string
	var connected bool
	if err = db.QueryRow(`SELECT name,token,connected,qrcode FROM instances WHERE id=$1`, id).Scan(&name, &token, &connected, &qr); err != nil || name != "existing-name" || token != "existing-token" || connected || qr != "existing-qr" {
		t.Fatal("provisioning changed instance/session/token")
	}
	if err = db.QueryRow(`SELECT activated_at::text FROM queue_v1_exclusive_instances WHERE instance_id=$1`, id).Scan(&again); err != nil || again != activated {
		t.Fatal("idempotent activation rewrote sticky record")
	}
	cap = queueHTTPRequest(r, "GET", "/queue/v1/capabilities", "", "fixture-new-token", Contract)
	if cap.Code != 200 || !strings.Contains(cap.Body.String(), `"exclusiveInstance":true`) {
		t.Fatal("activated gate not observable", cap.Body.String())
	}
	cap = queueHTTPRequest(r, "GET", "/queue/v1/capabilities", "", "fixture-other-token", Contract)
	if cap.Code != 200 || !strings.Contains(cap.Body.String(), `"exclusiveInstance":false`) || queuepolicy.Legacy(other) != nil {
		t.Fatal("another UUID activated implicitly")
	}
	if !errors.Is(queuepolicy.Legacy(id), queuepolicy.ErrExclusive) || tr.prepares.Load() != 0 || tr.sends.Load() != 0 || tr.presence.Load() != 0 {
		t.Fatal("gate not effective or provisioning performed transport I/O")
	}
	check, err = queuepolicy.Initialize(ctx, db, "")
	if err != nil {
		t.Fatal(err)
	}
	queuepolicy.Install(check)
	if active, err := queuepolicy.Exclusive(ctx, id); err != nil || !active {
		t.Fatal("restart lost policy")
	}
	for _, method := range []string{"DELETE", "PUT"} {
		if w = queueHTTPRequest(r, method, provisioningPath(id), "", provisioningAdminKey, Contract); w.Code != 404 {
			t.Fatal("deactivation API exists")
		}
	}
	w = queueHTTPRequest(r, "POST", provisioningPath(other), "", provisioningAdminKey, Contract)
	if w.Code != 404 || w.Body.String() != `{"error":"instance_not_found"}` {
		t.Fatal("unknown UUID activated")
	}
	if _, err = db.Exec(`DELETE FROM instances WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if w = queueHTTPRequest(r, "POST", provisioningPath(id), "", provisioningAdminKey, Contract); w.Code != 404 {
		t.Fatal("sticky replay bypassed existence validation")
	}
	if active, err := queuepolicy.Exclusive(ctx, id); err != nil || !active {
		t.Fatal("instance removal silently deactivated policy")
	}
}

func TestProvisioningOptionalProductionWiring(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	b, err := os.ReadFile(filepath.Join(root, "cmd/evolution-go/main.go"))
	if err != nil {
		t.Fatal(err)
	}
	call := `queuev1.RegisterOptional(r, db, whatsmeowService, auth_middleware.NewMiddleware(config, instanceService).Auth, os.Getenv("EVOLUTION_QUEUE_V1") == "true", auth_middleware.NewMiddleware(config, instanceService).AuthAdmin)`
	if strings.Count(string(b), call) != 1 {
		t.Fatal("production must wire existing real AuthAdmin under the explicit queue opt-in")
	}
}

func TestProvisioningRegisterOptionalRealInitialization(t *testing.T) {
	_, db, _ := postgresReal(t)
	provisioningReset(t)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS instances(id UUID PRIMARY KEY,name TEXT,token TEXT,connected BOOLEAN,qrcode TEXT)`); err != nil {
		t.Fatal(err)
	}
	check, err := queuepolicy.Initialize(context.Background(), db, "")
	if err != nil {
		t.Fatal(err)
	}
	queuepolicy.Install(check)
	orm, err := gorm.Open(postgres.New(postgres.Config{Conn: db}), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	auth := provisioningAuth()
	r := gin.New()
	RegisterOptional(r, orm, nil, auth.Auth, true, auth.AuthAdmin)
	w := queueHTTPRequest(r, "GET", "/queue/v1/capabilities", "", "fixture-token-one", Contract)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"exclusiveProvisioning":true`) {
		t.Fatal("runtime initialization did not publish provisioning", w.Code, w.Body.String())
	}
	// No WA service exists in this fixture: registration and provisioning cannot
	// start one, and missing UUID validation is SQL-only.
	w = queueHTTPRequest(r, "POST", provisioningPath(uuid.NewString()), "", provisioningAdminKey, Contract)
	if w.Code != 404 {
		t.Fatal("runtime provisioning was not registered", w.Code, w.Body.String())
	}
}
