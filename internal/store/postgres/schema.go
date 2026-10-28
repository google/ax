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
	_ "embed"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// schema creates every table and index the store needs. It only adds what is
// missing, so it is safe to run on every start.
//
// TODO: Add versioned migrations before the schema first changes after release.
//
//go:embed schema.sql
var schema string

// schemaLockID is the advisory lock key that serializes schema creation when
// several ax-server replicas start at once; concurrent CREATE ... IF NOT EXISTS
// statements can otherwise fail.
const schemaLockID = 0x6178_7363_6865_6d61 // "axschema"

// createSchema creates any missing tables and indexes.
func createSchema(ctx context.Context, pool *pgxpool.Pool) error {
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(schemaLockID)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, schema)
		return err
	})
	if err != nil {
		return fmt.Errorf("creating postgres schema: %w", err)
	}
	return nil
}
