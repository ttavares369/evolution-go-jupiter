package queuev1

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
)

func postgresMock(t *testing.T, max int) (*PostgresLedger, *sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock")
	}
	db.SetMaxOpenConns(max)
	p, err := NewPostgresLedger(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
		db.Close()
	})
	return p, db, mock
}

func TestPostgresConstructorInert(t *testing.T) {
	if _, err := NewPostgresLedger(nil); !errors.Is(err, ErrUnavailable) {
		t.Fatal("nil database accepted")
	}
	p, _, _ := postgresMock(t, 2)
	if p.scope != nil {
		t.Fatal("constructor unexpectedly scoped")
	}
	// sqlmock has no DDL, query, ping or transaction expectation.
}

func postgresMockAttempt(b Binding, state string) *sqlmock.Rows {
	now := time.Now().UTC()
	var sending, completed, receiptID, receiptAt any
	if state == Sending || state == Uncertain || state == Accepted {
		sending = now
	}
	if state == Accepted || state == Rejected {
		completed = now
	}
	if state == Accepted {
		receiptID, receiptAt = b.MessageID, now
	}
	return sqlmock.NewRows([]string{"instance_id", "attempt_id", "logical_id", "message_id", "content_hash", "state", "code", "created_at", "sending_at", "completed_at", "receipt_message_id", "receipt_accepted_at", "owner_id", "valid_until"}).
		AddRow(b.InstanceID, b.AttemptID, b.LogicalMessageID, b.MessageID, b.ContentHash, state, "", now, sending, completed, receiptID, receiptAt, b.OwnerID, b.ValidUntil)
}

func postgresExpectLocks(mock sqlmock.Sqlmock, b Binding, state string) {
	mock.ExpectQuery("SELECT active_attempt_id, active_owner_id.*FOR UPDATE").WithArgs(b.InstanceID).
		WillReturnRows(sqlmock.NewRows([]string{"active_attempt_id", "active_owner_id"}).AddRow(b.AttemptID, b.OwnerID))
	mock.ExpectQuery("SELECT message_id, content_hash, state, current_attempt_id, valid_until.*FOR UPDATE").WithArgs(b.InstanceID, b.LogicalMessageID).
		WillReturnRows(sqlmock.NewRows([]string{"message_id", "content_hash", "state", "current_attempt_id", "valid_until"}).AddRow(b.MessageID, b.ContentHash, state, b.AttemptID, b.ValidUntil))
	mock.ExpectQuery("SELECT .* FROM queue_v1_attempts.*FOR UPDATE").WithArgs(b.InstanceID, b.AttemptID).WillReturnRows(postgresMockAttempt(b, state))
}

func TestPostgresCompletionTransitionsOffline(t *testing.T) {
	for _, tc := range []struct {
		from, to string
		allowed  bool
	}{
		{Preparing, Accepted, false}, {Preparing, Uncertain, false}, {Preparing, Rejected, true},
		{Sending, Rejected, false}, {Sending, Uncertain, true}, {Sending, Accepted, true},
		{Uncertain, Rejected, false}, {Uncertain, Accepted, true}, {Uncertain, Uncertain, true},
		{Accepted, Accepted, true}, {Accepted, Rejected, false}, {Rejected, Rejected, true}, {Rejected, Accepted, false},
	} {
		t.Run(tc.from+"_to_"+tc.to, func(t *testing.T) {
			p, _, mock := postgresMock(t, 2)
			b := postgresBinding("instance")
			mock.ExpectBegin()
			postgresExpectLocks(mock, b, tc.from)
			cached := tc.from == Accepted || tc.from == Rejected || tc.from == tc.to
			if tc.allowed {
				if !cached {
					mock.ExpectQuery("UPDATE queue_v1_attempts SET state=").WillReturnRows(postgresMockAttempt(b, tc.to))
					mock.ExpectExec("UPDATE queue_v1_operations SET state=").WillReturnResult(sqlmock.NewResult(0, 1))
					if tc.to != Uncertain {
						mock.ExpectExec("UPDATE queue_v1_instances SET active_attempt_id=NULL,active_owner_id=NULL").WithArgs(b.InstanceID, b.AttemptID, b.OwnerID).WillReturnResult(sqlmock.NewResult(0, 1))
					}
				}
				mock.ExpectCommit()
			} else {
				mock.ExpectRollback()
			}
			var receipt *Receipt
			if tc.to == Accepted {
				receipt = &Receipt{MessageID: b.MessageID, AcceptedAt: time.Now()}
			}
			a, err := p.Complete(context.Background(), b, tc.to, receipt, "")
			if tc.allowed {
				if err != nil || a.State != tc.to {
					t.Fatalf("transition failed: %v", err)
				}
			} else if !errors.Is(err, ErrConflict) {
				t.Fatal("unsafe transition authorized")
			}
		})
	}
}

