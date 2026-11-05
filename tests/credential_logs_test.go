// Delivery: copy to pkg/config/credential_logs_test.go after credential-logs.patch.
// AST proof additionally needs CREDENTIAL_LOGS_BASELINE pointing to the immutable
// full baseline after PR154 + lembrai-webhook (mount it read-only when testing).
package config_test

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/evolution-foundation/evolution-go/pkg/config"
	instance_model "github.com/evolution-foundation/evolution-go/pkg/instance/model"
	instance_repository "github.com/evolution-foundation/evolution-go/pkg/instance/repository"
	logger_wrapper "github.com/evolution-foundation/evolution-go/pkg/logger"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

const credentialTestUUID = "2d5c3e50-d78f-4c09-94ec-8b82065bfb51"

// Synthetic only: no production environment, credentials or requests are used.
var credentialCanaries = []string{
	"test-only-instance-token-canary", "test-only-global-api-key-canary",
	"test-only-proxy-password-canary", "test-only-dsn-password-canary",
	"test-only-qr-session-canary", "test-only-auth-device-canary",
	"test-only-crypto-key-canary", "test-only-webhook-response-canary",
}

func assertNoCredentials(t *testing.T, output string) {
	t.Helper()
	for _, secret := range credentialCanaries {
		if strings.Contains(output, secret) {
			t.Fatal("a synthetic credential reached the diagnostic sink")
		}
	}
}

type credentialHostile struct{ calls *int }

func (h credentialHostile) Error() string  { *h.calls++; return strings.Join(credentialCanaries, " ") }
func (h credentialHostile) String() string { *h.calls++; return strings.Join(credentialCanaries, " ") }
func (h credentialHostile) Format(s fmt.State, _ rune) {
	*h.calls++
	io.WriteString(s, strings.Join(credentialCanaries, " "))
}

func TestCredentialLogsBothSinks(t *testing.T) {
	t.Setenv("DEBUG_ENABLED", "1")
	var console bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&console)
	t.Cleanup(func() { log.SetOutput(oldOutput) })
	cfg := &config.Config{LogDirectory: t.TempDir(), LogMaxSize: 1}
	manager := logger_wrapper.NewLoggerManager(cfg)
	logger := manager.GetLogger(credentialTestUUID)
	t.Cleanup(func() { _ = logger.Close() })
	calls := 0
	values := []interface{}{
		errors.New(strings.Join(credentialCanaries, " ")),
		credentialHostile{&calls},
		&config.Config{GlobalApiKey: credentialCanaries[1], PostgresPassword: credentialCanaries[3], ProxyPassword: credentialCanaries[2]},
		&instance_model.Instance{Token: credentialCanaries[0], Qrcode: credentialCanaries[4], Proxy: credentialCanaries[2]},
		map[string]interface{}{"device": credentialCanaries[5], "key": credentialCanaries[6]},
		[]byte(credentialCanaries[6]),
		struct{ AuthDevice, CryptoKey string }{credentialCanaries[5], credentialCanaries[6]},
	}
	levels := []struct {
		name  string
		write func(string, ...interface{})
	}{
		{"INFO", logger.LogInfo}, {"WARN", logger.LogWarn},
		{"ERROR", logger.LogError}, {"DEBUG", logger.LogDebug},
	}
	for _, level := range levels {
		for _, value := range values {
			level.write("[%s] event=credential_audit attempt=%d diagnostic=%+v", credentialTestUUID, 2, value)
		}
	}
	if calls != 0 {
		t.Fatal("logger invoked a payload formatter")
	}
	data, err := os.ReadFile(filepath.Join(cfg.LogDirectory, credentialTestUUID, "instance.log"))
	if err != nil {
		t.Fatal(err)
	}
	assertNoCredentials(t, console.String())
	assertNoCredentials(t, string(data))
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	if len(lines) != len(levels)*len(values) {
		t.Fatal("diagnostics were discarded")
	}
	for _, line := range lines {
		var entry logger_wrapper.LogEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatal(err)
		}
		if entry.InstanceId != credentialTestUUID || !strings.Contains(entry.Message, "event=credential_audit attempt=2") {
			t.Fatal("instance/event/attempt diagnostics changed")
		}
	}
	for _, level := range levels {
		if !bytes.Contains(data, []byte(`"level":"`+level.name+`"`)) || !strings.Contains(console.String(), level.name[:3]) {
			t.Fatalf("missing severity %s", level.name)
		}
	}
}

