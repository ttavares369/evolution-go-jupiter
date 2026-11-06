package queuev1

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

const testQueueInstance = "00000000-0000-4000-8000-000000000001"
const testQueueID = "3EB0000000000000000001"
const testQueueTarget = "5511999999999@s.whatsapp.net"

var testQueueTime = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

type queueFakeClient struct {
	connected    bool
	loggedIn     bool
	sends        atomic.Int32
	uploads      atomic.Int32
	checks       atomic.Int32
	check        func(context.Context, []string) ([]types.IsOnWhatsAppResponse, error)
	send         func(context.Context, types.JID, *waE2E.Message, ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error)
	upload       func(context.Context, []byte, whatsmeow.MediaType) (whatsmeow.UploadResponse, error)
	presence     func(context.Context, types.Presence) error
	chatPresence func(context.Context, types.JID, types.ChatPresence, types.ChatPresenceMedia) error
}

func newQueueFakeClient() *queueFakeClient   { return &queueFakeClient{connected: true, loggedIn: true} }
func (f *queueFakeClient) IsConnected() bool { return f.connected }
func (f *queueFakeClient) IsLoggedIn() bool  { return f.loggedIn }
func (f *queueFakeClient) IsOnWhatsApp(ctx context.Context, numbers []string) ([]types.IsOnWhatsAppResponse, error) {
	f.checks.Add(1)
	if f.check != nil {
		return f.check(ctx, numbers)
	}
	return []types.IsOnWhatsAppResponse{{IsIn: true, JID: types.NewJID(strings.TrimPrefix(strings.Split(numbers[0], "@")[0], "+"), types.DefaultUserServer)}}, nil
}
func (f *queueFakeClient) SendMessage(ctx context.Context, to types.JID, msg *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	f.sends.Add(1)
	if f.send != nil {
		return f.send(ctx, to, msg, extra...)
	}
	return whatsmeow.SendResponse{ID: extra[0].ID, Timestamp: testQueueTime}, nil
}
func (f *queueFakeClient) Upload(ctx context.Context, data []byte, media whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	f.uploads.Add(1)
	return f.upload(ctx, data, media)
}
func (f *queueFakeClient) SendPresence(ctx context.Context, state types.Presence) error {
	if f.presence != nil {
		return f.presence(ctx, state)
	}
	return ctx.Err()
}
func (f *queueFakeClient) SendChatPresence(ctx context.Context, to types.JID, state types.ChatPresence, media types.ChatPresenceMedia) error {
	if f.chatPresence != nil {
		return f.chatPresence(ctx, to, state, media)
	}
	return ctx.Err()
}

func queueFakeTransport(t *testing.T, f *queueFakeClient) (*queueTransport, *atomic.Int32) {
	t.Helper()
	releases := &atomic.Int32{}
	return &queueTransport{lease: func(ctx context.Context, id string) (WAClient, context.Context, func(), error) {
		if id != testQueueInstance {
			t.Errorf("instance = %q", id)
		}
		leased, cancel := context.WithCancel(ctx)
		return f, leased, func() { releases.Add(1); cancel() }, nil
	}, wait: waitQueuePresence}, releases
}

