package send_service

import (
	"context"
	"fmt"
	"strings"

	"github.com/evolution-foundation/evolution-go/pkg/utils"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// ResolveMessageRecipient shares legacy recipient preparation without connecting,
// gating or sending. Pass checkUserExists=true for the legacy default. Lookup
// starts with the raw input; only IsIn=false triggers a formatted lookup. A first
// lookup error falls back to local validation, except when ctx is canceled.
func ResolveMessageRecipient(ctx context.Context, client interface {
	IsOnWhatsApp(context.Context, []string) ([]types.IsOnWhatsAppResponse, error)
}, phone string, formatJid *bool, checkUserExists bool) (types.JID, error) {
	if err := ctx.Err(); err != nil {
		return types.NewJID("", types.DefaultUserServer), err
	}

	validate := func(number string, format *bool) (types.JID, error) {
		recipient, err := validateMessageFields(number, format, nil, nil)
		// SendMessage applies this same normalization before the actual send.
		recipient.User = strings.ReplaceAll(recipient.User, "+", "")
		return recipient, err
	}
	if !checkUserExists || messageRecipientSkipsLookup(phone) {
		return validate(phone, formatJid)
	}

	remoteJID, found, err := lookupMessageRecipient(ctx, client, phone, false)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return types.NewJID("", types.DefaultUserServer), ctxErr
	}
	if err != nil {
		return validate(phone, formatJid)
	}
	if !found {
		remoteJIDRetry, foundRetry, errRetry := lookupMessageRecipient(ctx, client, phone, true)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return types.NewJID("", types.DefaultUserServer), ctxErr
		}
		if errRetry == nil && foundRetry {
			remoteJID = remoteJIDRetry
			found = foundRetry
		}
	}
	if !found {
		return types.NewJID("", types.DefaultUserServer), fmt.Errorf("number %s is not registered on WhatsApp", phone)
	}

	formatJidFalse := false
	return validate(remoteJID, &formatJidFalse)
}

func messageRecipientSkipsLookup(phone string) bool {
	return strings.Contains(phone, "@g.us") || strings.Contains(phone, "@broadcast") || strings.Contains(phone, "@newsletter") || strings.Contains(phone, "@lid")
}

func lookupMessageRecipient(ctx context.Context, client interface {
	IsOnWhatsApp(context.Context, []string) ([]types.IsOnWhatsAppResponse, error)
}, phone string, formatJid bool) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	phoneNumbers, err := utils.PrepareNumbersForWhatsAppCheck([]string{phone}, &formatJid)
	if err != nil {
		return "", false, fmt.Errorf("failed to prepare number for WhatsApp check: %v", err)
	}
	resp, err := client.IsOnWhatsApp(ctx, phoneNumbers)
	if err != nil {
		return "", false, fmt.Errorf("failed to check if number %s exists on WhatsApp: %v", phoneNumbers[0], err)
	}
	if len(resp) == 0 {
		return "", false, fmt.Errorf("number %s not found in WhatsApp response", phoneNumbers[0])
	}
	if !resp[0].IsIn {
		return "", false, nil
	}
	return fmt.Sprintf("%v", resp[0].JID), true, nil
}

// SharedTextMessage builds only the legacy text body. Quoted messages, mentions
// and ContextInfo remain the caller's responsibility.
func SharedTextMessage(text string) *waE2E.ExtendedTextMessage {
	return &waE2E.ExtendedTextMessage{Text: &text}
}

// SharedImageMessage builds the legacy image body after upload. MIME detection
// and conversion remain the caller's responsibility; mimeType is kept verbatim.
// Newsletter uploads deliberately omit encryption fields and use uploaded length.
func SharedImageMessage(fileData []byte, mimeType, caption string, uploaded whatsmeow.UploadResponse, isNewsletter bool) *waE2E.ImageMessage {
	jpegThumb := makeJPEGThumbnail(fileData, 72)
	if isNewsletter {
		return &waE2E.ImageMessage{
			Caption:       proto.String(caption),
			URL:           &uploaded.URL,
			DirectPath:    &uploaded.DirectPath,
			Mimetype:      proto.String(mimeType),
			FileSHA256:    uploaded.FileSHA256,
			FileLength:    &uploaded.FileLength,
			JPEGThumbnail: jpegThumb,
		}
	}
	return &waE2E.ImageMessage{
		Caption:       proto.String(caption),
		URL:           proto.String(uploaded.URL),
		DirectPath:    proto.String(uploaded.DirectPath),
		MediaKey:      uploaded.MediaKey,
		Mimetype:      proto.String(mimeType),
		FileEncSHA256: uploaded.FileEncSHA256,
		FileSHA256:    uploaded.FileSHA256,
		FileLength:    proto.Uint64(uint64(len(fileData))),
		JPEGThumbnail: jpegThumb,
	}
}