func TestCredentialLogsDefaultORMErrorRegressionFixture(t *testing.T) {
	// The old path interpolates SQL even at Error level; ParameterizedQueries
	// alone would also leave driver error strings and literal SQL unprotected.
	var vulnerableOutput bytes.Buffer
	stock := gormlogger.New(log.New(&vulnerableOutput, "", 0), gormlogger.Config{LogLevel: gormlogger.Error})
	fc := func() (string, int64) {
		return "SELECT * FROM instances WHERE token = '" + credentialCanaries[0] + "'", 0
	}
	err := errors.New("driver echoed password=" + credentialCanaries[3])
	stock.Trace(context.Background(), time.Now(), fc, err)
	if !strings.Contains(vulnerableOutput.String(), credentialCanaries[0]) || !strings.Contains(vulnerableOutput.String(), credentialCanaries[3]) {
		t.Fatal("synthetic fixture did not reproduce the default ORM leak")
	}
	var safeOutput bytes.Buffer
	config.NewSafeORMLogger(&safeOutput, gormlogger.Error).Trace(context.Background(), time.Now(), fc, err)
	assertNoCredentials(t, safeOutput.String())
	if !strings.Contains(safeOutput.String(), `"event":"query_error"`) {
		t.Fatal("safe error diagnostic missing")
	}
}

func TestCredentialLogsORMModesAndLazySQL(t *testing.T) {
	for _, level := range []gormlogger.LogLevel{gormlogger.Silent, gormlogger.Error, gormlogger.Warn, gormlogger.Info} {
		t.Run(fmt.Sprint(level), func(t *testing.T) {
			var output bytes.Buffer
			logger := config.NewSafeORMLogger(&output, level)
			calls := 0
			hostile := credentialHostile{&calls}
			ctx := context.Background()
			fc := func() (string, int64) {
				t.Error("ORM logger evaluated SQL/parameters")
				return strings.Join(credentialCanaries, " "), 1
			}
			logger.Info(ctx, credentialCanaries[0]+" %v", hostile)
			logger.Warn(ctx, credentialCanaries[1]+" %v", hostile)
			logger.Error(ctx, credentialCanaries[2]+" %v", hostile)
			logger.Trace(ctx, time.Now(), fc, hostile)
			logger.Trace(ctx, time.Now().Add(-time.Second), fc, nil)
			logger.Trace(ctx, time.Now(), fc, nil)
			assertNoCredentials(t, output.String())
			if calls != 0 {
				t.Fatal("ORM invoked a driver error formatter")
			}
			if level == gormlogger.Silent && output.Len() != 0 {
				t.Fatal("silent logger emitted diagnostics")
			}
			if level >= gormlogger.Error && !strings.Contains(output.String(), `"event":"query_error"`) {
				t.Fatal("query error diagnostic missing")
			}
			if level >= gormlogger.Warn && !strings.Contains(output.String(), `"event":"slow_query"`) {
				t.Fatal("slow query diagnostic missing")
			}
			if level == gormlogger.Info && !strings.Contains(output.String(), `"event":"query"`) {
				t.Fatal("info query diagnostic missing")
			}
			before := output.Len()
			logger.LogMode(gormlogger.Silent).Error(ctx, credentialCanaries[0], hostile)
			if output.Len() != before {
				t.Fatal("LogMode did not respect Silent")
			}
			logger.Error(ctx, credentialCanaries[0], hostile)
			if level >= gormlogger.Error && output.Len() == before {
				t.Fatal("LogMode mutated original logger")
			}
			filter := logger.(interface {
				ParamsFilter(context.Context, string, ...interface{}) (string, []interface{})
			})
			query, params := filter.ParamsFilter(ctx, "SELECT '"+credentialCanaries[0]+"'", hostile)
			assertNoCredentials(t, query)
			if len(params) != 0 {
				t.Fatal("log parameter filter retained parameters")
			}
		})
	}
}

// In-memory database/sql driver: exercise the real repository and GORM callbacks
// without a database, network, HTTP listener or server process.
type credentialDBState struct {
	query string
	args  []driver.NamedValue
	err   error
	empty bool
}
type credentialConnector struct{ state *credentialDBState }

func (c credentialConnector) Connect(context.Context) (driver.Conn, error) {
	return credentialConn{c.state}, nil
}
func (c credentialConnector) Driver() driver.Driver { return credentialDriver{} }

type credentialDriver struct{}

func (credentialDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("use test connector")
}

type credentialConn struct{ state *credentialDBState }

