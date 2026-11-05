package queuev1

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Concrete Whatsmeow event clients are deliberately never connected in tests.
// Pin these non-HTTP guards and the runtime installation order structurally,
// alongside behavioral service/HTTP/SQL tests and the reverse-patch baseline proof.
func TestExclusiveAutomaticEmissionAndRuntimeWiringInventory(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	read := func(path string) string {
		b, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	wa := read("pkg/whatsmeow/service/whatsmeow.go")
	for _, guard := range []string{
		"if mycli.Instance.RejectCall && queuepolicy.Legacy(mycli.userID) == nil {",
		"if mycli.Instance.MsgRejectCall != \"\" && queuepolicy.Legacy(mycli.userID) == nil {",
		"func processPresenceUpdates(mycli *MyClient) {\n\tif queuepolicy.Legacy(mycli.userID) != nil {\n\t\treturn\n\t}",
		"evt.Name == appstate.WAPatchCriticalBlock && queuepolicy.Legacy(mycli.userID) == nil",
		"if queuepolicy.Legacy(mycli.userID) == nil {\n\t\t\t\tif mycli.Instance.AlwaysOnline {",
	} {
		if strings.Count(wa, guard) != 1 {
			t.Fatalf("automatic emitter guard changed: %s", guard)
		}
	}
	main := read("cmd/evolution-go/main.go")
	install := strings.Index(main, `queuepolicy.Configure(db, os.Getenv("EVOLUTION_QUEUE_V1_EXCLUSIVE_INSTANCE_IDS"))`)
	runtimeStart := strings.Index(main, "whatsmeow_service.NewWhatsmeowService(")
	optional := strings.Index(main, "queuev1.RegisterOptional(")
	if install < 0 || install > runtimeStart || install > optional {
		t.Fatal("policy not installed before runtimes and independent of queue opt-in")
	}
	chat := read("pkg/chat/service/chat_service.go")
	if strings.Contains(chat, "queuepolicy") || !strings.Contains(chat, "whatsmeow.SendRequestExtra{Peer: true}") {
		t.Fatal("technical history peer operation intercepted")
	}
}
