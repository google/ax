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

// Package sqlite implements store.Store on a single SQLite file. It backs the
// local development mode of the ax CLI, where every command opens the
// database, does its work and exits, so the store is safe to share between
// concurrent processes: writes are serialized by SQLite and WatchTask polls
// the database rather than relying on in-process notifications.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/ax/internal/store"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" driver
)

const (
	kindTask      = "Task"
	kindWorkspace = "Workspace"
	kindModel     = "Model"

	// DefaultWatchInterval is how often WatchTask polls for changes made by
	// other processes.
	DefaultWatchInterval = 250 * time.Millisecond
)

var (
	marshalOpts   = protojson.MarshalOptions{}
	unmarshalOpts = protojson.UnmarshalOptions{DiscardUnknown: true}
)

const schema = `
CREATE TABLE IF NOT EXISTS resources (
	kind     TEXT    NOT NULL,
	atespace TEXT    NOT NULL,
	name     TEXT    NOT NULL,
	data     TEXT    NOT NULL,
	saved_at INTEGER NOT NULL,
	version  INTEGER NOT NULL DEFAULT 1,
	PRIMARY KEY (kind, atespace, name)
);
CREATE INDEX IF NOT EXISTS resources_by_kind_saved ON resources (kind, saved_at DESC);
`

// Options configures a Store.
type Options struct {
	// WatchInterval is how often WatchTask polls the database. Zero means
	// DefaultWatchInterval.
	WatchInterval time.Duration
}

// Store is a store.Store backed by a SQLite database file.
type Store struct {
	db            *sql.DB
	watchInterval time.Duration

	closeOnce sync.Once
	done      chan struct{}
}

var _ store.Store = (*Store)(nil)

// Open opens (creating if necessary) the SQLite database at path. The parent
// directory is created with owner-only permissions.
func Open(path string, opts ...Options) (*Store, error) {
	var opt Options
	if len(opts) > 0 {
		opt = opts[0]
	}
	if opt.WatchInterval <= 0 {
		opt.WatchInterval = DefaultWatchInterval
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating database directory: %w", err)
	}

	// WAL lets readers (for example a long-running `ax watch`) proceed while
	// another process writes; busy_timeout makes concurrent writers wait for
	// each other instead of failing; immediate transactions take the write
	// lock up front so read-modify-write updates cannot deadlock.
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(10000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_txlock", "immediate")
	dsn := "file:" + path + "?" + q.Encode()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	// One connection per process keeps SQLite's locking simple; the CLI does
	// not issue enough concurrent queries for this to matter.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initializing schema in %s: %w", path, err)
	}
	// The database may hold model credentials references and task env; keep
	// it private to the user. Errors are ignored for exotic filesystems.
	_ = os.Chmod(path, 0o600)

	return &Store{
		db:            db,
		watchInterval: opt.WatchInterval,
		done:          make(chan struct{}),
	}, nil
}

func defaultAtespace(atespace string) string {
	if atespace == "" {
		return "default"
	}
	return atespace
}

// put upserts a resource. saved_at is refreshed so listings show the most
// recently applied resources first, matching the Redis store.
func (s *Store) put(ctx context.Context, kind, atespace, name string, m proto.Message) error {
	data, err := marshalOpts.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshaling %s: %w", kind, err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO resources (kind, atespace, name, data, saved_at, version)
		VALUES (?, ?, ?, ?, ?, 1)
		ON CONFLICT (kind, atespace, name) DO UPDATE SET
			data = excluded.data,
			saved_at = excluded.saved_at,
			version = resources.version + 1`,
		kind, atespace, name, string(data), time.Now().UnixNano())
	if err != nil {
		return fmt.Errorf("saving %s %s/%s: %w", kind, atespace, name, err)
	}
	return nil
}

func (s *Store) get(ctx context.Context, kind, atespace, name string, m proto.Message) (version int64, err error) {
	var data string
	err = s.db.QueryRowContext(ctx,
		`SELECT data, version FROM resources WHERE kind = ? AND atespace = ? AND name = ?`,
		kind, defaultAtespace(atespace), name).Scan(&data, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, store.ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("getting %s %s/%s: %w", kind, atespace, name, err)
	}
	if err := unmarshalOpts.Unmarshal([]byte(data), m); err != nil {
		return 0, fmt.Errorf("unmarshaling %s %s/%s: %w", kind, atespace, name, err)
	}
	return version, nil
}

func (s *Store) del(ctx context.Context, kind, atespace, name string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM resources WHERE kind = ? AND atespace = ? AND name = ?`,
		kind, defaultAtespace(atespace), name)
	if err != nil {
		return fmt.Errorf("deleting %s %s/%s: %w", kind, atespace, name, err)
	}
	return nil
}