func (credentialConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (credentialConn) Close() error              { return nil }
func (credentialConn) Begin() (driver.Tx, error) { return nil, errors.New("unexpected transaction") }
func (c credentialConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.state.query, c.state.args = query, append([]driver.NamedValue(nil), args...)
	if c.state.err != nil {
		return nil, c.state.err
	}
	return &credentialRows{done: c.state.empty}, nil
}
func (c credentialConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.state.query, c.state.args = query, append([]driver.NamedValue(nil), args...)
	return driver.RowsAffected(1), c.state.err
}

type credentialRows struct{ done bool }

func (*credentialRows) Columns() []string { return []string{"id", "token"} }
func (*credentialRows) Close() error      { return nil }
func (r *credentialRows) Next(values []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	values[0], values[1] = credentialTestUUID, credentialCanaries[0]
	return nil
}

func TestCredentialLogsRealGetInstanceByToken(t *testing.T) {
	for _, outcome := range []string{"found", "not_found", "driver_error"} {
		t.Run(outcome, func(t *testing.T) {
			state := &credentialDBState{empty: outcome == "not_found"}
			if outcome == "driver_error" {
				state.err = errors.New("password=" + credentialCanaries[3] + " token=" + credentialCanaries[0])
			}
			sqlDB := sql.OpenDB(credentialConnector{state})
			t.Cleanup(func() { _ = sqlDB.Close() })
			var output bytes.Buffer
			db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{
				Logger:               config.NewSafeORMLogger(&output, gormlogger.Info),
				DisableAutomaticPing: true, SkipDefaultTransaction: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			repo := instance_repository.NewInstanceRepository(db)
			instance, err := repo.GetInstanceByToken(credentialCanaries[0])
			switch outcome {
			case "found":
				if err != nil || instance == nil || instance.Token != credentialCanaries[0] {
					t.Fatal("repository success result changed")
				}
			case "not_found":
				if !errors.Is(err, gorm.ErrRecordNotFound) || instance != nil {
					t.Fatal("record-not-found semantics changed")
				}
			case "driver_error":
				if err != state.err || instance != nil {
					t.Fatal("driver error identity changed")
				}
			}
			if !strings.Contains(state.query, "token = $1") || strings.Contains(state.query, credentialCanaries[0]) || len(state.args) != 2 || state.args[0].Value != credentialCanaries[0] || fmt.Sprint(state.args[1].Value) != "1" {
				t.Fatal("query/parameter execution changed")
			}
			assertNoCredentials(t, output.String())
			if strings.Contains(output.String(), "SELECT") {
				t.Fatal("ORM emitted SQL")
			}
			// Write paths carry QR/proxy credentials as bound parameters, unchanged.
			for _, update := range []struct{ field, value string }{{"qrcode", credentialCanaries[4]}, {"proxy", credentialCanaries[2]}} {
				state.err = errors.New("driver echoed " + update.value)
				err := db.Model(&instance_model.Instance{}).Where("token = ?", credentialCanaries[0]).Update(update.field, update.value).Error
				if err != state.err {
					t.Fatal("write error identity changed")
				}
				if len(state.args) != 2 || state.args[0].Value != update.value || state.args[1].Value != credentialCanaries[0] {
					t.Fatal("write parameters changed")
				}
				assertNoCredentials(t, output.String())
			}
		})
	}
}

var credentialOwnedFiles = []string{
	"cmd/evolution-go/main.go",
	"pkg/config/config.go", "pkg/core/c0.go", "pkg/logger/logger.go",
	"pkg/utils/utils.go", "pkg/whatsmeow/service/whatsmeow.go",
	"pkg/user/service/user_service.go", "pkg/storage/minio/media_storage.go",
	"pkg/sendMessage/service/send_service.go", "pkg/instance/repository/instance_repository.go",
	"pkg/instance/service/instance_service.go", "pkg/events/nats/nats_producer.go",
	"pkg/events/rabbitmq/rabbitmq_producer.go", "pkg/events/webhook/webhook_producer.go",
}

func credentialSourceRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate source tree")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
}

func credentialLogCall(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	switch sel.Sel.Name {
	case "LogInfo", "LogWarn", "LogError", "LogDebug", "LogFatal":
		return true
	case "Printf", "Println", "Fatalf":
		id, ok := sel.X.(*ast.Ident)
		return ok && (id.Name == "fmt" || id.Name == "log")
	}
	return false
}

func TestCredentialLogsNoSensitiveCallArguments(t *testing.T) {
	root := credentialSourceRoot(t)
	for _, path := range credentialOwnedFiles {
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, path), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !credentialLogCall(call) {
				return true
			}
			for _, arg := range call.Args {
				ast.Inspect(arg, func(n ast.Node) bool {
					if sel, ok := n.(*ast.SelectorExpr); ok {
						switch strings.ToLower(sel.Sel.Name) {
						case "token", "globalapikey", "apikey", "password", "proxypassword", "postgrespassword", "postgresusersdb", "qrcode", "publickey", "privatekey", "secretkey", "url", "registerurl", "mediahandle", "code", "error":
							// Passing an error object is safe in Logger's two sinks; calling
							// Error() first would bypass the object guard.
							if sel.Sel.Name != "Error" {
								t.Errorf("%s: credential-bearing log argument %s", path, sel.Sel.Name)
							}
						}
					}
					if nested, ok := n.(*ast.CallExpr); ok {
						if sel, ok := nested.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Error" {
							t.Errorf("%s: preformatted error in log", path)
						}
					}
					if id, ok := n.(*ast.Ident); ok {
						switch id.Name {
						case "token", "globalApiKey", "responseBody", "jsonEvt", "VCstring", "openURL", "publicBase", "buttonText", "buttonId", "msg", "code", "presignedURL":
							t.Errorf("%s: raw sensitive log argument %s", path, id.Name)
						}
					}
					return true
				})
			}
			return false
		})
	}
}

