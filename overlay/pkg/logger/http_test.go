package logger

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/gin-gonic/gin"
)

var httpCredentialCanaries = []string{
	"synthetic-query-token-canary", "synthetic-path-token-canary",
	"synthetic-apikey-canary", "synthetic-cookie-canary",
	"synthetic-body-canary", "synthetic-private-error-canary",
	"synthetic-panic-canary", "synthetic-client-ip-canary",
}

func captureHTTPDiagnostics(t *testing.T, mode string) *bytes.Buffer {
	t.Helper()
	output := &bytes.Buffer{}
	oldOutput, oldWriter, oldErrorWriter, oldMode := log.Writer(), gin.DefaultWriter, gin.DefaultErrorWriter, gin.Mode()
	log.SetOutput(output)
	gin.DefaultWriter, gin.DefaultErrorWriter = output, output
	gin.SetMode(mode)
	t.Cleanup(func() {
		log.SetOutput(oldOutput)
		gin.DefaultWriter, gin.DefaultErrorWriter = oldWriter, oldErrorWriter
		gin.SetMode(oldMode)
	})
	return output
}

func assertSafeHTTPDiagnostics(t *testing.T, output string) {
	t.Helper()
	for _, secret := range httpCredentialCanaries {
		if strings.Contains(output, secret) {
			t.Fatal("synthetic credential reached HTTP diagnostics")
		}
	}
	for _, forbidden := range []string{"[Recovery]", "goroutine ", "Authorization:", "Cookie:", "Apikey:", "X-Canary:"} {
		if strings.Contains(output, forbidden) {
			t.Fatal("HTTP diagnostics contain a request/stack/header dump")
		}
	}
}

func httpTestCORS(c *gin.Context) {
	c.Header("Access-Control-Allow-Origin", "*")
	c.Header("Access-Control-Allow-Credentials", "true")
	c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
	c.Header("Access-Control-Allow-Headers", "Origin, Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization, Accept, Cache-Control, X-Requested-With, apikey, ApiKey")
	c.Header("Access-Control-Expose-Headers", "Content-Length")
	if c.Request.Method == http.MethodOptions {
		c.AbortWithStatus(http.StatusOK)
		return
	}
	c.Next()
}

func httpTestEngine(safe bool) *gin.Engine {
	router := gin.New()
	if safe {
		router.Use(CredentialSafeHTTP(), CredentialSafeRecovery())
	} else {
		// Equivalent middleware installation to gin.Default(), without a server.
		router.Use(gin.Logger(), gin.Recovery())
	}
	router.Use(httpTestCORS)
	return router
}

func credentialHTTPRequest(method, target string) *http.Request {
	request := httptest.NewRequest(method, target, strings.NewReader(httpCredentialCanaries[4]))
	request.Header.Set("apikey", httpCredentialCanaries[2])
	request.Header.Set("Authorization", "Bearer "+httpCredentialCanaries[2])
	request.Header.Set("Cookie", "session="+httpCredentialCanaries[3])
	request.Header.Set("X-Canary", httpCredentialCanaries[2])
	request.Header.Set("X-Forwarded-For", httpCredentialCanaries[7])
	request.RemoteAddr = httpCredentialCanaries[7]
	return request
}

