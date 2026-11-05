package queuev1

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"errors"
	"sync"
	"time"
)

const postgresSessionTimeout = 2 * time.Second

// This is only a process resource guard. PostgreSQL locks and persisted rows,
// not this counter, decide whether an instance or operation can execute.
var postgresSessions struct {
	sync.Mutex
	active int
}

// PostgresLedger is inert until explicitly migrated and called. The caller owns
// db and must supply a fresh, server-generated random Binding.OwnerID for each
// new execution. OwnerID is persisted but never included in public projections.
// Do not use a transaction-pooling proxy: WithInstance needs session affinity.
type PostgresLedger struct {
	db    *sql.DB
	scope *postgresScope
}

type postgresScope struct {
	sync.Mutex
	conn     *sql.Conn
	instance string
	closed   bool
}

var _ Ledger = (*PostgresLedger)(nil)

// NewPostgresLedger neither connects nor performs DDL, and never owns db.Close.
func NewPostgresLedger(db *sql.DB) (*PostgresLedger, error) {
	if db == nil {
		return nil, ErrUnavailable
	}
	return &PostgresLedger{db: db}, nil
}

// Migrate creates only queue_v1_ tables. Schema changes are explicit, atomic and
// serialized across processes; an unknown schema version is never upgraded.
func (p *PostgresLedger) Migrate(ctx context.Context) error {
	if p.scope != nil {
		return ErrUnavailable
	}
	return p.transaction(ctx, "", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(719628120025100001)`); err != nil {
			return postgresError(ctx)
		}
		if _, err := tx.ExecContext(ctx, postgresVersionDDL); err != nil {
			return postgresError(ctx)
		}
		var version int
		if err := tx.QueryRowContext(ctx, `SELECT version FROM queue_v1_schema WHERE singleton = TRUE`).Scan(&version); err != nil || (version != 1 && version != 2) {
			return ErrUnavailable
		}
		if _, err := tx.ExecContext(ctx, postgresTablesDDL); err != nil {
			return postgresError(ctx)
		}
		if version == 1 {
			if _, err := tx.ExecContext(ctx, postgresDeadlineDDL); err != nil {
				return postgresError(ctx)
			}
		}
		return nil
	})
}

// Driver errors can contain DSNs, SQL or parameters. Never wrap or return them.
func postgresError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return errors.Join(ErrUnavailable, err)
	}
	return ErrUnavailable
}

func (p *PostgresLedger) transaction(ctx context.Context, instance string, fn func(*sql.Tx) error) error {
	var tx *sql.Tx
	var err error
	if p.scope != nil {
		p.scope.Lock()
		defer p.scope.Unlock()
		if p.scope.closed || p.scope.instance != instance {
			return ErrUnavailable
		}
		tx, err = p.scope.conn.BeginTx(ctx, nil)
	} else {
		tx, err = p.db.BeginTx(ctx, nil)
	}
	if err != nil {
		return postgresError(ctx)
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		// A lost COMMIT response is not evidence of a pre-effect rejection.
		return postgresError(ctx)
	}
	return nil
}

const postgresAttemptColumns = `instance_id, attempt_id, logical_id, message_id, content_hash,
 state, code, created_at, sending_at, completed_at, receipt_message_id, receipt_accepted_at, owner_id, valid_until`

type postgresScanner interface{ Scan(...any) error }

type postgresAttempt struct {
	Attempt
	owner string
}

func postgresScan(row postgresScanner) (postgresAttempt, error) {
	var a postgresAttempt
	var sending, completed, accepted sql.NullTime
	var receiptID sql.NullString
	err := row.Scan(&a.InstanceID, &a.AttemptID, &a.LogicalMessageID, &a.MessageID, &a.ContentHash,
		&a.State, &a.Code, &a.CreatedAt, &sending, &completed, &receiptID, &accepted, &a.owner, &a.ValidUntil)
	if err != nil {
		return a, err
	}
	a.CreatedAt = a.CreatedAt.UTC()
	if sending.Valid {
		t := sending.Time.UTC()
		a.SendingAt = &t
	}
	if completed.Valid {
		t := completed.Time.UTC()
		a.CompletedAt = &t
	}
	if receiptID.Valid && accepted.Valid {
		a.Receipt = &Receipt{MessageID: receiptID.String, AcceptedAt: accepted.Time.UTC()}
	}
	return a, nil
}

func (p *PostgresLedger) Lookup(ctx context.Context, instance, attempt string) (Attempt, error) {
	const query = `SELECT ` + postgresAttemptColumns + ` FROM queue_v1_attempts WHERE instance_id=$1 AND attempt_id=$2`
	var a postgresAttempt
	var err error
	if p.scope != nil {
		p.scope.Lock()
		defer p.scope.Unlock()
		if p.scope.closed || p.scope.instance != instance {
			return Attempt{}, ErrUnavailable
		}
		a, err = postgresScan(p.scope.conn.QueryRowContext(ctx, query, instance, attempt))
	} else {
		a, err = postgresScan(p.db.QueryRowContext(ctx, query, instance, attempt))
	}
	if errors.Is(err, sql.ErrNoRows) {
		return Attempt{}, ErrNotFound
	}
	if err != nil {
		return Attempt{}, postgresError(ctx)
	}
	return a.Attempt, nil
}

type postgresInstance struct{ attempt, owner sql.NullString }
type postgresOperation struct{ message, hash, state, attempt, validUntil string }

// Every mutator takes row locks in this order, even for rejected/replayed calls.
func postgresLockInstance(ctx context.Context, tx *sql.Tx, instance string, create bool) (postgresInstance, error) {
	var i postgresInstance
	if create {
		if _, err := tx.ExecContext(ctx, `INSERT INTO queue_v1_instances(instance_id) VALUES($1) ON CONFLICT DO NOTHING`, instance); err != nil {
			return i, postgresError(ctx)
		}
	}
	err := tx.QueryRowContext(ctx, `SELECT active_attempt_id, active_owner_id FROM queue_v1_instances WHERE instance_id=$1 FOR UPDATE`, instance).Scan(&i.attempt, &i.owner)
	if errors.Is(err, sql.ErrNoRows) {
		return i, ErrNotFound
	}
	if err != nil {
		return i, postgresError(ctx)
	}
	return i, nil
}

func postgresLockOperation(ctx context.Context, tx *sql.Tx, b Binding) (postgresOperation, error) {
	var op postgresOperation
	err := tx.QueryRowContext(ctx, `SELECT message_id, content_hash, state, current_attempt_id, valid_until FROM queue_v1_operations WHERE instance_id=$1 AND logical_id=$2 FOR UPDATE`, b.InstanceID, b.LogicalMessageID).Scan(&op.message, &op.hash, &op.state, &op.attempt, &op.validUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return op, ErrNotFound
	}
	if err != nil {
		return op, postgresError(ctx)
	}
	if op.message != b.MessageID || op.hash != b.ContentHash || op.validUntil != b.ValidUntil {
		return op, ErrConflict
	}
	return op, nil
}

func postgresLockAttempt(ctx context.Context, tx *sql.Tx, b Binding) (postgresAttempt, error) {
	a, err := postgresScan(tx.QueryRowContext(ctx, `SELECT `+postgresAttemptColumns+` FROM queue_v1_attempts WHERE instance_id=$1 AND attempt_id=$2 FOR UPDATE`, b.InstanceID, b.AttemptID))
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	if err != nil {
		return a, postgresError(ctx)
	}
	if a.LogicalMessageID != b.LogicalMessageID || a.MessageID != b.MessageID || a.ContentHash != b.ContentHash || a.ValidUntil != b.ValidUntil {
		return a, ErrConflict
	}
	return a, nil
}

func postgresValidBinding(b Binding) bool {
	return b.InstanceID != "" && b.AttemptID != "" && b.LogicalMessageID != "" && b.MessageID != "" && b.ContentHash != "" && b.OwnerID != "" && canonicalDeadline(b.ValidUntil)
}

func (p *PostgresLedger) Admit(ctx context.Context, b Binding, denyCode string) (Attempt, bool, error) {
	if !postgresValidBinding(b) {
		return Attempt{}, false, ErrConflict
	}
	var result Attempt
	var execute bool
	err := p.transaction(ctx, b.InstanceID, func(tx *sql.Tx) error {
		i, err := postgresLockInstance(ctx, tx, b.InstanceID, true)
		if err != nil {
			return err
		}
		// The instance lock also serializes insertion of a previously absent
		// operation. ON CONFLICT covers the independent message-ID uniqueness.
		_, err = tx.ExecContext(ctx, `INSERT INTO queue_v1_operations(instance_id, logical_id, message_id, content_hash, state, current_attempt_id, valid_until)
 VALUES($1,$2,$3,$4,'rejected',$5,$6) ON CONFLICT DO NOTHING`, b.InstanceID, b.LogicalMessageID, b.MessageID, b.ContentHash, b.AttemptID, b.ValidUntil)
		if err != nil {
			return postgresError(ctx)
		}
		op, err := postgresLockOperation(ctx, tx, b)
		if errors.Is(err, ErrNotFound) {
			return ErrConflict
		}
		if err != nil {
			return err
		}
		prior, err := postgresLockAttempt(ctx, tx, b)
		if err == nil {
			result = prior.Attempt
			return nil // Owner changes on replay never grant execution rights.
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		state, code := Preparing, denyCode
		// A denial applies to this admission, never disproves an earlier
		// effect of its logical message. Preserve that stronger evidence even
		// when WithInstance could not acquire the busy session lock.
		switch {
		case op.state == Accepted:
			code = "logical_accepted"
		case op.state != Rejected:
			code = "logical_pending"
		case code == "" && i.attempt.Valid:
			code = "instance_busy"
		}
		if code != "" {
			state = Rejected
		}
		a, err := postgresScan(tx.QueryRowContext(ctx, `INSERT INTO queue_v1_attempts
 (instance_id,attempt_id,logical_id,message_id,content_hash,owner_id,state,code,completed_at,valid_until)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,CASE WHEN $7='rejected' THEN clock_timestamp() ELSE NULL END,$9)
 RETURNING `+postgresAttemptColumns, b.InstanceID, b.AttemptID, b.LogicalMessageID, b.MessageID, b.ContentHash, b.OwnerID, state, code, b.ValidUntil))
		if err != nil {
			return postgresError(ctx)
		}
		// A busy/denied attempt binds its identity, but cannot replace the
		// operation that is already preparing/sending/uncertain/accepted.
		if op.state == Rejected {
			if _, err := tx.ExecContext(ctx, `UPDATE queue_v1_operations SET state=$3,current_attempt_id=$4 WHERE instance_id=$1 AND logical_id=$2`, b.InstanceID, b.LogicalMessageID, state, b.AttemptID); err != nil {
				return postgresError(ctx)
			}
		}
		if state == Preparing {
			if _, err := tx.ExecContext(ctx, `UPDATE queue_v1_instances SET active_attempt_id=$2,active_owner_id=$3 WHERE instance_id=$1`, b.InstanceID, b.AttemptID, b.OwnerID); err != nil {
				return postgresError(ctx)
			}
		}
		result, execute = a.Attempt, state == Preparing
		return nil
	})
	if err != nil {
		return Attempt{}, false, err
	}
	return result, execute, nil
}

func postgresOwns(i postgresInstance, op postgresOperation, a postgresAttempt, b Binding) bool {
	return a.owner == b.OwnerID && op.attempt == b.AttemptID && i.attempt.Valid && i.owner.Valid && i.attempt.String == b.AttemptID && i.owner.String == b.OwnerID
}

func (p *PostgresLedger) MarkSending(ctx context.Context, b Binding) error {
	if !postgresValidBinding(b) {
		return ErrConflict
	}
	return p.transaction(ctx, b.InstanceID, func(tx *sql.Tx) error {
		i, err := postgresLockInstance(ctx, tx, b.InstanceID, false)
		if err != nil {
			return err
		}
		op, err := postgresLockOperation(ctx, tx, b)
		if err != nil {
			return err
		}
		a, err := postgresLockAttempt(ctx, tx, b)
		if err != nil {
			return err
		}
		if a.State != Preparing || op.state != Preparing || !postgresOwns(i, op, a, b) {
			return ErrConflict
		}
		r, err := tx.ExecContext(ctx, `UPDATE queue_v1_attempts SET state='sending',sending_at=clock_timestamp()
 WHERE instance_id=$1 AND attempt_id=$2 AND state='preparing' AND owner_id=$3`, b.InstanceID, b.AttemptID, b.OwnerID)
		if err != nil {
			return postgresError(ctx)
		}
		if n, err := r.RowsAffected(); err != nil || n != 1 {
			return ErrUnavailable
		}
		if _, err := tx.ExecContext(ctx, `UPDATE queue_v1_operations SET state='sending' WHERE instance_id=$1 AND logical_id=$2`, b.InstanceID, b.LogicalMessageID); err != nil {
			return postgresError(ctx)
		}
		return nil
	})
}

func (p *PostgresLedger) Complete(ctx context.Context, b Binding, state string, receipt *Receipt, code string) (Attempt, error) {
	if !postgresValidBinding(b) {
		return Attempt{}, ErrConflict
	}
	if state != Accepted && state != Rejected && state != Uncertain {
		return Attempt{}, ErrConflict
	}
	if state == Accepted {
		if receipt == nil || receipt.MessageID != b.MessageID || receipt.AcceptedAt.IsZero() {
			return Attempt{}, ErrConflict
		}
	} else if receipt != nil {
		return Attempt{}, ErrConflict
	}
	var result Attempt
	err := p.transaction(ctx, b.InstanceID, func(tx *sql.Tx) error {
		i, err := postgresLockInstance(ctx, tx, b.InstanceID, false)
		if err != nil {
			return err
		}
		op, err := postgresLockOperation(ctx, tx, b)
		if err != nil {
			return err
		}
		a, err := postgresLockAttempt(ctx, tx, b)
		if err != nil {
			return err
		}
		if a.owner != b.OwnerID {
			return ErrConflict
		}
		if a.State == Accepted || a.State == Rejected {
			if a.State != state {
				return ErrConflict
			}
			result = a.Attempt
			return nil // Never clear another attempt's newer active barrier.
		}
		if !postgresOwns(i, op, a, b) || op.state != a.State {
			return ErrConflict
		}
		if a.State == state {
			result = a.Attempt // Repeated uncertainty must not overwrite its original code.
			return nil
		}
		if state == Rejected {
			// Only the original synchronous owner can attest that the final
			// transport deadline guard prevented the call after MarkSending.
			if a.State != Preparing && !(a.State == Sending && code == "expired" && b.ValidUntil != "") {
				return ErrConflict
			}
		} else if a.State != Sending && a.State != Uncertain {
			return ErrConflict
		}
		var receiptID any
		var receiptAt any
		if receipt != nil {
			receiptID, receiptAt = receipt.MessageID, receipt.AcceptedAt.UTC()
		}
		updated, err := postgresScan(tx.QueryRowContext(ctx, `UPDATE queue_v1_attempts SET state=$3,code=$4,
 receipt_message_id=$5,receipt_accepted_at=$6,completed_at=CASE WHEN $3='uncertain' THEN NULL ELSE clock_timestamp() END
 WHERE instance_id=$1 AND attempt_id=$2 AND owner_id=$7 RETURNING `+postgresAttemptColumns,
			b.InstanceID, b.AttemptID, state, code, receiptID, receiptAt, b.OwnerID))
		if err != nil {
			return postgresError(ctx)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE queue_v1_operations SET state=$3 WHERE instance_id=$1 AND logical_id=$2 AND current_attempt_id=$4`, b.InstanceID, b.LogicalMessageID, state, b.AttemptID); err != nil {
			return postgresError(ctx)
		}
		if state != Uncertain {
			r, err := tx.ExecContext(ctx, `UPDATE queue_v1_instances SET active_attempt_id=NULL,active_owner_id=NULL
 WHERE instance_id=$1 AND active_attempt_id=$2 AND active_owner_id=$3`, b.InstanceID, b.AttemptID, b.OwnerID)
			if err != nil {
				return postgresError(ctx)
			}
			if n, err := r.RowsAffected(); err != nil || n != 1 {
				return ErrUnavailable
			}
		}
		result = updated.Attempt
		return nil
	})
	if err != nil {
		return Attempt{}, err
	}
	return result, nil
}

