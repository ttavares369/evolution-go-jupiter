package queuev1

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/png"
	"reflect"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

func TestQueuePreparedOutcomesPersistWithoutFalseAcceptanceOrReplay(t *testing.T) {
	for _, outcome := range []string{Rejected, Uncertain, Accepted} {
		t.Run(outcome, func(t *testing.T) {
			f := newQueueFakeClient()
			if outcome == Rejected {
				f.check = func(context.Context, []string) ([]types.IsOnWhatsAppResponse, error) {
					return []types.IsOnWhatsAppResponse{{IsIn: false}}, nil
				}
			}
			if outcome == Uncertain {
				f.send = func(context.Context, types.JID, *waE2E.Message, ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
					return whatsmeow.SendResponse{}, errors.New("lost transport ACK")
				}
			}
			tr, releases := queueFakeTransport(t, f)
			ledger := serviceNewLedger()
			req := serviceTestRequest()
			first, err := NewService(ledger, tr).Send(context.Background(), testQueueInstance, req)
			if err != nil || first.State != outcome || (first.Receipt != nil) != (outcome == Accepted) {
				t.Fatalf("wrong persisted transport outcome: %+v, %v", first, err)
			}
			checks := f.checks.Load()
			replayed, err := NewService(ledger, tr).Send(context.Background(), testQueueInstance, req)
			if err != nil || !reflect.DeepEqual(replayed, first) || f.checks.Load() != checks {
				t.Fatal("restart/replay repeated preparation or changed the evidence")
			}
			sends := int32(1)
			if outcome == Rejected {
				sends = 0
			}
			queueAssertCounts(t, f, releases, sends, 0)
		})
	}
}

func TestQueueTransportUsesLegacyLookupRemoteJIDAndMessageBodies(t *testing.T) {
	for _, kind := range []string{"text", "image"} {
		for _, server := range []string{types.DefaultUserServer, types.HiddenUserServer} {
			t.Run(kind+"/"+server, func(t *testing.T) {
				f := newQueueFakeClient()
				remote := types.NewJID("553199999999", server)
				var queries []string
				f.check = func(ctx context.Context, numbers []string) ([]types.IsOnWhatsAppResponse, error) {
					if _, ok := ctx.Deadline(); !ok {
						t.Fatal("recipient lookup is not bounded")
					}
					queries = append(queries, numbers...)
					if len(queries) == 1 {
						return []types.IsOnWhatsAppResponse{{IsIn: false}}, nil
					}
					return []types.IsOnWhatsAppResponse{{IsIn: true, JID: remote}}, nil
				}
				msg := Message{Kind: kind, To: "5531999999999@s.whatsapp.net", Text: "Olá <>& 🌍", Caption: "Legenda <>& 🌍"}
				if kind == "image" {
					var encoded bytes.Buffer
					if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
						t.Fatal(err)
					}
					msg.ImageBytes = encoded.Bytes()
					hash := sha256.Sum256(msg.ImageBytes)
					msg.ImageSHA256 = hex.EncodeToString(hash[:])
					msg.MIME = "image/png"
					f.upload = func(_ context.Context, data []byte, media whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
						if !bytes.Equal(data, msg.ImageBytes) || media != whatsmeow.MediaImage {
							t.Fatal("upload changed image bytes or media type")
						}
						return queueTestUpload(data), nil
					}
				}
				f.send = func(_ context.Context, to types.JID, body *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
					if to != remote || len(extra) != 1 || extra[0].ID != testQueueID {
						t.Fatal("send did not use verified remoteJID and pinned ledger ID")
					}
					if body.Conversation != nil {
						t.Fatal("queue must use the legacy ExtendedTextMessage body")
					}
					if kind == "text" {
						if body.GetExtendedTextMessage().GetText() != msg.Text || body.GetExtendedTextMessage().ContextInfo == nil {
							t.Fatal("legacy text/context was not restored")
						}
					} else {
						im := body.GetImageMessage()
						if im == nil || im.GetCaption() != msg.Caption || im.GetMimetype() != msg.MIME || im.ContextInfo == nil || len(im.JPEGThumbnail) == 0 {
							t.Fatal("legacy image/caption/context/thumbnail was not restored")
						}
						if _, format, err := image.DecodeConfig(bytes.NewReader(im.JPEGThumbnail)); err != nil || format != "jpeg" {
							t.Fatal("invalid legacy JPEG preview")
						}
					}
					return whatsmeow.SendResponse{ID: extra[0].ID, Timestamp: testQueueTime}, nil
				}
				tr, releases := queueFakeTransport(t, f)
				p, err := tr.Prepare(context.Background(), testQueueInstance, testQueueID, msg)
				if err != nil {
					t.Fatal(err)
				}
				defer p.Close()
				if !reflect.DeepEqual(queries, []string{"5531999999999", "+553199999999"}) {
					t.Fatalf("legacy raw/formatted queries = %v", queries)
				}
				if _, err := p.Send(context.Background()); err != nil {
					t.Fatal(err)
				}
				if _, err := p.Send(context.Background()); !errors.Is(err, ErrPreparedUsed) {
					t.Fatal("same prepared authorization replayed")
				}
				uploads := int32(0)
				if kind == "image" {
					uploads = 1
				}
				queueAssertCounts(t, f, releases, 1, uploads)
			})
		}
	}
}

