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
	"errors"
	"fmt"
	"time"

	"github.com/google/ax/internal/store"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/protobuf/encoding/protojson"
)

const (
	// pgCodeUniqueViolation is PostgreSQL's SQLSTATE error code for
	// unique_violation, returned when an INSERT collides with an existing row's
	// PRIMARY KEY (atespace, name).
	pgCodeUniqueViolation = "23505"

	// maxApplyInsertAttempts bounds Apply retries when two concurrent callers
	// race to insert the same missing task row (initial attempt + 1 retry).
	maxApplyInsertAttempts = 2
)

// GetTaskVersion returns the stored row for key without locking it, or store.ErrNotFound.
func (s *Store) GetTaskVersion(ctx context.Context, key store.TaskKey) (*store.TaskVersion, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key = key.Normalized()

	row := s.pool.QueryRow(ctx, `
		SELECT data, generation
		FROM tasks
		WHERE atespace = $1 AND name = $2`,
		key.Atespace, key.Name)
	return scanTaskVersion(row)
}

// Apply executes fn inside a transaction holding the row lock for key and
// commits the resulting insert, update, or deletion.
func (s *Store) Apply(ctx context.Context, key store.TaskKey, fn store.ApplyFunc) (*store.TaskVersion, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key = key.Normalized()
	if key.Name == "" {
		return nil, errors.New("task name is required")
	}

	// Retry once if two concurrent callers race to insert the same missing row:
	// the loser sees current == nil first, hits unique_violation on INSERT, and
	// retries against the row committed by the winner.
	for attempt := range maxApplyInsertAttempts {
		ret, retry, err := s.applyOnce(ctx, key, fn, attempt == 0)
		if retry {
			continue
		}
		return ret, err
	}
	return nil, errors.New("retrying concurrent task insert failed")
}

func (s *Store) applyOnce(ctx context.Context, key store.TaskKey, fn store.ApplyFunc, allowInsertRetry bool) (*store.TaskVersion, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("beginning task apply transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	stored, err := scanTaskVersion(tx.QueryRow(ctx, `
		SELECT data, generation
		FROM tasks
		WHERE atespace = $1 AND name = $2
		FOR UPDATE`,
		key.Atespace, key.Name))
	if errors.Is(err, store.ErrNotFound) {
		stored = nil
	} else if err != nil {
		return nil, false, err
	}

	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return nil, false, fmt.Errorf("reading postgres clock: %w", err)
	}
	now = now.UTC()

	// Pass a clone to fn so in-place mutations to current do not leak into
	// the returned row if fn returns nil or current itself.
	var passedCurrent *store.TaskVersion
	if stored != nil {
		passedCurrent = stored.Clone()
	}

	next, err := fn(passedCurrent, now)
	switch {
	case err != nil:
		return nil, false, err

	case next == store.DeleteTaskVersion:
		if stored != nil {
			if _, err := tx.Exec(ctx, `DELETE FROM tasks WHERE atespace = $1 AND name = $2`, key.Atespace, key.Name); err != nil {
				return nil, false, fmt.Errorf("deleting task row: %w", err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, false, fmt.Errorf("committing task row delete: %w", err)
		}
		return nil, false, nil

	case next == nil || (passedCurrent != nil && next == passedCurrent):
		if stored == nil {
			return nil, false, nil
		}
		return stored.Clone(), false, nil
	}

	written, err := store.PrepareTaskVersionForApply(next, stored, key)
	if err != nil {
		return nil, false, err
	}
	data, err := protojson.Marshal(written.Task)
	if err != nil {
		return nil, false, fmt.Errorf("marshaling task: %w", err)
	}
	if stored == nil {
		_, err = tx.Exec(ctx, `
			INSERT INTO tasks (atespace, name, data, saved_at, generation)
			VALUES ($1, $2, $3, $4, $5)`,
			key.Atespace, key.Name, string(data), now, written.Generation,
		)
		if err != nil {
			if allowInsertRetry && isUniqueViolation(err) {
				return nil, true, nil
			}
			return nil, false, fmt.Errorf("inserting task row: %w", err)
		}
	} else {
		_, err = tx.Exec(ctx, `
			UPDATE tasks SET data = $3, generation = $4
			WHERE atespace = $1 AND name = $2`,
			key.Atespace, key.Name, string(data), written.Generation,
		)
		if err != nil {
			return nil, false, fmt.Errorf("updating task row: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("committing task row apply: %w", err)
	}
	return written.Clone(), false, nil
}

// scanTaskVersion scans one row from the tasks table (in the column order selected
// by GetTaskVersion and Apply) into a TaskVersion, unmarshals the protojson data
// column, and reconciles Generation via max(column, Status.Generation) so
// legacy SaveTask/UpdateTaskStatus writes are preserved before Apply runs.
func scanTaskVersion(row pgx.Row) (*store.TaskVersion, error) {
	var (
		data       []byte
		generation int64
	)
	err := row.Scan(&data, &generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scanning task row: %w", err)
	}

	var task v1alpha1.Task
	if err := unmarshalOpts.Unmarshal(data, &task); err != nil {
		return nil, fmt.Errorf("unmarshaling task row: %w", err)
	}
	out := &store.TaskVersion{
		Task:       &task,
		Generation: max(generation, task.GetStatus().GetGeneration()),
	}
	store.OverlayTaskGeneration(out)
	return out, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgCodeUniqueViolation
}
