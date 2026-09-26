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

package memory

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/ax/pkg/apis/v1alpha1"
)

func TestListTasksPagesDoNotOverlap(t *testing.T) {
	ctx := context.Background()
	s := NewStore()
	const n = 8
	for i := 0; i < n; i++ {
		task := &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{
				Name:     fmt.Sprintf("task-%02d", i),
				Atespace: "default",
			},
		}
		if err := s.SaveTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}

	for attempt := 0; attempt < 20; attempt++ {
		first, err := s.ListTasks(ctx, "default", 4, 0)
		if err != nil {
			t.Fatal(err)
		}
		second, err := s.ListTasks(ctx, "default", 4, 4)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]struct{}{}
		for _, task := range append(first, second...) {
			name := task.GetMetadata().GetName()
			if _, ok := seen[name]; ok {
				t.Fatalf("attempt %d: task %q appears on both pages", attempt, name)
			}
			seen[name] = struct{}{}
		}
		if len(seen) != n {
			t.Fatalf("attempt %d: pages covered %d tasks, want %d", attempt, len(seen), n)
		}
	}
}