func queuePrepareText(t *testing.T, tr Transport, ctx context.Context) Prepared {
	t.Helper()
	p, err := tr.Prepare(ctx, testQueueInstance, testQueueID, Message{Kind: "text", To: testQueueTarget, Text: "owned text"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func queueAssertCounts(t *testing.T, f *queueFakeClient, releases *atomic.Int32, sends, uploads int32) {
	t.Helper()
	if f.sends.Load() != sends || f.uploads.Load() != uploads || releases.Load() != 1 {
		t.Fatalf("sends=%d uploads=%d releases=%d", f.sends.Load(), f.uploads.Load(), releases.Load())
	}
}

func TestQueueTransportTextSingleCallPinnedIDAndNoTyping(t *testing.T) {
	f := newQueueFakeClient()
	f.presence = func(context.Context, types.Presence) error { t.Error("typing during send"); return nil }
	f.chatPresence = func(context.Context, types.JID, types.ChatPresence, types.ChatPresenceMedia) error {
		t.Error("chatstate during send")
		return nil
	}
	f.send = func(ctx context.Context, to types.JID, msg *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
		if to.String() != testQueueTarget || !proto.Equal(msg, &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("owned text"), ContextInfo: &waE2E.ContextInfo{}}}) {
			t.Errorf("unexpected payload/target")
		}
		if len(extra) != 1 || extra[0].ID != testQueueID || extra[0].Timeout <= 0 || extra[0].Timeout > queueSendTimeout {
			t.Errorf("extra = %+v", extra)
		}
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > queueSendTimeout {
			t.Error("send not bounded")
		}
		msg.ExtendedTextMessage.Text = proto.String("client mutation")
		return whatsmeow.SendResponse{ID: extra[0].ID, Timestamp: testQueueTime}, nil
	}
	tr, releases := queueFakeTransport(t, f)
	p := queuePrepareText(t, tr, context.Background())
	receipt, err := p.Send(context.Background())
	if err != nil || receipt.MessageID != testQueueID || !receipt.AcceptedAt.Equal(testQueueTime) {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	if p.(*queuePrepared).message.GetExtendedTextMessage().GetText() != "owned text" {
		t.Fatal("client mutated retained protobuf")
	}
	if _, err = p.Send(context.Background()); !errors.Is(err, ErrPreparedUsed) {
		t.Fatal(err)
	}
	p.Close()
	p.Close()
	queueAssertCounts(t, f, releases, 1, 0)
}

func TestQueueTransportAmbiguousErrorsAndInvalidReceiptsNeverRetry(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response whatsmeow.SendResponse
		err      error
	}{
		{"timeout", whatsmeow.SendResponse{}, context.DeadlineExceeded},
		{"aborted-write", whatsmeow.SendResponse{}, context.Canceled},
		{"no-ack", whatsmeow.SendResponse{}, whatsmeow.ErrMessageTimedOut},
		{"disconnect", whatsmeow.SendResponse{}, whatsmeow.ErrNotConnected},
		{"mismatch", whatsmeow.SendResponse{ID: "other", Timestamp: testQueueTime}, nil},
		{"zero-time", whatsmeow.SendResponse{ID: testQueueID}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newQueueFakeClient()
			f.send = func(context.Context, types.JID, *waE2E.Message, ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
				return tc.response, tc.err
			}
			tr, releases := queueFakeTransport(t, f)
			p := queuePrepareText(t, tr, context.Background())
			r, err := p.Send(context.Background())
			if !errors.Is(err, ErrAmbiguousSend) || r != (Receipt{}) {
				t.Fatalf("receipt=%+v err=%v", r, err)
			}
			if tc.err != nil && !errors.Is(err, tc.err) {
				t.Fatalf("lost cause: %v", err)
			}
			if tc.err == nil && !errors.Is(err, ErrInvalidReceipt) {
				t.Fatalf("lost receipt error: %v", err)
			}
			_, _ = p.Send(context.Background())
			queueAssertCounts(t, f, releases, 1, 0)
		})
	}
}

