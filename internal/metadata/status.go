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

package metadata

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/google/ax/pkg/apis/v1alpha1"
)

// maxStatusBytes caps the size of the status file.
const maxStatusBytes = 16 << 10

// readStatus reads the status file of the workspace at wsPath,
// <workspace>/.ax/status, which only the task's agent writes. Without a file
// the status is empty: the task has not ended. A file that cannot be read, is
// not JSON, exceeds maxStatusBytes, or has an unknown state is an error.
func readStatus(wsPath string) (*v1alpha1.RunnerStatus, error) {
	f, err := os.Open(filepath.Join(wsPath, ".ax", "status"))
	if errors.Is(err, fs.ErrNotExist) {
		return &v1alpha1.RunnerStatus{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxStatusBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxStatusBytes {
		return nil, fmt.Errorf("status file exceeds %d bytes", maxStatusBytes)
	}
	st := &v1alpha1.RunnerStatus{}
	if err := json.Unmarshal(data, st); err != nil {
		return nil, fmt.Errorf("parsing status file: %w", err)
	}
	switch st.State {
	case v1alpha1.RunnerStateCompleted, v1alpha1.RunnerStateFailed:
		return st, nil
	case "REQUIRES_ACTION":
		// TODO(#690): Not acted on yet; the task has not ended.
		return &v1alpha1.RunnerStatus{}, nil
	}
	return nil, fmt.Errorf("status file has unknown state %q", st.State)
}
