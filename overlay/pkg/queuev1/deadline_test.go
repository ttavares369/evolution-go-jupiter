package queuev1

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

func TestDeadlineCanonicalIdentityAndJSON(t *testing.T) {
	req := serviceTestRequest()
	_, legacy, _ := serviceMessage(req)
	for _, value := range []string{"2026-11-05T13:00:00Z", "2026-11-05T13:00:00.00Z", "2026-11-05T13:00:00.0000Z", "2026-11-05T13:00:00.000+00:00", "2026-11-05T10:00:00.000-03:00", "2026-02-30T13:00:00.000Z", " 2026-11-05T13:00:00.000Z"} {
		req.ValidUntil = value
		if _, _, err := serviceMessage(req); !errors.Is(err, ErrInvalid) {
			t.Fatalf("noncanonical deadline accepted: %q", value)
		}
	}
	req.ValidUntil = "2026-11-05T13:00:00.000Z"
	msg, hash, err := serviceMessage(req)
	canonical, _ := json.Marshal([8]string{"queue-v1/content/1", req.Kind, req.To, req.Text, "", "", "", req.ValidUntil})
	sum := sha256.Sum256(canonical)
	if err != nil || hash == legacy || hash != hex.EncodeToString(sum[:]) || msg.ValidUntil != req.ValidUntil {
		t.Fatal("deadline not bound to canonical tuple8")
	}
	// Shared consumer vectors: tuple7 is unchanged; tuple8 appends, never
	// substitutes, the canonical deadline under the original content prefix.
	vector := serviceTestRequest()
	vector.Text = "hello"
	for _, v := range []struct{ deadline, hash string }{
		{"", "3c97e4881583e1cc954e7463d0e3b245967b0880927a4ac8996bedb92bb16c56"},
		{"2026-11-05T13:00:00.000Z", "60c779fddefad7cd7a56223c268409bde210b77b71a214992af47710de54e51e"},
	} {
		vector.ValidUntil = v.deadline
		if _, hash, err := serviceMessage(vector); err != nil || hash != v.hash {
			t.Fatalf("content hash vector changed: %s %v", hash, err)
		}
	}
	without, _ := json.Marshal(Attempt{})
	with, _ := json.Marshal(Attempt{ValidUntil: req.ValidUntil})
	if strings.Contains(string(without), "validUntil") || !strings.Contains(string(with), `"validUntil":"2026-11-05T13:00:00.000Z"`) {
		t.Fatal("optional attempt JSON changed")
	}
}

type deadlineLedger struct {
	Ledger
	afterMark func()
}

func (l deadlineLedger) WithInstance(ctx context.Context, id string, fn func(Ledger) error) error {
	return l.Ledger.WithInstance(ctx, id, func(scoped Ledger) error { return fn(deadlineLedger{scoped, l.afterMark}) })
}
func (l deadlineLedger) MarkSending(ctx context.Context, b Binding) error {
	err := l.Ledger.MarkSending(ctx, b)
	if err == nil && l.afterMark != nil {
		l.afterMark()
	}
	return err
}

func TestDeadlineServiceBoundariesAndDurableReplay(t *testing.T) {
	for _, point := range []string{"before_prepare", "during_prepare", "after_mark", "transport_guard", "after_call_ack", "after_call_error"} {
		t.Run(point, func(t *testing.T) {
			now := serviceTestTime
			deadline := now.Add(time.Second)
			req := serviceTestRequest()
			req.ValidUntil = deadline.Format(deadlineLayout)
			ledger := serviceNewLedger()
			tr := &serviceTransportFake{}
			var store Ledger = ledger
			switch point {
			case "before_prepare":
				now = deadline
			case "during_prepare":
				tr.prepare = func(context.Context, string, string, Message) error { now = deadline; return nil }
			case "after_mark":
				store = deadlineLedger{ledger, func() { now = deadline }}
			case "transport_guard":
				tr.send = func(context.Context, string) (Receipt, error) { now = deadline; return Receipt{}, ErrExpired }
			case "after_call_ack":
				tr.send = func(context.Context, string) (Receipt, error) {
					now = deadline
					return Receipt{MessageID: req.MessageID, AcceptedAt: now}, nil
				}
			case "after_call_error":
				tr.send = func(context.Context, string) (Receipt, error) {
					now = deadline
					return Receipt{}, errors.Join(ErrAmbiguousSend, ErrExpired)
				}
			}
			svc := NewService(store, tr)
			svc.now = func() time.Time { return now }
			a, err := svc.Send(context.Background(), serviceTestInstance, req)
			want, code := Rejected, "expired"
			if point == "after_call_ack" {
				want, code = Accepted, ""
			}
			if point == "after_call_error" {
				want, code = Uncertain, "send_uncertain"
			}
			if err != nil || a.State != want || a.Code != code || a.ValidUntil != req.ValidUntil {
				t.Fatalf("state=%s code=%s err=%v", a.State, a.Code, err)
			}
			if point == "before_prepare" && tr.prepares.Load() != 0 {
				t.Fatal("expired request prepared")
			}
			if (point == "before_prepare" || point == "during_prepare" || point == "after_mark") && tr.sends.Load() != 0 {
				t.Fatal("expired request sent")
			}
			now = deadline.Add(time.Hour)
			prepares, sends := tr.prepares.Load(), tr.sends.Load()
			replay, err := svc.Send(context.Background(), serviceTestInstance, req)
			if err != nil || replay.State != want || replay.Code != code || tr.prepares.Load() != prepares || tr.sends.Load() != sends {
				t.Fatal("expiry changed replay or reexecuted")
			}
			req.ValidUntil = deadline.Add(time.Second).Format(deadlineLayout)
			if _, err = svc.Send(context.Background(), serviceTestInstance, req); !errors.Is(err, ErrConflict) {
				t.Fatal("changed deadline did not conflict")
			}
			if want == Uncertain && ledger.d.active[serviceTestInstance] == "" {
				t.Fatal("expiry released uncertain barrier")
			}
		})
	}
}