func TestQueueTransportMergedCancellationAndLateWrite(t *testing.T) {
	for _, mode := range []string{"send-context", "lease-context", "deadline", "close", "duplicate"} {
		t.Run(mode, func(t *testing.T) {
			f := newQueueFakeClient()
			entered, canceled, finish := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var finishOnce sync.Once
			t.Cleanup(func() { finishOnce.Do(func() { close(finish) }) })
			f.send = func(ctx context.Context, _ types.JID, _ *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
				close(entered)
				<-ctx.Done()
				close(canceled)
				// Simulate a write/late ACK continuing after cancellation.
				<-finish
				return whatsmeow.SendResponse{ID: extra[0].ID, Timestamp: testQueueTime}, nil
			}
			tr, releases := queueFakeTransport(t, f)
			leaseCtx, cancelLease := context.WithCancel(context.Background())
			defer cancelLease()
			p := queuePrepareText(t, tr, leaseCtx)
			sendCtx, cancelSend := context.WithCancel(context.Background())
			defer cancelSend()
			if mode == "deadline" {
				var cancel context.CancelFunc
				sendCtx, cancel = context.WithTimeout(sendCtx, 50*time.Millisecond)
				defer cancel()
			}
			result := make(chan error, 1)
			go func() { _, err := p.Send(sendCtx); result <- err }()
			queueAwait(t, entered)
			switch mode {
			case "send-context":
				cancelSend()
			case "lease-context":
				cancelLease()
			case "close":
				p.Close()
			case "duplicate":
				if _, err := p.Send(context.Background()); !errors.Is(err, ErrPreparedUsed) {
					t.Fatal(err)
				}
				if p.(*queuePrepared).ctx.Err() != nil {
					t.Fatal("duplicate canceled the lease context")
				}
				select {
				case <-canceled:
					t.Fatal("duplicate canceled the authorized send")
				default:
				}
				cancelSend()
			}
			queueAwait(t, canceled)
			if releases.Load() != 0 {
				t.Fatal("lease released while client call is still running")
			}
			finishOnce.Do(func() { close(finish) })
			select {
			case err := <-result:
				if !errors.Is(err, ErrAmbiguousSend) {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("send did not return")
			}
			p.Close()
			queueAssertCounts(t, f, releases, 1, 0)
		})
	}
}

func queueAwait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for test synchronization")
	}
}

func TestQueueTransportConcurrentSendGuard(t *testing.T) {
	f := newQueueFakeClient()
	tr, releases := queueFakeTransport(t, f)
	p := queuePrepareText(t, tr, context.Background())
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = p.Send(context.Background()) }()
	}
	wg.Wait()
	if f.sends.Load() != 1 {
		t.Fatalf("commercial calls=%d, want exactly one", f.sends.Load())
	}
	if releases.Load() != 1 {
		t.Fatalf("release count=%d", releases.Load())
	}
}

func TestQueueTransportClosedOrCanceledBeforeSend(t *testing.T) {
	for _, mode := range []string{"closed", "canceled", "disconnected", "logged-out"} {
		t.Run(mode, func(t *testing.T) {
			f := newQueueFakeClient()
			tr, releases := queueFakeTransport(t, f)
			p := queuePrepareText(t, tr, context.Background())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "closed":
				p.Close()
			case "canceled":
				cancel()
			case "disconnected":
				f.connected = false
			case "logged-out":
				f.loggedIn = false
			}
			_, err := p.Send(ctx)
			if err == nil || errors.Is(err, ErrAmbiguousSend) {
				t.Fatalf("pre-call error=%v", err)
			}
			queueAssertCounts(t, f, releases, 0, 0)
		})
	}
}

func queueTestImage() Message {
	data := []byte("already-validated-image-fixture")
	hash := sha256.Sum256(data)
	return Message{Kind: "image", To: testQueueTarget, Caption: "caption", MIME: "image/png", ImageSHA256: hex.EncodeToString(hash[:]), ImageBytes: data}
}

func queueTestUpload(data []byte) whatsmeow.UploadResponse {
	hash := sha256.Sum256(data)
	return whatsmeow.UploadResponse{URL: "https://media.invalid/image", DirectPath: "/image", MediaKey: bytes.Repeat([]byte{1}, 32), FileEncSHA256: bytes.Repeat([]byte{2}, 32), FileSHA256: bytes.Clone(hash[:]), FileLength: uint64(len(data))}
}