// This is the ONLY additive queue wiring permitted by the main AST proof.
// Compare canonical pretty-printed AST before removing the expression statement.
const credentialOptionalQueueCall = `queuev1.RegisterOptional(r, db, whatsmeowService, auth_middleware.NewMiddleware(config, instanceService).Auth, os.Getenv("EVOLUTION_QUEUE_V1") == "true")`

const credentialSafeHTTPUse = `r.Use(logger_wrapper.CredentialSafeHTTP(), logger_wrapper.CredentialSafeRecovery())`

func credentialPretty(node ast.Node) string {
	var output bytes.Buffer
	if err := format.Node(&output, token.NewFileSet(), node); err != nil {
		panic(err)
	}
	return output.String()
}

func credentialIsQueueCall(node ast.Node) bool {
	call, ok := node.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "RegisterOptional" {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "queuev1"
}

// Main gets no broad logging/assignment/config normalization: all declarations
// must compare unchanged except the exact invalid-token diagnostic, the exact
// gin.Default -> gin.New + safe middleware pair, and one optional queue ExprStmt.
func credentialNormalizeMain(file *ast.File) error {
	var setup *ast.FuncDecl
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "setupRouter" && fn.Recv == nil {
			if setup != nil {
				return errors.New("duplicate setupRouter")
			}
			setup = fn
		}
	}
	if setup == nil || setup.Body == nil {
		return errors.New("setupRouter missing")
	}
	var allowed *ast.ExprStmt
	for _, stmt := range setup.Body.List {
		if expr, ok := stmt.(*ast.ExprStmt); ok && credentialIsQueueCall(expr.X) {
			if allowed != nil {
				return errors.New("duplicate optional queue registration")
			}
			if credentialPretty(expr.X) != credentialOptionalQueueCall {
				return errors.New("optional queue registration differs from exact contract")
			}
			allowed = expr
		}
	}
	var invalid bool
	ast.Inspect(file, func(node ast.Node) bool {
		if credentialIsQueueCall(node) && (allowed == nil || node != allowed.X) {
			invalid = true
		}
		return true
	})
	if invalid {
		return errors.New("optional queue registration must be one top-level ExprStmt in setupRouter")
	}
	var engine *ast.AssignStmt
	var safeUse *ast.ExprStmt
	var safeUseCall *ast.CallExpr
	for index, stmt := range setup.Body.List {
		switch credentialPretty(stmt) {
		case "r := gin.Default()", "r := gin.New()":
			if engine != nil {
				return errors.New("duplicate Gin engine initialization")
			}
			engine = stmt.(*ast.AssignStmt)
			if credentialPretty(stmt) == "r := gin.New()" {
				if index+1 >= len(setup.Body.List) || credentialPretty(setup.Body.List[index+1]) != credentialSafeHTTPUse {
					return errors.New("gin.New must be immediately followed by the exact safe HTTP/recovery installation")
				}
				safeUse = setup.Body.List[index+1].(*ast.ExprStmt)
				safeUseCall = safeUse.X.(*ast.CallExpr)
			}
		}
	}
	if engine == nil {
		return errors.New("exact Gin engine initialization missing")
	}
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if ok && id.Name == "logger_wrapper" && (sel.Sel.Name == "CredentialSafeHTTP" || sel.Sel.Name == "CredentialSafeRecovery") {
			if safeUseCall == nil || (call != safeUseCall.Args[0] && call != safeUseCall.Args[1]) {
				invalid = true
			}
		}
		return true
	})
	if invalid {
		return errors.New("safe HTTP middleware must occur only in the exact top-level installation")
	}
	// Validate both exceptions before mutating anything. Keep the original
	// engine assignment and normalize only the constructor plus that exact Use.
	originalConstructor, err := parser.ParseExpr("gin.Default()")
	if err != nil {
		return err
	}
	engine.Rhs[0] = originalConstructor
	var statements []ast.Stmt
	for _, stmt := range setup.Body.List {
		if stmt != allowed && stmt != safeUse {
			statements = append(statements, stmt)
		}
	}
	setup.Body.List = statements
	// Only these exact diagnostic pairs may differ elsewhere in main. Fatal
	// remains Fatalf (same exit); no returned error or control flow is normalized.
	pairs := [][2]string{
		{`logger.LogError("Token inválido: %s", token)`, `logger.LogError("Token inválido")`},
		{`log.Fatal(err)`, `log.Fatalf("startup failed: %T", err)`},
		{`log.Fatal("Failed to migrate runtime_configs: ", err)`, `log.Fatalf("Failed to migrate runtime_configs: %T", err)`},
		{`logger.LogError("Failed to create global RabbitMQ queues: %v", err)`, `logger.LogError("Failed to create global RabbitMQ queues: %T", err)`},
		{`logger.LogWarn("Auto-setup auth DB failed (will try connecting anyway): %v", err)`, `logger.LogWarn("Auto-setup auth DB failed (will try connecting anyway): %T", err)`},
		{`logger.LogError("Failed to connect to RabbitMQ, err: %v", err)`, `logger.LogError("Failed to connect to RabbitMQ, err: %T", err)`},
		{`logger.LogError("Failed to close RabbitMQ connection, err: %v", err)`, `logger.LogError("Failed to close RabbitMQ connection, err: %T", err)`},
		{`log.Fatalf("server error: %v", err)`, `log.Fatalf("server error: %T", err)`},
		{`logger.LogError("[SHUTDOWN] Server forced to shutdown: %v", err)`, `logger.LogError("[SHUTDOWN] Server forced to shutdown: %T", err)`},
	}
	ast.Inspect(file, func(node ast.Node) bool {
		if expr, ok := node.(*ast.ExprStmt); ok {
			for index, pair := range pairs {
				if text := credentialPretty(expr.X); text == pair[0] || text == pair[1] {
					expr.X = ast.NewIdent(fmt.Sprintf("credentialMainDiagnostic%d", index))
					return false
				}
			}
		}
		return true
	})
	return nil
}

