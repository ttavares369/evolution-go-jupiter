package whatsmeow_service

import (
	"testing"

	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
)

func TestApplyWhatsAppClientVersion(t *testing.T) {
	original := store.GetWAVersion()
	t.Cleanup(func() { store.SetWAVersion(original) })
	want := store.WAVersionContainer{2, 3000, 1052855845}
	if !applyWhatsAppClientVersion(clientVersion{2, 3000, 1052855845}) {
		t.Fatal("valid web version was not applied")
	}
	// Inspect the actual payload used for an existing session, not DeviceProps.
	jid := types.NewJID("5511999999999", types.DefaultUserServer)
	device := &store.Device{ID: &jid}
	version := device.GetClientPayload().GetUserAgent().GetAppVersion()
	got := store.WAVersionContainer{version.GetPrimary(), version.GetSecondary(), version.GetTertiary()}
	if got != want || store.GetWAVersion() != want {
		t.Fatalf("login payload version = %v; want %v", got, want)
	}
	for _, invalid := range []clientVersion{{}, {2, 3000, 0}, {2, 0, 1052855845}, {-1, 3000, 1052855845}} {
		if applyWhatsAppClientVersion(invalid) || store.GetWAVersion() != want {
			t.Fatalf("invalid version %v replaced working version", invalid)
		}
	}
}