func TestCredentialSafeHTTPContractsAndRedaction(t *testing.T) {
	for _, mode := range []string{gin.DebugMode, gin.ReleaseMode} {
		t.Run(mode, func(t *testing.T) {
			output := captureHTTPDiagnostics(t, mode)
			engines := []*gin.Engine{httpTestEngine(false), httpTestEngine(true)}
			for _, router := range engines {
				router.GET("/ws", func(c *gin.Context) {
					_ = c.Error(errors.New(httpCredentialCanaries[5]))
					c.Set("secret", httpCredentialCanaries[2])
					c.JSON(http.StatusUnauthorized, gin.H{"error": "Token inválido"})
				})
				echo := func(c *gin.Context) {
					body, err := io.ReadAll(c.Request.Body)
					if err != nil {
						panic(err)
					}
					_ = c.Error(errors.New(httpCredentialCanaries[5]))
					c.Set("secret", httpCredentialCanaries[2])
					c.Header("X-Contract", "preserved")
					c.JSON(http.StatusOK, gin.H{
						"token": c.Param("token"), "query": c.Query("token"),
						"header": c.GetHeader("apikey"), "body": string(body),
					})
				}
				router.GET("/passkey-ceremony/:token", echo)
				router.POST("/passkey-ceremony/:token/response", echo)
				router.POST("/passkey-ceremony/:token/confirm", echo)
			}
			for _, tc := range []struct {
				method, path, template string
				status                 int
			}{
				{http.MethodGet, "/ws?token=" + httpCredentialCanaries[0], "/ws", http.StatusUnauthorized},
				{http.MethodGet, "/passkey-ceremony/" + httpCredentialCanaries[1] + "?token=" + httpCredentialCanaries[0], "/passkey-ceremony/:token", http.StatusOK},
				{http.MethodPost, "/passkey-ceremony/" + httpCredentialCanaries[1] + "/response", "/passkey-ceremony/:token/response", http.StatusOK},
				{http.MethodPost, "/passkey-ceremony/" + httpCredentialCanaries[1] + "/confirm", "/passkey-ceremony/:token/confirm", http.StatusOK},
				{http.MethodGet, "/unknown/" + httpCredentialCanaries[1] + "?token=" + httpCredentialCanaries[0], "[unmatched]", http.StatusNotFound},
				{http.MethodGet, "/passkey-ceremony/" + httpCredentialCanaries[1] + "%2Fextra", "[unmatched]", http.StatusNotFound},
				{http.MethodOptions, "/ws?token=" + httpCredentialCanaries[0], "[unmatched]", http.StatusOK},
				{httpCredentialCanaries[6], "/unknown", "[unmatched]", http.StatusNotFound},
			} {
				var oldResponse *httptest.ResponseRecorder
				for i, router := range engines {
					output.Reset()
					request := credentialHTTPRequest(tc.method, tc.path)
					uri, rawQuery, path, headers := request.RequestURI, request.URL.RawQuery, request.URL.Path, request.Header.Clone()
					response := httptest.NewRecorder()
					router.ServeHTTP(response, request)
					if response.Code != tc.status {
						t.Fatalf("unexpected status: %d", response.Code)
					}
					if request.RequestURI != uri || request.URL.RawQuery != rawQuery || request.URL.Path != path || request.Method != tc.method || !reflect.DeepEqual(request.Header, headers) {
						t.Fatal("diagnostic middleware mutated the request")
					}
					if i == 0 {
						oldResponse = response
						continue
					}
					if response.Code != oldResponse.Code || response.Body.String() != oldResponse.Body.String() || !reflect.DeepEqual(response.Header(), oldResponse.Header()) {
						t.Fatal("status/body/headers/CORS differ from legacy middleware")
					}
					text := output.String()
					assertSafeHTTPDiagnostics(t, text)
					method := tc.method
					if method == httpCredentialCanaries[6] {
						method = "[other]"
					}
					for _, field := range []string{"method=" + method, fmt.Sprintf("status=%d", tc.status), "route=" + fmt.Sprintf("%q", tc.template), "latency="} {
						if !strings.Contains(text, field) {
							t.Fatalf("missing safe diagnostic field %s", field)
						}
					}
				}
			}
		})
	}
}

type hostileHTTPPanic struct{ calls *int }

func (p hostileHTTPPanic) Error() string  { *p.calls++; return httpCredentialCanaries[6] }
func (p hostileHTTPPanic) String() string { *p.calls++; return httpCredentialCanaries[6] }
func (p hostileHTTPPanic) Format(state fmt.State, _ rune) {
	*p.calls++
	io.WriteString(state, httpCredentialCanaries[6])
}