func credentialMainFixture(input string) (string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", input, parser.SkipObjectResolution)
	if err != nil {
		return "", err
	}
	if err := credentialNormalizeMain(file); err != nil {
		return "", err
	}
	var declarations []ast.Decl
	for _, decl := range file.Decls {
		if d, ok := decl.(*ast.GenDecl); ok && d.Tok == token.IMPORT {
			continue
		}
		declarations = append(declarations, decl)
	}
	file.Decls = declarations
	return credentialPretty(file), nil
}

func TestCredentialLogsHTTPInstallationASTContract(t *testing.T) {
	fixture := func(installation string) string {
		return `package main
func setupRouter() { ` + installation + `; legacyCORS(); legacyRoutes() }
func untouched() { keep() }`
	}
	want, err := credentialMainFixture(fixture("r := gin.Default()"))
	if err != nil {
		t.Fatal(err)
	}
	exact := "r := gin.New(); " + credentialSafeHTTPUse
	for _, installation := range []string{exact, exact + "; " + credentialOptionalQueueCall} {
		got, err := credentialMainFixture(fixture(installation))
		if err != nil || got != want {
			t.Fatalf("exact HTTP installation rejected: %v", err)
		}
	}
	for i, installation := range []string{
		"r := gin.New()",
		"r := gin.New(); r.Use(logger_wrapper.CredentialSafeRecovery(), logger_wrapper.CredentialSafeHTTP())",
		"r := gin.New(); r.Use(logger_wrapper.CredentialSafeHTTP())",
		"r := gin.New(); r.Use(logger_wrapper.CredentialSafeRecovery())",
		"r := gin.New(); r.Use(logger_wrapper.CredentialSafeHTTP(), logger_wrapper.CredentialSafeRecovery(), extra())",
		"r := gin.New(); r.Use(logger_wrapper.CredentialSafeHTTP(config), logger_wrapper.CredentialSafeRecovery())",
		"r := gin.New(); legacyCORS(); " + credentialSafeHTTPUse,
		"r := gin.Default(); " + credentialSafeHTTPUse,
		exact + "; " + credentialSafeHTTPUse,
		exact + "; r.Use(gin.Logger())",
		exact + "; r.Use(gin.Recovery())",
		"r := gin.New(); if flag { " + credentialSafeHTTPUse + " }",
		"r := gin.New(); go " + credentialSafeHTTPUse,
		"r := gin.New(); defer " + credentialSafeHTTPUse,
		strings.Replace(exact, "r.Use", "other.Use", 1),
		strings.Replace(exact, "logger_wrapper.CredentialSafeHTTP", "other.CredentialSafeHTTP", 1),
		strings.Replace(exact, "r :=", "r =", 1),
	} {
		got, err := credentialMainFixture(fixture(installation))
		if err == nil && got == want {
			t.Errorf("invalid HTTP installation %d was normalized", i)
		}
	}
	for _, changed := range []string{
		strings.Replace(fixture(exact), "legacyCORS()", "changedCORS()", 1),
		strings.Replace(fixture(exact), "legacyRoutes()", "changedRoutes()", 1),
		strings.Replace(fixture(exact), "keep()", "changedAuth()", 1),
		fixture(exact) + "\nfunc elsewhere() { " + credentialSafeHTTPUse + " }",
	} {
		got, err := credentialMainFixture(changed)
		if err == nil && got == want {
			t.Fatal("HTTP normalization hid a non-diagnostic change")
		}
	}
}

