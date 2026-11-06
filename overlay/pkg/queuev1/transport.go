package queuev1

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	send_service "github.com/evolution-foundation/evolution-go/pkg/sendMessage/service"
	whatsmeow_service "github.com/evolution-foundation/evolution-go/pkg/whatsmeow/service"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

const queueSendTimeout = 25 * time.Second

var (
	ErrAmbiguousSend  = errors.New("queue send outcome is ambiguous")
	ErrPreparedUsed   = errors.New("queue prepared message already used or closed")
	ErrInvalidReceipt = errors.New("queue send receipt does not match the operation")
)

// WAClient is deliberately smaller than the legacy send service. Protocol-level
// retransmissions remain the responsibility of the single SendMessage call.
type WAClient interface {
	IsConnected() bool
	IsLoggedIn() bool
	IsOnWhatsApp(context.Context, []string) ([]types.IsOnWhatsAppResponse, error)
	SendMessage(context.Context, types.JID, *waE2E.Message, ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error)
	Upload(context.Context, []byte, whatsmeow.MediaType) (whatsmeow.UploadResponse, error)
	SendPresence(context.Context, types.Presence) error
	SendChatPresence(context.Context, types.JID, types.ChatPresence, types.ChatPresenceMedia) error
}

// ClientLease is the injectable lifetime seam; release must be idempotent and
// must not be called while a client method is still executing.
type ClientLease func(context.Context, string) (WAClient, context.Context, func(), error)

type queueTransport struct {
	lease ClientLease
	wait  func(context.Context, time.Duration) error
	now   func() time.Time
}

// NewTransport never starts a runtime or routes through legacy send wrappers.
func NewTransport(svc whatsmeow_service.WhatsmeowService) Transport {
	return &queueTransport{
		lease: func(ctx context.Context, id string) (WAClient, context.Context, func(), error) {
			client, leasedCtx, release, err := whatsmeow_service.AcquireQueueClient(svc, ctx, id)
			return client, leasedCtx, release, err
		},
		wait: waitQueuePresence,
		now:  time.Now,
	}
}

func queueTarget(to string) (types.JID, error) {
	jid, err := types.ParseJID(to)
	if err != nil || jid.User == "" || jid.Device != 0 || jid.RawAgent != 0 || jid.Integrator != 0 || jid.String() != to {
		return types.EmptyJID, errors.New("queue target must be a canonical non-device JID")
	}
	switch jid.Server {
	case types.DefaultUserServer, types.HiddenUserServer, types.GroupServer:
		return jid, nil
	default:
		return types.EmptyJID, errors.New("queue target server is unsupported")
	}
}

// Resolve with the same number normalization and WhatsApp remoteJID lookup as
// legacy sending, but without entering its connection/send retry wrappers.
func resolveQueueTarget(ctx context.Context, client WAClient, to string) (types.JID, error) {
	original, err := queueTarget(to)
	if err != nil {
		return types.EmptyJID, err
	}
	phone := to
	if original.Server == types.DefaultUserServer {
		// Legacy Number is a phone number. Passing a PN JID to IsOnWhatsApp
		// would query "number@s.whatsapp.net@c.us" instead of that number.
		phone = original.User
	}
	target, err := send_service.ResolveMessageRecipient(ctx, client, phone, nil, true)
	if err != nil {
		return types.EmptyJID, err
	}
	target, err = queueTarget(target.String())
	if err != nil || (original.Server != types.GroupServer && target.Server == types.GroupServer) {
		return types.EmptyJID, errors.New("queue resolved recipient is invalid")
	}
	return target, nil
}

func queueClientReady(ctx context.Context, client WAClient) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if client == nil || !client.IsConnected() || !client.IsLoggedIn() {
		return errors.New("queue client is not connected and logged in")
	}
	return nil
}

