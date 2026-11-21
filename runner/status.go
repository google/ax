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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/google/ax/pkg/apis/v1alpha1"
)

// statusFileName is the base name of AX_STATUS_FILE, in the workspace's .ax
// directory next to AX_OUTPUT_FILE.
const statusFileName = "STATUS"

// statusPath is AX_STATUS_FILE for a workspace.
func statusPath(wsPath string) string {
	return filepath.Join(wsPath, ".ax", statusFileName)
}

// statusFile is the content of AX_STATUS_FILE. Fields it does not know, such
// as "details", are ignored.
type statusFile struct {
	State    string `json:"state"`
	Message  string `json:"message,omitempty"`
	ExitCode *int32 `json:"exitCode,omitempty"`
}

// readStatusFile returns the status in path, and false when there is none: the
// file is missing or empty, or not a regular file the agent owns (see
// readOutputFileSafe). A file that is too large, is not JSON, or names an
// unknown state is reported as FAILED, so a malformed report never reads as
// success.
func readStatusFile(path string, dropPrivs bool) (statusFile, bool) {
	raw, ok := readOutputFileSafe(path, dropPrivs)
	if !ok || len(bytes.TrimSpace(raw)) == 0 {
		return statusFile{}, false
	}
	invalid := func(format string, args ...any) (statusFile, bool) {
		return statusFile{State: v1alpha1.RunnerStateFailed, Message: "invalid " + StatusFileEnv + ": " + fmt.Sprintf(format, args...)}, true
	}
	if len(raw) > MaxOutputBytes {
		return invalid("larger than %d bytes", MaxOutputBytes)
	}
	var st statusFile
	if err := json.Unmarshal(raw, &st); err != nil {
		return invalid("%v", err)
	}
	switch st.State {
	case v1alpha1.RunnerStateCompleted, v1alpha1.RunnerStateFailed:
		return st, true
	default:
		return invalid("unknown state %q", st.State)
	}
}

// exitStatus is the status a command that wrote no AX_STATUS_FILE ends in:
// COMPLETED for exit status zero, FAILED otherwise.
func exitStatus(exitCode int) statusFile {
	code := int32(exitCode)
	if exitCode == 0 {
		return statusFile{State: v1alpha1.RunnerStateCompleted, ExitCode: &code}
	}
	msg := fmt.Sprintf("exit status %d", exitCode)
	if exitCode < 0 {
		msg = "killed by a signal"
	}
	return statusFile{State: v1alpha1.RunnerStateFailed, Message: msg, ExitCode: &code}
}

// writeStatusFile writes st to path atomically, through a temporary file in
// the same directory, so a resumed runner finds how the task ended. When the
// task runs as UID 1000 the file is handed to it, as readStatusFile expects.
// A failure is logged: the status is still reported, it only does not
// survive a suspend.
func writeStatusFile(path string, st statusFile, dropPrivs bool) {
	dir := filepath.Dir(path)
	// The agent can replace the directory with a symlink; never write through one.
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
		slog.Warn("not writing the task status: its directory is missing or not a directory", "path", path)
		return
	}
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, "."+statusFileName+"-*")
	if err != nil {
		slog.Warn("failed to write the task status", "path", path, "error", err)
		return
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.Write(append(data, '\n'))
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		slog.Warn("failed to write the task status", "path", path, "error", err)
		return
	}
	_ = os.Chmod(tmp.Name(), 0o644)
	if dropPrivs {
		_ = os.Chown(tmp.Name(), unprivilegedUID, unprivilegedGID)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		slog.Warn("failed to write the task status", "path", path, "error", err)
	}
}

// runnerStatus turns st and output into what v1alpha1.RunnerStatusPath
// reports. exitCode is the status the command exited with in this runner, or
// nil when it ended before a suspend and st comes from AX_STATUS_FILE alone.
func runnerStatus(st statusFile, exitCode *int, output string) v1alpha1.RunnerStatus {
	code := 0
	switch {
	case exitCode != nil:
		code = *exitCode
	case st.ExitCode != nil:
		code = int(*st.ExitCode)
	}
	return v1alpha1.RunnerStatus{
		State:    st.State,
		Message:  st.Message,
		ExitCode: int32(code),
		Output:   output,
	}
}

// endStatus is how a task whose command exited with exitCode ended: the status
// the command wrote to AX_STATUS_FILE or, when it wrote none, the one its exit
// status implies, which is then written to AX_STATUS_FILE so it outlives a
// suspend. The output is AX_OUTPUT_FILE, or the end of stdout and stderr.
func endStatus(wsPath string, exitCode int, tail *tailBuffer, dropPrivs bool) v1alpha1.RunnerStatus {
	path := statusPath(wsPath)
	st, ok := readStatusFile(path, dropPrivs)
	if !ok {
		st = exitStatus(exitCode)
		writeStatusFile(path, st, dropPrivs)
	}
	slog.Info("task ended", "state", st.State, "message", st.Message, "exitCode", exitCode)
	return runnerStatus(st, &exitCode, readCapturedTaskOutput(outputPath(wsPath), tail, dropPrivs))
}

// finishedLauncherStatus is how a task whose launcher finished before a
// suspend ended: the status it left in AX_STATUS_FILE, or COMPLETED, since a
// launcher is only recorded as finished when it exited with status zero.
func finishedLauncherStatus(wsPath string, dropPrivs bool) v1alpha1.RunnerStatus {
	st, ok := readStatusFile(statusPath(wsPath), dropPrivs)
	if !ok {
		st = exitStatus(0)
	}
	return runnerStatus(st, nil, readCapturedTaskOutput(outputPath(wsPath), nil, dropPrivs))
}