func TestQueueTransportImageUploadAndOwnedProtobuf(t *testing.T) {
	f := newQueueFakeClient()
	msg := queueTestImage()
	original := bytes.Clone(msg.ImageBytes)
	upload := queueTestUpload(original)
	want := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{URL: proto.String(upload.URL), DirectPath: proto.String(upload.DirectPath), MediaKey: bytes.Clone(upload.MediaKey), FileEncSHA256: bytes.Clone(upload.FileEncSHA256), FileSHA256: bytes.Clone(upload.FileSHA256), FileLength: proto.Uint64(upload.FileLength), Caption: proto.String(msg.Caption), Mimetype: proto.String(msg.MIME), ContextInfo: &waE2E.ContextInfo{}}}
	f.upload = func(ctx context.Context, data []byte, media whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
		if !bytes.Equal(data, original) || media != whatsmeow.MediaImage {
			t.Error("wrong upload")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Error("unbounded upload")
		}
		data[0] ^= 0xff
		return upload, nil
	}
	f.send = func(_ context.Context, _ types.JID, got *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
		if !proto.Equal(got, want) || extra[0].ID != testQueueID {
			t.Error("image payload changed")
		}
		got.ImageMessage.MediaKey[0] ^= 0xff
		return whatsmeow.SendResponse{ID: testQueueID, Timestamp: testQueueTime}, nil
	}
	tr, releases := queueFakeTransport(t, f)
	p, err := tr.Prepare(context.Background(), testQueueInstance, testQueueID, msg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if !bytes.Equal(msg.ImageBytes, original) {
		t.Fatal("upload mutated caller buffer")
	}
	msg.ImageBytes[0] ^= 0xff
	upload.MediaKey[0] ^= 0xff
	upload.FileSHA256[0] ^= 0xff
	upload.FileEncSHA256[0] ^= 0xff
	if _, err = p.Send(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(p.(*queuePrepared).message, want) {
		t.Fatal("shared retained image protobuf")
	}
	queueAssertCounts(t, f, releases, 1, 1)
}

func TestQueueTransportPrepareFailuresReleaseWithoutSend(t *testing.T) {
	for _, mode := range []string{"upload-failed", "upload-bad-hash", "upload-bad-length", "upload-bad-key", "upload-canceled", "disconnected", "logged-out"} {
		t.Run(mode, func(t *testing.T) {
			f := newQueueFakeClient()
			msg := queueTestImage()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.upload = func(context.Context, []byte, whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
				u := queueTestUpload(msg.ImageBytes)
				switch mode {
				case "upload-failed":
					return u, errors.New("upload rejected")
				case "upload-bad-hash":
					u.FileSHA256[0] ^= 0xff
				case "upload-bad-length":
					u.FileLength++
				case "upload-bad-key":
					u.MediaKey = nil
				case "upload-canceled":
					cancel()
				}
				return u, nil
			}
			if mode == "disconnected" {
				f.connected = false
			}
			if mode == "logged-out" {
				f.loggedIn = false
			}
			tr, releases := queueFakeTransport(t, f)
			p, err := tr.Prepare(ctx, testQueueInstance, testQueueID, msg)
			if err == nil || p != nil || errors.Is(err, ErrAmbiguousSend) {
				t.Fatalf("prepared=%v err=%v", p, err)
			}
			uploads := int32(1)
			if mode == "disconnected" || mode == "logged-out" {
				uploads = 0
			}
			queueAssertCounts(t, f, releases, 0, uploads)
		})
	}
}

func TestQueueTransportValidationAndDefaultUnavailable(t *testing.T) {
	tr := NewTransport(nil)
	if p, err := tr.Prepare(context.Background(), testQueueInstance, testQueueID, Message{Kind: "text", To: testQueueTarget, Text: "x"}); err == nil || p != nil {
		t.Fatal("nil runtime accepted")
	}
	f := newQueueFakeClient()
	fake, _ := queueFakeTransport(t, f)
	fake.lease = func(context.Context, string) (WAClient, context.Context, func(), error) {
		t.Fatal("lease acquired before validation")
		return nil, nil, nil, nil
	}
	for _, tc := range []struct {
		id  string
		msg Message
	}{
		{"", Message{Kind: "text", To: testQueueTarget, Text: "x"}},
		{testQueueID, Message{Kind: "text", To: testQueueTarget}},
		{testQueueID, Message{Kind: "other", To: testQueueTarget}},
		{testQueueID, Message{Kind: "text", To: "status@broadcast", Text: "x"}},
		{testQueueID, Message{Kind: "text", To: "1@newsletter", Text: "x"}},
		{testQueueID, Message{Kind: "text", To: "5511999999999:2@s.whatsapp.net", Text: "x"}},
		{testQueueID, Message{Kind: "image", To: testQueueTarget, MIME: "image/png", ImageBytes: []byte("bad hash")}},
	} {
		if _, err := fake.Prepare(context.Background(), testQueueInstance, tc.id, tc.msg); err == nil {
			t.Errorf("accepted invalid %+v", tc)
		}
	}
	for _, ms := range []int{0, 7999, 26001} {
		if err := fake.Presence(context.Background(), testQueueInstance, testQueueTarget, ms); err == nil {
			t.Fatal("accepted invalid duration")
		}
	}
}

func TestQueueTransportLeaseFailureCleanup(t *testing.T) {
	f := newQueueFakeClient()
	tr, releases := queueFakeTransport(t, f)
	cause := errors.New("lease refused")
	tr.lease = func(context.Context, string) (WAClient, context.Context, func(), error) {
		return nil, nil, func() { releases.Add(1) }, cause
	}
	if _, err := tr.Prepare(context.Background(), testQueueInstance, testQueueID, Message{Kind: "text", To: testQueueTarget, Text: "x"}); !errors.Is(err, cause) {
		t.Fatal(err)
	}
	if err := tr.Presence(context.Background(), testQueueInstance, testQueueTarget, 8000); !errors.Is(err, cause) {
		t.Fatal(err)
	}
	if releases.Load() != 2 || f.sends.Load() != 0 {
		t.Fatal("lease error cleanup")
	}
}

func TestQueueTransportTechnicalResendsRemainInsideOneCall(t *testing.T) {
	f := newQueueFakeClient()
	var technicalIDs []string
	f.send = func(_ context.Context, _ types.JID, _ *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
		// This seam models protocol retransmission, not actual websocket frames.
		for range 3 {
			technicalIDs = append(technicalIDs, extra[0].ID)
		}
		return whatsmeow.SendResponse{ID: extra[0].ID, Timestamp: testQueueTime}, nil
	}
	tr, releases := queueFakeTransport(t, f)
	p := queuePrepareText(t, tr, context.Background())
	if _, err := p.Send(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(technicalIDs, []string{testQueueID, testQueueID, testQueueID}) {
		t.Fatal(technicalIDs)
	}
	queueAssertCounts(t, f, releases, 1, 0)
}

func TestQueuePresenceClockSchedulesAndFaults(t *testing.T) {
	for _, ms := range []int{8000, 17500, 26000} {
		t.Run(fmt.Sprint(ms), func(t *testing.T) {
			f := newQueueFakeClient()
			tr, releases := queueFakeTransport(t, f)
			var elapsed time.Duration
			var composing []time.Duration
			var paused []time.Duration
			online := 0
			tr.wait = func(ctx context.Context, d time.Duration) error {
				if d <= 0 || d > 5*time.Second {
					t.Error("invalid clock step")
				}
				elapsed += d
				return ctx.Err()
			}
			f.presence = func(_ context.Context, state types.Presence) error {
				if state != types.PresenceAvailable {
					t.Error("not available")
				}
				online++
				return nil
			}
			f.chatPresence = func(_ context.Context, to types.JID, state types.ChatPresence, media types.ChatPresenceMedia) error {
				if to.String() != testQueueTarget || media != types.ChatPresenceMediaText {
					t.Error("wrong chatstate target/media")
				}
				if state == types.ChatPresenceComposing {
					composing = append(composing, elapsed)
				} else if state == types.ChatPresencePaused {
					paused = append(paused, elapsed)
				} else {
					t.Error("wrong state")
				}
				return nil
			}
			if err := tr.Presence(context.Background(), testQueueInstance, testQueueTarget, ms); err != nil {
				t.Fatal(err)
			}
			duration := time.Duration(ms) * time.Millisecond
			var expected []time.Duration
			for at := time.Duration(0); at < duration; at += 5 * time.Second {
				expected = append(expected, at)
			}
			if elapsed != duration || online != 1 || !reflect.DeepEqual(composing, expected) || !reflect.DeepEqual(paused, []time.Duration{duration}) {
				t.Fatalf("elapsed=%v composing=%v paused=%v", elapsed, composing, paused)
			}
			queueAssertCounts(t, f, releases, 0, 0)
		})
	}
	for _, mode := range []string{"available", "initial", "refresh", "paused", "clock", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			f := newQueueFakeClient()
			tr, releases := queueFakeTransport(t, f)
			cause := errors.New(mode)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			composes, pauses := 0, 0
			tr.wait = func(ctx context.Context, d time.Duration) error {
				if mode == "clock" {
					return cause
				}
				if mode == "cancel" {
					cancel()
					return ctx.Err()
				}
				return nil
			}
			f.presence = func(context.Context, types.Presence) error {
				if mode == "available" {
					return cause
				}
				return nil
			}
			f.chatPresence = func(_ context.Context, _ types.JID, state types.ChatPresence, _ types.ChatPresenceMedia) error {
				if state == types.ChatPresencePaused {
					pauses++
					if mode == "paused" {
						return cause
					}
				} else {
					composes++
					if mode == "initial" || (mode == "refresh" && composes == 2) {
						return cause
					}
				}
				return nil
			}
			err := tr.Presence(ctx, testQueueInstance, testQueueTarget, 17500)
			want := cause
			if mode == "cancel" {
				want = context.Canceled
			}
			if !errors.Is(err, want) {
				t.Fatalf("fault not confirmed: %v", err)
			}
			if mode != "available" && pauses != 1 {
				t.Fatalf("paused calls=%d", pauses)
			}
			queueAssertCounts(t, f, releases, 0, 0)
		})
	}
}

