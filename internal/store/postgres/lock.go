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
)

// Lock implements lock.Locker with leases stored in the locks table. All
// lease times are taken from the database clock, so ax-server replicas do not
// need synchronized clocks.
//
// A lock is a row holding a random token and an expiry. Acquiring inserts the
// row, or takes it over if its lease has expired; releasing deletes it (only
// if it still holds the caller's token). Waiters retry every
// LockRetryInterval.
//
// Advisory locks were not used because they belong to a database session: a
// lock held across a long Substrate call would pin a pooled connection for
// that long, and would be released silently if that connection dropped.
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
func (s *Store) Lock(ctx context.Context, kind, atespace, name string) (func(), error) {
	key := lock.Key(kind, atespace, name)

	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, fmt.Errorf("generating lock token: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Fast path: the lock is usually free.
	ok, err := s.tryLock(ctx, key, token)
	if err != nil {
		return nil, err
	}
	if ok {
		return s.makeUnlock(key, token), nil
	}

	// Slow path: retry until the holder releases the lock or its lease
	// expires.
	ticker := time.NewTicker(s.opts.LockRetryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
		ok, err := s.tryLock(ctx, key, token)
		if err != nil {
			return nil, err
		}
		if ok {
			return s.makeUnlock(key, token), nil
		}
	}
}

// tryLock attempts to take the lease for key, succeeding if no lease exists or
// the existing one has expired.
func (s *Store) tryLock(ctx context.Context, key, token string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO locks (key, token, expires_at)
		VALUES ($1, $2, clock_timestamp() + $3 * interval '1 millisecond')
		ON CONFLICT (key) DO UPDATE
		SET token = EXCLUDED.token, expires_at = EXCLUDED.expires_at
		WHERE locks.expires_at <= clock_timestamp()`,
		key, token, s.ttlMillis())
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, ctxErr
		}
		return false, fmt.Errorf("acquiring lock %s: %w", key, err)
	}
	return tag.RowsAffected() == 1, nil
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
