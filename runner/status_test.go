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

package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/ax/pkg/apis/v1alpha1"
)

func TestReadStatusFile(t *testing.T) {
	cases := []struct {
		name        string
		content     string // "" leaves the file out
		wantOK      bool
		wantState   string
		wantMessage string // substring
	}{
		{name: "missing", wantOK: false},
		{name: "blank", content: "  \n", wantOK: false},
		{name: "completed", content: `{"state":"COMPLETED"}`, wantOK: true, wantState: v1alpha1.RunnerStateCompleted},
		{
			name:    "failed with message and details",
			content: `{"state":"FAILED","message":"tests failed: 3 of 120","details":{"failedTests":["a"]}}`,
			wantOK:  true, wantState: v1alpha1.RunnerStateFailed, wantMessage: "tests failed: 3 of 120",
		},
		{name: "requires action is not a state yet", content: `{"state":"REQUIRES_ACTION"}`, wantOK: true, wantState: v1alpha1.RunnerStateFailed, wantMessage: `unknown state "REQUIRES_ACTION"`},
		{name: "not json", content: `done!`, wantOK: true, wantState: v1alpha1.RunnerStateFailed, wantMessage: "invalid AX_STATUS_FILE"},
		{name: "unknown state", content: `{"state":"DONE"}`, wantOK: true, wantState: v1alpha1.RunnerStateFailed, wantMessage: `unknown state "DONE"`},
		{name: "too large", content: `{"state":"COMPLETED","message":"` + strings.Repeat("x", MaxOutputBytes) + `"}`, wantOK: true, wantState: v1alpha1.RunnerStateFailed, wantMessage: "larger than"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), statusFileName)
			if tc.content != "" {
				if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			st, ok := readStatusFile(path, false)
			if ok != tc.wantOK || st.State != tc.wantState || !strings.Contains(st.Message, tc.wantMessage) {
				t.Errorf("readStatusFile = (%+v, %v), want state %q, message containing %q, ok %v", st, ok, tc.wantState, tc.wantMessage, tc.wantOK)
			}
		})
	}
}

func TestReadStatusFile_RejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere")
	if err := os.WriteFile(target, []byte(`{"state":"COMPLETED"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, statusFileName)
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if st, ok := readStatusFile(path, false); ok {
		t.Errorf("readStatusFile followed a symlink: %+v", st)
	}
}

func TestExitStatusIsWrittenAndReadBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), statusFileName)
	writeStatusFile(path, exitStatus(3), false)
	st, ok := readStatusFile(path, false)
	if !ok || st.State != v1alpha1.RunnerStateFailed || st.Message != "exit status 3" || st.ExitCode == nil || *st.ExitCode != 3 {
		t.Fatalf("read back %+v (ok=%v), want FAILED, exit status 3, exitCode 3", st, ok)
	}
	if got := runnerStatus(st, nil, "out"); got.ExitCode != 3 || got.Output != "out" {
		t.Errorf("runnerStatus without a run = %+v, want the file's exit code 3", got)
	}

	writeStatusFile(path, exitStatus(0), false)
	if st, ok := readStatusFile(path, false); !ok || st.State != v1alpha1.RunnerStateCompleted {
		t.Errorf("read back %+v (ok=%v), want COMPLETED", st, ok)
	}
	if got := exitStatus(-1); got.State != v1alpha1.RunnerStateFailed || got.Message != "killed by a signal" {
		t.Errorf("exitStatus(-1) = %+v", got)
	}
}
