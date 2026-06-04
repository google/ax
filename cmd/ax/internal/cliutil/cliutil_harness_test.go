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
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/ax/internal/config/harnessconfig"
)

func TestNewControllerFromConfig_DefaultHarness(t *testing.T) {
	cfg := &harnessconfig.Config{
		EventLog: harnessconfig.EventLogConfig{
			SQLiteConfig: harnessconfig.SQLiteConfig{
				Filename: filepath.Join(t.TempDir(), "log.sqlite"),
			},
		},
		Harnesses: harnessconfig.HarnessesConfig{
			Default: "ag",
			Antigravity: []harnessconfig.AntigravityHarnessConfig{
				{ID: "ag", Address: "localhost:50053"},
			},
		},
	}

	c, err := NewControllerFromConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewControllerFromConfig: %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil controller")
	}
	c.Close()
}

func TestNewControllerFromConfig_UnknownDefaultHarness(t *testing.T) {
	cfg := &harnessconfig.Config{
		Harnesses: harnessconfig.HarnessesConfig{
			Default: "missing",
			Antigravity: []harnessconfig.AntigravityHarnessConfig{
				{ID: "ag", Address: "localhost:50053"},
			},
		},
	}

	_, err := NewControllerFromConfig(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error for unknown default harness, got nil")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("expected error to mention %q, got: %v", "missing", err)
	}
}
