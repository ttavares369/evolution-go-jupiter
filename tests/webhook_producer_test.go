package webhook_producer

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/evolution-foundation/evolution-go/pkg/config"
	logger_wrapper "github.com/evolution-foundation/evolution-go/pkg/logger"
)

const testSecret = "test-only-lembrai-webhook-secret-at-least-32-chars"

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func response(req *http.Request, code int, location string) *http.Response {
	headers := make(http.Header)
	if location != "" {
		headers.Set("Location", location)
	}
	return &http.Response{
		StatusCode: code, Status: http.StatusText(code), Header: headers,
		Body: io.NopCloser(strings.NewReader("ok")), Request: req,
	}
}

func TestWebhookHeaderOnlyForExactLembrAIURL(t *testing.T) {
	t.Setenv(lembraiSecretEnv, testSecret)
	body := []byte(`{"event":"QRCode","data":{"code":"test"}}`)
	cases := []struct {
		name, url string
		wantSecret bool
	}{
		{"exact", lembraiWebhookURL, true},
		{"other domain", "https://example.org/api/v1/whatsapp/webhook", false},
		{"other subdomain", "https://other.api.lembrai.jupiterti.com/api/v1/whatsapp/webhook", false},
		{"other path", "https://api.lembrai.jupiterti.com/api/v1/whatsapp/other", false},
		{"similar URL with query", lembraiWebhookURL + "?id=1", false},
		{"similar URL with port", "https://api.lembrai.jupiterti.com:443/api/v1/whatsapp/webhook", false},
		{"HTTP", "http://api.lembrai.jupiterti.com/api/v1/whatsapp/webhook", false},
		{"global third-party webhook", "https://example.org/global", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := 0
			p := &webhookProducer{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				called++
				if got := req.Header.Get(lembraiSecretHeader); (got == testSecret) != tc.wantSecret || (got != "" && got != testSecret) {
					t.Errorf("unexpected webhook authentication header for %s", tc.name)
				}
				if req.Header.Get("Content-Type") != "application/json" {
					t.Error("Content-Type changed")
				}
				gotBody, err := io.ReadAll(req.Body)
				if err != nil || !bytes.Equal(gotBody, body) {
					t.Errorf("payload changed: %v", err)
				}
				return response(req, http.StatusOK, ""), nil
			})}}
			err, _, status := p.sendWebhook(tc.url, body, "test-instance")
			if err != nil || status != http.StatusOK || called != 1 {
				t.Fatalf("callback failed: status=%d calls=%d error=%v", status, called, err)
			}
		})
	}
}

func TestLembrAIRedirectIsNeverFollowed(t *testing.T) {
	t.Setenv(lembraiSecretEnv, testSecret)
	for _, status := range []int{301, 302, 303, 307, 308} {
		for _, target := range []string{"https://evil.example/steal", lembraiWebhookURL} {
			t.Run(http.StatusText(status)+" to "+target, func(t *testing.T) {
				calls := 0
				p := &webhookProducer{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					calls++
					if calls != 1 || req.URL.String() != lembraiWebhookURL || req.Header.Get(lembraiSecretHeader) != testSecret {
						t.Error("redirect followed or unexpected destination/header")
					}
					return response(req, status, target), nil
				})}}
				err, _, gotStatus := p.sendWebhook(lembraiWebhookURL, []byte(`{}`), "test-instance")
				if err == nil || gotStatus != status || calls != 1 {
					t.Fatalf("redirect not rejected: status=%d calls=%d error=%v", gotStatus, calls, err)
				}
			})
		}
	}
}

func TestMissingOrShortSecretFailsClosed(t *testing.T) {
	for _, secret := range []string{"", "short-secret"} {
		t.Run(secret, func(t *testing.T) {
			t.Setenv(lembraiSecretEnv, secret)
			calls := 0
			logDir := t.TempDir()
			p := &webhookProducer{
				loggerWrapper: logger_wrapper.NewLoggerManager(&config.Config{LogDirectory: logDir, LogMaxSize: 1}),
				client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					calls++
					return response(req, http.StatusOK, ""), nil
				})},
			}
			err, _, _ := p.sendWebhook(lembraiWebhookURL, []byte(`{}`), "instance")
			if err == nil || calls != 0 {
				t.Fatalf("unauthenticated callback attempted: %v", err)
			}
			p.sendWebhookWithRetry(lembraiWebhookURL, []byte(`{}`), 5, time.Hour, "instance")
			if calls != 0 {
				t.Fatal("retry attempted an unauthenticated callback")
			}
			logs, err := os.ReadFile(filepath.Join(logDir, "instance", "instance.log"))
			if err != nil || !bytes.Contains(logs, []byte("authentication unavailable")) || (secret != "" && bytes.Contains(logs, []byte(secret))) {
				t.Fatalf("missing safe operational error or secret exposed: %v", err)
			}
		})
	}
}

func TestSubscribedEventsUseSameTransportForGlobalAndInstanceURLs(t *testing.T) {
	t.Setenv(lembraiSecretEnv, testSecret)
	for _, event := range []string{"QRCode", "QRTimeout", "PairSuccess", "Connected", "Disconnected", "LoggedOut"} {
		t.Run(event, func(t *testing.T) {
			body := []byte(`{"event":"` + event + `","instanceId":"test-id"}`)
			requests := make(chan *http.Request, 2)
			logDir := t.TempDir()
			p := &webhookProducer{
				url: "https://example.org/global",
				loggerWrapper: logger_wrapper.NewLoggerManager(&config.Config{LogDirectory: logDir, LogMaxSize: 1}),
				client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					requests <- req
					return response(req, http.StatusOK, ""), nil
				})},
			}
			if err := p.Produce("test-id."+event, body, lembraiWebhookURL, "test-id"); err != nil {
				t.Fatal(err)
			}
			seen := make(map[string]bool)
			for range 2 {
				select {
				case req := <-requests:
					gotBody, err := io.ReadAll(req.Body)
					if err != nil || !bytes.Equal(gotBody, body) || req.Header.Get("Content-Type") != "application/json" {
						t.Errorf("payload or Content-Type changed: %v", err)
					}
					wantSecret := req.URL.String() == lembraiWebhookURL
					if got := req.Header.Get(lembraiSecretHeader); (got == testSecret) != wantSecret || (got != "" && got != testSecret) {
						t.Error("secret sent to wrong webhook")
					}
					seen[req.URL.String()] = true
				case <-time.After(3 * time.Second):
					t.Fatal("callback did not arrive")
				}
			}
			if !seen[lembraiWebhookURL] || !seen[p.url] {
				t.Fatalf("missing global or instance callback: %v", seen)
			}
			// Produce dispatches asynchronously; wait for both workers to finish
			// writing logs before the test's temporary directory is removed.
			deadline := time.Now().Add(3 * time.Second)
			for {
				logs, _ := os.ReadFile(filepath.Join(logDir, "test-id", "instance.log"))
				if bytes.Count(logs, []byte("webhook sent successfully")) == 2 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("asynchronous callbacks did not finish")
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}
