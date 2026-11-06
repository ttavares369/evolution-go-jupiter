package send_service

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/chai2010/webp"
	"github.com/evolution-foundation/evolution-go/pkg/config"
	instance_model "github.com/evolution-foundation/evolution-go/pkg/instance/model"
	logger_wrapper "github.com/evolution-foundation/evolution-go/pkg/logger"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

type preparationLookupResult struct {
	responses []types.IsOnWhatsAppResponse
	err       error
}

type preparationLookup struct {
	t       *testing.T
	ctx     context.Context
	results []preparationLookupResult
	calls   [][]string
	onCall  func(int)
}

func (f *preparationLookup) IsOnWhatsApp(ctx context.Context, phones []string) ([]types.IsOnWhatsAppResponse, error) {
	f.t.Helper()
	if ctx != f.ctx {
		f.t.Fatal("lookup did not receive the caller's context")
	}
	f.calls = append(f.calls, append([]string(nil), phones...))
	i := len(f.calls) - 1
	if i >= len(f.results) {
		f.t.Fatal("unexpected lookup (no send/retry is allowed)")
	}
	if f.onCall != nil {
		f.onCall(i)
	}
	return f.results[i].responses, f.results[i].err
}

func preparationRegistered(user, server string) preparationLookupResult {
	return preparationLookupResult{responses: []types.IsOnWhatsAppResponse{{IsIn: true, JID: types.NewJID(user, server)}}}
}

func TestResolveMessageRecipientRawFirstAndRemoteJID(t *testing.T) {
	for _, tc := range []struct{ name, phone, user, server string }{
		{"raw-PN", "+55 (31) 99999-8888", "553199998888", types.DefaultUserServer},
		{"PN-JID-input", "+5531999998888@s.whatsapp.net", "5531999998888", types.DefaultUserServer},
		{"remote-plus-PN", "5531999998888", "+5531999998888", types.DefaultUserServer},
		{"remote-LID", "5511999999999", "9876543210", "lid"},
		{"remote-group", "5511999999999", "120363012345678901", "g.us"},
	} {
		for _, flag := range []string{"nil", "true", "false"} {
			t.Run(tc.name+"/"+flag, func(t *testing.T) {
				var format *bool
				if flag != "nil" {
					value := flag == "true"
					format = &value
				}
				ctx := context.Background()
				fake := &preparationLookup{t: t, ctx: ctx, results: []preparationLookupResult{preparationRegistered(tc.user, tc.server)}}
				got, err := ResolveMessageRecipient(ctx, fake, tc.phone, format, true)
				want := types.NewJID(strings.ReplaceAll(tc.user, "+", ""), tc.server)
				if err != nil || got != want {
					t.Fatalf("got %v, %v; want %v", got, err, want)
				}
				if !reflect.DeepEqual(fake.calls, [][]string{{tc.phone}}) {
					t.Fatalf("first lookup must be verbatim: %v", fake.calls)
				}
			})
		}
	}
}

func TestResolveMessageRecipientFormattedFallback(t *testing.T) {
	for _, tc := range []struct{ name, phone, formatted string }{
		{"BR-DDD31-removes-ninth", "5531999998888", "+553199998888"},
		{"BR-DDD11-keeps-ninth", "5511999998888", "+5511999998888"},
		{"AR-legacy-removes-position2", "5491123456789", "+541123456789"},
		{"MX-legacy-removes-positions2and3", "5215551234567", "+52551234567"},
		{"PN-JID", "+5531999998888@s.whatsapp.net", "+553199998888"},
	} {
		for _, flag := range []string{"nil", "true", "false"} {
			t.Run(tc.name+"/"+flag, func(t *testing.T) {
				var format *bool
				if flag != "nil" {
					value := flag == "true"
					format = &value
				}
				ctx := context.Background()
				fake := &preparationLookup{t: t, ctx: ctx, results: []preparationLookupResult{
					{responses: []types.IsOnWhatsAppResponse{{IsIn: false, JID: types.NewJID("ignored", "lid")}}},
					preparationRegistered("returned-by-server", "lid"),
				}}
				got, err := ResolveMessageRecipient(ctx, fake, tc.phone, format, true)
				if err != nil || got != types.NewJID("returned-by-server", "lid") {
					t.Fatalf("fallback must use remote JID: %v, %v", got, err)
				}
				if !reflect.DeepEqual(fake.calls, [][]string{{tc.phone}, {tc.formatted}}) {
					t.Fatalf("got queries %v; want raw then %q", fake.calls, tc.formatted)
				}
			})
		}
	}
}