func TestPostgresAdmitCommitFailureNeverAuthorizes(t *testing.T) {
	p, _, mock := postgresMock(t, 2)
	b := postgresBinding("instance")
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO queue_v1_instances").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT active_attempt_id, active_owner_id.*FOR UPDATE").WillReturnRows(sqlmock.NewRows([]string{"active_attempt_id", "active_owner_id"}).AddRow(nil, nil))
	mock.ExpectExec("INSERT INTO queue_v1_operations").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT message_id, content_hash, state, current_attempt_id, valid_until.*FOR UPDATE").WillReturnRows(sqlmock.NewRows([]string{"message_id", "content_hash", "state", "current_attempt_id", "valid_until"}).AddRow(b.MessageID, b.ContentHash, Rejected, b.AttemptID, b.ValidUntil))
	mock.ExpectQuery("SELECT .* FROM queue_v1_attempts.*FOR UPDATE").WillReturnRows(sqlmock.NewRows([]string{"instance_id"}))
	mock.ExpectQuery("INSERT INTO queue_v1_attempts").WillReturnRows(postgresMockAttempt(b, Preparing))
	mock.ExpectExec("UPDATE queue_v1_operations SET state=").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE queue_v1_instances SET active_attempt_id=").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit().WillReturnError(errors.New("lost response password=secret"))
	a, run, err := p.Admit(context.Background(), b, "")
	if !errors.Is(err, ErrUnavailable) || run || a.State != "" || strings.Contains(err.Error(), "secret") {
		t.Fatal("ambiguous commit authorized effect or leaked driver error")
	}
}

func TestPostgresScopedTransactionNeedsNoSecondConnection(t *testing.T) {
	p, db, mock := postgresMock(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	reserve, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer reserve.Close()
	b := postgresBinding("instance")
	mock.ExpectQuery("SELECT pg_try_advisory_lock").WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
	mock.ExpectBegin()
	postgresExpectLocks(mock, b, Preparing)
	mock.ExpectExec("UPDATE queue_v1_attempts SET state='sending'").WithArgs(b.InstanceID, b.AttemptID, b.OwnerID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE queue_v1_operations SET state='sending'").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectQuery("SELECT pg_advisory_unlock").WillReturnRows(sqlmock.NewRows([]string{"unlocked"}).AddRow(true))
	if err := p.WithInstance(ctx, b.InstanceID, func(l Ledger) error { return l.MarkSending(ctx, b) }); err != nil {
		t.Fatal("scoped transaction failed:", err)
	}
	if db.Stats().WaitCount != 0 {
		t.Fatal("scoped transaction waited for a second connection")
	}
}

func TestPostgresMigrationVersionCheck(t *testing.T) {
	for _, version := range []int{1, 2, 3} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			p, _, mock := postgresMock(t, 2)
			mock.ExpectBegin()
			mock.ExpectExec("SELECT pg_advisory_xact_lock").WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec(regexp.QuoteMeta(postgresVersionDDL)).WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectQuery("SELECT version FROM queue_v1_schema").WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(version))
			if version <= 2 {
				mock.ExpectExec(regexp.QuoteMeta(postgresTablesDDL)).WillReturnResult(sqlmock.NewResult(0, 1))
				if version == 1 {
					mock.ExpectExec(regexp.QuoteMeta(postgresDeadlineDDL)).WillReturnResult(sqlmock.NewResult(0, 1))
				}
				mock.ExpectCommit()
			} else {
				mock.ExpectRollback()
			}
			err := p.Migrate(context.Background())
			if version <= 2 && err != nil {
				t.Fatal(err)
			}
			if version > 2 && !errors.Is(err, ErrUnavailable) {
				t.Fatal("unknown schema version accepted")
			}
		})
	}
}

func TestPostgresDriverErrorsSanitized(t *testing.T) {
	p, _, mock := postgresMock(t, 2)
	mock.ExpectQuery("SELECT").WillReturnError(errors.New("password=do-not-leak host=private SQL secret"))
	_, err := p.Lookup(context.Background(), "instance", "attempt")
	if !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "password") {
		t.Fatal("unsafe database error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = p.Lookup(ctx, "instance", "attempt")
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrUnavailable) {
		t.Fatal("cancellation not classified")
	}
}