func TestQueuePresenceRealWaitCancelsWithoutSleep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitQueuePresence(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestQueueWhatsmeowRetryCachePreservesIDBeforeIO(t *testing.T) {
	// Exercise the pinned dependency, but stop in PreRetryCallback before any
	// encryption or I/O. This does not certify retransmitted network frames.
	own := types.NewJID("5511000000000", types.DefaultUserServer)
	device := &store.Device{ID: &own}
	device.SetAllStores(&store.NoopStore{})
	client := whatsmeow.NewClient(device, nil)
	to := types.NewJID("5511999999999", types.DefaultUserServer)
	msg := &waE2E.Message{Conversation: proto.String("cached text")}
	internal := client.DangerousInternals()
	if err := internal.AddRecentMessage(context.Background(), to, testQueueID, msg, nil); err != nil {
		t.Fatal(err)
	}
	receipt := &events.Receipt{MessageSource: types.MessageSource{Chat: to, Sender: to}, MessageIDs: []types.MessageID{testQueueID}}
	calls := 0
	client.PreRetryCallback = func(got *events.Receipt, id types.MessageID, count int, payload *waE2E.Message) bool {
		calls++
		if got != receipt || id != testQueueID || count != calls || !proto.Equal(payload, msg) {
			t.Error("retry identity/content changed")
		}
		return false
	}
	for count := 1; count <= 2; count++ {
		node := &waBinary.Node{Attrs: waBinary.Attrs{"from": to}, Content: []waBinary.Node{{Tag: "retry", Attrs: waBinary.Attrs{"id": testQueueID, "count": fmt.Sprint(count), "t": fmt.Sprint(testQueueTime.Unix())}}}}
		if err := internal.HandleRetryReceipt(context.Background(), receipt, node); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 || client.IsConnected() {
		t.Fatalf("callbacks=%d connected=%v", calls, client.IsConnected())
	}
	if internal.GetRecentMessage(to, testQueueID).IsEmpty() {
		t.Fatal("retry payload lost")
	}
}