func TestResolveMessageRecipientBypassAndLocalValidation(t *testing.T) {
	for _, check := range []bool{false, true} {
		for _, phone := range []string{"120363012345678901@g.us", "123456789@lid", "12345@newsletter", "status@broadcast"} {
			got, err := ResolveMessageRecipient(context.Background(), nil, phone, nil, check)
			if err != nil || got.String() != phone {
				t.Fatalf("bypass %q (check=%v): %v, %v", phone, check, got, err)
			}
		}
	}
	for _, tc := range []struct {
		phone, want string
		format      bool
	}{
		{"+5531999998888@s.whatsapp.net", "5531999998888@s.whatsapp.net", false},
		{"+5531999998888@s.whatsapp.net", "553199998888@s.whatsapp.net", true},
		// ParseJID itself formats raw numbers even when formatJid is false.
		{"5531999998888", "553199998888@s.whatsapp.net", false},
	} {
		got, err := ResolveMessageRecipient(context.Background(), nil, tc.phone, &tc.format, false)
		if err != nil || got.String() != tc.want {
			t.Fatalf("disabled check: got %v, %v; want %s", got, err, tc.want)
		}
	}
	if _, err := ResolveMessageRecipient(context.Background(), nil, "", nil, false); err == nil {
		t.Fatal("disabled lookup must still validate input")
	}
}

func TestResolveMessageRecipientLookupErrorsPreserveLegacy(t *testing.T) {
	notIn := preparationLookupResult{responses: []types.IsOnWhatsAppResponse{{IsIn: false}}}
	netError := preparationLookupResult{err: errors.New("offline network error")}
	for _, tc := range []struct {
		name    string
		results []preparationLookupResult
		wantErr string
	}{
		{"first-network-error-local-validation", []preparationLookupResult{netError}, ""},
		{"first-empty-response-local-validation", []preparationLookupResult{{}}, ""},
		{"not-in-twice", []preparationLookupResult{notIn, notIn}, "not registered on WhatsApp"},
		{"fallback-network-error-not-registered", []preparationLookupResult{notIn, netError}, "not registered on WhatsApp"},
		{"fallback-empty-response-not-registered", []preparationLookupResult{notIn, {}}, "not registered on WhatsApp"},
		{"invalid-remote-JID", []preparationLookupResult{preparationRegistered("", types.DefaultUserServer)}, "could not parse formatted phone"},
		{"only-first-response-counts", []preparationLookupResult{{responses: []types.IsOnWhatsAppResponse{
			{IsIn: false}, {IsIn: true, JID: types.NewJID("ignored", "lid")},
		}}, notIn}, "not registered on WhatsApp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			fake := &preparationLookup{t: t, ctx: ctx, results: tc.results}
			format := false
			got, err := ResolveMessageRecipient(ctx, fake, "+5531999998888@s.whatsapp.net", &format, true)
			if tc.wantErr == "" {
				if err != nil || got.String() != "5531999998888@s.whatsapp.net" {
					t.Fatalf("first error must use original local validation: %v, %v", got, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got %v; want error containing %q", err, tc.wantErr)
			}
			if len(fake.calls) != len(tc.results) {
				t.Fatalf("got %d lookups, want %d", len(fake.calls), len(tc.results))
			}
		})
	}
}

func TestResolveMessageRecipientContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, check := range []bool{false, true} {
		if _, err := ResolveMessageRecipient(ctx, nil, "123@lid", nil, check); !errors.Is(err, context.Canceled) {
			t.Fatalf("pre-canceled context: %v", err)
		}
	}
	deadline, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	if _, err := ResolveMessageRecipient(deadline, nil, "5511999999999", nil, true); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired context: %v", err)
	}
	for _, cancelOn := range []int{0, 1} {
		ctx, cancel := context.WithCancel(context.Background())
		fake := &preparationLookup{t: t, ctx: ctx, results: []preparationLookupResult{
			{responses: []types.IsOnWhatsAppResponse{{IsIn: false}}},
			{err: context.Canceled},
		}, onCall: func(i int) {
			if i == cancelOn {
				cancel()
			}
		}}
		_, err := ResolveMessageRecipient(ctx, fake, "5511999999999", nil, true)
		cancel()
		if !errors.Is(err, context.Canceled) || len(fake.calls) != cancelOn+1 {
			t.Fatalf("cancel on lookup %d: err=%v, calls=%v", cancelOn, err, fake.calls)
		}
	}
}

