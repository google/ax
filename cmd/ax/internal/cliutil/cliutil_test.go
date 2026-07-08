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

	"github.com/google/ax/internal/config"
)

func TestNewControllerFromConfig_BuiltinSubstrate(t *testing.T) {
	t.Setenv("AX_SUBSTRATE", "1")

	cfg := &config.Config{
		EventLog: config.EventLogConfig{
			SQLiteConfig: config.SQLiteConfig{
				Filename: filepath.Join(t.TempDir(), "log.sqlite"),
			},
		},
		Harnesses: config.HarnessesConfig{
			Antigravity: config.AntigravityHarnessConfig{
				Default: true,
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

func TestNewControllerFromConfig_CustomHarnessRequiresSubstrateMode(t *testing.T) {
	t.Setenv("AX_SUBSTRATE", "")

	cfg := &config.Config{
		EventLog: config.EventLogConfig{
			SQLiteConfig: config.SQLiteConfig{
				Filename: filepath.Join(t.TempDir(), "log.sqlite"),
			},
		},
		Harnesses: config.HarnessesConfig{
			Substrate: []config.SubstrateHarnessConfig{
				{ID: "custom", Namespace: "team-ns", Template: "custom-template"},
			},
		},
	}

	_, err := NewControllerFromConfig(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error for custom substrate harness without AX_SUBSTRATE=1, got nil")
	}
	if !strings.Contains(err.Error(), "AX_SUBSTRATE=1") {
		t.Errorf("expected error to mention AX_SUBSTRATE=1, got: %v", err)
	}
}

func TestNewControllerFromConfig_CustomHarnessInSubstrateMode(t *testing.T) {
	t.Setenv("AX_SUBSTRATE", "1")

	cfg := &config.Config{
		EventLog: config.EventLogConfig{
			SQLiteConfig: config.SQLiteConfig{
				Filename: filepath.Join(t.TempDir(), "log.sqlite"),
			},
		},
		Harnesses: config.HarnessesConfig{
			Substrate: []config.SubstrateHarnessConfig{
				{ID: "custom", Namespace: "team-ns", Template: "custom-template"},
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
