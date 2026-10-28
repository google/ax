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

// Package postgres implements store.Store and lock.Locker on PostgreSQL.
//
// Open takes a libpq-style connection URL; standard libpq environment
// variables such as PGPASSWORD fill in what it leaves out.
// Open creates any missing tables and indexes.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/ax/internal/lock"
	"github.com/google/ax/internal/store"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

var unmarshalOpts = protojson.UnmarshalOptions{DiscardUnknown: true}

// Options configures the Postgres backend.
type Options struct {
	// LockTTL is the lease duration of a lock (default 30s). Held locks are
	// renewed every LockTTL/3, so this does not limit how long a lock may be
	// held; it bounds how long a lock survives a crashed holder. See Lock.
	LockTTL time.Duration
	// LockRetryInterval is how often lock waiters retry acquisition
	// (default 1s).
	LockRetryInterval time.Duration
}

// Store is a PostgreSQL implementation of store.Store. It is also a
// lock.Locker (see Lock) whose locks are shared by every ax-server replica
// using the same database.
type Store struct {
	pool *pgxpool.Pool
	opts Options
}

var (
	_ store.Store = (*Store)(nil)
	_ lock.Locker = (*Store)(nil)
)

// Open connects to the database at connURL and creates any missing tables.
func Open(ctx context.Context, connURL string, opts Options) (*Store, error) {
	if opts.LockTTL <= 0 {
		opts.LockTTL = 30 * time.Second
	}
	if opts.LockRetryInterval <= 0 {
		opts.LockRetryInterval = time.Second
	}

	cfg, err := pgxpool.ParseConfig(connURL)
	if err != nil {
		return nil, fmt.Errorf("parsing postgres URL: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}
	if err := createSchema(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool, opts: opts}, nil
}

// Close closes all connections.
func (s *Store) Close() error {
	s.pool.Close()
	return nil
}

func defaultAtespace(atespace string) string {
	if atespace == "" {
		return "default"
	}
	return atespace
}

// table describes how a resource kind is stored. Table names are constants,
// never user input.
type table string

const (
	tasksTable      table = "tasks"
	workspacesTable table = "workspaces"
	modelsTable     table = "models"
)

// save upserts a resource.
func (s *Store) save(ctx context.Context, t table, meta *v1alpha1.ObjectMeta, m proto.Message) error {
	data, err := protojson.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshaling %s: %w", t, err)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO `+string(t)+` (atespace, name, data, saved_at)
		VALUES ($1, $2, $3, clock_timestamp())
		ON CONFLICT (atespace, name) DO UPDATE
		SET data = EXCLUDED.data, saved_at = EXCLUDED.saved_at`,
		meta.Atespace, meta.Name, string(data))
	if err != nil {
		return fmt.Errorf("saving %s: %w", t, err)
	}
	return nil
}

// get loads a single resource into m.
func (s *Store) get(ctx context.Context, t table, atespace, name string, m proto.Message) error {
	var data []byte
	err := s.pool.QueryRow(ctx, `SELECT data FROM `+string(t)+` WHERE atespace = $1 AND name = $2`,
		defaultAtespace(atespace), name).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("getting %s: %w", t, err)
	}
	if err := unmarshalOpts.Unmarshal(data, m); err != nil {
		return fmt.Errorf("unmarshaling %s: %w", t, err)
	}
	return nil
}

// list returns resources of one kind, most recently saved first, in atespace
// or across all atespaces when atespace is "" or "*". A limit <= 0 means no
// limit. Records that no longer parse are skipped and logged.
func list[T proto.Message](ctx context.Context, s *Store, t table, atespace string, limit, offset int64, newT func() T) ([]T, error) {
	query := `SELECT data FROM ` + string(t)
	args := []any{}
	if atespace != "" && atespace != "*" {
		query += ` WHERE atespace = $1`
		args = append(args, atespace)
	}
	query += ` ORDER BY saved_at DESC, atespace, name`
	if limit > 0 {
		args = append(args, limit)
		query += fmt.Sprintf(` LIMIT $%d`, len(args))
	}
	if offset > 0 {
		args = append(args, offset)
		query += fmt.Sprintf(` OFFSET $%d`, len(args))
	}

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", t, err)
	}
	datas, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", t, err)
	}
	out := make([]T, 0, len(datas))
	for _, data := range datas {
		m := newT()
		if err := unmarshalOpts.Unmarshal(data, m); err != nil {
			slog.Warn("skipping unreadable record", "table", t, "error", err)
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

func (s *Store) delete(ctx context.Context, t table, atespace, name string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM `+string(t)+` WHERE atespace = $1 AND name = $2`,
		defaultAtespace(atespace), name); err != nil {
		return fmt.Errorf("deleting %s: %w", t, err)
	}
	return nil
}

// --- Tasks ---

// SaveTask stores or replaces a task.
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
		task.Status.Phase = v1alpha1.PhasePending
	}
	return s.save(ctx, tasksTable, task.Metadata, task)
}

// GetTask retrieves a task by atespace and name.
func (s *Store) GetTask(ctx context.Context, atespace, name string) (*v1alpha1.Task, error) {
	var task v1alpha1.Task
	if err := s.get(ctx, tasksTable, atespace, name, &task); err != nil {
		return nil, err
	}
	return &task, nil
}

