package queuev1

import (
	"context"
	"github.com/DATA-DOG/go-sqlmock"
	"testing"
	"time"
)

func TestAdmissionDenialPreservesLogicalEvidence(t *testing.T) {
	for _, state := range []string{Preparing, Sending, Uncertain, Accepted, Rejected} {
		t.Run(state, func(t *testing.T) {
			p, _, mock := postgresMock(t, 4)
			b := postgresBinding("instance")
			now := time.Now().UTC()
			code := "logical_pending"
			if state == Accepted {
				code = "logical_accepted"
			}
			if state == Rejected {
				code = "instance_busy"
			}
			mock.ExpectBegin()
			mock.ExpectExec("INSERT INTO queue_v1_instances").WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectQuery("SELECT active_attempt_id, active_owner_id.*FOR UPDATE").WillReturnRows(sqlmock.NewRows([]string{"active_attempt_id", "active_owner_id"}).AddRow("original-attempt", "original-owner"))
			mock.ExpectExec("INSERT INTO queue_v1_operations").WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectQuery("SELECT message_id, content_hash, state, current_attempt_id, valid_until.*FOR UPDATE").WillReturnRows(sqlmock.NewRows([]string{"message_id", "content_hash", "state", "current_attempt_id", "valid_until"}).AddRow(b.MessageID, b.ContentHash, state, "original-attempt", b.ValidUntil))
			mock.ExpectQuery("SELECT .* FROM queue_v1_attempts.*FOR UPDATE").WillReturnRows(sqlmock.NewRows([]string{"instance_id"}))
			mock.ExpectQuery("INSERT INTO queue_v1_attempts").WithArgs(b.InstanceID, b.AttemptID, b.LogicalMessageID, b.MessageID, b.ContentHash, b.OwnerID, Rejected, code, b.ValidUntil).WillReturnRows(sqlmock.NewRows([]string{"instance_id", "attempt_id", "logical_id", "message_id", "content_hash", "state", "code", "created_at", "sending_at", "completed_at", "receipt_message_id", "receipt_accepted_at", "owner_id", "valid_until"}).AddRow(b.InstanceID, b.AttemptID, b.LogicalMessageID, b.MessageID, b.ContentHash, Rejected, code, now, nil, now, nil, nil, b.OwnerID, b.ValidUntil))
			if state == Rejected {
				mock.ExpectExec("UPDATE queue_v1_operations SET state=").WillReturnResult(sqlmock.NewResult(0, 1))
			}
			mock.ExpectCommit()
			a, run, err := p.Admit(context.Background(), b, "instance_busy")
			if err != nil || run || a.Code != code {
				t.Fatalf("logical evidence lost: state=%s code=%s run=%v err=%v", state, a.Code, run, err)
			}
		})
	}
}

func TestAdmissionRealDenialPreservesLogicalEvidence(t *testing.T) {
	p, _, _ := postgresReal(t)
	ctx := context.Background()
	for _, state := range []string{Preparing, Sending, Uncertain, Accepted, Rejected} {
		t.Run(state, func(t *testing.T) {
			b := postgresBinding(postgresID())
			postgresAdmit(t, p, b, "", true, Preparing)
			if state != Preparing && state != Rejected {
				if err := p.MarkSending(ctx, b); err != nil {
					t.Fatal(err)
				}
			}
			if state == Uncertain || state == Accepted || state == Rejected {
				postgresComplete(t, p, b, state)
			}
			next := postgresNextAttempt(b)
			code := "logical_pending"
			if state == Accepted {
				code = "logical_accepted"
			}
			if state == Rejected {
				code = "instance_busy"
			}
			// Exercise the root denial used while another owner holds the session lock.
			err := p.WithInstance(ctx, b.InstanceID, func(Ledger) error {
				a := postgresAdmit(t, p, next, "instance_busy", false, Rejected)
				if a.Code != code {
					t.Errorf("wrong negative evidence: %s", a.Code)
				}
				replay := postgresAdmit(t, p, b, "instance_busy", false, state)
				if replay.State != state {
					t.Error("original changed")
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			a := postgresAdmit(t, p, next, "", false, Rejected)
			if a.Code != code {
				t.Fatal("replay overwrote denial")
			}
			original, err := p.Lookup(ctx, b.InstanceID, b.AttemptID)
			if err != nil || original.State != state {
				t.Fatal("original evidence changed")
			}
		})
	}
}
