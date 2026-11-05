package queuev1

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	serviceTestInstance = "00000000-0000-4000-8000-000000000001"
	serviceTestOther    = "00000000-0000-4000-8000-000000000002"
	serviceTestAttempt  = "10000000-0000-4000-8000-000000000001"
	serviceTestLogical  = "20000000-0000-4000-8000-000000000001"
)

var serviceTestTime = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

func serviceTestRequest() SendRequest {
	return SendRequest{AttemptID: serviceTestAttempt, LogicalMessageID: serviceTestLogical, MessageID: "3EB000000000000000001", Kind: "text", To: "5511999999999@s.whatsapp.net", Text: "Olá, mundo 🌍"}
}

// This fake models committed state separately from the scoped lock session.
// Root mutations other than deny-admission panic to catch accidental pool use
// while a physical session is held. Failure flags also model lost COMMIT replies.
type serviceLedgerData struct {
	mu            sync.Mutex
	attempts      map[string]Attempt
	bindings      map[string]Binding
	operations    map[string]string
	active        map[string]string
	held          map[string]bool
	events        []string
	lookupErr     error
	lockErr       error
	unlockErr     error
	admitErr      error
	markErr       error
	completeErr   error
	idleErr       error
	markCommitted bool
	endCommitted  bool
	afterAdmit    func()
	beforeScope   func()
}

type serviceLedgerFake struct {
	d     *serviceLedgerData
	scope string
}

func serviceNewLedger() *serviceLedgerFake {
	return &serviceLedgerFake{d: &serviceLedgerData{
		attempts: map[string]Attempt{}, bindings: map[string]Binding{}, operations: map[string]string{}, active: map[string]string{}, held: map[string]bool{},
	}}
}

func serviceKey(instance, id string) string { return instance + "/" + id }

func (f *serviceLedgerFake) event(name string) {
	if f.scope == "" {
		name += "/root"
	} else {
		if !f.d.held[f.scope] {
			panic("scoped ledger used after session release")
		}
		name += "/scoped"
	}
	f.d.events = append(f.d.events, name)
}

func (f *serviceLedgerFake) scoped(instance string) {
	if f.scope != instance {
		panic("mutation must use correct scoped ledger")
	}
}

func (f *serviceLedgerFake) Lookup(ctx context.Context, instance, id string) (Attempt, error) {
	f.d.mu.Lock()
	defer f.d.mu.Unlock()
	f.event("lookup")
	if err := ctx.Err(); err != nil {
		return Attempt{}, err
	}
	if f.d.lookupErr != nil {
		return Attempt{}, f.d.lookupErr
	}
	a, ok := f.d.attempts[serviceKey(instance, id)]
	if !ok {
		return Attempt{}, ErrNotFound
	}
	return a, nil
}

func (f *serviceLedgerFake) Admit(ctx context.Context, b Binding, deny string) (Attempt, bool, error) {
	if deny == "" {
		f.scoped(b.InstanceID)
	} else if f.scope != "" || deny != "instance_busy" {
		panic("unexpected deny admission")
	}
	f.d.mu.Lock()
	defer func() {
		hook := f.d.afterAdmit
		f.d.mu.Unlock()
		if hook != nil {
			hook()
		}
	}()
	f.event("admit:" + deny)
	if err := ctx.Err(); err != nil {
		return Attempt{}, false, err
	}
	if f.d.admitErr != nil {
		return Attempt{}, false, f.d.admitErr
	}
	if !serviceUUID(b.OwnerID) {
		panic("owner must be a server UUID")
	}
	key, op := serviceKey(b.InstanceID, b.AttemptID), serviceKey(b.InstanceID, b.LogicalMessageID)
	if prior, ok := f.d.attempts[key]; ok {
		if !serviceMatches(prior, b) {
			return Attempt{}, false, ErrConflict
		}
		return prior, false, nil
	}
	previousKey := f.d.operations[op]
	if previousKey != "" {
		previous := f.d.attempts[previousKey]
		if previous.MessageID != b.MessageID || previous.ContentHash != b.ContentHash {
			return Attempt{}, false, ErrConflict
		}
		switch previous.State {
		case Accepted:
			deny = "logical_accepted"
		case Preparing, Sending, Uncertain:
			deny = "logical_pending"
		}
	}
	for k, a := range f.d.attempts {
		if k != previousKey && a.InstanceID == b.InstanceID && a.MessageID == b.MessageID && a.LogicalMessageID != b.LogicalMessageID {
			return Attempt{}, false, ErrConflict
		}
	}
	if deny == "" && f.d.active[b.InstanceID] != "" {
		deny = "instance_busy"
	}
	a := Attempt{InstanceID: b.InstanceID, AttemptID: b.AttemptID, LogicalMessageID: b.LogicalMessageID, MessageID: b.MessageID, ContentHash: b.ContentHash, ValidUntil: b.ValidUntil, State: Preparing, CreatedAt: serviceTestTime}
	if deny != "" {
		a.State, a.Code, a.CompletedAt = Rejected, deny, &serviceTestTime
	} else {
		f.d.active[b.InstanceID] = key
	}
	f.d.attempts[key], f.d.bindings[key] = a, b
	if previousKey == "" || f.d.attempts[previousKey].State == Rejected {
		f.d.operations[op] = key
	}
	return a, deny == "", nil
}

func (f *serviceLedgerFake) MarkSending(ctx context.Context, b Binding) error {
	f.scoped(b.InstanceID)
	f.d.mu.Lock()
	defer f.d.mu.Unlock()
	f.event("mark")
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.d.markErr != nil && !f.d.markCommitted {
		return f.d.markErr
	}
	key := serviceKey(b.InstanceID, b.AttemptID)
	a := f.d.attempts[key]
	if a.State != Preparing || f.d.bindings[key].OwnerID != b.OwnerID || f.d.active[b.InstanceID] != key {
		return ErrConflict
	}
	a.State, a.SendingAt = Sending, &serviceTestTime
	f.d.attempts[key] = a
	return f.d.markErr
}

