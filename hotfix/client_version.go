package whatsmeow_service

import "go.mau.fi/whatsmeow/store"

// applyWhatsAppClientVersion updates the actual login and QR pairing payloads.
// A failed version lookup must not replace the library version with zero values.
func applyWhatsAppClientVersion(version clientVersion) bool {
	if version.Major <= 0 || version.Minor <= 0 || version.Patch <= 0 {
		return false
	}
	store.SetWAVersion(store.WAVersionContainer{uint32(version.Major), uint32(version.Minor), uint32(version.Patch)})
	return true
}