func TestMessagePreparationLegacyWrapperConfigAndQuotedFields(t *testing.T) {
	cfg := &config.Config{LogDirectory: t.TempDir()}
	s := &sendService{config: cfg, loggerWrapper: logger_wrapper.NewLoggerManager(cfg)}
	inst := &instance_model.Instance{Id: "preparation-offline"}
	id, participant := "quoted-id", "123@lid"
	for _, check := range []bool{false, true} {
		cfg.CheckUserExists = check
		for _, tc := range []struct {
			id, participant *string
			wantErr         string
		}{
			{nil, nil, ""}, {&id, &participant, ""},
			{&id, nil, "missing Participant in ContextInfo"},
			{nil, &participant, "missing StanzaId in ContextInfo"},
		} {
			got, err := s.validateAndCheckUserExists("123@lid", nil, tc.id, tc.participant, inst)
			if tc.wantErr == "" {
				if err != nil || got.String() != "123@lid" {
					t.Fatalf("wrapper bypass: %v, %v", got, err)
				}
			} else if err == nil || err.Error() != tc.wantErr {
				t.Fatalf("quoted validation: %v; want %s", err, tc.wantErr)
			}
		}
	}
	cfg.CheckUserExists = false
	got, err := s.validateAndCheckUserExists("+5531999998888@s.whatsapp.net", nil, nil, nil, inst)
	if err != nil || got.String() != "553199998888@s.whatsapp.net" {
		t.Fatalf("config=false must not ensure a client: %v, %v", got, err)
	}
}

func TestSharedTextMessageLegacyEquivalence(t *testing.T) {
	for _, text := range []string{"", "texto com legenda 🌎\nsegunda linha", "@5511999999999 https://example.invalid"} {
		want := &waE2E.ExtendedTextMessage{Text: &text}
		got := SharedTextMessage(text)
		if !proto.Equal(got, want) || !reflect.DeepEqual(got, want) || got.ContextInfo != nil {
			t.Fatalf("legacy text body changed: %v", got)
		}
		preparationAssertWireEqual(t, got, want)
		// Callers may attach their own context without mutating another body.
		got.ContextInfo = &waE2E.ContextInfo{}
		if SharedTextMessage(text).ContextInfo != nil {
			t.Fatal("shared text context leaked between calls")
		}
	}
}

// Frozen literals from both legacy image branches (file and URL). MIME conversion
// happens before these literals; WebP input reaches them as image/jpeg.
func preparationLegacyImage(fileData []byte, mimeType, caption string, uploaded whatsmeow.UploadResponse, newsletter bool) *waE2E.ImageMessage {
	jpegThumb := makeJPEGThumbnail(fileData, 72)
	if newsletter {
		return &waE2E.ImageMessage{
			Caption: proto.String(caption), URL: &uploaded.URL, DirectPath: &uploaded.DirectPath,
			Mimetype: proto.String(mimeType), FileSHA256: uploaded.FileSHA256,
			FileLength: &uploaded.FileLength, JPEGThumbnail: jpegThumb,
		}
	}
	return &waE2E.ImageMessage{
		Caption: proto.String(caption), URL: proto.String(uploaded.URL), DirectPath: proto.String(uploaded.DirectPath),
		MediaKey: uploaded.MediaKey, Mimetype: proto.String(mimeType), FileEncSHA256: uploaded.FileEncSHA256,
		FileSHA256: uploaded.FileSHA256, FileLength: proto.Uint64(uint64(len(fileData))), JPEGThumbnail: jpegThumb,
	}
}

