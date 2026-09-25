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

package substrate

import (
	"testing"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/protobuf/proto"
)

func TestResourceLimits(t *testing.T) {
	tests := []struct {
		name string
		reqs *v1alpha1.ResourceReqs
		want *ateapipb.Resources
	}{
		{name: "nil"},
		{name: "empty", reqs: &v1alpha1.ResourceReqs{}},
		{
			name: "requests only are not applied",
			reqs: &v1alpha1.ResourceReqs{Requests: &v1alpha1.ResourceList{Cpu: "500m", Memory: "1Gi"}},
		},
		{
			name: "cpu limit",
			reqs: &v1alpha1.ResourceReqs{Limits: &v1alpha1.ResourceList{Cpu: "2"}},
			want: &ateapipb.Resources{Limits: []*ateapipb.Limits{{Name: "cpu", Quantity: "2"}}},
		},
		{
			name: "memory limit",
			reqs: &v1alpha1.ResourceReqs{Limits: &v1alpha1.ResourceList{Memory: "4Gi"}},
			want: &ateapipb.Resources{Limits: []*ateapipb.Limits{{Name: "memory", Quantity: "4Gi"}}},
		},
		{
			name: "cpu and memory limits",
			reqs: &v1alpha1.ResourceReqs{
				Requests: &v1alpha1.ResourceList{Cpu: "500m", Memory: "1Gi"},
				Limits:   &v1alpha1.ResourceList{Cpu: "2", Memory: "4Gi"},
			},
			want: &ateapipb.Resources{Limits: []*ateapipb.Limits{
				{Name: "cpu", Quantity: "2"},
				{Name: "memory", Quantity: "4Gi"},
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResourceLimits(tt.reqs)
			if !proto.Equal(got, tt.want) {
				t.Fatalf("ResourceLimits() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildActorTemplate_Resources(t *testing.T) {
	limits := &ateapipb.Resources{Limits: []*ateapipb.Limits{
		{Name: "cpu", Quantity: "2"},
		{Name: "memory", Quantity: "4Gi"},
	}}
	tmpl := BuildActorTemplate("default", "task-tmpl-01234567", "ghcr.io/my-org/agent@sha256:abc", nil, nil, "", limits)
	if !proto.Equal(tmpl.GetResources(), limits) {
		t.Fatalf("template resources = %v, want %v", tmpl.GetResources(), limits)
	}

	// Without limits the template must not carry a resources block, so the
	// worker defaults keep applying.
	tmpl = BuildActorTemplate("default", "task-tmpl-01234567", "ghcr.io/my-org/agent@sha256:abc", nil, nil, "", nil)
	if tmpl.GetResources() != nil {
		t.Fatalf("template without limits has resources %v, want none", tmpl.GetResources())
	}
}