func TestQueuePresenceUsesVerifiedRecipientWithoutSending(t *testing.T) {
	f := newQueueFakeClient()
	remote := types.NewJID("553199999999", types.DefaultUserServer)
	f.check = func(context.Context, []string) ([]types.IsOnWhatsAppResponse, error) {
		return []types.IsOnWhatsAppResponse{{IsIn: true, JID: remote}}, nil
	}
	f.chatPresence = func(_ context.Context, to types.JID, _ types.ChatPresence, _ types.ChatPresenceMedia) error {
		if to != remote {
			t.Fatal("presence sent to unresolved recipient")
		}
		return nil
	}
	tr, releases := queueFakeTransport(t, f)
	var duration time.Duration
	tr.wait = func(_ context.Context, d time.Duration) error { duration += d; return nil }
	if err := tr.Presence(context.Background(), testQueueInstance, testQueueTarget, 8000); err != nil {
		t.Fatal(err)
	}
	if duration != 8*time.Second || f.checks.Load() != 1 {
		t.Fatal("resolution changed the required typing duration")
	}
	queueAssertCounts(t, f, releases, 0, 0)
}

func TestQueueRecipientDenialBeforeCommercialCall(t *testing.T) {
	for _, mode := range []string{"not-registered", "invalid-remote", "device-remote", "group-instead-of-person", "canceled", "expired-during-lookup"} {
		t.Run(mode, func(t *testing.T) {
			f := newQueueFakeClient()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			now := testQueueTime
			f.check = func(context.Context, []string) ([]types.IsOnWhatsAppResponse, error) {
				jid := types.NewJID("553199999999", types.DefaultUserServer)
				switch mode {
				case "not-registered":
					return []types.IsOnWhatsAppResponse{{IsIn: false}}, nil
				case "invalid-remote":
					jid.Server = "invalid.example"
				case "device-remote":
					jid.Device = 2
				case "group-instead-of-person":
					jid.Server = types.GroupServer
				case "canceled":
					cancel()
				case "expired-during-lookup":
					now = now.Add(time.Hour)
				}
				return []types.IsOnWhatsAppResponse{{IsIn: true, JID: jid}}, nil
			}
			tr, releases := queueFakeTransport(t, f)
			tr.now = func() time.Time { return now }
			msg := Message{Kind: "text", To: testQueueTarget, Text: "not sent"}
			if mode == "expired-during-lookup" {
				msg.ValidUntil = testQueueTime.Add(time.Minute).Format("2006-01-02T15:04:05.000Z")
			}
			p, err := tr.Prepare(ctx, testQueueInstance, testQueueID, msg)
			if err == nil || p != nil {
				t.Fatal("unusable recipient authorized a message")
			}
			if mode == "expired-during-lookup" && !errors.Is(err, ErrExpired) {
				t.Fatal("deadline was not checked after recipient preparation")
			}
			queueAssertCounts(t, f, releases, 0, 0)
		})
	}
}

func TestQueueRecipientGroupsAndLIDsAreNotReformattedAsPhones(t *testing.T) {
	for _, to := range []string{"120363000000000001@g.us", "123456789012345@lid"} {
		f := newQueueFakeClient()
		f.check = func(context.Context, []string) ([]types.IsOnWhatsAppResponse, error) {
			t.Fatal("group/LID must bypass phone lookup")
			return nil, nil
		}
		f.send = func(_ context.Context, got types.JID, _ *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
			if got.String() != to {
				t.Fatal("canonical group/LID changed")
			}
			return whatsmeow.SendResponse{ID: extra[0].ID, Timestamp: testQueueTime}, nil
		}
		tr, releases := queueFakeTransport(t, f)
		p, err := tr.Prepare(context.Background(), testQueueInstance, testQueueID, Message{Kind: "text", To: to, Text: "owned text"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Send(context.Background()); err != nil {
			t.Fatal(err)
		}
		p.Close()
		queueAssertCounts(t, f, releases, 1, 0)
	}
}