func TestCredentialLogsOptionalQueueASTContract(t *testing.T) {
	before := `package main
func setupRouter() { r := gin.Default(); logger.LogError("Token inválido: %s", token); legacy() }
func untouched() { keep() }`
	want, err := credentialMainFixture(before)
	if err != nil {
		t.Fatal(err)
	}
	fixture := func(statement string) string {
		return `package main
import "github.com/evolution-foundation/evolution-go/pkg/queuev1"
func setupRouter() { r := gin.New(); ` + credentialSafeHTTPUse + `; logger.LogError("Token inválido"); ` + statement + `; legacy() }
func untouched() { keep() }`
	}
	for _, statement := range []string{"", credentialOptionalQueueCall, strings.ReplaceAll(credentialOptionalQueueCall, ", ", ",\n")} {
		got, err := credentialMainFixture(fixture(statement))
		if err != nil || got != want {
			t.Fatalf("exact optional wiring/import not accepted: %v", err)
		}
	}
	invalid := []string{
		strings.Replace(credentialOptionalQueueCall, "(r,", "(other,", 1),
		strings.Replace(credentialOptionalQueueCall, ", db,", ", authDB,", 1),
		strings.Replace(credentialOptionalQueueCall, "whatsmeowService,", "otherService,", 1),
		strings.Replace(credentialOptionalQueueCall, "NewMiddleware(config, instanceService)", "NewMiddleware(instanceService, config)", 1),
		strings.Replace(credentialOptionalQueueCall, ".Auth,", ".AuthAdmin,", 1),
		strings.Replace(credentialOptionalQueueCall, "EVOLUTION_QUEUE_V1", "OTHER_FLAG", 1),
		strings.Replace(credentialOptionalQueueCall, `== "true"`, `!= "true"`, 1),
		strings.Replace(credentialOptionalQueueCall, `os.Getenv("EVOLUTION_QUEUE_V1") == "true"`, "true", 1),
		"go " + credentialOptionalQueueCall,
		"defer " + credentialOptionalQueueCall,
		"result := " + credentialOptionalQueueCall,
		"if enabled { " + credentialOptionalQueueCall + " }",
		"func() { " + credentialOptionalQueueCall + " }()",
		credentialOptionalQueueCall + "; " + credentialOptionalQueueCall,
		strings.Replace(credentialOptionalQueueCall, "queuev1.RegisterOptional", "other.RegisterOptional", 1),
	}
	for i, statement := range invalid {
		got, err := credentialMainFixture(fixture(statement))
		if err == nil && got == want {
			t.Errorf("invalid registration variant %d was normalized", i)
		}
	}
	outside := fixture("") + "\nfunc elsewhere() { " + credentialOptionalQueueCall + " }"
	if _, err := credentialMainFixture(outside); err == nil {
		t.Fatal("registration outside setupRouter was accepted")
	}
	for _, changed := range []string{
		strings.Replace(fixture(credentialOptionalQueueCall), "legacy()", "changedEndpoint()", 1),
		strings.Replace(fixture(credentialOptionalQueueCall), "keep()", "changedAuth()", 1),
		fixture(credentialOptionalQueueCall) + "\nfunc added() {}",
		strings.Replace(fixture(credentialOptionalQueueCall), "func setupRouter()", "func setupRouter(changed bool)", 1),
	} {
		got, err := credentialMainFixture(changed)
		if err == nil && got == want {
			t.Fatal("non-diagnostic main change was hidden by normalization")
		}
	}
}

