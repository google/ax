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

package lock

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

var (
	ErrLockFailed = errors.New("failed to acquire lock")
)

// Locker provides exclusive locking per resource kind, atespace, and name.
type Locker interface {
	// Lock acquires an exclusive lock for the specified resource.
	// It blocks until the lock is acquired or ctx is cancelled.
	// The returned function releases the lock.
	Lock(ctx context.Context, kind, atespace, name string) (unlock func(), err error)
}

// Key formats the standard lock key.
func Key(kind, atespace, name string) string {
	if atespace == "" {
		atespace = "default"
	}
	return fmt.Sprintf("lock:%s:%s:%s", kind, atespace, name)
}

// MemoryLocker is an in-memory keyed mutex implementation suitable for tests and local mode.
type MemoryLocker struct {
	mu    sync.Mutex
	locks map[string]*entry
}

type entry struct {
	mu  sync.Mutex
	ref int
}

// NewMemoryLocker creates an in-memory Locker.
func NewMemoryLocker() *MemoryLocker {
	return &MemoryLocker{
		locks: make(map[string]*entry),
	}
}

func (m *MemoryLocker) Lock(ctx context.Context, kind, atespace, name string) (func(), error) {
	key := Key(kind, atespace, name)

	m.mu.Lock()
	e, ok := m.locks[key]
	if !ok {
		e = &entry{}
		m.locks[key] = e
	}
	e.ref++
	m.mu.Unlock()

	// Acquire lock with ctx awareness
	locked := make(chan struct{})
	go func() {
		e.mu.Lock()
		close(locked)
	}()

	select {
	case <-ctx.Done():
		// Clean up ref if timed out waiting
		go func() {
			<-locked
			e.mu.Unlock()
			m.mu.Lock()
			e.ref--
			if e.ref == 0 {
				delete(m.locks, key)
			}
			m.mu.Unlock()
		}()
		return nil, ctx.Err()
	case <-locked:
	}

	var once sync.Once
	unlock := func() {
		once.Do(func() {
			e.mu.Unlock()
			m.mu.Lock()
			e.ref--
			if e.ref == 0 {
				delete(m.locks, key)
			}
			m.mu.Unlock()
		})
	}
	return unlock, nil
}

// RedisLocker implements distributed locking in Redis using SET NX PX with token validation
// and Redis Pub/Sub notification for instant contended lock hand-off.
//
// # Lease renewal
//
// A held lock is kept alive by a background goroutine that extends its TTL
// every TTL/3 (only while the key still holds this holder's token) until the
// unlock function is called. The TTL therefore does not bound how long a lock
// may be held; it only bounds how long a lock can be orphaned after its holder
// crashes or loses connectivity to Redis.
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
// keep failing for longer than the TTL (e.g. a prolonged Redis outage), the
// lease can expire while the holder is still working, and the holder is not
// interrupted; the loss is only logged. The alternative considered was a long
// TTL combined with a server-side deadline shorter than it on every locked
// operation, which avoids the goroutine but trades away fast crash recovery.
type RedisLocker struct {
	client           *redis.Client
	ttl              time.Duration
	fallbackInterval time.Duration
}

type RedisLockerOptions struct {
	// TTL is the lease duration of a lock key in Redis (default 30s). Held
	// locks are renewed every TTL/3, so this does not limit how long a lock
	// may be held; it bounds how long a lock survives a crashed holder. See
	// the RedisLocker doc for why a short renewed lease is preferred over a
	// long TTL.
	TTL time.Duration
	// FallbackInterval is how often waiters retry acquisition in case a
	// release notification is missed or a lease expires without a publish
	// (default 1s).
	FallbackInterval time.Duration
}

var releaseAndNotifyScript = redis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
    local res = redis.call("del", KEYS[1])
    redis.call("publish", KEYS[2], "1")
    return res
else
    return 0
end
`)

// renewScript extends the lock TTL only if it is still held by the caller's token.
var renewScript = redis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
    return redis.call("pexpire", KEYS[1], ARGV[2])
else
    return 0
end
`)