func (f *serviceLedgerFake) Complete(ctx context.Context, b Binding, state string, receipt *Receipt, code string) (Attempt, error) {
	f.scoped(b.InstanceID)
	f.d.mu.Lock()
	defer f.d.mu.Unlock()
	f.event("complete:" + state)
	if err := ctx.Err(); err != nil {
		return Attempt{}, err
	}
	if f.d.completeErr != nil && !f.d.endCommitted {
		return Attempt{}, f.d.completeErr
	}
	key := serviceKey(b.InstanceID, b.AttemptID)
	a := f.d.attempts[key]
	if !serviceMatches(a, b) || f.d.bindings[key].OwnerID != b.OwnerID || f.d.active[b.InstanceID] != key {
		return Attempt{}, ErrConflict
	}
	if state == Rejected && a.State != Preparing && !(a.State == Sending && code == "expired" && b.ValidUntil != "") || state != Rejected && a.State != Sending {
		return Attempt{}, ErrConflict
	}
	a.State, a.Code, a.Receipt = state, code, receipt
	if state != Uncertain {
		a.CompletedAt = &serviceTestTime
		delete(f.d.active, b.InstanceID)
	}
	f.d.attempts[key] = a
	if f.d.completeErr != nil {
		return Attempt{}, f.d.completeErr
	}
	return a, nil
}

func (f *serviceLedgerFake) Idle(ctx context.Context, instance string) error {
	f.scoped(instance)
	f.d.mu.Lock()
	defer f.d.mu.Unlock()
	f.event("idle")
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.d.idleErr != nil {
		return f.d.idleErr
	}
	if f.d.active[instance] != "" {
		return ErrBusy
	}
	return nil
}

func (f *serviceLedgerFake) WithInstance(ctx context.Context, instance string, fn func(Ledger) error) error {
	if f.scope != "" {
		panic("nested WithInstance")
	}
	f.d.mu.Lock()
	f.event("with")
	if err := ctx.Err(); err != nil {
		f.d.mu.Unlock()
		return err
	}
	if f.d.lockErr != nil {
		err := f.d.lockErr
		f.d.mu.Unlock()
		return err
	}
	if f.d.held[instance] {
		f.d.mu.Unlock()
		return ErrBusy
	}
	f.d.held[instance] = true
	hook := f.d.beforeScope
	f.d.mu.Unlock()
	defer func() {
		f.d.mu.Lock()
		delete(f.d.held, instance)
		f.d.mu.Unlock()
	}()
	if hook != nil {
		hook()
	}
	err := fn(&serviceLedgerFake{d: f.d, scope: instance})
	if f.d.unlockErr != nil {
		return f.d.unlockErr
	}
	return err
}

func (f *serviceLedgerFake) events() []string {
	f.d.mu.Lock()
	defer f.d.mu.Unlock()
	return append([]string(nil), f.d.events...)
}

func (f *serviceLedgerFake) seed(req SendRequest, instance, state string) Attempt {
	_, hash, err := serviceMessage(req)
	if err != nil {
		panic(err)
	}
	key := serviceKey(instance, req.AttemptID)
	a := Attempt{InstanceID: instance, AttemptID: req.AttemptID, LogicalMessageID: req.LogicalMessageID, MessageID: req.MessageID, ContentHash: hash, ValidUntil: req.ValidUntil, State: state, CreatedAt: serviceTestTime}
	if state == Accepted {
		a.Receipt = &Receipt{MessageID: req.MessageID, AcceptedAt: serviceTestTime}
	}
	f.d.attempts[key] = a
	f.d.operations[serviceKey(instance, req.LogicalMessageID)] = key
	f.d.bindings[key] = Binding{InstanceID: instance, AttemptID: req.AttemptID, LogicalMessageID: req.LogicalMessageID, MessageID: req.MessageID, ContentHash: hash, ValidUntil: req.ValidUntil, OwnerID: serviceTestOther}
	if state == Preparing || state == Sending || state == Uncertain {
		f.d.active[instance] = key
	}
	return a
}

type serviceTransportFake struct {
	prepares atomic.Int32
	sends    atomic.Int32
	closes   atomic.Int32
	presence atomic.Int32
	prepare  func(context.Context, string, string, Message) error
	send     func(context.Context, string) (Receipt, error)
	typing   func(context.Context, string, string, int) error
	nilPrep  bool
}

type servicePreparedFake struct {
	t  *serviceTransportFake
	id string
}

func (f *serviceTransportFake) Prepare(ctx context.Context, instance, id string, msg Message) (Prepared, error) {
	f.prepares.Add(1)
	p := &servicePreparedFake{t: f, id: id}
	if f.prepare != nil {
		if err := f.prepare(ctx, instance, id, msg); err != nil {
			return p, err // Also test cleanup of partial preparation.
		}
	}
	if f.nilPrep {
		return nil, nil
	}
	return p, nil
}

func (p *servicePreparedFake) Send(ctx context.Context) (Receipt, error) {
	p.t.sends.Add(1)
	if p.t.send != nil {
		return p.t.send(ctx, p.id)
	}
	return Receipt{MessageID: p.id, AcceptedAt: serviceTestTime}, nil
}

func (p *servicePreparedFake) Close() { p.t.closes.Add(1) }

func (f *serviceTransportFake) Presence(ctx context.Context, instance, to string, ms int) error {
	f.presence.Add(1)
	if f.typing != nil {
		return f.typing(ctx, instance, to, ms)
	}
	return ctx.Err()
}