func TestDeadlineTransportFinalGuardAndPostCallUncertainty(t *testing.T) {
	for _, mode := range []string{"before_prepare", "after_prepare", "after_call_ack", "after_call_error"} {
		t.Run(mode, func(t *testing.T) {
			now := testQueueTime
			deadline := now.Add(time.Second)
			f := newQueueFakeClient()
			tr, releases := queueFakeTransport(t, f)
			tr.now = func() time.Time { return now }
			msg := Message{Kind: "text", To: testQueueTarget, Text: "owned", ValidUntil: deadline.Format(deadlineLayout)}
			if mode == "before_prepare" {
				now = deadline
			}
			if strings.HasPrefix(mode, "after_call") {
				f.send = func(context.Context, types.JID, *waE2E.Message, ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
					now = deadline
					if mode == "after_call_error" {
						return whatsmeow.SendResponse{}, ErrExpired
					}
					return whatsmeow.SendResponse{ID: testQueueID, Timestamp: now}, nil
				}
			}
			p, err := tr.Prepare(context.Background(), testQueueInstance, testQueueID, msg)
			if mode == "before_prepare" {
				if !errors.Is(err, ErrExpired) || p != nil || f.sends.Load() != 0 {
					t.Fatal("prepare expiry failed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode == "after_prepare" {
				now = deadline
			}
			_, err = p.Send(context.Background())
			p.Close()
			switch mode {
			case "after_prepare":
				if !errors.Is(err, ErrExpired) || f.sends.Load() != 0 {
					t.Fatal("final guard missed deadline")
				}
			case "after_call_ack":
				if err != nil || f.sends.Load() != 1 {
					t.Fatal("expiry discarded ack")
				}
			case "after_call_error":
				if !errors.Is(err, ErrAmbiguousSend) || f.sends.Load() != 1 {
					t.Fatal("post-call expiry proved false rejection")
				}
			}
			if releases.Load() != 1 {
				t.Fatal("lease leaked")
			}
		})
	}
}

func TestDeadlineCrashSurvivorsNeverExpire(t *testing.T) {
	for _, state := range []string{Preparing, Sending, Uncertain, Accepted} {
		req := serviceTestRequest()
		req.ValidUntil = serviceTestTime.Format(deadlineLayout)
		ledger := serviceNewLedger()
		ledger.seed(req, serviceTestInstance, state)
		tr := &serviceTransportFake{}
		svc := NewService(ledger, tr)
		svc.now = func() time.Time { return serviceTestTime.Add(time.Hour) }
		a, err := svc.Send(context.Background(), serviceTestInstance, req)
		if err != nil || a.State != state || tr.prepares.Load() != 0 || tr.sends.Load() != 0 {
			t.Fatalf("survivor %s expired or sent", state)
		}
	}
}

func TestDeadlineRealLedgerIdentityAndExpiry(t *testing.T) {
	p, _, _ := postgresReal(t)
	req := serviceTestRequest()
	req.AttemptID = "10000000-0000-4000-8000-000000000099"
	req.LogicalMessageID = "20000000-0000-4000-8000-000000000099"
	instance := uuid.NewString()
	req.ValidUntil = serviceTestTime.Add(time.Second).Format(deadlineLayout)
	now := serviceTestTime
	tr := &serviceTransportFake{}
	svc := NewService(p, tr)
	svc.now = func() time.Time { return now }
	a, err := svc.Send(context.Background(), instance, req)
	if err != nil || a.State != Accepted || a.ValidUntil != req.ValidUntil {
		t.Fatalf("persist deadline: %+v %v", a, err)
	}
	now = now.Add(time.Hour)
	// A new service and ledger must replay the original acceptance after expiry.
	reopened, _ := NewPostgresLedger(p.db)
	svc = NewService(reopened, tr)
	svc.now = func() time.Time { return now }
	a, err = svc.Send(context.Background(), instance, req)
	if err != nil || a.State != Accepted || tr.sends.Load() != 1 {
		t.Fatal("expired accepted replay lost")
	}
	req.ValidUntil = now.Format(deadlineLayout)
	if _, err = svc.Send(context.Background(), instance, req); !errors.Is(err, ErrConflict) {
		t.Fatal("deadline mutation not conflicting")
	}
	req.AttemptID = "10000000-0000-4000-8000-000000000098"
	if _, err = svc.Send(context.Background(), instance, req); !errors.Is(err, ErrConflict) {
		t.Fatal("logical deadline mutation not conflicting")
	}
	req.LogicalMessageID = "20000000-0000-4000-8000-000000000098"
	req.MessageID = "EXPIRED98"
	store := deadlineLedger{reopened, func() { now = now.Add(time.Second) }}
	svc = NewService(store, tr)
	svc.now = func() time.Time { return now }
	req.ValidUntil = now.Add(time.Second).Format(deadlineLayout)
	a, err = svc.Send(context.Background(), instance, req)
	if err != nil || a.State != Rejected || a.Code != "expired" || tr.sends.Load() != 1 {
		t.Fatalf("post-mark expiry not durable: %+v %v", a, err)
	}
	if err = p.Idle(context.Background(), instance); err != nil {
		t.Fatal("proven pre-call expiry retained barrier")
	}
}

func TestDeadlineHTTPExplicitValueCannotEraseDeadline(t *testing.T) {
	for _, value := range []string{`null`, `""`, `123`, `"2026-11-05T13:00:00Z"`, `"2026-11-05T13:00:00.000+00:00"`} {
		var req SendRequest
		if err := json.Unmarshal([]byte(`{"validUntil":`+value+`}`), &req); err == nil {
			t.Fatalf("invalid explicit deadline accepted: %s", value)
		}
	}
	var req SendRequest
	if err := json.Unmarshal([]byte(`{"validUntil":"2026-11-05T13:00:00.000Z","unknown":1}`), &req); err == nil {
		t.Fatal("custom decoder weakened unknown-field rejection")
	}
}

func TestDeadlineImageUploadBoundaries(t *testing.T) {
	for _, mode := range []string{"before_upload", "during_upload"} {
		t.Run(mode, func(t *testing.T) {
			now := testQueueTime
			deadline := now.Add(time.Second)
			f := newQueueFakeClient()
			tr, releases := queueFakeTransport(t, f)
			tr.now = func() time.Time { return now }
			originalLease := tr.lease
			if mode == "before_upload" {
				tr.lease = func(ctx context.Context, id string) (WAClient, context.Context, func(), error) {
					c, ctx, release, err := originalLease(ctx, id)
					now = deadline
					return c, ctx, release, err
				}
			}
			f.upload = func(_ context.Context, data []byte, _ whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
				now = deadline
				return queueTestUpload(data), nil
			}
			msg := queueTestImage()
			msg.ValidUntil = deadline.Format(deadlineLayout)
			p, err := tr.Prepare(context.Background(), testQueueInstance, testQueueID, msg)
			if p != nil || !errors.Is(err, ErrExpired) || f.sends.Load() != 0 || releases.Load() != 1 {
				t.Fatal("expired image retained/send authorization")
			}
			if mode == "before_upload" && f.uploads.Load() != 0 {
				t.Fatal("deadline passed before upload")
			}
			if mode == "during_upload" && f.uploads.Load() != 1 {
				t.Fatal("missing controlled upload")
			}
		})
	}
}

func TestExpiredNewAttemptPreservesPriorLogicalEvidence(t *testing.T) {
	for _, state := range []string{Accepted, Uncertain} {
		req := serviceTestRequest()
		req.ValidUntil = serviceTestTime.Format(deadlineLayout)
		ledger := serviceNewLedger()
		ledger.seed(req, serviceTestInstance, state)
		req.AttemptID = "10000000-0000-4000-8000-000000000002"
		tr := &serviceTransportFake{}
		svc := NewService(ledger, tr)
		svc.now = func() time.Time { return serviceTestTime.Add(time.Hour) }
		a, err := svc.Send(context.Background(), serviceTestInstance, req)
		code := "logical_accepted"
		if state == Uncertain {
			code = "logical_pending"
		}
		if err != nil || a.Code != code || tr.prepares.Load() != 0 || tr.sends.Load() != 0 {
			t.Fatal("expiry hid stronger logical evidence")
		}
	}
}
