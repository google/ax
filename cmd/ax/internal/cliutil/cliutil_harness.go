//go:build harness

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

package cliutil

import (
	"context"

	"github.com/google/ax/internal/config"
	"github.com/google/ax/internal/controller/executor"
	"github.com/google/ax/internal/controller2"
	"github.com/google/ax/internal/harness"
)

// Controller is the active controller type for this build.
type Controller = *controller2.Controller

// ExecHandler is the handler type accepted by Controller.Exec.
type ExecHandler = controller2.ExecHandler

// NewControllerFromConfig creates a controller2.Controller.
func NewControllerFromConfig(ctx context.Context, cfg *config.Config) (*controller2.Controller, error) {
	reg := controller2.NewRegistry()
	reg.RegisterHarness("antigravity", harness.NewAntigravityHarness(""))
	return controller2.New(ctx, controller2.Config{
		Registry: reg,
		EventLogBuilder: func() (executor.EventLog, error) {
			return executor.OpenSQLiteEventLog(cfg.EventLog.SQLiteConfig.Filename)
		},
	})
}
