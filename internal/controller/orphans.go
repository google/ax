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

package controller

import (
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/ax/pkg/apis/v1alpha1"
)

// taskKey identifies a task or actor by atespace and name. A struct key avoids
// collisions that a joined string would have when a name contains a separator.
type taskKey struct {
	atespace, name string
}

// FindOrphanActors returns the actors that have no matching task.
// An actor matches a task when both its name and its atespace equal the task's.
func FindOrphanActors(actors []*ateapipb.Actor, tasks []*v1alpha1.Task) []*ateapipb.Actor {
	// Step 1: record each task's key so lookups are fast
	taskKeys := map[taskKey]bool{}
	for _, task := range tasks {
		taskKeys[taskKey{task.GetMetadata().GetAtespace(), task.GetMetadata().GetName()}] = true
	}

	// Step 2: keep each actor whose key is not in that set
	var orphans []*ateapipb.Actor
	for _, actor := range actors {
		key := taskKey{actor.GetMetadata().GetAtespace(), actor.GetMetadata().GetName()}
		if !taskKeys[key] {
			orphans = append(orphans, actor)
		}
	}
	return orphans
}