func (t *queueTransport) Prepare(ctx context.Context, instanceID, messageID string, msg Message) (Prepared, error) {
	if !canonicalDeadline(msg.ValidUntil) {
		return nil, ErrInvalid
	}
	if deadlineExpired(msg.ValidUntil, t.now) {
		return nil, ErrExpired
	}
	if instanceID == "" || messageID == "" {
		return nil, errors.New("queue instance and stable message ID are required")
	}
	target, err := queueTarget(msg.To)
	if err != nil {
		return nil, err
	}
	if msg.Kind != "text" && msg.Kind != "image" {
		return nil, errors.New("queue message kind is unsupported")
	}
	if msg.Kind == "text" && msg.Text == "" {
		return nil, errors.New("queue text is required")
	}
	// Do not retain the caller's image buffer or pass it to a mutating uploader.
	image := bytes.Clone(msg.ImageBytes)
	if msg.Kind == "image" {
		hash := sha256.Sum256(image)
		if len(image) == 0 || len(image) > MaxImageBytes || msg.MIME == "" || hex.EncodeToString(hash[:]) != msg.ImageSHA256 {
			return nil, errors.New("queue validated image metadata does not match its bytes")
		}
	}
	client, leasedCtx, release, err := t.lease(ctx, instanceID)
	if err != nil {
		if release != nil {
			release()
		}
		return nil, err
	}
	ownedCtx, cancel := context.WithCancel(leasedCtx)
	p := &queuePrepared{client: client, ctx: ownedCtx, cancel: cancel, release: release, target: target, id: messageID, validUntil: msg.ValidUntil, now: t.now}
	prepared := false
	defer func() {
		if !prepared {
			p.Close()
		}
	}()
	if err = queueClientReady(ownedCtx, client); err != nil {
		return nil, err
	}
	lookupCtx, stopLookup := context.WithTimeout(ownedCtx, queueSendTimeout)
	p.target, err = resolveQueueTarget(lookupCtx, client, msg.To)
	stopLookup()
	if err != nil {
		return nil, err
	}
	if msg.Kind == "text" {
		body := send_service.SharedTextMessage(msg.Text)
		body.ContextInfo = &waE2E.ContextInfo{}
		p.message = &waE2E.Message{ExtendedTextMessage: body}
	} else {
		uploadCtx, stop := context.WithTimeout(ownedCtx, queueSendTimeout)
		defer stop()
		if deadlineExpired(msg.ValidUntil, t.now) {
			return nil, ErrExpired
		}
		upload, uploadErr := client.Upload(uploadCtx, bytes.Clone(image), whatsmeow.MediaImage)
		if uploadErr != nil {
			return nil, fmt.Errorf("queue image upload failed: %w", uploadErr)
		}
		hash := sha256.Sum256(image)
		if upload.URL == "" || upload.DirectPath == "" || len(upload.MediaKey) != 32 || len(upload.FileEncSHA256) != sha256.Size || !bytes.Equal(upload.FileSHA256, hash[:]) || upload.FileLength != uint64(len(image)) {
			return nil, errors.New("queue image upload response is invalid")
		}
		body := send_service.SharedImageMessage(image, msg.MIME, msg.Caption, upload, false)
		body.ContextInfo = &waE2E.ContextInfo{}
		p.message = proto.Clone(&waE2E.Message{ImageMessage: body}).(*waE2E.Message)
	}
	if err = ownedCtx.Err(); err != nil {
		return nil, err
	}
	if deadlineExpired(msg.ValidUntil, t.now) {
		return nil, ErrExpired
	}
	prepared = true
	return p, nil
}

type queuePrepared struct {
	client      WAClient
	ctx         context.Context
	cancel      context.CancelFunc
	release     func()
	releaseOnce sync.Once
	used        atomic.Bool
	mu          sync.Mutex
	closed      bool
	sending     bool
	target      types.JID
	id          string
	message     *waE2E.Message
	validUntil  string
	now         func() time.Time
}

func (p *queuePrepared) releaseLease() {
	p.releaseOnce.Do(func() {
		if p.release != nil {
			p.release()
		}
	})
}

func (p *queuePrepared) Close() {
	p.mu.Lock()
	p.closed = true
	p.cancel()
	active := p.sending
	p.mu.Unlock()
	// A concurrent Close cancels Send, but must not let shutdown close its store
	// until the actual client call returns (even if the client ignores context).
	if !active {
		p.releaseLease()
	}
}

