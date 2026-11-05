package queuepolicy

import (
	"context"
	"database/sql"
	"errors"

	"github.com/google/uuid"
)

var (
	ErrInvalidInstance  = errors.New("invalid instance UUID")
	ErrInstanceNotFound = errors.New("instance not found")
)

const exclusivePolicyLock = `SELECT pg_advisory_xact_lock(719628120025100002)`
const exclusiveTableDDL = `CREATE TABLE IF NOT EXISTS queue_v1_exclusive_instances (
 instance_id UUID PRIMARY KEY CHECK(instance_id <> '00000000-0000-0000-0000-000000000000'::uuid),
 activated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
)`

// Activate adds an existing instance from this server's DB. It neither connects
// a client nor modifies instance settings, tokens or sessions. A successful return
// confirms COMMIT, not gate installation: the HTTP caller must consult the gate.
func Activate(ctx context.Context, db *sql.DB, instance string) error {
	u, err := uuid.Parse(instance)
	if err != nil || u == uuid.Nil || u.String() != instance {
		return ErrInvalidInstance
	}
	return persistAdhesions(ctx, db, []string{instance}, true)
}

// Config and admin provisioning share one transactional lock and INSERT-only
// policy. Config keeps its existing behavior for explicit/pre-provisioned IDs;
// the admin endpoint must validate and hold the actual instance row until commit.
func persistAdhesions(ctx context.Context, db *sql.DB, ids []string, requireExisting bool) error {
	if db == nil {
		return ErrUnavailable
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return ErrUnavailable
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, exclusivePolicyLock); err != nil {
		return ErrUnavailable
	}
	if requireExisting {
		for _, id := range ids {
			var stored string
			err := tx.QueryRowContext(ctx, `SELECT id FROM instances WHERE id=$1 FOR KEY SHARE`, id).Scan(&stored)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrInstanceNotFound
			}
			if err != nil || stored != id {
				return ErrUnavailable
			}
		}
	}
	if _, err = tx.ExecContext(ctx, exclusiveTableDDL); err != nil {
		return ErrUnavailable
	}
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, `INSERT INTO queue_v1_exclusive_instances(instance_id) VALUES($1) ON CONFLICT DO NOTHING`, id); err != nil {
			return ErrUnavailable
		}
	}
	if err = tx.Commit(); err != nil {
		// A lost COMMIT response may have persisted the sticky policy, but never
		// authorizes a positive HTTP claim. Retrying this addition is idempotent.
		return ErrUnavailable
	}
	return nil
}