// ListTasks lists tasks ordered by most recently saved first. A limit <= 0
// defaults to 50.
func (s *Store) ListTasks(ctx context.Context, atespace string, limit, offset int64) ([]*v1alpha1.Task, error) {
	if limit <= 0 {
		limit = 50
	}
	return list(ctx, s, tasksTable, atespace, limit, offset, func() *v1alpha1.Task { return &v1alpha1.Task{} })
}

// UpdateTaskStatus replaces only the status of a task. It does not change the
// task's position in lists.
func (s *Store) UpdateTaskStatus(ctx context.Context, atespace, name string, status *v1alpha1.TaskStatus) error {
	atespace = defaultAtespace(atespace)
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var data []byte
		err := tx.QueryRow(ctx, `SELECT data FROM tasks WHERE atespace = $1 AND name = $2 FOR UPDATE`,
			atespace, name).Scan(&data)
		if errors.Is(err, pgx.ErrNoRows) {
			return store.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("getting task: %w", err)
		}
		var task v1alpha1.Task
		if err := unmarshalOpts.Unmarshal(data, &task); err != nil {
			return fmt.Errorf("unmarshaling task: %w", err)
		}
		task.Status = status
		if data, err = protojson.Marshal(&task); err != nil {
			return fmt.Errorf("marshaling task status: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE tasks SET data = $3 WHERE atespace = $1 AND name = $2`,
			atespace, name, string(data)); err != nil {
			return fmt.Errorf("updating task status: %w", err)
		}
		return nil
	})
}

// DeleteTask removes a task. Deleting a missing task is not an error.
func (s *Store) DeleteTask(ctx context.Context, atespace, name string) error {
	return s.delete(ctx, tasksTable, atespace, name)
}

// --- Workspaces ---

// SaveWorkspace stores or replaces a workspace.
func (s *Store) SaveWorkspace(ctx context.Context, ws *v1alpha1.Workspace) error {
	if ws.Metadata == nil {
		ws.Metadata = &v1alpha1.ObjectMeta{}
	}
	if ws.Metadata.Name == "" {
		return errors.New("workspace name is required")
	}
	ws.Metadata.Atespace = defaultAtespace(ws.Metadata.Atespace)
	if ws.ApiVersion == "" {
		ws.ApiVersion = v1alpha1.APIVersion
	}
	if ws.Kind == "" {
		ws.Kind = v1alpha1.KindWorkspace
	}
	return s.save(ctx, workspacesTable, ws.Metadata, ws)
}

// GetWorkspace retrieves a workspace by atespace and name.
func (s *Store) GetWorkspace(ctx context.Context, atespace, name string) (*v1alpha1.Workspace, error) {
	var ws v1alpha1.Workspace
	if err := s.get(ctx, workspacesTable, atespace, name, &ws); err != nil {
		return nil, err
	}
	return &ws, nil
}

// ListWorkspaces lists workspaces in an atespace, or across all atespaces when
// atespace is "" or "*", most recently saved first.
func (s *Store) ListWorkspaces(ctx context.Context, atespace string) ([]*v1alpha1.Workspace, error) {
	return list(ctx, s, workspacesTable, atespace, 0, 0, func() *v1alpha1.Workspace { return &v1alpha1.Workspace{} })
}

// DeleteWorkspace removes a workspace. Deleting a missing workspace is not an error.
func (s *Store) DeleteWorkspace(ctx context.Context, atespace, name string) error {
	return s.delete(ctx, workspacesTable, atespace, name)
}

// --- Models ---

// SaveModel stores or replaces a model.
func (s *Store) SaveModel(ctx context.Context, model *v1alpha1.Model) error {
	if model.Metadata == nil {
		model.Metadata = &v1alpha1.ObjectMeta{}
	}
	if model.Metadata.Name == "" {
		return errors.New("model name is required")
	}
	model.Metadata.Atespace = defaultAtespace(model.Metadata.Atespace)
	if model.ApiVersion == "" {
		model.ApiVersion = v1alpha1.APIVersion
	}
	if model.Kind == "" {
		model.Kind = v1alpha1.KindModel
	}
	return s.save(ctx, modelsTable, model.Metadata, model)
}

// GetModel retrieves a model by atespace and name.
func (s *Store) GetModel(ctx context.Context, atespace, name string) (*v1alpha1.Model, error) {
	var m v1alpha1.Model
	if err := s.get(ctx, modelsTable, atespace, name, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// ListModels lists models in an atespace, or across all atespaces when
// atespace is "" or "*", most recently saved first.
func (s *Store) ListModels(ctx context.Context, atespace string) ([]*v1alpha1.Model, error) {
	return list(ctx, s, modelsTable, atespace, 0, 0, func() *v1alpha1.Model { return &v1alpha1.Model{} })
}

// DeleteModel removes a model. Deleting a missing model is not an error.
func (s *Store) DeleteModel(ctx context.Context, atespace, name string) error {
	return s.delete(ctx, modelsTable, atespace, name)
}
