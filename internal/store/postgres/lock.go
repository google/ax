// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/ax/internal/lock"
	"github.com/jackc/pgx/v5"
)

type lockMode string

const (
	lockModeShared    lockMode = "S"
	lockModeExclusive lockMode = "X"
)

// Lock and RLock implement lock.Locker with leases stored in the locks table.
// All lease times are taken from the database clock, so ax-server replicas do
// not need synchronized clocks.
//
// Each acquired lock is a (key, token) row holding its mode ('S' shared or 'X'
// exclusive) and an expiry. Multiple 'S' rows may coexist for the same key as
// long as no unexpired 'X' row exists; acquiring 'X' requires no unexpired rows
// of either mode for that key. Waiters retry every LockRetryInterval.
//
// Session-level advisory locks were not used to hold leases across operations
// because they belong to a database session: a lock held across a long
// Substrate call would pin a pooled connection for that long, and would be
// released silently if that connection dropped. Instead, tryAcquire uses a
// transaction-scoped advisory lock (pg_advisory_xact_lock) only for the
// sub-millisecond check-and-insert transaction so concurrent shared and
// exclusive acquisitions on the same key serialize cleanly under READ COMMITTED.
//
// # Lease renewal
//
// A held lock is kept alive by a background goroutine that extends its lease
// every LockTTL/3 (only while the row still holds this holder's token) until
// the unlock function is called. The TTL therefore does not bound how long a
// lock may be held; it only bounds how long a lock can be orphaned after its
// holder crashes or loses connectivity to Postgres.
//
// We deliberately renew a short lease instead of using a single long TTL:
//
//   - The server does not control how long a critical section runs. Handlers
//     hold the lock across Substrate calls (e.g. snapshot/restore of a microVM
//     during SuspendTask/ResumeTask) that run under the caller's request
//     context, so their duration is set by the client's deadline, not by
//     ax-server. A fixed TTL would have to exceed the longest deadline any
//     client might use, and would silently allow concurrent holders whenever
//     an operation ran past it.
//   - The TTL is the recovery time after a crash. A long TTL (minutes) would
//     wedge the affected task/workspace/model for that long if ax-server died
//     mid-operation; a short renewed lease (30s by default) keeps recovery
//     fast.
//   - One locker guards every resource kind and operation, so renewal avoids
//     having to retune the TTL whenever a slower operation is added.
//
// Limitation: renewal is best-effort and there is no fencing token. If renewals
// keep failing for longer than the TTL (e.g. a prolonged database outage), the
// lease can expire while the holder is still working, and the holder is not
// interrupted; the loss is only logged.
// Lock acquires an exclusive distributed lease for (kind, atespace, name).
func (s *Store) Lock(ctx context.Context, kind, atespace, name string) (func(), error) {
	return s.acquireLock(ctx, kind, atespace, name, lockModeExclusive)
}

// RLock acquires a shared distributed lease for (kind, atespace, name).
func (s *Store) RLock(ctx context.Context, kind, atespace, name string) (func(), error) {
	return s.acquireLock(ctx, kind, atespace, name, lockModeShared)
}

func (s *Store) acquireLock(ctx context.Context, kind, atespace, name string, mode lockMode) (func(), error) {
	key := lock.Key(kind, atespace, name)

	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, fmt.Errorf("generating lock token: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Fast path: the lock is usually available.
	ok, err := s.tryAcquire(ctx, key, token, mode)
	if err != nil {
		return nil, err
	}
	if ok {
		return s.makeUnlock(key, token), nil
	}

	// Slow path: retry until conflicting holders release or their leases expire.
	ticker := time.NewTicker(s.opts.LockRetryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
		ok, err := s.tryAcquire(ctx, key, token, mode)
		if err != nil {
			return nil, err
		}
		if ok {
			return s.makeUnlock(key, token), nil
		}
	}
}

// tryAcquire atomically purges expired leases for key and inserts a new lease
// row if no conflicting lease is currently held.
func (s *Store) tryAcquire(ctx context.Context, key, token string, mode lockMode) (bool, error) {
	var acquired bool
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// 1. Serialize concurrent RLock/Lock attempts on the same key (2-arg int4
		// advisory lock space, released automatically when BeginFunc commits).
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('ax:lock'), hashtext($1))`, key); err != nil {
			return err
		}

		// 2. Reclaim any expired leases left behind by crashed holders.
		if _, err := tx.Exec(ctx, `DELETE FROM locks WHERE key = $1 AND expires_at <= clock_timestamp()`, key); err != nil {
			return err
		}

		// 3. Check for conflicting active leases: exclusive ('X') conflicts with
		// any active row; shared ('S') conflicts only with an active 'X' row.
		conflictSQL := `SELECT EXISTS (SELECT 1 FROM locks WHERE key = $1)`
		if mode == lockModeShared {
			conflictSQL = `SELECT EXISTS (SELECT 1 FROM locks WHERE key = $1 AND mode = 'X')`
		}
		var blocked bool
		if err := tx.QueryRow(ctx, conflictSQL, key).Scan(&blocked); err != nil {
			return err
		}
		if blocked {
			return nil
		}

		// 4. No conflict: record this holder's lease row.
		if _, err := tx.Exec(ctx, `
			INSERT INTO locks (key, token, mode, expires_at)
			VALUES ($1, $2, $3, clock_timestamp() + $4 * interval '1 millisecond')`,
			key, token, string(mode), s.ttlMillis()); err != nil {
			return err
		}
		acquired = true
		return nil
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, ctxErr
		}
		return false, fmt.Errorf("acquiring lock %s (%s): %w", key, mode, err)
	}
	return acquired, nil
}

func (s *Store) ttlMillis() int64 {
	return max(s.opts.LockTTL.Milliseconds(), 1)
}

// makeUnlock starts lease renewal for an acquired lock and returns an
// idempotent function that stops renewal and releases the lock.
func (s *Store) makeUnlock(key, token string) func() {
	renewCtx, cancelRenew := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.renew(renewCtx, key, token)
	}()

	var once sync.Once
	return func() {
		once.Do(func() {
			cancelRenew()
			<-done
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := s.pool.Exec(ctx, `DELETE FROM locks WHERE key = $1 AND token = $2`, key, token)
			if err != nil {
				// The lease will expire on its own after LockTTL.
				slog.Warn("releasing lock", "key", key, "error", err)
			}
		})
	}
}

// renew periodically extends the lease of the lock identified by key/token
// until ctx is canceled or the lock is found to be no longer held by token.
func (s *Store) renew(ctx context.Context, key, token string) {
	interval := max(s.opts.LockTTL/3, time.Millisecond)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		renewCtx, cancel := context.WithTimeout(ctx, interval)
		tag, err := s.pool.Exec(renewCtx, `
			UPDATE locks SET expires_at = clock_timestamp() + $3 * interval '1 millisecond'
			WHERE key = $1 AND token = $2`,
			key, token, s.ttlMillis())
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			// Transient error: keep trying on the next tick while the
			// remaining lease still covers us.
			slog.Warn("renewing lock lease", "key", key, "error", err)
			continue
		}
		if tag.RowsAffected() == 0 {
			// The lease expired and was taken over; there is nothing left to renew.
			slog.Error("lock lease lost before release", "key", key)
			return
		}
	}
}