// Regression fixture reproducing the old Gin leak, not the new safe middleware.
// Requests are served entirely in memory; no listener or real token is used.
func TestCredentialLogsAuditGinDefaultExposure(t *testing.T) {
	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(oldMode) })
	var access, recovery bytes.Buffer
	router := gin.New()
	router.Use(gin.LoggerWithWriter(&access), gin.RecoveryWithWriter(&recovery))
	router.GET("/ws", func(c *gin.Context) { c.JSON(http.StatusUnauthorized, gin.H{"error": "Token inválido"}) })
	router.GET("/passkey-ceremony/:token", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	router.POST("/passkey-ceremony/:token/response", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	router.POST("/passkey-ceremony/:token/confirm", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	router.GET("/panic", func(c *gin.Context) { panic(errors.New(credentialCanaries[6])) })
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/ws?token=" + credentialCanaries[0], http.StatusUnauthorized},
		{http.MethodGet, "/passkey-ceremony/" + credentialCanaries[4], http.StatusNoContent},
		{http.MethodPost, "/passkey-ceremony/" + credentialCanaries[4] + "/response", http.StatusNoContent},
		{http.MethodPost, "/passkey-ceremony/" + credentialCanaries[4] + "/confirm", http.StatusNoContent},
	} {
		access.Reset()
		request := httptest.NewRequest(tc.method, tc.path, nil)
		originalURI := request.RequestURI
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != tc.status || request.RequestURI != originalURI {
			t.Fatal("audit fixture changed request/response semantics")
		}
		if !strings.Contains(access.String(), credentialCanaries[0]) && !strings.Contains(access.String(), credentialCanaries[4]) {
			t.Fatal("fixture did not reproduce Gin's raw URL/path exposure")
		}
	}
	// Debug recovery dumps request headers; release recovery still prints panic
	// payloads. Neither is protected by the application Logger argument guard.
	gin.SetMode(gin.DebugMode)
	request := httptest.NewRequest(http.MethodGet, "/panic?token="+credentialCanaries[0], nil)
	request.Header.Set("apikey", credentialCanaries[1])
	request.Header.Set("Cookie", "session="+credentialCanaries[5])
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatal("recovery behavior changed")
	}
	for _, index := range []int{0, 1, 5, 6} {
		if !strings.Contains(recovery.String(), credentialCanaries[index]) {
			t.Fatal("fixture did not reproduce Gin recovery exposure")
		}
	}
}

// Normalize only permitted diagnostic changes, not control flow, returns, token
// accesses, response construction, queries, goroutines, retry or lifecycle state.
func credentialNormalize(file *ast.File) {
	ast.Inspect(file, func(n ast.Node) bool {
		if block, ok := n.(*ast.BlockStmt); ok {
			var statements []ast.Stmt
			for _, stmt := range block.List {
				if assign, ok := stmt.(*ast.AssignStmt); ok && len(assign.Rhs) == 1 {
					if call, ok := assign.Rhs[0].(*ast.CallExpr); ok {
						if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "safeLogArgs" {
							continue
						}
					}
				}
				statements = append(statements, stmt)
			}
			block.List = statements
		}
		if expr, ok := n.(*ast.ExprStmt); ok && credentialLogCall(expr.X) {
			expr.X = ast.NewIdent("credentialDiagnostic")
			return false
		}
		if assign, ok := n.(*ast.AssignStmt); ok {
			for _, lhs := range assign.Lhs {
				if id, ok := lhs.(*ast.Ident); ok && (id.Name == "responseBody" || id.Name == "jsonEvt") {
					id.Name = "_"
				}
			}
		}
		if composite, ok := n.(*ast.CompositeLit); ok {
			if sel, ok := composite.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Config" {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "gorm" {
					var fields []ast.Expr
					for _, field := range composite.Elts {
						if kv, ok := field.(*ast.KeyValueExpr); ok {
							if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Logger" {
								continue
							}
						}
						fields = append(fields, field)
					}
					composite.Elts = fields
				}
			}
		}
		return true
	})
}

