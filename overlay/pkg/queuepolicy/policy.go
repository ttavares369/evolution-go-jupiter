// Package queuepolicy gates application emissions, never Whatsmeow protocol retries.
package queuepolicy

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

var (
	ErrExclusive   = errors.New("instance requires queue-v1")
	ErrUnavailable = errors.New("queue instance policy unavailable")
)

type Checker func(context.Context, string) (bool, error)

var gate struct {
	sync.RWMutex
	check Checker
}

// Install must run before any runtimes or HTTP routes start. A nil checker is
// fail-closed, not a way to disable the gate. There is no client-selected bypass.
func Install(check Checker) {
	if check == nil {
		check = func(context.Context, string) (bool, error) { return false, ErrUnavailable }
	}
	gate.Lock()
	gate.check = check
	gate.Unlock()
}

// Installed distinguishes a real runtime installation from standalone routes.
// Availability is still checked through Exclusive: a failed installation closes it.
func Installed() bool {
	gate.RLock()
	defer gate.RUnlock()
	return gate.check != nil
}

// Exclusive reports true only for an installed, functioning, persisted gate.
func Exclusive(ctx context.Context, instance string) (bool, error) {
	gate.RLock()
	check := gate.check
	gate.RUnlock()
	if check == nil {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return check(ctx, instance)
}

func Legacy(instance string) error {
	// Production setup always installs a checker before constructing services.
	// A forgotten installation must not become an application-emission bypass.
	gate.RLock()
	installed := gate.check != nil
	gate.RUnlock()
	if !installed {
		return ErrUnavailable
	}
	exclusive, err := Exclusive(context.Background(), instance)
	if err != nil {
		return ErrUnavailable
	}
	if exclusive {
		return ErrExclusive
	}
	return nil
}

// Initialize persists only additions. Removing an ID from configuration, disabling
// queue-v1, or restarting cannot silently reopen legacy application emissions.
// Without configuration and without an existing table it performs no DDL.
func Initialize(ctx context.Context, db *sql.DB, allowlist string) (Checker, error) {
	ids := []string{}
	if strings.TrimSpace(allowlist) != "" {
		for _, entry := range strings.Split(allowlist, ",") {
			id := strings.TrimSpace(entry)
			u, err := uuid.Parse(id)
			if err != nil || u == uuid.Nil || u.String() != id {
				return nil, ErrUnavailable
			}
			ids = append(ids, id)
		}
	}
	if db == nil {
		return nil, ErrUnavailable
	}
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('queue_v1_exclusive_instances') IS NOT NULL`).Scan(&exists); err != nil {
		return nil, ErrUnavailable
	}
	if len(ids) > 0 {
		if err := persistAdhesions(ctx, db, ids, false); err != nil {
			return nil, err
		}
	}
	var observed atomic.Bool
	observed.Store(exists || len(ids) > 0)
	return func(ctx context.Context, id string) (bool, error) {
		// Recheck existence rather than caching a negative: another producer may
		// activate the policy later. A dropped table after initialization fails
		// closed, including processes started with an empty allowlist.
		var present bool
		if err := db.QueryRowContext(ctx, `SELECT to_regclass('queue_v1_exclusive_instances') IS NOT NULL`).Scan(&present); err != nil {
			return false, ErrUnavailable
		}
		if !present {
			if observed.Load() {
				return false, ErrUnavailable
			}
			return false, nil
		}
		observed.Store(true)
		var exclusive bool
		if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM queue_v1_exclusive_instances WHERE instance_id::text=$1)`, id).Scan(&exclusive); err != nil {
			return false, ErrUnavailable
		}
		return exclusive, nil
	}, nil
}