func (p *PostgresLedger) Idle(ctx context.Context, instance string) error {
	const query = `SELECT active_attempt_id IS NOT NULL FROM queue_v1_instances WHERE instance_id=$1`
	var busy bool
	var err error
	if p.scope != nil {
		p.scope.Lock()
		defer p.scope.Unlock()
		if p.scope.closed || p.scope.instance != instance {
			return ErrUnavailable
		}
		err = p.scope.conn.QueryRowContext(ctx, query, instance).Scan(&busy)
	} else {
		err = p.db.QueryRowContext(ctx, query, instance).Scan(&busy)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return postgresError(ctx)
	}
	if busy {
		return ErrBusy
	}
	return nil
}

func postgresSessionKey(instance string) int64 {
	sum := sha256.Sum256([]byte("queue-v1/instance/" + instance))
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

func postgresSessionSlot(db *sql.DB) bool {
	limit := 4
	if max := db.Stats().MaxOpenConnections; max > 0 && max-1 < limit {
		limit = max - 1
	}
	postgresSessions.Lock()
	defer postgresSessions.Unlock()
	if postgresSessions.active >= limit {
		return false
	}
	postgresSessions.active++
	return true
}

func postgresPoison(conn *sql.Conn) {
	// database/sql discards the physical driver connection on ErrBadConn.
	// Closing sql.Conn alone could return a still-locked session to the pool.
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
}

func (p *PostgresLedger) WithInstance(ctx context.Context, instance string, fn func(Ledger) error) (result error) {
	if p.scope != nil || instance == "" || fn == nil {
		return ErrConflict
	}
	if err := ctx.Err(); err != nil {
		return postgresError(ctx)
	}
	if !postgresSessionSlot(p.db) {
		return ErrBusy
	}
	defer func() { postgresSessions.Lock(); postgresSessions.active--; postgresSessions.Unlock() }()
	acquireCtx, cancel := context.WithTimeout(ctx, postgresSessionTimeout)
	conn, err := p.db.Conn(acquireCtx)
	cancel()
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			return ErrBusy
		}
		return postgresError(ctx)
	}
	defer conn.Close()
	lockCtx, cancel := context.WithTimeout(ctx, postgresSessionTimeout)
	var locked bool
	err = conn.QueryRowContext(lockCtx, `SELECT pg_try_advisory_lock($1)`, postgresSessionKey(instance)).Scan(&locked)
	cancel()
	if err != nil {
		postgresPoison(conn)
		return postgresError(ctx)
	}
	if !locked {
		return ErrBusy
	}
	scope := &postgresScope{conn: conn, instance: instance}
	defer func() {
		scope.Lock()
		defer scope.Unlock()
		scope.closed = true
		// Cleanup must survive callback cancellation (and panic). A failed or
		// unverified unlock must never release this physical session to a pool.
		unlockCtx, cancel := context.WithTimeout(context.Background(), postgresSessionTimeout)
		defer cancel()
		var unlocked bool
		err := conn.QueryRowContext(unlockCtx, `SELECT pg_advisory_unlock($1)`, postgresSessionKey(instance)).Scan(&unlocked)
		if err != nil || !unlocked {
			postgresPoison(conn)
			result = ErrUnavailable
		}
	}()
	return fn(&PostgresLedger{db: p.db, scope: scope})
}