func TestPostgresWithInstanceScopeAndCleanup(t *testing.T) {
	p, db, mock := postgresMock(t, 2)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT pg_try_advisory_lock($1)")).WithArgs(postgresSessionKey("i")).WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
	mock.ExpectQuery("SELECT active_attempt_id IS NOT NULL").WithArgs("i").WillReturnRows(sqlmock.NewRows([]string{"busy"}).AddRow(false))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT pg_advisory_unlock($1)")).WithArgs(postgresSessionKey("i")).WillReturnRows(sqlmock.NewRows([]string{"unlocked"}).AddRow(true))
	var escaped Ledger
	err := p.WithInstance(context.Background(), "i", func(l Ledger) error {
		escaped = l
		if db.Stats().InUse != 1 {
			t.Error("physical connection not pinned")
		}
		if err := l.Idle(context.Background(), "other"); !errors.Is(err, ErrUnavailable) {
			t.Error("scope escaped instance")
		}
		if err := l.WithInstance(context.Background(), "i", func(Ledger) error { return nil }); !errors.Is(err, ErrConflict) {
			t.Error("nested lock allowed")
		}
		return l.Idle(context.Background(), "i")
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := escaped.Idle(context.Background(), "i"); !errors.Is(err, ErrUnavailable) {
		t.Fatal("escaped session reused")
	}
}

func TestPostgresUnlockFailureDiscardsSession(t *testing.T) {
	for _, mode := range []string{"false", "error", "acquire_error"} {
		t.Run(mode, func(t *testing.T) {
			p, db, mock := postgresMock(t, 2)
			q := mock.ExpectQuery("SELECT pg_try_advisory_lock")
			if mode == "acquire_error" {
				q.WillReturnError(errors.New("lost lock response"))
			} else {
				q.WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
				q = mock.ExpectQuery("SELECT pg_advisory_unlock")
				if mode == "error" {
					q.WillReturnError(errors.New("lost unlock response"))
				} else {
					q.WillReturnRows(sqlmock.NewRows([]string{"unlocked"}).AddRow(false))
				}
			}
			mock.ExpectClose()
			err := p.WithInstance(context.Background(), "i", func(Ledger) error { return nil })
			if !errors.Is(err, ErrUnavailable) {
				t.Fatal("unlock failure accepted")
			}
			if db.Stats().OpenConnections != 0 {
				t.Fatal("possibly locked session returned to pool")
			}
		})
	}
}

func TestPostgresCancelledCallbackStillUnlocks(t *testing.T) {
	p, _, mock := postgresMock(t, 2)
	mock.ExpectQuery("SELECT pg_try_advisory_lock").WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
	mock.ExpectQuery("SELECT pg_advisory_unlock").WillReturnRows(sqlmock.NewRows([]string{"unlocked"}).AddRow(true))
	ctx, cancel := context.WithCancel(context.Background())
	err := p.WithInstance(ctx, "i", func(Ledger) error { cancel(); return context.Canceled })
	if !errors.Is(err, context.Canceled) {
		t.Fatal("callback error lost")
	}
}

func TestPostgresPanicStillUnlocks(t *testing.T) {
	p, _, mock := postgresMock(t, 2)
	mock.ExpectQuery("SELECT pg_try_advisory_lock").WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
	mock.ExpectQuery("SELECT pg_advisory_unlock").WillReturnRows(sqlmock.NewRows([]string{"unlocked"}).AddRow(true))
	defer func() {
		if recover() != "callback panic" {
			t.Error("panic not preserved")
		}
	}()
	_ = p.WithInstance(context.Background(), "i", func(Ledger) error { panic("callback panic") })
}

func TestPostgresSessionBudgetAndContention(t *testing.T) {
	p, _, _ := postgresMock(t, 1)
	if err := p.WithInstance(context.Background(), "i", func(Ledger) error { t.Fatal("budget bypass"); return nil }); !errors.Is(err, ErrBusy) {
		t.Fatal("pool reserve not respected")
	}
	p, _, mock := postgresMock(t, 2)
	mock.ExpectQuery("SELECT pg_try_advisory_lock").WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(false))
	if err := p.WithInstance(context.Background(), "i", func(Ledger) error { t.Fatal("lock contention bypass"); return nil }); !errors.Is(err, ErrBusy) {
		t.Fatal("contention not busy")
	}
	// A new ledger wrapper must not get a separate process budget.
	p, db, mock := postgresMock(t, 2)
	other, _ := NewPostgresLedger(db)
	mock.ExpectQuery("SELECT pg_try_advisory_lock").WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
	mock.ExpectQuery("SELECT pg_advisory_unlock").WillReturnRows(sqlmock.NewRows([]string{"unlocked"}).AddRow(true))
	if err := p.WithInstance(context.Background(), "i", func(Ledger) error {
		returnIfBusy := other.WithInstance(context.Background(), "different", func(Ledger) error { t.Error("budget bypass"); return nil })
		if !errors.Is(returnIfBusy, ErrBusy) {
			t.Error("budget not shared")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// PostgreSQL tests use this variable ONLY. No .env, configuration file, default
// DSN or production connection is ever consulted. URLs are deliberately stricter
// than lib/pq DSNs to prevent query-string host/database overrides.
func postgresSafeTestDSN(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Fragment != "" || u.Opaque != "" {
		return false
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if host != "queue-v1-postgres" && (ip == nil || ip.To4() == nil || !ip.IsLoopback()) {
		return false
	}
	if !regexp.MustCompile(`^/evolution_queue_test_[A-Za-z0-9_]+$`).MatchString(u.Path) {
		return false
	}
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return false
	}
	for key := range values {
		switch key {
		case "sslmode", "connect_timeout", "application_name":
		default:
			return false
		}
	}
	return true
}

func TestPostgresDSNGuard(t *testing.T) {
	for _, raw := range []string{
		"postgres://u:p@127.0.0.1:5432/evolution_queue_test_local?sslmode=disable",
		"postgresql://u@queue-v1-postgres/evolution_queue_test_ci",
	} {
		if !postgresSafeTestDSN(raw) {
			t.Fatal("safe disposable URL refused")
		}
	}
	for _, raw := range []string{
		"", "host=127.0.0.1 dbname=evolution_queue_test_local", "postgres://u@production/evolution_queue_test_local",
		"postgres://u@[::1]/evolution_queue_test_local",
		"postgres://u@localhost/evolution_queue_test_local", "postgres://u@127.0.0.1/evolution", "postgres://u@127.0.0.1/evolution_queue_test_",
		"postgres://u@127.0.0.1/evolution_queue_test_local?host=production", "postgres://u@127.0.0.1/evolution_queue_test_local?dbname=evolution",
		"postgres://u@127.0.0.1/evolution_queue_test_local?options=-csearch_path=public", "postgres://u@127.0.0.1/evolution_queue_test_local#override",
		"postgres://u@127.0.0.1/evolution_queue_test_local?sslmode=disable;host=production",
	} {
		if postgresSafeTestDSN(raw) {
			t.Fatal("unsafe URL accepted")
		}
	}
}

func postgresReal(t *testing.T) (*PostgresLedger, *sql.DB, string) {
	t.Helper()
	raw := os.Getenv("QUEUE_V1_TEST_DSN")
	if raw == "" {
		t.Skip("QUEUE_V1_TEST_DSN absent; no PostgreSQL connection attempted")
	}
	if !postgresSafeTestDSN(raw) {
		t.Fatal("QUEUE_V1_TEST_DSN must be a disposable loopback/queue-v1-postgres PostgreSQL URL with database evolution_queue_test_<suffix>")
	}
	db, err := sql.Open("postgres", raw)
	if err != nil {
		t.Fatal("open disposable PostgreSQL failed")
	}
	db.SetMaxOpenConns(8)
	t.Cleanup(func() { db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatal("disposable PostgreSQL unavailable")
	}
	p, _ := NewPostgresLedger(db)
	if err := p.Migrate(ctx); err != nil {
		t.Fatal("explicit migration failed:", err)
	}
	if err := p.Migrate(ctx); err != nil {
		t.Fatal("migration not idempotent:", err)
	}
	return p, db, raw
}

func postgresID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("test random source failed")
	}
	return hex.EncodeToString(b[:])
}

func postgresBinding(instance string) Binding {
	return Binding{InstanceID: instance, AttemptID: postgresID(), LogicalMessageID: postgresID(), MessageID: postgresID(), ContentHash: postgresID(), OwnerID: postgresID()}
}

func postgresNextAttempt(b Binding) Binding {
	b.AttemptID = postgresID()
	b.OwnerID = postgresID()
	return b
}

func postgresAdmit(t *testing.T, l Ledger, b Binding, deny string, execute bool, state string) Attempt {
	t.Helper()
	a, run, err := l.Admit(context.Background(), b, deny)
	if err != nil || run != execute || a.State != state {
		t.Fatalf("admit: state=%q execute=%v err=%v", a.State, run, err)
	}
	if a.InstanceID != b.InstanceID || a.AttemptID != b.AttemptID || a.LogicalMessageID != b.LogicalMessageID || a.MessageID != b.MessageID || a.ContentHash != b.ContentHash || a.CreatedAt.IsZero() {
		t.Fatal("identity/time projection lost")
	}
	return a
}

func postgresComplete(t *testing.T, l Ledger, b Binding, state string) Attempt {
	t.Helper()
	var receipt *Receipt
	if state == Accepted {
		receipt = &Receipt{MessageID: b.MessageID, AcceptedAt: time.Now().UTC()}
	}
	a, err := l.Complete(context.Background(), b, state, receipt, "test_result")
	if err != nil || a.State != state {
		t.Fatalf("complete %s: %v", state, err)
	}
	return a
}

func TestPostgresReal(t *testing.T) {
	p, db, raw := postgresReal(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	t.Run("identity_replay_conflicts_and_private_owner", func(t *testing.T) {
		b := postgresBinding(postgresID())
		first := postgresAdmit(t, p, b, "", true, Preparing)
		replay := b
		replay.OwnerID = postgresID()
		a := postgresAdmit(t, p, replay, "deny_must_not_overwrite", false, Preparing)
		if !first.CreatedAt.Equal(a.CreatedAt) {
			t.Fatal("replay changed creation time")
		}
		for _, change := range []func(*Binding){
			func(b *Binding) { b.LogicalMessageID = postgresID() }, func(b *Binding) { b.MessageID = postgresID() }, func(b *Binding) { b.ContentHash = postgresID() },
		} {
			changed := b
			change(&changed)
			if _, _, err := p.Admit(ctx, changed, ""); !errors.Is(err, ErrConflict) {
				t.Fatal("divergent attempt accepted")
			}
		}
		changed := postgresNextAttempt(b)
		changed.LogicalMessageID = postgresID()
		if _, _, err := p.Admit(ctx, changed, ""); !errors.Is(err, ErrConflict) {
			t.Fatal("message ID reused by another logical operation")
		}
		changed = postgresNextAttempt(b)
		changed.ContentHash = postgresID()
		if _, _, err := p.Admit(ctx, changed, ""); !errors.Is(err, ErrConflict) {
			t.Fatal("logical content changed")
		}
		if err := p.MarkSending(ctx, replay); !errors.Is(err, ErrConflict) {
			t.Fatal("replay owner stole execution")
		}
		if err := p.MarkSending(ctx, b); err != nil {
			t.Fatal(err)
		}
		if err := p.MarkSending(ctx, b); !errors.Is(err, ErrConflict) {
			t.Fatal("MarkSending was not CAS")
		}
		accepted := postgresComplete(t, p, b, Accepted)
		found, err := p.Lookup(ctx, b.InstanceID, b.AttemptID)
		if err != nil || found.Receipt == nil || found.Receipt.MessageID != b.MessageID || found.Receipt.AcceptedAt.IsZero() || found.SendingAt == nil || found.CompletedAt == nil || !found.CompletedAt.Equal(*accepted.CompletedAt) {
			t.Fatal("receipt/time roundtrip failed")
		}
		encoded, _ := json.Marshal(found)
		if strings.Contains(string(encoded), b.OwnerID) || strings.Contains(string(encoded), "owner") {
			t.Fatal("owner leaked in projection")
		}
		var owner string
		if err := db.QueryRowContext(ctx, `SELECT owner_id FROM queue_v1_attempts WHERE instance_id=$1 AND attempt_id=$2`, b.InstanceID, b.AttemptID).Scan(&owner); err != nil || owner != b.OwnerID {
			t.Fatal("private owner not persisted")
		}
		postgresAdmit(t, p, postgresNextAttempt(b), "", false, Rejected)
		if err := p.Idle(ctx, b.InstanceID); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("busy_and_denied_bindings_without_owner_clobber", func(t *testing.T) {
		b := postgresBinding(postgresID())
		postgresAdmit(t, p, b, "", true, Preparing)
		same := postgresNextAttempt(b)
		postgresAdmit(t, p, same, "", false, Rejected)
		other := postgresBinding(b.InstanceID)
		a := postgresAdmit(t, p, other, "", false, Rejected)
		if a.Code != "instance_busy" {
			t.Fatal("busy code missing")
		}
		denied := postgresBinding(b.InstanceID)
		if a := postgresAdmit(t, p, denied, "policy_denied", false, Rejected); a.Code != "policy_denied" {
			t.Fatal("deny code lost")
		}
		if err := p.MarkSending(ctx, b); err != nil {
			t.Fatal("denial clobbered active owner:", err)
		}
		postgresComplete(t, p, b, Accepted)
		postgresAdmit(t, p, other, "", false, Rejected)
		postgresAdmit(t, p, denied, "", false, Rejected)
		retry := postgresNextAttempt(other)
		postgresAdmit(t, p, retry, "", true, Preparing)
		// Old negative replays must not clear the retry's active barrier.
		postgresComplete(t, p, other, Rejected)
		if err := p.Idle(ctx, b.InstanceID); !errors.Is(err, ErrBusy) {
			t.Fatal("old rejection cleared active retry")
		}
		if err := p.MarkSending(ctx, retry); err != nil {
			t.Fatal(err)
		}
		postgresComplete(t, p, retry, Accepted)
	})

	t.Run("pre_effect_retry_and_refused_negative_cleanup", func(t *testing.T) {
		b := postgresBinding(postgresID())
		postgresAdmit(t, p, b, "", true, Preparing)
		if _, err := p.Complete(ctx, b, Accepted, &Receipt{MessageID: b.MessageID, AcceptedAt: time.Now()}, ""); !errors.Is(err, ErrConflict) {
			t.Fatal("accepted without sending")
		}
		postgresComplete(t, p, b, Rejected)
		retry := postgresNextAttempt(b)
		postgresAdmit(t, p, retry, "", true, Preparing)
		postgresComplete(t, p, b, Rejected)
		if err := p.MarkSending(ctx, retry); err != nil {
			t.Fatal(err)
		}
		for _, receipt := range []*Receipt{nil, {MessageID: "wrong", AcceptedAt: time.Now()}, {MessageID: b.MessageID}} {
			if _, err := p.Complete(ctx, retry, Accepted, receipt, ""); !errors.Is(err, ErrConflict) {
				t.Fatal("invalid receipt accepted")
			}
		}
		if _, err := p.Complete(ctx, retry, Rejected, nil, "negative_cleanup"); !errors.Is(err, ErrConflict) {
			t.Fatal("sending was negatively cleaned up")
		}
		postgresComplete(t, p, retry, Uncertain)
		if _, err := p.Complete(ctx, retry, Rejected, nil, "negative_cleanup"); !errors.Is(err, ErrConflict) {
			t.Fatal("uncertainty was cleared by rejection")
		}
		if err := p.Idle(ctx, b.InstanceID); !errors.Is(err, ErrBusy) {
			t.Fatal("uncertain barrier lost")
		}
		postgresAdmit(t, p, postgresNextAttempt(b), "", false, Rejected)
		wrongOwner := retry
		wrongOwner.OwnerID = postgresID()
		if _, err := p.Complete(ctx, wrongOwner, Accepted, &Receipt{MessageID: b.MessageID, AcceptedAt: time.Now()}, ""); !errors.Is(err, ErrConflict) {
			t.Fatal("unrelated owner supplied late ACK")
		}
		late := postgresComplete(t, p, retry, Accepted)
		other := postgresBinding(b.InstanceID)
		postgresAdmit(t, p, other, "", true, Preparing)
		replayed := postgresComplete(t, p, retry, Accepted)
		if !late.CompletedAt.Equal(*replayed.CompletedAt) {
			t.Fatal("positive replay rewrote receipt")
		}
		if err := p.Idle(ctx, b.InstanceID); !errors.Is(err, ErrBusy) {
			t.Fatal("late positive replay cleared newer owner")
		}
		postgresComplete(t, p, other, Rejected)
	})

	t.Run("restart_preparing_sending_uncertain_remain_blocked", func(t *testing.T) {
		for _, state := range []string{Preparing, Sending, Uncertain} {
			b := postgresBinding(postgresID())
			postgresAdmit(t, p, b, "", true, Preparing)
			if state != Preparing {
				if err := p.MarkSending(ctx, b); err != nil {
					t.Fatal(err)
				}
			}
			if state == Uncertain {
				postgresComplete(t, p, b, Uncertain)
			}
			restartDB, err := sql.Open("postgres", raw)
			if err != nil {
				t.Fatal("restart pool failed")
			}
			restartDB.SetMaxOpenConns(3)
			restart, _ := NewPostgresLedger(restartDB)
			postgresAdmit(t, restart, b, "", false, state)
			postgresAdmit(t, restart, postgresNextAttempt(b), "", false, Rejected)
			postgresAdmit(t, restart, postgresBinding(b.InstanceID), "", false, Rejected)
			if err := restart.Idle(ctx, b.InstanceID); !errors.Is(err, ErrBusy) {
				t.Fatal("restart lost persisted barrier")
			}
			restartDB.Close()
		}
	})

	t.Run("real_concurrency_two_pools_and_instances", func(t *testing.T) {
		secondDB, err := sql.Open("postgres", raw)
		if err != nil {
			t.Fatal("second pool failed")
		}
		defer secondDB.Close()
		secondDB.SetMaxOpenConns(8)
		second, _ := NewPostgresLedger(secondDB)
		b := postgresBinding(postgresID())
		var runs atomic.Int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		failures := make(chan error, 24)
		for n := 0; n < 24; n++ {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				<-start
				var l Ledger = p
				if n%2 == 1 {
					l = second
				}
				_, execute, err := l.Admit(ctx, b, "")
				if err != nil {
					failures <- err
					return
				}
				if execute {
					runs.Add(1)
				}
			}(n)
		}
		close(start)
		wg.Wait()
		close(failures)
		for err := range failures {
			t.Error(err)
		}
		if runs.Load() != 1 {
			t.Fatalf("executions=%d", runs.Load())
		}
		// Distinct attempts/logicals racing on one instance still get one owner.
		instance := postgresID()
		runs.Store(0)
		failures = make(chan error, 16)
		start = make(chan struct{})
		for n := 0; n < 16; n++ {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				<-start
				l := p
				if n%2 == 1 {
					l = second
				}
				_, execute, err := l.Admit(ctx, postgresBinding(instance), "")
				if err != nil {
					failures <- err
					return
				}
				if execute {
					runs.Add(1)
				}
			}(n)
		}
		close(start)
		wg.Wait()
		close(failures)
		for err := range failures {
			t.Error(err)
		}
		if runs.Load() != 1 {
			t.Fatal("multiple instance owners")
		}
		// The same WhatsApp ID cannot be claimed by racing logical operations.
		left := postgresBinding(postgresID())
		right := postgresBinding(left.InstanceID)
		right.MessageID = left.MessageID
		var conflicts atomic.Int32
		runs.Store(0)
		failures = make(chan error, 2)
		start = make(chan struct{})
		for n, binding := range []Binding{left, right} {
			wg.Add(1)
			go func(n int, binding Binding) {
				defer wg.Done()
				<-start
				l := p
				if n == 1 {
					l = second
				}
				_, execute, err := l.Admit(ctx, binding, "")
				if errors.Is(err, ErrConflict) {
					conflicts.Add(1)
				} else if err != nil {
					failures <- err
				}
				if execute {
					runs.Add(1)
				}
			}(n, binding)
		}
		close(start)
		wg.Wait()
		close(failures)
		for err := range failures {
			t.Error(err)
		}
		if runs.Load() != 1 || conflicts.Load() != 1 {
			t.Fatal("concurrent logical operations shared a WhatsApp ID")
		}
		postgresAdmit(t, second, postgresBinding(postgresID()), "", true, Preparing)
		// Session locks are also shared by independent pools, not Go objects.
		if err := p.WithInstance(ctx, b.InstanceID, func(Ledger) error {
			if err := second.WithInstance(ctx, b.InstanceID, func(Ledger) error { t.Error("same instance entered twice"); return nil }); !errors.Is(err, ErrBusy) {
				t.Error("cross-pool lock contention not busy")
			}
			return second.WithInstance(ctx, postgresID(), func(Ledger) error { return nil })
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("same_physical_connection_no_callback_transaction", func(t *testing.T) {
		localDB, err := sql.Open("postgres", raw)
		if err != nil {
			t.Fatal("local pool failed")
		}
		defer localDB.Close()
		localDB.SetMaxOpenConns(2)
		localDB.SetMaxIdleConns(2)
		local, _ := NewPostgresLedger(localDB)
		observer, err := localDB.Conn(ctx)
		if err != nil {
			t.Fatal("observer unavailable")
		}
		defer observer.Close()
		b := postgresBinding(postgresID())
		var escaped Ledger
		err = local.WithInstance(ctx, b.InstanceID, func(l Ledger) error {
			escaped = l
			var pid int
			scoped := l.(*PostgresLedger)
			if err := scoped.scope.conn.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				return ErrUnavailable
			}
			assertIdle := func() {
				var state string
				var xact sql.NullTime
				if err := observer.QueryRowContext(ctx, `SELECT state,xact_start FROM pg_stat_activity WHERE pid=$1`, pid).Scan(&state, &xact); err != nil {
					t.Fatal("observe session failed")
				}
				if state != "idle" || xact.Valid {
					t.Fatal("SQL transaction held during callback")
				}
			}
			assertIdle()
			before := localDB.Stats().WaitCount
			postgresAdmit(t, l, b, "", true, Preparing)
			if _, err := l.Lookup(ctx, b.InstanceID, b.AttemptID); err != nil {
				return err
			}
			if err := l.MarkSending(ctx, b); err != nil {
				return err
			}
			postgresComplete(t, l, b, Accepted)
			if err := l.Idle(ctx, b.InstanceID); err != nil {
				return err
			}
			if localDB.Stats().WaitCount != before {
				t.Fatal("scoped method acquired second pool connection")
			}
			assertIdle()
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := escaped.Idle(ctx, b.InstanceID); !errors.Is(err, ErrUnavailable) {
			t.Fatal("escaped scope still live")
		}
	})

	t.Run("lost_session_lock_is_poisoned", func(t *testing.T) {
		localDB, err := sql.Open("postgres", raw)
		if err != nil {
			t.Fatal("local pool failed")
		}
		defer localDB.Close()
		localDB.SetMaxOpenConns(2)
		localDB.SetMaxIdleConns(2)
		local, _ := NewPostgresLedger(localDB)
		instance := postgresID()
		err = local.WithInstance(ctx, instance, func(l Ledger) error {
			var unlocked bool
			if err := l.(*PostgresLedger).scope.conn.QueryRowContext(ctx, `SELECT pg_advisory_unlock($1)`, postgresSessionKey(instance)).Scan(&unlocked); err != nil || !unlocked {
				t.Error("test unlock failed")
			}
			return nil
		})
		if !errors.Is(err, ErrUnavailable) || localDB.Stats().OpenConnections != 0 {
			t.Fatal("lost-lock connection was not discarded")
		}
		if err := local.WithInstance(ctx, instance, func(Ledger) error { return nil }); err != nil {
			t.Fatal("replacement connection failed")
		}
	})

	t.Run("sql_constraints_and_immutable_bindings", func(t *testing.T) {
		b := postgresBinding(postgresID())
		postgresAdmit(t, p, b, "", true, Preparing)
		if err := p.MarkSending(ctx, b); err != nil {
			t.Fatal(err)
		}
		accepted := postgresComplete(t, p, b, Accepted)
		for _, tc := range []struct{ name, query, code string }{
			{"attempt_pk", `INSERT INTO queue_v1_attempts SELECT * FROM queue_v1_attempts WHERE instance_id=$1`, "23505"},
			{"logical_pk", `INSERT INTO queue_v1_operations SELECT * FROM queue_v1_operations WHERE instance_id=$1`, "23505"},
			{"message_unique", `INSERT INTO queue_v1_operations(instance_id,logical_id,message_id,content_hash,state,current_attempt_id)
 SELECT instance_id,logical_id||'_other',message_id,content_hash,state,current_attempt_id FROM queue_v1_operations WHERE instance_id=$1`, "23505"},
			{"operation_hash_immutable", `UPDATE queue_v1_operations SET content_hash='changed' WHERE instance_id=$1`, "23503"},
			{"attempt_hash_bound", `UPDATE queue_v1_attempts SET content_hash='changed' WHERE instance_id=$1`, "23503"},
			{"receipt_id_matches", `UPDATE queue_v1_attempts SET receipt_message_id='wrong' WHERE instance_id=$1`, "23514"},
			{"receipt_required", `UPDATE queue_v1_attempts SET receipt_accepted_at=NULL WHERE instance_id=$1`, "23514"},
			{"receipt_zero", `UPDATE queue_v1_attempts SET receipt_accepted_at='0001-01-01 00:00:00+00' WHERE instance_id=$1`, "23514"},
			{"receipt_infinite", `UPDATE queue_v1_attempts SET receipt_accepted_at='infinity' WHERE instance_id=$1`, "23514"},
			{"receipt_negative_infinite", `UPDATE queue_v1_attempts SET receipt_accepted_at='-infinity' WHERE instance_id=$1`, "23514"},
			{"state", `UPDATE queue_v1_attempts SET state='invalid' WHERE instance_id=$1`, "23514"},
			{"owner", `UPDATE queue_v1_attempts SET owner_id='' WHERE instance_id=$1`, "23514"},
			{"sending_time", `UPDATE queue_v1_attempts SET sending_at=NULL WHERE instance_id=$1`, "23514"},
			{"completion_time", `UPDATE queue_v1_attempts SET completed_at=NULL WHERE instance_id=$1`, "23514"},
			{"barrier_pair", `UPDATE queue_v1_instances SET active_attempt_id='unpaired' WHERE instance_id=$1`, "23514"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				tx, err := db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal("constraint transaction failed")
				}
				defer tx.Rollback()
				_, err = tx.ExecContext(ctx, tc.query, b.InstanceID)
				var pgErr *pq.Error
				if !errors.As(err, &pgErr) || string(pgErr.Code) != tc.code {
					t.Fatal("expected SQL integrity constraint was not enforced")
				}
			})
		}
		a, err := p.Lookup(ctx, b.InstanceID, b.AttemptID)
		if err != nil || a.Receipt == nil || !a.Receipt.AcceptedAt.Equal(accepted.Receipt.AcceptedAt) || a.ContentHash != b.ContentHash {
			t.Fatal("failed integrity writes changed persisted receipt")
		}
		if err := p.Idle(ctx, b.InstanceID); err != nil {
			t.Fatal("constraint tests changed active barrier")
		}
	})

	t.Run("uncertain_completion_replay_preserves_original_result", func(t *testing.T) {
		b := postgresBinding(postgresID())
		postgresAdmit(t, p, b, "", true, Preparing)
		if err := p.MarkSending(ctx, b); err != nil {
			t.Fatal(err)
		}
		first, err := p.Complete(ctx, b, Uncertain, nil, "original_uncertainty")
		if err != nil {
			t.Fatal(err)
		}
		replay, err := p.Complete(ctx, b, Uncertain, nil, "must_not_overwrite")
		if err != nil {
			t.Fatal(err)
		}
		if replay.Code != first.Code || replay.SendingAt == nil || !replay.SendingAt.Equal(*first.SendingAt) || replay.CompletedAt != nil {
			t.Fatal("uncertain replay rewrote original result")
		}
		if err := p.Idle(ctx, b.InstanceID); !errors.Is(err, ErrBusy) {
			t.Fatal("uncertain replay cleared barrier")
		}
		postgresComplete(t, p, b, Accepted)
	})

	t.Run("row_lock_timeout_fails_closed_without_cleanup", func(t *testing.T) {
		b := postgresBinding(postgresID())
		postgresAdmit(t, p, b, "", true, Preparing)
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal("blocking transaction failed")
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, `SELECT instance_id FROM queue_v1_instances WHERE instance_id=$1 FOR UPDATE`, b.InstanceID); err != nil {
			t.Fatal("block instance row failed")
		}
		short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		err = p.MarkSending(short, b)
		cancel()
		if !errors.Is(err, ErrUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("row-lock timeout did not fail closed")
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal("release row lock failed")
		}
		a, err := p.Lookup(ctx, b.InstanceID, b.AttemptID)
		if err != nil || a.State != Preparing || a.SendingAt != nil {
			t.Fatal("timed-out transaction changed attempt")
		}
		if err := p.Idle(ctx, b.InstanceID); !errors.Is(err, ErrBusy) {
			t.Fatal("timed-out mutation cleared barrier")
		}
		if err := p.MarkSending(ctx, b); err != nil {
			t.Fatal("subsequent mutation failed:", err)
		}
		postgresComplete(t, p, b, Accepted)
	})

	t.Run("pool_acquisition_bounded_and_budget_released", func(t *testing.T) {
		localDB, err := sql.Open("postgres", raw)
		if err != nil {
			t.Fatal("local pool failed")
		}
		defer localDB.Close()
		localDB.SetMaxOpenConns(2)
		local, _ := NewPostgresLedger(localDB)
		first, err := localDB.Conn(ctx)
		if err != nil {
			t.Fatal("reserve first connection failed")
		}
		defer first.Close()
		second, err := localDB.Conn(ctx)
		if err != nil {
			t.Fatal("reserve second connection failed")
		}
		defer second.Close()
		instance := postgresID()
		entered := false
		started := time.Now()
		err = local.WithInstance(ctx, instance, func(Ledger) error { entered = true; return nil })
		if !errors.Is(err, ErrBusy) || entered || time.Since(started) > postgresSessionTimeout+2*time.Second || localDB.Stats().WaitCount == 0 {
			t.Fatal("saturated pool was not bounded busy")
		}
		short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		err = local.WithInstance(short, instance, func(Ledger) error { entered = true; return nil })
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) || entered {
			t.Fatal("caller deadline did not bound pool acquisition")
		}
		if err := second.Close(); err != nil {
			t.Fatal("release reserved connection failed")
		}
		if err := local.WithInstance(ctx, instance, func(Ledger) error { return nil }); err != nil {
			t.Fatal("pool timeout leaked session budget:", err)
		}
	})

	t.Run("cancelled_callback_unlocks_with_independent_context", func(t *testing.T) {
		instance := postgresID()
		cancelled, cancel := context.WithCancel(ctx)
		err := p.WithInstance(cancelled, instance, func(Ledger) error { cancel(); return context.Canceled })
		if !errors.Is(err, context.Canceled) {
			t.Fatal("cancelled callback error lost")
		}
		otherDB, err := sql.Open("postgres", raw)
		if err != nil {
			t.Fatal("verification pool failed")
		}
		defer otherDB.Close()
		otherDB.SetMaxOpenConns(3)
		other, _ := NewPostgresLedger(otherDB)
		if err := other.WithInstance(ctx, instance, func(Ledger) error { return nil }); err != nil {
			t.Fatal("cancelled callback leaked advisory lock:", err)
		}
	})

	t.Run("terminated_backend_discards_session_but_retains_sending_barrier", func(t *testing.T) {
		localDB, err := sql.Open("postgres", raw)
		if err != nil {
			t.Fatal("local pool failed")
		}
		defer localDB.Close()
		localDB.SetMaxOpenConns(3)
		localDB.SetMaxIdleConns(3)
		local, _ := NewPostgresLedger(localDB)
		b := postgresBinding(postgresID())
		err = local.WithInstance(ctx, b.InstanceID, func(l Ledger) error {
			postgresAdmit(t, l, b, "", true, Preparing)
			if err := l.MarkSending(ctx, b); err != nil {
				return err
			}
			var pid int
			if err := l.(*PostgresLedger).scope.conn.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				t.Fatal("read owned backend pid failed")
			}
			// Kill only the physical session acquired by THIS fixture callback.
			var killed bool
			if err := db.QueryRowContext(ctx, `SELECT pg_terminate_backend($1)`, pid).Scan(&killed); err != nil || !killed {
				t.Fatal("terminate owned fixture backend failed")
			}
			return nil
		})
		if !errors.Is(err, ErrUnavailable) || localDB.Stats().OpenConnections != 0 {
			t.Fatal("terminated locked session returned to pool")
		}
		a, err := local.Lookup(ctx, b.InstanceID, b.AttemptID)
		if err != nil || a.State != Sending {
			t.Fatal("session loss erased durable sending state")
		}
		postgresAdmit(t, local, postgresNextAttempt(b), "", false, Rejected)
		if err := local.WithInstance(ctx, b.InstanceID, func(l Ledger) error { return l.Idle(ctx, b.InstanceID) }); !errors.Is(err, ErrBusy) {
			t.Fatal("session loss cleared persistent barrier")
		}
		postgresComplete(t, local, b, Uncertain)
		postgresComplete(t, local, b, Accepted)
	})

	t.Run("closed_database_fails_closed", func(t *testing.T) {
		closedDB, err := sql.Open("postgres", raw)
		if err != nil {
			t.Fatal("open closed pool failed")
		}
		closedDB.Close()
		closed, _ := NewPostgresLedger(closedDB)
		b := postgresBinding(postgresID())
		if _, run, err := closed.Admit(ctx, b, ""); run || !errors.Is(err, ErrUnavailable) {
			t.Fatal("failed DB authorized effect")
		}
		if err := closed.MarkSending(ctx, b); !errors.Is(err, ErrUnavailable) {
			t.Fatal("failed DB authorized sending")
		}
		if _, err := closed.Complete(ctx, b, Rejected, nil, ""); !errors.Is(err, ErrUnavailable) {
			t.Fatal("failed DB fabricated rejection")
		}
		if err := closed.Idle(ctx, b.InstanceID); !errors.Is(err, ErrUnavailable) {
			t.Fatal("failed DB reported idle")
		}
	})
}
