package whatsmeow_service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/evolution-foundation/evolution-go/pkg/config"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
		"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite"
)

func preserveVersionGlobals(t *testing.T) {
	t.Helper()
	original := store.GetWAVersion()
	props := proto.Clone(store.DeviceProps).(*waCompanionReg.DeviceProps)
	t.Cleanup(func() {
		store.SetWAVersion(original)
		store.DeviceProps = props
	})
}

func assertHandshakeVersion(t *testing.T, device *store.Device, want store.WAVersionContainer) {
	t.Helper()
	client := whatsmeow.NewClient(device, nil)
	// This is the default payload branch used by whatsmeow/handshake.go.
	if client.GetClientPayload != nil {
		t.Fatal("must preserve Whatsmeow's registration/login payload selection")
	}
	payload := client.Store.GetClientPayload()
	got := payload.GetUserAgent().GetAppVersion()
	if got.GetPrimary() != want[0] || got.GetSecondary() != want[1] || got.GetTertiary() != want[2] {
		t.Fatalf("handshake AppVersion = %v, want %v", got, want)
	}
	if device.ID == nil {
		pairing := payload.GetDevicePairingData()
		hash := want.Hash()
		if pairing == nil || !bytes.Equal(pairing.GetBuildHash(), hash[:]) {
			t.Fatal("new-session registration hash does not match the handshake version")
		}
		props := &waCompanionReg.DeviceProps{}
		if err := proto.Unmarshal(pairing.GetDeviceProps(), props); err != nil {
			t.Fatal(err)
		}
		if props.GetVersion().GetPrimary() != want[0] || props.GetVersion().GetSecondary() != want[1] || props.GetVersion().GetTertiary() != want[2] {
			t.Fatal("registration DeviceProps version differs from handshake")
		}
	} else if payload.GetDevicePairingData() != nil || payload.GetUsername() != device.ID.UserInt() || payload.GetDevice() != uint32(device.ID.Device) {
		t.Fatal("authenticated session must keep its identity and use the login payload")
	}
}

func TestWhatsAppVersionSelectionAndHandshake(t *testing.T) {
	cases := []struct {
		name      string
		cfg       config.Config
		fetchErr  error
		want      store.WAVersionContainer
		wantFetch bool
	}{
		{"configured", config.Config{WhatsappVersionMajor: 2, WhatsappVersionMinor: 3000, WhatsappVersionPatch: 1053430301}, nil, store.WAVersionContainer{2, 3000, 1053430301}, false},
		{"from_web", config.Config{}, nil, store.WAVersionContainer{2, 3000, 1053430300}, true},
		{"partial_configuration_uses_web", config.Config{WhatsappVersionMajor: 2}, nil, store.WAVersionContainer{2, 3000, 1053430300}, true},
		{"web_unavailable_keeps_library_version", config.Config{}, errors.New("synthetic fetch failure"), store.WAVersionContainer{2, 3000, 1042386815}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			preserveVersionGlobals(t)
			store.SetWAVersion(store.WAVersionContainer{2, 3000, 1042386815})
			state := &clientVersionState{}
			fetches := 0
			version, err := state.initialize(&tc.cfg, func() (*clientVersion, error) {
				fetches++
				return &clientVersion{2, 3000, 1053430300}, tc.fetchErr
			})
			if version != tc.want || store.GetWAVersion() != tc.want || !errors.Is(err, tc.fetchErr) || (fetches == 1) != tc.wantFetch {
				t.Fatalf("selection: version=%v err=%v fetches=%d", version, err, fetches)
			}
			device := (&sqlstore.Container{}).NewDevice()
			assertHandshakeVersion(t, device, tc.want)
			jid := types.NewADJID("15550000001", 0, 7)
			device.ID = &jid
			assertHandshakeVersion(t, device, tc.want)
			// A reconnect/start from another instance must not rewrite globals.
			version, _ = state.initialize(&config.Config{}, func() (*clientVersion, error) {
				t.Fatal("process version must not be fetched again during reconnect")
				return nil, nil
			})
			if version != tc.want {
				t.Fatal("reconnect changed the selected process version")
			}
			assertHandshakeVersion(t, device, tc.want)
		})
	}
}

func TestWhatsAppVersionConcurrentStarts(t *testing.T) {
	preserveVersionGlobals(t)
	state := &clientVersionState{}
	want := store.WAVersionContainer{2, 3000, 1053430300}
	var fetches atomic.Int32
	var workers sync.WaitGroup
	for i := 0; i < 32; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			got, err := state.initialize(&config.Config{}, func() (*clientVersion, error) {
				fetches.Add(1)
				return &clientVersion{2, 3000, 1053430300}, nil
			})
			if err != nil || got != want {
				t.Error("concurrent start got a different version")
				return
			}
			assertHandshakeVersion(t, (&sqlstore.Container{}).NewDevice(), want)
		}()
	}
	workers.Wait()
	if fetches.Load() != 1 {
		t.Fatal("version globals must be initialized exactly once before handshakes")
	}
}