func preparationAssertWireEqual(t *testing.T, got, want proto.Message) {
	t.Helper()
	options := proto.MarshalOptions{Deterministic: true}
	gotBytes, err := options.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	wantBytes, err := options.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotBytes, wantBytes) {
		t.Fatal("legacy protobuf wire bytes changed")
	}
}

func TestSharedImageMessageLegacyEquivalence(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 144, 96))
	for y := 0; y < 96; y++ {
		for x := 0; x < 144; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 120, A: 255})
		}
	}
	for _, format := range []string{"png", "jpeg", "webp"} {
		var buf bytes.Buffer
		var err error
		switch format {
		case "png":
			err = png.Encode(&buf, img)
		case "jpeg":
			err = jpeg.Encode(&buf, img, nil)
		case "webp":
			err = webp.Encode(&buf, img, &webp.Options{Lossless: true})
		}
		if err != nil {
			t.Fatal(err)
		}
		fileData := buf.Bytes()
		mimeType := "image/" + format
		if format == "webp" {
			mimeType = "image/jpeg" // Preserved legacy pre-builder conversion.
		}
		for _, newsletter := range []bool{false, true} {
			for _, caption := range []string{"", "legenda 🌎\ncom contexto"} {
				t.Run(format+"/"+map[bool]string{false: "normal", true: "newsletter"}[newsletter]+"/"+caption, func(t *testing.T) {
					uploaded := whatsmeow.UploadResponse{
						URL: "https://example.invalid/media", DirectPath: "/media/path", Handle: "newsletter-handle",
						MediaKey: []byte{1, 2, 3}, FileEncSHA256: []byte{4, 5, 6}, FileSHA256: []byte{7, 8, 9},
						FileLength: uint64(len(fileData) + 999),
					}
					got := SharedImageMessage(fileData, mimeType, caption, uploaded, newsletter)
					want := preparationLegacyImage(fileData, mimeType, caption, uploaded, newsletter)
					if !proto.Equal(got, want) || !reflect.DeepEqual(got, want) {
						t.Fatalf("legacy image fields changed: got %v; want %v", got, want)
					}
					preparationAssertWireEqual(t, got, want)
					cfg, kind, err := image.DecodeConfig(bytes.NewReader(got.JPEGThumbnail))
					if err != nil || kind != "jpeg" || cfg.Width != 72 || cfg.Height != 48 {
						t.Fatalf("legacy thumbnail changed: %v, %s, %v", cfg, kind, err)
					}
					if got.GetMimetype() != mimeType || got.ContextInfo != nil {
						t.Fatal("MIME/context changed")
					}
					if newsletter && (got.MediaKey != nil || got.FileEncSHA256 != nil || got.GetFileLength() != uploaded.FileLength) {
						t.Fatal("newsletter encryption/length changed")
					}
					if !newsletter && got.GetFileLength() != uint64(len(fileData)) {
						t.Fatal("normal images must use original file length")
					}
				})
			}
		}
	}
	for _, data := range [][]byte{nil, []byte("invalid image")} {
		for _, newsletter := range []bool{false, true} {
			got := SharedImageMessage(data, "image/webp", "", whatsmeow.UploadResponse{}, newsletter)
			want := preparationLegacyImage(data, "image/webp", "", whatsmeow.UploadResponse{}, newsletter)
			if got.JPEGThumbnail != nil || got.GetMimetype() != "image/webp" || !reflect.DeepEqual(got, want) {
				t.Fatal("invalid images must retain MIME and omit thumbnail, not fail")
			}
			preparationAssertWireEqual(t, got, want)
		}
	}
}
