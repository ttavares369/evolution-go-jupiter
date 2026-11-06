package whatsmeow_service

import (
	"sync"

	"github.com/evolution-foundation/evolution-go/pkg/config"
	"go.mau.fi/whatsmeow/store"
	"google.golang.org/protobuf/proto"
)

// Whatsmeow's version and pairing hash are process globals. Initialize them before
// any client connects, then leave them immutable for concurrent handshakes and
// automatic reconnects. A version override takes effect on process restart.
var processClientVersion clientVersionState

type clientVersionState struct {
	once    sync.Once
	version store.WAVersionContainer
	err     error
}

func (state *clientVersionState) initialize(cfg *config.Config, fetch func() (*clientVersion, error)) (store.WAVersionContainer, error) {
	state.once.Do(func() {
		state.version = store.GetWAVersion()
		if cfg.WhatsappVersionMajor > 0 && cfg.WhatsappVersionMinor > 0 && cfg.WhatsappVersionPatch > 0 {
			state.version = store.WAVersionContainer{
				uint32(cfg.WhatsappVersionMajor), uint32(cfg.WhatsappVersionMinor), uint32(cfg.WhatsappVersionPatch),
			}
		} else {
			var webVersion *clientVersion
			webVersion, state.err = fetch()
			if state.err == nil {
				state.version = store.WAVersionContainer{uint32(webVersion.Major), uint32(webVersion.Minor), uint32(webVersion.Patch)}
			}
		}
		// DeviceProps alone does not set the client handshake version (PR #229).
		store.SetWAVersion(state.version)
		store.DeviceProps.Version.Primary = proto.Uint32(state.version[0])
		store.DeviceProps.Version.Secondary = proto.Uint32(state.version[1])
		store.DeviceProps.Version.Tertiary = proto.Uint32(state.version[2])
	})
	return state.version, state.err
}