// ChannelKey returns the Pub/Sub channel used to notify waiters when a lock is released.
func ChannelKey(kind, atespace, name string) string {
	if atespace == "" {
		atespace = "default"
	}
	return fmt.Sprintf("lock:chan:%s:%s:%s", kind, atespace, name)
}

// NewRedisLocker creates a distributed Redis locker with Pub/Sub notification.
func NewRedisLocker(client *redis.Client, opts RedisLockerOptions) *RedisLocker {
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	fallback := opts.FallbackInterval
	if fallback <= 0 {
		fallback = 1 * time.Second
	}
	return &RedisLocker{
		client:           client,
		ttl:              ttl,
		fallbackInterval: fallback,
	}
}

func (r *RedisLocker) Lock(ctx context.Context, kind, atespace, name string) (func(), error) {
	key := Key(kind, atespace, name)
	chanKey := ChannelKey(kind, atespace, name)

	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, fmt.Errorf("generating lock token: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)

	// makeUnlock is called once the lock is acquired. It starts the lease
	// renewal goroutine (see "Lease renewal" on RedisLocker) and returns an
	// unlock function that stops renewal before releasing the key.
	makeUnlock := func() func() {
		stop := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			r.renew(key, token, stop)
		}()

		var once sync.Once
		return func() {
			once.Do(func() {
				close(stop)
				<-done
				releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = releaseAndNotifyScript.Run(releaseCtx, r.client, []string{key, chanKey}, token).Err()
			})
		}
	}

	// 1. Fast Path: Try acquiring immediately without subscribing to Pub/Sub
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	ok, err := r.client.SetNX(ctx, key, token, r.ttl).Result()
	if err != nil && !errors.Is(err, context.Canceled) {
		return nil, fmt.Errorf("acquiring lock %s: %w", key, err)
	}
	if ok {
		return makeUnlock(), nil
	}

	// 2. Slow Path: Subscribe to Pub/Sub channel for instant wakeup when released
	pubsub := r.client.Subscribe(ctx, chanKey)
	defer pubsub.Close()

	// Try acquiring again immediately in case it was released before subscribe completed
	ok, err = r.client.SetNX(ctx, key, token, r.ttl).Result()
	if err != nil && !errors.Is(err, context.Canceled) {
		return nil, fmt.Errorf("acquiring lock %s: %w", key, err)
	}
	if ok {
		return makeUnlock(), nil
	}

	msgCh := pubsub.Channel()
	ticker := time.NewTicker(r.fallbackInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-msgCh:
			// Woken up instantly by lock release notification
		case <-ticker.C:
			// Fallback ticker in case of missed notification or TTL expiration without publish
		}

		ok, err := r.client.SetNX(ctx, key, token, r.ttl).Result()
		if err != nil && !errors.Is(err, context.Canceled) {
			return nil, fmt.Errorf("acquiring lock %s: %w", key, err)
		}
		if ok {
			return makeUnlock(), nil
		}
	}
}

// renew periodically extends the TTL of the lock identified by key/token until
// stop is closed or the lock is found to be no longer held by token.
func (r *RedisLocker) renew(key, token string, stop <-chan struct{}) {
	interval := r.ttl / 3
	if interval <= 0 {
		interval = time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	ttlMillis := r.ttl.Milliseconds()
	if ttlMillis <= 0 {
		ttlMillis = 1
	}

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}

		renewCtx, cancel := context.WithTimeout(context.Background(), interval)
		res, err := renewScript.Run(renewCtx, r.client, []string{key}, token, ttlMillis).Int()
		cancel()
		if err != nil {
			// Transient Redis error: keep trying on the next tick while the
			// remaining TTL still covers us.
			slog.Warn("renewing lock lease", "key", key, "error", err)
			continue
		}
		if res == 0 {
			// The lock expired or was taken over; there is nothing left to renew.
			slog.Error("lock lease lost before release", "key", key)
			return
		}
	}
}