func serviceAssertCounts(t *testing.T, tr *serviceTransportFake, prepare, send, close, presence int32) {
	t.Helper()
	if tr.prepares.Load() != prepare || tr.sends.Load() != send || tr.closes.Load() != close || tr.presence.Load() != presence {
		t.Fatalf("transport counts: prepare=%d send=%d close=%d presence=%d", tr.prepares.Load(), tr.sends.Load(), tr.closes.Load(), tr.presence.Load())
	}
}

func TestServiceStableAPIAndJSON(t *testing.T) {
	var _ interface {
		Send(context.Context, string, SendRequest) (Attempt, error)
		Presence(context.Context, string, PresenceRequest) error
		Lookup(context.Context, string, string) (Attempt, error)
	} = NewService(nil, nil)
	for _, value := range []any{SendRequest{}, PresenceRequest{}} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		want := []string{"attemptId", "logicalMessageId", "messageId", "to", "text", "caption", "mime", "imageSha256", "imageBase64", "kind"}
		if _, ok := value.(PresenceRequest); ok {
			want = []string{"to", "durationMs"}
		}
		if len(fields) != len(want) {
			t.Fatalf("JSON fields: %v", fields)
		}
		for _, key := range want {
			if _, ok := fields[key]; !ok {
				t.Errorf("missing JSON field %s", key)
			}
		}
	}
}