func credentialDeclKey(decl ast.Decl) string {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		key := d.Name.Name
		if d.Recv != nil {
			var b bytes.Buffer
			_ = ast.Fprint(&b, nil, d.Recv.List[0].Type, func(_ string, v reflect.Value) bool { return v.Type() != reflect.TypeOf(token.Pos(0)) })
			key = b.String() + key
		}
		return key
	case *ast.GenDecl:
		if d.Tok == token.IMPORT {
			return ""
		}
		if len(d.Specs) > 0 {
			switch spec := d.Specs[0].(type) {
			case *ast.TypeSpec:
				return "type:" + spec.Name.Name
			case *ast.ValueSpec:
				return "value:" + spec.Names[0].Name
			}
		}
	}
	return "unknown"
}

func credentialASTDeclarations(t *testing.T, path string) map[string]string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(filepath.ToSlash(path), "/cmd/evolution-go/main.go") {
		if err := credentialNormalizeMain(file); err != nil {
			t.Fatal(err)
		}
	} else {
		credentialNormalize(file)
	}
	decls := map[string]string{}
	for _, decl := range file.Decls {
		key := credentialDeclKey(decl)
		if key == "" {
			continue
		}
		var output bytes.Buffer
		if err := ast.Fprint(&output, nil, decl, func(name string, v reflect.Value) bool {
			return v.Type() != reflect.TypeOf(token.Pos(0)) && name != "Doc" && name != "Comment" && name != "Obj"
		}); err != nil {
			t.Fatal(err)
		}
		if _, duplicate := decls[key]; duplicate {
			t.Fatalf("duplicate declaration %s in %s", key, path)
		}
		decls[key] = output.String()
	}
	return decls
}

// The exclusivity/IDOR patch is a separate, explicit functional exception.
// Reverse exactly that reviewed patch in a disposable Go-only snapshot, then
// retain the original strict diagnostic AST/byte proof (no broad normalization).
func credentialBeforeExclusivePatch(t *testing.T, root string) string {
	t.Helper()
	patch := os.Getenv("EXCLUSIVE_INSTANCES_PATCH")
	if patch == "" {
		t.Fatal("EXCLUSIVE_INSTANCES_PATCH required with CREDENTIAL_LOGS_BASELINE")
	}
	patch, err := filepath.Abs(patch)
	if err != nil {
		t.Fatal(err)
	}
	copy := t.TempDir()
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		dest := filepath.Join(copy, rel)
		if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dest, data, 0600)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"apply", "--reverse", "--check", patch}, {"apply", "--reverse", patch}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = copy
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("exact functional patch cannot be reversed: %v: %s", err, output)
		}
	}
	return copy
}

func TestCredentialLogsLegacyASTProof(t *testing.T) {
	baseline := os.Getenv("CREDENTIAL_LOGS_BASELINE")
	if baseline == "" {
		t.Skip("mount immutable baseline and set CREDENTIAL_LOGS_BASELINE for AST proof")
	}
	root := credentialBeforeExclusivePatch(t, credentialSourceRoot(t))
	owned := map[string]bool{}
	for _, path := range credentialOwnedFiles {
		owned[path] = true
		before := credentialASTDeclarations(t, filepath.Join(baseline, path))
		after := credentialASTDeclarations(t, filepath.Join(root, path))
		for key, want := range before {
			if after[key] != want {
				t.Errorf("%s: non-diagnostic AST changed in %s", path, key)
			}
			delete(after, key)
		}
		for key := range after {
			if path == "pkg/config/config.go" && (key == "type:credentialSafeORMLogger" || key == "NewSafeORMLogger" || strings.Contains(key, `Name: "credentialSafeORMLogger"`)) {
				continue
			}
			if path == "pkg/logger/logger.go" && key == "safeLogArgs" {
				continue
			}
			t.Errorf("%s: unexpected new declaration %s", path, key)
		}
	}
	// Every other pre-existing Go file, including all handlers, auth middleware,
	// routes, repositories and models, must remain byte-identical. Additive
	// files belonging to other workstreams are intentionally not traversed.
	err := filepath.WalkDir(baseline, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, err := filepath.Rel(baseline, path)
		if err != nil {
			return err
		}
		if owned[rel] {
			return nil
		}
		before, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		after, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			return err
		}
		if !bytes.Equal(before, after) {
			t.Errorf("out-of-scope pre-existing Go file changed: %s", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