// list returns the raw JSON of resources of a kind, newest first. An empty or
// "*" atespace lists across all atespaces. limit <= 0 means no limit.
func (s *Store) list(ctx context.Context, kind, atespace string, limit, offset int64) ([]string, error) {
	query := `SELECT data FROM resources WHERE kind = ?`
	args := []any{kind}
	if atespace != "" && atespace != "*" {
		query += ` AND atespace = ?`
		args = append(args, atespace)
	}
	query += ` ORDER BY saved_at DESC, atespace, name`
	if limit > 0 {
		query += ` LIMIT ? OFFSET ?`
		args = append(args, limit, max(offset, 0))
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", kind, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, fmt.Errorf("listing %s: %w", kind, err)
		}
		out = append(out, data)
	}
	return out, rows.Err()
}

// --- Tasks ---

func (s *Store) SaveTask(ctx context.Context, task *v1alpha1.Task) error {
	if task.Metadata == nil {
		task.Metadata = &v1alpha1.ObjectMeta{}
	}
	if task.Metadata.Name == "" {
		return errors.New("task name is required")
	}
	task.Metadata.Atespace = defaultAtespace(task.Metadata.Atespace)
	if task.ApiVersion == "" {
		task.ApiVersion = v1alpha1.APIVersion
	}
	if task.Kind == "" {
		task.Kind = v1alpha1.KindTask
	}
	if task.Status == nil {
		task.Status = &v1alpha1.TaskStatus{}
	}
	if task.Status.Phase == "" {
		task.Status.Phase = "Pending"
	}
	return s.put(ctx, kindTask, task.Metadata.Atespace, task.Metadata.Name, task)
}