func TestServiceAcceptedScopedOrderingAndReplay(t *testing.T) {
	ledger, tr := serviceNewLedger(), &serviceTransportFake{}
	svc, req := NewService(ledger, tr), serviceTestRequest()
	if len(ledger.events()) != 0 {
		t.Fatal("constructor operated")
	}
	tr.prepare = func(ctx context.Context, instance, id string, msg Message) error {
		if ledger.d.attempts[serviceKey(instance, req.AttemptID)].State != Preparing || id != req.MessageID || msg.Text != req.Text {
			t.Fatal("Prepare did not follow admission with original payload and ID")
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 71*time.Second || time.Until(deadline) < 65*time.Second {
			t.Fatal("work context is not bounded")
		}
		return nil
	}
	tr.send = func(ctx context.Context, id string) (Receipt, error) {
		if ledger.d.attempts[serviceKey(serviceTestInstance, req.AttemptID)].State != Sending || !ledger.d.held[serviceTestInstance] {
			t.Fatal("Send without committed sending state and held session")
		}
		return Receipt{MessageID: id, AcceptedAt: serviceTestTime.In(time.FixedZone("other", 3600))}, nil
	}
	a, err := svc.Send(context.Background(), serviceTestInstance, req)
	if err != nil || a.State != Accepted || a.Receipt == nil || a.Receipt.AcceptedAt.Location() != time.UTC {
		t.Fatalf("accepted: %+v %v", a, err)
	}
	want := []string{"lookup/root", "with/root", "admit:/scoped", "mark/scoped", "complete:accepted/scoped"}
	if !reflect.DeepEqual(ledger.events(), want) {
		t.Fatalf("events: %v", ledger.events())
	}
	binding := ledger.d.bindings[serviceKey(serviceTestInstance, req.AttemptID)]
	if !serviceUUID(binding.OwnerID) || binding.OwnerID == req.AttemptID || binding.OwnerID == req.LogicalMessageID {
		t.Fatal("invalid server ownership")
	}
	encoded, _ := json.Marshal(binding)
	if bytes.Contains(encoded, []byte(req.Text)) || len(binding.ContentHash) != 64 {
		t.Fatal("ledger retained content")
	}
	replay, err := svc.Send(context.Background(), serviceTestInstance, req)
	if err != nil || !reflect.DeepEqual(a, replay) || len(ledger.events()) != len(want)+1 {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	serviceAssertCounts(t, tr, 1, 1, 1, 0)
}

func TestServiceReplayRejectsDivergentBindings(t *testing.T) {
	for _, change := range []struct {
		name string
		fn   func(*SendRequest)
	}{
		{"logical", func(r *SendRequest) { r.LogicalMessageID = serviceTestOther }},
		{"message", func(r *SendRequest) { r.MessageID += "A" }},
		{"body", func(r *SendRequest) { r.Text += "!" }},
		{"target", func(r *SendRequest) { r.To = "123@lid" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			ledger, tr := serviceNewLedger(), &serviceTransportFake{}
			req := serviceTestRequest()
			ledger.seed(req, serviceTestInstance, Accepted)
			change.fn(&req)
			a, err := NewService(ledger, tr).Send(context.Background(), serviceTestInstance, req)
			if !errors.Is(err, ErrConflict) || a.State != "" || !reflect.DeepEqual(ledger.events(), []string{"lookup/root"}) {
				t.Fatalf("conflict: %+v %v %v", a, err, ledger.events())
			}
			serviceAssertCounts(t, tr, 0, 0, 0, 0)
		})
	}
}

func TestServiceRestartNeverResendsOrReleasesPending(t *testing.T) {
	for _, state := range []string{Preparing, Sending, Uncertain, Accepted, Rejected} {
		t.Run(state, func(t *testing.T) {
			ledger, tr := serviceNewLedger(), &serviceTransportFake{}
			req := serviceTestRequest()
			prior := ledger.seed(req, serviceTestInstance, state)
			svc := NewService(ledger, tr)
			got, err := svc.Lookup(context.Background(), serviceTestInstance, req.AttemptID)
			if err != nil || !reflect.DeepEqual(got, prior) {
				t.Fatalf("lookup: %+v %v", got, err)
			}
			got, err = svc.Send(context.Background(), serviceTestInstance, req)
			if err != nil || !reflect.DeepEqual(got, prior) {
				t.Fatalf("restart replay: %+v %v", got, err)
			}
			if state == Preparing || state == Sending || state == Uncertain {
				if err := svc.Presence(context.Background(), serviceTestInstance, PresenceRequest{To: req.To, DurationMS: 8000}); !errors.Is(err, ErrBusy) {
					t.Fatalf("pending presence: %v", err)
				}
				if ledger.d.active[serviceTestInstance] == "" {
					t.Fatal("crash barrier released")
				}
			}
			serviceAssertCounts(t, tr, 0, 0, 0, 0)
		})
	}
}

func TestServiceLogicalRejectionDoesNotDenyEarlierEffect(t *testing.T) {
	for _, state := range []string{Preparing, Sending, Uncertain, Accepted} {
		t.Run(state, func(t *testing.T) {
			ledger, tr := serviceNewLedger(), &serviceTransportFake{}
			req := serviceTestRequest()
			original := ledger.seed(req, serviceTestInstance, state)
			req.AttemptID = serviceTestOther
			a, err := NewService(ledger, tr).Send(context.Background(), serviceTestInstance, req)
			code := "logical_pending"
			if state == Accepted {
				code = "logical_accepted"
			}
			if err != nil || a.State != Rejected || a.Code != code {
				t.Fatalf("logical rejection: %+v %v", a, err)
			}
			if !reflect.DeepEqual(original, ledger.d.attempts[serviceKey(serviceTestInstance, serviceTestAttempt)]) {
				t.Fatal("new rejection changed prior outcome")
			}
			serviceAssertCounts(t, tr, 0, 0, 0, 0)
		})
	}
}

func TestServiceBusyBeforeCallbackPersistsRootDenial(t *testing.T) {
	ledger, tr := serviceNewLedger(), &serviceTransportFake{}
	ledger.d.lockErr = ErrBusy
	svc, req := NewService(ledger, tr), serviceTestRequest()
	a, err := svc.Send(context.Background(), serviceTestInstance, req)
	if err != nil || a.State != Rejected || a.Code != "instance_busy" || ledger.d.active[serviceTestInstance] != "" {
		t.Fatalf("busy: %+v %v", a, err)
	}
	if !reflect.DeepEqual(ledger.events(), []string{"lookup/root", "with/root", "admit:instance_busy/root"}) {
		t.Fatal(ledger.events())
	}
	b, err := svc.Send(context.Background(), serviceTestInstance, req)
	if err != nil || !reflect.DeepEqual(a, b) {
		t.Fatalf("busy replay: %+v %v", b, err)
	}
	serviceAssertCounts(t, tr, 0, 0, 0, 0)
}

func TestServicePreparationFailureAllowsOnlyNewAttempt(t *testing.T) {
	for _, nilPrep := range []bool{false, true} {
		t.Run(fmt.Sprint(nilPrep), func(t *testing.T) {
			ledger, tr := serviceNewLedger(), &serviceTransportFake{nilPrep: nilPrep}
			if !nilPrep {
				tr.prepare = func(context.Context, string, string, Message) error { return errors.New("private upload error") }
			}
			svc, req := NewService(ledger, tr), serviceTestRequest()
			a, err := svc.Send(context.Background(), serviceTestInstance, req)
			if err != nil || a.State != Rejected || a.Code != "preparation_failed" || ledger.d.active[serviceTestInstance] != "" {
				t.Fatalf("preparation: %+v %v", a, err)
			}
			if _, err := svc.Send(context.Background(), serviceTestInstance, req); err != nil || tr.prepares.Load() != 1 {
				t.Fatal("failed attempt retried")
			}
			tr.prepare, tr.nilPrep = nil, false
			req.AttemptID = serviceTestOther
			a, err = svc.Send(context.Background(), serviceTestInstance, req)
			if err != nil || a.State != Accepted {
				t.Fatalf("new attempt: %+v %v", a, err)
			}
			closes := int32(2)
			if nilPrep {
				closes = 1
			}
			serviceAssertCounts(t, tr, 2, 1, closes, 0)
		})
	}
}

func TestServiceSendErrorsAndInvalidReceiptsAreUncertain(t *testing.T) {
	for _, tc := range []struct {
		name    string
		receipt Receipt
		err     error
		code    string
	}{
		{"unknown", Receipt{}, errors.New("unknown network result"), "send_uncertain"},
		{"timeout", Receipt{}, context.DeadlineExceeded, "send_uncertain"},
		{"error_with_receipt", Receipt{MessageID: serviceTestRequest().MessageID, AcceptedAt: serviceTestTime}, errors.New("lost result"), "send_uncertain"},
		{"wrong_id", Receipt{MessageID: "OTHER", AcceptedAt: serviceTestTime}, nil, "receipt_mismatch"},
		{"missing_date", Receipt{MessageID: serviceTestRequest().MessageID}, nil, "receipt_mismatch"},
		{"empty", Receipt{}, nil, "receipt_mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ledger, tr := serviceNewLedger(), &serviceTransportFake{}
			tr.send = func(context.Context, string) (Receipt, error) { return tc.receipt, tc.err }
			svc, req := NewService(ledger, tr), serviceTestRequest()
			a, err := svc.Send(context.Background(), serviceTestInstance, req)
			if err != nil || a.State != Uncertain || a.Code != tc.code || a.Receipt != nil || ledger.d.active[serviceTestInstance] == "" {
				t.Fatalf("uncertain: %+v %v", a, err)
			}
			if _, err := NewService(ledger, tr).Send(context.Background(), serviceTestInstance, req); err != nil {
				t.Fatal(err)
			}
			serviceAssertCounts(t, tr, 1, 1, 1, 0)
		})
	}
}

func TestServiceLedgerFailuresNeverClaimSuccess(t *testing.T) {
	for _, stage := range []string{"lookup", "lock", "admit", "mark", "mark_commit", "complete", "complete_commit", "unlock", "prepare_complete", "send_complete", "busy_deny", "callback_busy"} {
		t.Run(stage, func(t *testing.T) {
			ledger, tr := serviceNewLedger(), &serviceTransportFake{}
			sendCount := int32(0)
			switch stage {
			case "lookup":
				ledger.d.lookupErr = ErrUnavailable
			case "lock":
				ledger.d.lockErr = ErrUnavailable
			case "admit":
				ledger.d.admitErr = ErrUnavailable
			case "mark", "mark_commit":
				ledger.d.markErr, ledger.d.markCommitted = ErrUnavailable, stage == "mark_commit"
			case "complete", "complete_commit":
				ledger.d.completeErr, ledger.d.endCommitted, sendCount = ErrUnavailable, stage == "complete_commit", 1
			case "unlock":
				ledger.d.unlockErr, sendCount = ErrUnavailable, 1
			case "prepare_complete":
				ledger.d.completeErr = ErrUnavailable
				tr.prepare = func(context.Context, string, string, Message) error { return errors.New("prepare failed") }
			case "send_complete":
				ledger.d.completeErr, sendCount = ErrUnavailable, 1
				tr.send = func(context.Context, string) (Receipt, error) { return Receipt{}, errors.New("unknown") }
			case "busy_deny":
				ledger.d.lockErr, ledger.d.admitErr = ErrBusy, ErrUnavailable
			case "callback_busy":
				ledger.d.admitErr = ErrBusy
			}
			a, err := NewService(ledger, tr).Send(context.Background(), serviceTestInstance, serviceTestRequest())
			wantErr := ErrUnavailable
			if stage == "callback_busy" {
				wantErr = ErrBusy
			}
			if !errors.Is(err, wantErr) || a.State != "" || tr.sends.Load() != sendCount {
				t.Fatalf("SQL failure: %+v %v sends=%d", a, err, tr.sends.Load())
			}
			if stage == "callback_busy" && len(ledger.events()) != 3 {
				t.Fatal("callback ErrBusy incorrectly invoked root denial")
			}
			if stage == "mark_commit" || stage == "mark" || stage == "complete" || stage == "prepare_complete" || stage == "send_complete" {
				if ledger.d.active[serviceTestInstance] == "" {
					t.Fatal("ambiguous ledger failure cleared barrier")
				}
				// Restart sees committed state and must not re-enter Prepare/Send.
				before := tr.sends.Load()
				if _, err := NewService(ledger, tr).Send(context.Background(), serviceTestInstance, serviceTestRequest()); err != nil || tr.sends.Load() != before {
					t.Fatalf("failed SQL replay: %v", err)
				}
			}
		})
	}
}

func TestServiceCancellationBeforeAdmissionDoesNotOperate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ledger, tr := serviceNewLedger(), &serviceTransportFake{}
	svc := NewService(ledger, tr)
	if _, err := svc.Send(ctx, serviceTestInstance, serviceTestRequest()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := svc.Lookup(ctx, serviceTestInstance, serviceTestAttempt); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := svc.Presence(ctx, serviceTestInstance, PresenceRequest{To: serviceTestRequest().To, DurationMS: 8000}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(ledger.events()) != 0 {
		t.Fatal(ledger.events())
	}
	ctx, cancel = context.WithCancel(context.Background())
	ledger.d.beforeScope = cancel
	if _, err := svc.Send(ctx, serviceTestInstance, serviceTestRequest()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ledger.events(), []string{"lookup/root", "with/root"}) {
		t.Fatalf("canceled before Admit: %v", ledger.events())
	}
	serviceAssertCounts(t, tr, 0, 0, 0, 0)
}

func TestServiceHTTPCancellationAfterAdmissionKeepsBookkeeping(t *testing.T) {
	for _, stage := range []string{"admit", "prepare", "send"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ledger, tr := serviceNewLedger(), &serviceTransportFake{}
			if stage == "admit" {
				ledger.d.afterAdmit = cancel
			}
			tr.prepare = func(op context.Context, _, _ string, _ Message) error {
				if stage == "prepare" {
					cancel()
				}
				if err := op.Err(); err != nil {
					t.Fatal("HTTP canceled preparation", err)
				}
				return nil
			}
			tr.send = func(op context.Context, id string) (Receipt, error) {
				cancel()
				if err := op.Err(); err != nil {
					t.Fatal("HTTP canceled owned Send", err)
				}
				return Receipt{MessageID: id, AcceptedAt: serviceTestTime}, nil
			}
			a, err := NewService(ledger, tr).Send(ctx, serviceTestInstance, serviceTestRequest())
			if err != nil || a.State != Accepted || ctx.Err() == nil {
				t.Fatalf("cancellation: %+v %v", a, err)
			}
			serviceAssertCounts(t, tr, 1, 1, 1, 0)
		})
	}
}