func TestCredentialSafeRecoveryPanicRedaction(t *testing.T) {
	for _, mode := range []string{gin.DebugMode, gin.ReleaseMode} {
		t.Run(mode, func(t *testing.T) {
			output := captureHTTPDiagnostics(t, mode)
			calls := 0
			for _, value := range []any{
				httpCredentialCanaries[6], errors.New(httpCredentialCanaries[6]),
				map[string]string{"key": httpCredentialCanaries[6]}, []string{httpCredentialCanaries[6]},
				hostileHTTPPanic{&calls}, nil,
				fmt.Errorf("%s: %w", httpCredentialCanaries[6], http.ErrAbortHandler),
			} {
				router := httpTestEngine(true)
				router.GET("/panic/:token", func(c *gin.Context) {
					_ = c.Error(errors.New(httpCredentialCanaries[5]))
					c.Set("secret", httpCredentialCanaries[2])
					panic(value)
				})
				output.Reset()
				response := httptest.NewRecorder()
				router.ServeHTTP(response, credentialHTTPRequest(http.MethodGet, "/panic/"+httpCredentialCanaries[1]+"?token="+httpCredentialCanaries[0]))
				if response.Code != http.StatusInternalServerError || response.Body.Len() != 0 || response.Header().Get("Access-Control-Allow-Origin") != "*" {
					t.Fatal("panic must preserve generic empty HTTP 500 with CORS")
				}
				assertSafeHTTPDiagnostics(t, output.String())
				if !strings.Contains(output.String(), "[HTTP] panic recovered") || !strings.Contains(output.String(), `route="/panic/:token"`) || !strings.Contains(output.String(), "status=500") {
					t.Fatal("missing generic recovery/access diagnostics")
				}
			}
			if calls != 0 {
				t.Fatal("recovery formatted the panic value")
			}
		})
	}
}

func TestCredentialSafeRecoveryLegacyResponseAndBrokenPipe(t *testing.T) {
	output := captureHTTPDiagnostics(t, gin.DebugMode)
	for _, tc := range []struct {
		name      string
		value     any
		committed bool
	}{
		{"ordinary", errors.New(httpCredentialCanaries[6]), false},
		{"abort_handler", http.ErrAbortHandler, false},
		{"committed", errors.New(httpCredentialCanaries[6]), true},
		{"broken_pipe", &net.OpError{Op: httpCredentialCanaries[6], Net: "tcp", Err: &os.SyscallError{Syscall: "write", Err: syscall.EPIPE}}, false},
		{"reset", &net.OpError{Op: httpCredentialCanaries[6], Net: "tcp", Err: &os.SyscallError{Syscall: "write", Err: syscall.ECONNRESET}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var oldResponse *httptest.ResponseRecorder
			var oldAborted bool
			var oldErrors int
			for _, safe := range []bool{false, true} {
				router := httpTestEngine(safe)
				var observed *gin.Context
				router.GET("/panic/:token", func(c *gin.Context) {
					observed = c
					if tc.committed {
						c.String(http.StatusAccepted, "partial response")
					}
					panic(tc.value)
				})
				output.Reset()
				response := httptest.NewRecorder()
				router.ServeHTTP(response, credentialHTTPRequest(http.MethodGet, "/panic/"+httpCredentialCanaries[1]))
				if !safe {
					oldResponse, oldAborted, oldErrors = response, observed.IsAborted(), len(observed.Errors)
					continue
				}
				if response.Code != oldResponse.Code || response.Body.String() != oldResponse.Body.String() || !reflect.DeepEqual(response.Header(), oldResponse.Header()) || observed.IsAborted() != oldAborted || len(observed.Errors) != oldErrors {
					t.Fatal("response/abort/broken-connection semantics differ from Gin recovery")
				}
				assertSafeHTTPDiagnostics(t, output.String())
			}
		})
	}
}