func (p *queuePrepared) Send(ctx context.Context) (Receipt, error) {
	p.mu.Lock()
	if !p.used.CompareAndSwap(false, true) {
		active := p.sending
		p.mu.Unlock()
		// The original call owns the lease until it returns. A duplicate must
		// neither release its store early nor cancel the authorized send.
		if !active {
			p.Close()
		}
		return Receipt{}, ErrPreparedUsed
	}
	if p.closed {
		p.mu.Unlock()
		p.Close()
		return Receipt{}, ErrPreparedUsed
	}
	p.sending = true
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.sending = false
		p.closed = true
		p.cancel()
		p.mu.Unlock()
		p.releaseLease()
	}()

	sendCtx, cancel := context.WithTimeout(p.ctx, queueSendTimeout)
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	defer cancel()
	if err := ctx.Err(); err != nil {
		return Receipt{}, err
	}
	if err := queueClientReady(sendCtx, p.client); err != nil {
		return Receipt{}, err
	}
	message := proto.Clone(p.message).(*waE2E.Message)
	// Last application instruction before entering Whatsmeow. Once this call
	// starts, expiry is never negative evidence, even if it returns an error.
	if deadlineExpired(p.validUntil, p.now) {
		return Receipt{}, ErrExpired
	}
	resp, err := p.client.SendMessage(sendCtx, p.target, message, whatsmeow.SendRequestExtra{ID: p.id, Timeout: queueSendTimeout})
	if err != nil {
		return Receipt{}, fmt.Errorf("%w: %w", ErrAmbiguousSend, err)
	}
	if err := sendCtx.Err(); err != nil {
		return Receipt{}, fmt.Errorf("%w: %w", ErrAmbiguousSend, err)
	}
	if resp.ID != p.id || resp.Timestamp.IsZero() {
		return Receipt{}, fmt.Errorf("%w: %w", ErrAmbiguousSend, ErrInvalidReceipt)
	}
	return Receipt{MessageID: resp.ID, AcceptedAt: resp.Timestamp}, nil
}

func waitQueuePresence(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}

func (t *queueTransport) Presence(ctx context.Context, instanceID, to string, durationMS int) (err error) {
	if instanceID == "" || durationMS < 8000 || durationMS > 26000 {
		return errors.New("queue presence duration must be between 8000 and 26000 ms")
	}
	target, err := queueTarget(to)
	if err != nil {
		return err
	}
	client, leasedCtx, release, err := t.lease(ctx, instanceID)
	if release != nil {
		defer release()
	}
	if err != nil {
		return err
	}
	remaining := time.Duration(durationMS) * time.Millisecond
	presenceCtx, cancel := context.WithTimeout(leasedCtx, remaining+queueSendTimeout)
	defer cancel()
	if err = queueClientReady(presenceCtx, client); err != nil {
		return err
	}
	target, err = resolveQueueTarget(presenceCtx, client, to)
	if err != nil {
		return err
	}
	if err = client.SendPresence(presenceCtx, types.PresenceAvailable); err != nil {
		return err
	}
	defer func() {
		pausedErr := client.SendChatPresence(presenceCtx, target, types.ChatPresencePaused, types.ChatPresenceMediaText)
		if pausedErr != nil {
			err = errors.Join(err, fmt.Errorf("queue paused confirmation failed: %w", pausedErr))
		}
	}()
	if err = client.SendChatPresence(presenceCtx, target, types.ChatPresenceComposing, types.ChatPresenceMediaText); err != nil {
		return err
	}
	for remaining > 0 {
		step := min(remaining, 5*time.Second)
		if err = t.wait(presenceCtx, step); err != nil {
			return err
		}
		remaining -= step
		if remaining > 0 {
			if err = client.SendChatPresence(presenceCtx, target, types.ChatPresenceComposing, types.ChatPresenceMediaText); err != nil {
				return err
			}
		}
	}
	return presenceCtx.Err()
}

var _ WAClient = (*whatsmeow.Client)(nil)
var _ Transport = (*queueTransport)(nil)
var _ Prepared = (*queuePrepared)(nil)