func serviceWait[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for test operation")
		var zero T
		return zero
	}
}

type serviceTestResult struct {
	a   Attempt
	err error
}

func TestServiceConcurrentReplayBusyAndIndependentInstances(t *testing.T) {
	ledger, tr := serviceNewLedger(), &serviceTransportFake{}
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	tr.send = func(ctx context.Context, id string) (Receipt, error) {
		if tr.sends.Load() == 1 {
			close(started)
			<-release // Intentionally ignore context to test no abandoned Send.
		}
		return Receipt{MessageID: id, AcceptedAt: serviceTestTime}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc, req := NewService(ledger, tr), serviceTestRequest()
	done := make(chan serviceTestResult, 1)
	go func() {
		a, err := svc.Send(ctx, serviceTestInstance, req)
		done <- serviceTestResult{a, err}
	}()
	serviceWait(t, started)
	cancel()
	select {
	case <-done:
		t.Fatal("Send returned while transport was executing")
	default:
	}
	if tr.closes.Load() != 0 {
		t.Fatal("lease released while Send was executing")
	}
	// Many concurrent same-attempt calls are strictly read-only replays.
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, err := svc.Send(context.Background(), serviceTestInstance, req)
			if err != nil || a.State != Sending {
				t.Errorf("concurrent replay: %+v %v", a, err)
			}
		}()
	}
	wg.Wait()
	other := req
	other.AttemptID, other.LogicalMessageID, other.MessageID = serviceTestOther, serviceTestOther, "OTHER123"
	a, err := svc.Send(context.Background(), serviceTestInstance, other)
	if err != nil || a.State != Rejected || a.Code != "instance_busy" {
		t.Fatalf("concurrent busy: %+v %v", a, err)
	}
	if err := svc.Presence(context.Background(), serviceTestInstance, PresenceRequest{To: req.To, DurationMS: 8000}); !errors.Is(err, ErrBusy) {
		t.Fatalf("presence overlaps send: %v", err)
	}
	// Same caller IDs are independent in a different instance.
	a, err = svc.Send(context.Background(), serviceTestOther, req)
	if err != nil || a.State != Accepted {
		t.Fatalf("independent instance: %+v %v", a, err)
	}
	releaseOnce.Do(func() { close(release) })
	result := serviceWait(t, done)
	if result.err != nil || result.a.State != Accepted {
		t.Fatalf("first send: %+v %v", result.a, result.err)
	}
	serviceAssertCounts(t, tr, 2, 2, 2, 0)
}

func TestServiceConcurrentAbsentAttemptUsesAdmitReplay(t *testing.T) {
	// Force both initial Lookup calls to observe absence, then hold preparation.
	ledger, tr := serviceNewLedger(), &serviceTransportFake{}
	ready, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	tr.prepare = func(context.Context, string, string, Message) error {
		close(ready)
		<-release
		return nil
	}
	svc, req := NewService(ledger, tr), serviceTestRequest()
	done := make(chan serviceTestResult, 1)
	go func() {
		a, err := svc.Send(context.Background(), serviceTestInstance, req)
		done <- serviceTestResult{a, err}
	}()
	serviceWait(t, ready)
	// A stale read can happen before the first admission's commit. Wrapping
	// Lookup makes the second service take the pre-callback busy denial path;
	// Admit must return the existing preparing attempt, never reject/execute it.
	stale := &serviceStaleLookup{Ledger: ledger}
	a, err := NewService(stale, tr).Send(context.Background(), serviceTestInstance, req)
	if err != nil || a.State != Preparing {
		t.Fatalf("admit replay after stale lookup: %+v %v", a, err)
	}
	once.Do(func() { close(release) })
	result := serviceWait(t, done)
	if result.err != nil || result.a.State != Accepted {
		t.Fatal(result)
	}
	serviceAssertCounts(t, tr, 1, 1, 1, 0)
}

type serviceStaleLookup struct{ Ledger }

func (*serviceStaleLookup) Lookup(context.Context, string, string) (Attempt, error) {
	return Attempt{}, ErrNotFound
}

func TestServicePresenceScopedIdleBoundsAndFailures(t *testing.T) {
	for _, ms := range []int{8000, 26000} {
		ledger, tr := serviceNewLedger(), &serviceTransportFake{}
		req := PresenceRequest{To: "123@lid", DurationMS: ms}
		tr.typing = func(ctx context.Context, instance, to string, duration int) error {
			if !ledger.d.held[instance] || to != req.To || duration != ms {
				t.Fatal("presence not serialized or changed arguments")
			}
			return ctx.Err()
		}
		if err := NewService(ledger, tr).Presence(context.Background(), serviceTestInstance, req); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(ledger.events(), []string{"with/root", "idle/scoped"}) {
			t.Fatal(ledger.events())
		}
		serviceAssertCounts(t, tr, 0, 0, 0, 1)
	}
	for _, stage := range []string{"lock", "idle", "transport", "unlock"} {
		t.Run(stage, func(t *testing.T) {
			ledger, tr := serviceNewLedger(), &serviceTransportFake{}
			switch stage {
			case "lock":
				ledger.d.lockErr = ErrUnavailable
			case "idle":
				ledger.d.idleErr = ErrUnavailable
			case "transport":
				tr.typing = func(context.Context, string, string, int) error { return ErrUnavailable }
			case "unlock":
				ledger.d.unlockErr = ErrUnavailable
			}
			if err := NewService(ledger, tr).Presence(context.Background(), serviceTestInstance, PresenceRequest{To: serviceTestRequest().To, DurationMS: 8000}); !errors.Is(err, ErrUnavailable) {
				t.Fatal(err)
			}
			if stage == "idle" || stage == "lock" {
				serviceAssertCounts(t, tr, 0, 0, 0, 0)
			}
		})
	}
}

func TestServiceUnknownLookupAndMissingDependencies(t *testing.T) {
	ledger, tr := serviceNewLedger(), &serviceTransportFake{}
	if _, err := NewService(ledger, tr).Lookup(context.Background(), serviceTestInstance, serviceTestAttempt); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ledger.events(), []string{"lookup/root"}) {
		t.Fatal(ledger.events())
	}
	serviceAssertCounts(t, tr, 0, 0, 0, 0)
	var nilService *Service
	for _, svc := range []*Service{nilService, NewService(nil, tr), NewService(ledger, nil)} {
		if _, err := svc.Send(context.Background(), serviceTestInstance, serviceTestRequest()); !errors.Is(err, ErrUnavailable) {
			t.Fatal(err)
		}
		if err := svc.Presence(context.Background(), serviceTestInstance, PresenceRequest{To: serviceTestRequest().To, DurationMS: 8000}); !errors.Is(err, ErrUnavailable) {
			t.Fatal(err)
		}
	}
}