func (s *Store) GetTask(ctx context.Context, atespace, name string) (*v1alpha1.Task, error) {
	var t v1alpha1.Task
	if _, err := s.get(ctx, kindTask, atespace, name, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *Store) ListTasks(ctx context.Context, atespace string, limit, offset int64) ([]*v1alpha1.Task, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.list(ctx, kindTask, atespace, limit, offset)
	if err != nil {
		return nil, err
	}
	out := make([]*v1alpha1.Task, 0, len(rows))
	for _, data := range rows {
		var t v1alpha1.Task
		if err := unmarshalOpts.Unmarshal([]byte(data), &t); err == nil {
			out = append(out, &t)
		}
	}
	return out, nil
}

// UpdateTaskStatus replaces the status of a task without touching its spec or
// its position in listings. The read and write happen in one transaction so
// a concurrent writer in another process cannot be lost.
func (s *Store) UpdateTaskStatus(ctx context.Context, atespace, name string, status *v1alpha1.TaskStatus) error {
	atespace = defaultAtespace(atespace)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("updating task status: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var data string
	err = tx.QueryRowContext(ctx,
		`SELECT data FROM resources WHERE kind = ? AND atespace = ? AND name = ?`,
		kindTask, atespace, name).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("updating task status: %w", err)
	}
	var t v1alpha1.Task
	if err := unmarshalOpts.Unmarshal([]byte(data), &t); err != nil {
		return fmt.Errorf("unmarshaling task %s/%s: %w", atespace, name, err)
	}
	t.Status = status
	updated, err := marshalOpts.Marshal(&t)
	if err != nil {
		return fmt.Errorf("marshaling task %s/%s: %w", atespace, name, err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE resources SET data = ?, version = version + 1 WHERE kind = ? AND atespace = ? AND name = ?`,
		string(updated), kindTask, atespace, name); err != nil {
		return fmt.Errorf("updating task status: %w", err)
	}
	return tx.Commit()
}

func (s *Store) DeleteTask(ctx context.Context, atespace, name string) error {
	return s.del(ctx, kindTask, atespace, name)
}

// WatchTask emits the task every time it changes, whichever process changed
// it. Changes are detected by polling the row's version, so rapid successive
// writes may be coalesced into a single event carrying the latest state.
func (s *Store) WatchTask(ctx context.Context, atespace, name string) (<-chan *v1alpha1.Task, io.Closer, error) {
	ch := make(chan *v1alpha1.Task, 10)
	stop := make(chan struct{})
	var stopOnce sync.Once
	closer := closerFunc(func() error {
		stopOnce.Do(func() { close(stop) })
		return nil
	})

	// Record the current version so only subsequent changes are reported;
	// callers fetch the initial state themselves.
	var last int64
	var t v1alpha1.Task
	if v, err := s.get(ctx, kindTask, atespace, name, &t); err == nil {
		last = v
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, nil, err
	}

	go func() {
		defer close(ch)
		ticker := time.NewTicker(s.watchInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-s.done:
				return
			case <-ticker.C:
			}
			var cur v1alpha1.Task
			v, err := s.get(ctx, kindTask, atespace, name, &cur)
			if err != nil || v == last {
				continue
			}
			last = v
			select {
			case ch <- &cur:
			case <-ctx.Done():
				return
			case <-stop:
				return
			}
		}
	}()
	return ch, closer, nil
}

// --- Workspaces ---

func (s *Store) SaveWorkspace(ctx context.Context, ws *v1alpha1.Workspace) error {
	if ws.Metadata == nil || ws.Metadata.Name == "" {
		return errors.New("workspace name is required")
	}
	ws.Metadata.Atespace = defaultAtespace(ws.Metadata.Atespace)
	return s.put(ctx, kindWorkspace, ws.Metadata.Atespace, ws.Metadata.Name, ws)
}

func (s *Store) GetWorkspace(ctx context.Context, atespace, name string) (*v1alpha1.Workspace, error) {
	var ws v1alpha1.Workspace
	if _, err := s.get(ctx, kindWorkspace, atespace, name, &ws); err != nil {
		return nil, err
	}
	return &ws, nil
}

func (s *Store) ListWorkspaces(ctx context.Context, atespace string) ([]*v1alpha1.Workspace, error) {
	rows, err := s.list(ctx, kindWorkspace, atespace, 0, 0)
	if err != nil {
		return nil, err
	}
	out := make([]*v1alpha1.Workspace, 0, len(rows))
	for _, data := range rows {
		var ws v1alpha1.Workspace
		if err := unmarshalOpts.Unmarshal([]byte(data), &ws); err == nil {
			out = append(out, &ws)
		}
	}
	return out, nil
}

func (s *Store) DeleteWorkspace(ctx context.Context, atespace, name string) error {
	return s.del(ctx, kindWorkspace, atespace, name)
}

// --- Models ---

func (s *Store) SaveModel(ctx context.Context, m *v1alpha1.Model) error {
	if m.Metadata == nil || m.Metadata.Name == "" {
		return errors.New("model name is required")
	}
	m.Metadata.Atespace = defaultAtespace(m.Metadata.Atespace)
	return s.put(ctx, kindModel, m.Metadata.Atespace, m.Metadata.Name, m)
}

func (s *Store) GetModel(ctx context.Context, atespace, name string) (*v1alpha1.Model, error) {
	var m v1alpha1.Model
	if _, err := s.get(ctx, kindModel, atespace, name, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *Store) ListModels(ctx context.Context, atespace string) ([]*v1alpha1.Model, error) {
	rows, err := s.list(ctx, kindModel, atespace, 0, 0)
	if err != nil {
		return nil, err
	}
	out := make([]*v1alpha1.Model, 0, len(rows))
	for _, data := range rows {
		var m v1alpha1.Model
		if err := unmarshalOpts.Unmarshal([]byte(data), &m); err == nil {
			out = append(out, &m)
		}
	}
	return out, nil
}

func (s *Store) DeleteModel(ctx context.Context, atespace, name string) error {
	return s.del(ctx, kindModel, atespace, name)
}

// Close stops all watchers and closes the database.
func (s *Store) Close() error {
	var err error
	s.closeOnce.Do(func() {
		close(s.done)
		err = s.db.Close()
	})
	return err
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }
