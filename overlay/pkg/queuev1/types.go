// Package queuev1 is an opt-in transport contract, not the legacy send service.
package queuev1

import (
	"context"
	"errors"
	"time"
)

const Contract = "queue-v1"
const OptInHeader = "X-Evolution-Queue-Contract"
const MaxImageBytes = 16 << 20
const MaxBodyBytes = 24 << 20

const (
	Preparing = "preparing"
	Sending   = "sending"
	Accepted  = "accepted"
	Rejected  = "rejected"
	Uncertain = "uncertain"
)

var (
	ErrNotFound    = errors.New("attempt not found")
	ErrConflict    = errors.New("identity conflict")
	ErrBusy        = errors.New("instance busy")
	ErrUnavailable = errors.New("queue ledger unavailable")
	// ErrExpired is proof that the commercial client call has NOT begun.
	// A transport must never return this unwrapped after calling SendMessage.
	ErrExpired = errors.New("queue deadline expired before commercial call")
)

// Credentials and message bodies never belong in these records.
type Binding struct {
	InstanceID       string
	AttemptID        string
	LogicalMessageID string
	MessageID        string
	ContentHash      string
	ValidUntil       string
	OwnerID          string
}

type Receipt struct {
	MessageID  string    `json:"messageId"`
	AcceptedAt time.Time `json:"acceptedAt"`
}

type Attempt struct {
	InstanceID       string     `json:"instanceId"`
	AttemptID        string     `json:"attemptId"`
	LogicalMessageID string     `json:"logicalMessageId"`
	MessageID        string     `json:"messageId"`
	ContentHash      string     `json:"contentHash"`
	ValidUntil       string     `json:"validUntil,omitempty"`
	State            string     `json:"state"`
	Code             string     `json:"code,omitempty"`
	CreatedAt        time.Time  `json:"createdAt"`
	SendingAt        *time.Time `json:"sendingAt,omitempty"`
	CompletedAt      *time.Time `json:"completedAt,omitempty"`
	Receipt          *Receipt   `json:"receipt,omitempty"`
}

// SQL methods are short transactions. WithInstance holds a physical advisory
// lock session, never a SQL transaction, while the bounded transport executes.
type Ledger interface {
	Lookup(context.Context, string, string) (Attempt, error)
	// denyCode records a definitive admission rejection without claiming an effect.
	// A repeated attempt returns execute=false; divergent identities conflict.
	Admit(context.Context, Binding, string) (attempt Attempt, execute bool, err error)
	MarkSending(context.Context, Binding) error
	Complete(context.Context, Binding, string, *Receipt, string) (Attempt, error)
	Idle(context.Context, string) error
	WithInstance(context.Context, string, func(Ledger) error) error
}

// Message has already been validated and canonicalized by the HTTP service.
// ImageBytes are owned by this operation, never shared with another send.
type Message struct {
	Kind        string
	To          string
	Text        string
	Caption     string
	MIME        string
	ImageSHA256 string
	ImageBytes  []byte
	ValidUntil  string
}

type Prepared interface {
	// Exactly one commercial call, with the persisted ID, no application retry.
	Send(context.Context) (Receipt, error)
	Close()
}

type Transport interface {
	// No commercial effect is permitted during Prepare (upload is allowed).
	Prepare(context.Context, string, string, Message) (Prepared, error)
	// Presence is separate and does not manufacture message-send authorization.
	Presence(context.Context, string, string, int) error
}