func TestWhatsAppVersionRestoredSession(t *testing.T) {
	preserveVersionGlobals(t)
	state := &clientVersionState{}
	want, err := state.initialize(&config.Config{WhatsappVersionMajor: 2, WhatsappVersionMinor: 3000, WhatsappVersionPatch: 1053430300}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dsn := "file:" + filepath.Join(t.TempDir(), "synthetic-session.db") + "?_pragma=foreign_keys(1)"
	container, err := sqlstore.New(ctx, "sqlite", dsn, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = container.Close() })
	device := container.NewDevice()
	jid := types.NewADJID("15550000001", 0, 7)
	device.ID = &jid
	device.Account = &waAdv.ADVSignedDeviceIdentity{
		Details: []byte{1}, AccountSignature: make([]byte, 64), AccountSignatureKey: make([]byte, 32), DeviceSignature: make([]byte, 64),
	}
	if err := device.Save(ctx); err != nil {
		t.Fatal(err)
	}
	identity := *device.IdentityKey.Priv
	if err := container.Close(); err != nil {
		t.Fatal(err)
	}
	container, err = sqlstore.New(ctx, "sqlite", dsn, nil)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := container.GetDevice(ctx, jid)
	if err != nil || restored == nil {
		t.Fatalf("cannot restore synthetic authenticated session: %v", err)
	}
	if *restored.IdentityKey.Priv != identity {
		t.Fatal("restoring the session changed its identity")
	}
	assertHandshakeVersion(t, restored, want)
	assertHandshakeVersion(t, restored, want)
}

func TestWhatsAppVersionQRChannelWithoutNetwork(t *testing.T) {
	preserveVersionGlobals(t)
	state := &clientVersionState{}
	want, err := state.initialize(&config.Config{WhatsappVersionMajor: 2, WhatsappVersionMinor: 3000, WhatsappVersionPatch: 1053430300}, nil)
	if err != nil {
		t.Fatal(err)
	}
	device := (&sqlstore.Container{}).NewDevice()
	assertHandshakeVersion(t, device, want)
	client := whatsmeow.NewClient(device, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	channel, err := client.GetQRChannel(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Inject only a synthetic server event: never Connect or contact WhatsApp.
	client.DangerousInternals().DispatchEvent(&events.QR{Codes: []string{"synthetic-qr-only"}})
	select {
	case item := <-channel:
		if item.Event != whatsmeow.QRChannelEventCode || item.Code != "synthetic-qr-only" {
			t.Fatal("QR channel did not forward the new-session event")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("QR channel timed out")
	}
	cancel()
	select {
	case _, open := <-channel:
		if open {
			t.Fatal("QR channel must close on cancellation")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("QR channel did not stop")
	}
	jid := types.NewADJID("15550000001", 0, 7)
	device.ID = &jid
	if _, err := client.GetQRChannel(context.Background()); !errors.Is(err, whatsmeow.ErrQRStoreContainsID) {
		t.Fatal("authenticated session must reconnect, not request a new QR")
	}
	assertHandshakeVersion(t, device, want)
}

type versionRoundTripper func(*http.Request) (*http.Response, error)

func (f versionRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestWhatsAppVersionWebFetchAndCache(t *testing.T) {
	originalTransport := http.DefaultTransport
	originalVersion, originalAt := cachedWebVersion, cachedWebVersionAt
	t.Cleanup(func() {
		http.DefaultTransport = originalTransport
		cachedWebVersion, cachedWebVersionAt = originalVersion, originalAt
	})
	cachedWebVersion, cachedWebVersionAt = nil, time.Time{}
	calls := 0
	http.DefaultTransport = versionRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.String() != "https://web.whatsapp.com/sw.js" {
			t.Fatal("unexpected version source")
		}
		if deadline, ok := req.Context().Deadline(); !ok || time.Until(deadline) > 10*time.Second {
			t.Fatal("web version fetch must have a bounded timeout")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"client_revision":1053430300}`)), Header: make(http.Header)}, nil
	})
	for i := 0; i < 2; i++ {
		version, err := fetchWhatsAppWebVersion()
		if err != nil || *version != (clientVersion{2, 3000, 1053430300}) {
			t.Fatalf("unexpected parsed web version: %v, %v", version, err)
		}
	}
	if calls != 1 {
		t.Fatal("web version cache was not preserved")
	}
}

func TestWhatsAppVersionInitializationPrecedesClientCreation(t *testing.T) {
	data, err := os.ReadFile("whatsmeow.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	start := strings.Index(source, "func (w whatsmeowService) StartClient(")
	if start < 0 {
		t.Fatal("StartClient missing")
	}
	source = source[start:]
	init := strings.Index(source, "processClientVersion.initialize(w.config, fetchWhatsAppWebVersion)")
	newClient := strings.Index(source, "whatsmeow.NewClient(deviceStore, clientLog)")
	if init < 0 || newClient < 0 || init >= newClient {
		t.Fatal("all clients must pass the process version barrier before connecting")
	}
}