func TestServiceRequestValidationBeforeLedger(t *testing.T) {
	changes := []struct {
		name string
		fn   func(*SendRequest)
	}{
		{"attempt", func(r *SendRequest) { r.AttemptID = "bad" }},
		{"uppercase_uuid", func(r *SendRequest) { r.AttemptID = "ABCDEF00-0000-4000-8000-000000000001" }},
		{"compact_uuid", func(r *SendRequest) { r.AttemptID = strings.ReplaceAll(r.AttemptID, "-", "") }},
		{"nil_uuid", func(r *SendRequest) { r.AttemptID = "00000000-0000-0000-0000-000000000000" }},
		{"logical", func(r *SendRequest) { r.LogicalMessageID = "bad" }},
		{"message_empty", func(r *SendRequest) { r.MessageID = "" }},
		{"message_long", func(r *SendRequest) { r.MessageID = strings.Repeat("A", 65) }},
		{"message_punctuation", func(r *SendRequest) { r.MessageID = "3EB-123" }},
		{"message_unicode", func(r *SendRequest) { r.MessageID = "á" }},
		{"text_empty", func(r *SendRequest) { r.Text = "" }},
		{"text_whitespace", func(r *SendRequest) { r.Text = " \n\t" }},
		{"text_utf8", func(r *SendRequest) { r.Text = string([]byte{0xff}) }},
		{"text_size", func(r *SendRequest) { r.Text = strings.Repeat("a", serviceMaxTextBytes+1) }},
		{"text_image_fields", func(r *SendRequest) { r.Caption = "unused" }},
		{"text_mime", func(r *SendRequest) { r.MIME = "image/png" }},
		{"text_hash", func(r *SendRequest) { r.ImageSHA256 = strings.Repeat("a", 64) }},
		{"text_media", func(r *SendRequest) { r.ImageBase64 = "YQ==" }},
		{"kind", func(r *SendRequest) { r.Kind = "video" }},
		{"target_empty", func(r *SendRequest) { r.To = "" }},
		{"target_phone", func(r *SendRequest) { r.To = "5511999999999" }},
		{"target_device", func(r *SendRequest) { r.To = "123:1@s.whatsapp.net" }},
		{"target_legacy", func(r *SendRequest) { r.To = "123@c.us" }},
		{"target_broadcast", func(r *SendRequest) { r.To = "status@broadcast" }},
		{"target_utf8", func(r *SendRequest) { r.To = string([]byte{0xff}) + "@lid" }},
		{"target_size", func(r *SendRequest) { r.To = strings.Repeat("1", 256) + "@lid" }},
	}
	for _, change := range changes {
		t.Run(change.name, func(t *testing.T) {
			ledger, tr := serviceNewLedger(), &serviceTransportFake{}
			req := serviceTestRequest()
			change.fn(&req)
			if _, err := NewService(ledger, tr).Send(context.Background(), serviceTestInstance, req); !errors.Is(err, ErrInvalid) {
				t.Fatal(err)
			}
			if len(ledger.events()) != 0 {
				t.Fatal("invalid request reached ledger")
			}
			serviceAssertCounts(t, tr, 0, 0, 0, 0)
		})
	}
	ledger, tr := serviceNewLedger(), &serviceTransportFake{}
	svc := NewService(ledger, tr)
	for _, id := range []string{"", "bad", "urn:uuid:" + serviceTestInstance} {
		if _, err := svc.Send(context.Background(), id, serviceTestRequest()); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
		if _, err := svc.Lookup(context.Background(), id, serviceTestAttempt); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
	if _, err := svc.Lookup(context.Background(), serviceTestInstance, "bad"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	for _, req := range []PresenceRequest{{To: "123@lid", DurationMS: 7999}, {To: "123@lid", DurationMS: 26001}, {To: "status@broadcast", DurationMS: 8000}, {To: "123:1@lid", DurationMS: 8000}} {
		if err := svc.Presence(context.Background(), serviceTestInstance, req); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
	if len(ledger.events()) != 0 {
		t.Fatal(ledger.events())
	}
}

func serviceTestImage(t *testing.T, format string) SendRequest {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	var data []byte
	var err error
	switch format {
	case "png":
		err = png.Encode(&buf, img)
		data = buf.Bytes()
	case "jpeg":
		err = jpeg.Encode(&buf, img, nil)
		data = buf.Bytes()
	case "webp":
		// Tiny static VP8 fixture, decoded by the existing pure-Go dependency.
		data, err = base64.StdEncoding.DecodeString("UklGRiIAAABXRUJQVlA4IBYAAAAwAQCdASoBAAEADsD+JaQAA3AAAAAA")
	}
	if err != nil {
		t.Fatal(err)
	}
	req := serviceTestRequest()
	req.Kind, req.Text, req.Caption, req.MIME = "image", "", "Legenda 🌍", "image/"+format
	return serviceImageBytes(req, data)
}

func serviceImageBytes(req SendRequest, data []byte) SendRequest {
	sum := sha256.Sum256(data)
	req.ImageSHA256 = hex.EncodeToString(sum[:])
	req.ImageBase64 = base64.StdEncoding.EncodeToString(data)
	return req
}

func TestServiceImageValidationAndNoBodyPersistence(t *testing.T) {
	for _, format := range []string{"jpeg", "png", "webp"} {
		t.Run(format, func(t *testing.T) {
			ledger, tr := serviceNewLedger(), &serviceTransportFake{}
			req := serviceTestImage(t, format)
			tr.prepare = func(ctx context.Context, instance, id string, msg Message) error {
				sum := sha256.Sum256(msg.ImageBytes)
				if msg.MIME != req.MIME || msg.ImageSHA256 != hex.EncodeToString(sum[:]) || msg.Caption != req.Caption || len(msg.ImageBytes) == 0 {
					t.Fatal("image not validated")
				}
				msg.ImageBytes[0] ^= 0xff // Owned per operation; never changes hash.
				return nil
			}
			a, err := NewService(ledger, tr).Send(context.Background(), serviceTestInstance, req)
			if err != nil || a.State != Accepted {
				t.Fatalf("image: %+v %v", a, err)
			}
			encoded, _ := json.Marshal(ledger.d.bindings)
			if bytes.Contains(encoded, []byte(req.Caption)) || bytes.Contains(encoded, []byte(req.ImageBase64)) {
				t.Fatal("body persisted")
			}
			if _, err := NewService(ledger, tr).Send(context.Background(), serviceTestInstance, req); err != nil {
				t.Fatal(err)
			}
			serviceAssertCounts(t, tr, 1, 1, 1, 0)
		})
	}
}

func TestServiceImageInvalidInputs(t *testing.T) {
	for _, change := range []struct {
		name string
		fn   func(*SendRequest)
	}{
		{"empty", func(r *SendRequest) { r.ImageBase64 = "" }},
		{"base64", func(r *SendRequest) { r.ImageBase64 = "%%%" }},
		{"base64_newline", func(r *SendRequest) { r.ImageBase64 += "\n" }},
		{"base64_noncanonical", func(r *SendRequest) { r.ImageBase64 = "YR==" }},
		{"encoded_size", func(r *SendRequest) {
			r.ImageBase64 = strings.Repeat("A", base64.StdEncoding.EncodedLen(MaxImageBytes)+4)
		}},
		{"decoded_size", func(r *SendRequest) { *r = serviceImageBytes(*r, make([]byte, MaxImageBytes+1)) }},
		{"hash", func(r *SendRequest) { r.ImageSHA256 = strings.Repeat("0", 64) }},
		{"hash_uppercase", func(r *SendRequest) { r.ImageSHA256 = strings.ToUpper(r.ImageSHA256) }},
		{"mime_mismatch", func(r *SendRequest) { r.MIME = "image/jpeg" }},
		{"mime_parameters", func(r *SendRequest) { r.MIME += "; charset=utf-8" }},
		{"unsupported", func(r *SendRequest) { *r = serviceImageBytes(*r, []byte("GIF89a")); r.MIME = "image/gif" }},
		{"bad_config", func(r *SendRequest) { *r = serviceImageBytes(*r, []byte("\x89PNG\r\n\x1a\nmalformed")) }},
		{"text", func(r *SendRequest) { r.Text = "unused" }},
		{"caption_utf8", func(r *SendRequest) { r.Caption = string([]byte{0xff}) }},
		{"caption_size", func(r *SendRequest) { r.Caption = strings.Repeat("a", serviceMaxCaptionBytes+1) }},
	} {
		t.Run(change.name, func(t *testing.T) {
			ledger, tr := serviceNewLedger(), &serviceTransportFake{}
			req := serviceTestImage(t, "png")
			change.fn(&req)
			if _, err := NewService(ledger, tr).Send(context.Background(), serviceTestInstance, req); !errors.Is(err, ErrInvalid) {
				t.Fatal(err)
			}
			if len(ledger.events()) != 0 {
				t.Fatal("invalid image reached ledger")
			}
			serviceAssertCounts(t, tr, 0, 0, 0, 0)
		})
	}
	// Encode skinny/solid images: DecodeConfig must reject dimensions before
	// allocating a full decode. The compressed bytes are well below 16 MiB.
	for _, size := range [][2]int{{8193, 1}, {1, 8193}, {6000, 6000}} {
		var buf bytes.Buffer

		if err := png.Encode(&buf, serviceSolidImage{width: size[0], height: size[1]}); err != nil {
			t.Fatal(err)
		}
		req := serviceImageBytes(serviceTestImage(t, "png"), buf.Bytes())
		if _, _, err := serviceMessage(req); !errors.Is(err, ErrInvalid) {
			t.Fatalf("dimensions %v accepted: %v", size, err)
		}
	}
}

type serviceSolidImage struct{ width, height int }

func (i serviceSolidImage) ColorModel() color.Model { return color.GrayModel }
func (i serviceSolidImage) Bounds() image.Rectangle { return image.Rect(0, 0, i.width, i.height) }
func (i serviceSolidImage) At(int, int) color.Color { return color.Black }

func TestServiceCanonicalHashAndValidBoundaries(t *testing.T) {
	req := serviceTestRequest()
	_, hash, err := serviceMessage(req)
	if err != nil {
		t.Fatal(err)
	}
	// Golden tuple catches accidental contract/hash drift across restarts.
	canonical := `["queue-v1/content/1","text","5511999999999@s.whatsapp.net","Olá, mundo 🌍","","",""]`
	sum := sha256.Sum256([]byte(canonical))
	if hash != hex.EncodeToString(sum[:]) {
		t.Fatal("canonical hash changed", hash)
	}
	req.AttemptID, req.LogicalMessageID, req.MessageID = serviceTestOther, serviceTestOther, "Other123"
	if _, otherHash, err := serviceMessage(req); err != nil || otherHash != hash {
		t.Fatal("identities affect content hash", err)
	}
	for _, to := range []string{"123@s.whatsapp.net", "123@lid", "123-456@g.us"} {
		req.To, req.Text, req.MessageID = to, strings.Repeat("á", serviceMaxTextBytes/2), strings.Repeat("A", 64)
		if _, _, err := serviceMessage(req); err != nil {
			t.Fatal("valid boundary", err)
		}
	}
	img := serviceTestImage(t, "png")
	_, original, err := serviceMessage(img)
	if err != nil {
		t.Fatal(err)
	}
	img.Caption += "!"
	if _, changed, err := serviceMessage(img); err != nil || changed == original {
		t.Fatal("caption not bound into hash", err)
	}
	img.Caption = strings.Repeat("a", serviceMaxCaptionBytes)
	if _, _, err := serviceMessage(img); err != nil {
		t.Fatal("caption boundary", err)
	}
}