const postgresVersionDDL = `
CREATE TABLE IF NOT EXISTS queue_v1_schema (
 singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK(singleton),
 version INTEGER NOT NULL CHECK(version > 0)
);
INSERT INTO queue_v1_schema(singleton,version) VALUES(TRUE,1) ON CONFLICT DO NOTHING;
`

const postgresTablesDDL = `
CREATE TABLE IF NOT EXISTS queue_v1_instances (
 instance_id TEXT PRIMARY KEY CHECK(instance_id <> ''),
 active_attempt_id TEXT,
 active_owner_id TEXT,
 CHECK((active_attempt_id IS NULL) = (active_owner_id IS NULL)),
 CHECK(active_attempt_id IS NULL OR active_attempt_id <> ''),
 CHECK(active_owner_id IS NULL OR active_owner_id <> '')
);
CREATE TABLE IF NOT EXISTS queue_v1_operations (
 instance_id TEXT NOT NULL REFERENCES queue_v1_instances(instance_id),
 logical_id TEXT NOT NULL CHECK(logical_id <> ''),
 message_id TEXT NOT NULL CHECK(message_id <> ''),
 content_hash TEXT NOT NULL CHECK(content_hash <> ''),
 state TEXT NOT NULL CHECK(state IN ('preparing','sending','accepted','rejected','uncertain')),
 current_attempt_id TEXT NOT NULL CHECK(current_attempt_id <> ''),
 PRIMARY KEY(instance_id,logical_id),
 UNIQUE(instance_id,message_id),
 UNIQUE(instance_id,logical_id,message_id,content_hash)
);
CREATE TABLE IF NOT EXISTS queue_v1_attempts (
 instance_id TEXT NOT NULL,
 attempt_id TEXT NOT NULL CHECK(attempt_id <> ''),
 logical_id TEXT NOT NULL,
 message_id TEXT NOT NULL,
 content_hash TEXT NOT NULL,
 owner_id TEXT NOT NULL CHECK(owner_id <> ''),
 state TEXT NOT NULL CHECK(state IN ('preparing','sending','accepted','rejected','uncertain')),
 code TEXT NOT NULL DEFAULT '',
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 sending_at TIMESTAMPTZ,
 completed_at TIMESTAMPTZ,
 receipt_message_id TEXT,
 receipt_accepted_at TIMESTAMPTZ,
 PRIMARY KEY(instance_id,attempt_id),
 FOREIGN KEY(instance_id,logical_id,message_id,content_hash)
 REFERENCES queue_v1_operations(instance_id,logical_id,message_id,content_hash),
 CHECK((state IN ('sending','uncertain','accepted')) = (sending_at IS NOT NULL)),
 CHECK((state IN ('accepted','rejected')) = (completed_at IS NOT NULL)),
 CHECK((state = 'accepted') = (receipt_message_id IS NOT NULL)),
 CHECK((state = 'accepted') = (receipt_accepted_at IS NOT NULL)),
 CHECK(receipt_message_id IS NULL OR receipt_message_id = message_id),
 CHECK(receipt_accepted_at IS NULL OR (isfinite(receipt_accepted_at)
  AND receipt_accepted_at <> '0001-01-01 00:00:00+00'::timestamptz))
);
`
