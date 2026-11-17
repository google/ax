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

package store

import (
	"errors"

	"github.com/google/ax/pkg/apis/v1alpha1"
)

// PrepareTaskRowForApply validates and normalizes a non-nil, non-sentinel
// TaskRow returned from an [ApplyFunc] before storage writes it:
//   - clones next so caller-retained pointers cannot mutate stored state;
//   - defaults ApiVersion and Kind and stamps the normalized key onto Metadata;
//   - scrubs Task.Status via [StoredStatus] (initializing Task.Status if nil);
//   - assigns Generation (1 on insert, max(stored.Generation,
//     stored.Task.Status.Generation)+1 on update) and mirrors it onto
//     Task.Status.Generation.
func PrepareTaskRowForApply(next, stored *TaskRow, key TaskKey) (*TaskRow, error) {
	if next == nil || next.Task == nil {
		return nil, errors.New("task row returned from ApplyFunc has nil Task")
	}
	key = key.Normalized()
	if key.Name == "" {
		return nil, errors.New("task name is required")
	}

	written := next.Clone()
	if written.Task.ApiVersion == "" {
		written.Task.ApiVersion = v1alpha1.APIVersion
	}
	if written.Task.Kind == "" {
		written.Task.Kind = v1alpha1.KindTask
	}
	if written.Task.Metadata == nil {
		written.Task.Metadata = &v1alpha1.ObjectMeta{}
	}
	written.Task.Metadata.Atespace = key.Atespace
	written.Task.Metadata.Name = key.Name
	written.Task.Status = StoredStatus(written.Task.Status)

	nextGen := int64(1)
	if stored != nil {
		baseGen := max(stored.Generation, stored.Task.GetStatus().GetGeneration())
		nextGen = max(baseGen+1, 1)
	}
	written.Generation = nextGen
	written.Task.Status.Generation = nextGen
	return written, nil
}

// OverlayTaskGeneration mirrors row.Generation onto row.Task.Status.Generation
// when loading a TaskRow from storage.
func OverlayTaskGeneration(row *TaskRow) {
	if row == nil || row.Task == nil || row.Generation <= 0 {
		return
	}
	if row.Task.Status == nil {
		row.Task.Status = &v1alpha1.TaskStatus{}
	}
	row.Task.Status.Generation = row.Generation
}
