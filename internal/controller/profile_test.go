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

package controller_test

import (
	"strings"
	"testing"

	"github.com/google/ax/internal/apivalidation"
	"github.com/google/ax/internal/controller"
	"github.com/google/ax/pkg/apis/v1alpha1"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
)

func TestMergeProfile(t *testing.T) {
	empty := controller.MergeProfile(nil, nil)
	if empty.GetImage() != "" || empty.GetResources() != nil || empty.GetAteActorTemplate() != "" {
		t.Fatalf("MergeProfile(nil, nil) = %+v, want empty Profile", empty)
	}

	base := &v1alpha1.Profile{
		Image: "ws/image@sha256:aaa",
		Resources: &v1alpha1.ResourcesSpec{
			Limits: &v1alpha1.ResourceList{Cpu: "4", Memory: "8Gi"},
		},
	}
	override := &v1alpha1.Profile{
		Resources: &v1alpha1.ResourcesSpec{
			Limits: &v1alpha1.ResourceList{Memory: "16Gi"},
		},
	}

	merged := controller.MergeProfile(base, override)
	if merged.GetImage() != "ws/image@sha256:aaa" {
		t.Errorf("inherited image = %q, want ws/image@sha256:aaa", merged.GetImage())
	}
	if merged.GetResources().GetLimits().GetCpu() != "4" {
		t.Errorf("inherited cpu = %q, want 4", merged.GetResources().GetLimits().GetCpu())
	}
	if merged.GetResources().GetLimits().GetMemory() != "16Gi" {
		t.Errorf("overridden memory = %q, want 16Gi", merged.GetResources().GetLimits().GetMemory())
	}

	// Mutating merged must not mutate base.
	merged.Resources.Limits.Cpu = "8"
	if base.Resources.Limits.Cpu != "4" {
		t.Error("MergeProfile mutated base.Resources")
	}

	// Base AteActorTemplate is inherited when override is nil or empty, and
	// cleared when override sets explicit image/resources.
	inheritedTpl := controller.MergeProfile(&v1alpha1.Profile{AteActorTemplate: "gpu-base"}, &v1alpha1.Profile{})
	if inheritedTpl.GetAteActorTemplate() != "gpu-base" {
		t.Errorf("inherited AteActorTemplate = %q, want gpu-base", inheritedTpl.GetAteActorTemplate())
	}
	overriddenByImage := controller.MergeProfile(
		&v1alpha1.Profile{AteActorTemplate: "gpu-base"},
		&v1alpha1.Profile{Image: "task/image@sha256:bbb"},
	)
	if overriddenByImage.GetAteActorTemplate() != "" || overriddenByImage.GetImage() != "task/image@sha256:bbb" {
		t.Errorf("overriddenByImage = %+v", overriddenByImage)
	}
}

func TestResolveProfileTemplate(t *testing.T) {
	p1 := &v1alpha1.Profile{
		Image: "img@sha256:abc",
		Resources: &v1alpha1.ResourcesSpec{
			Limits: &v1alpha1.ResourceList{Cpu: "1", Memory: "1Gi"},
		},
	}
	p2 := &v1alpha1.Profile{
		Image: "img@sha256:abc",
		Resources: &v1alpha1.ResourcesSpec{
			Limits: &v1alpha1.ResourceList{Cpu: "1000m", Memory: "1024Mi"},
		},
	}

	n1, tmpl1, err := controller.ResolveProfileTemplate("default", p1, "runner@sha256:r1")
	if err != nil {
		t.Fatalf("ResolveProfileTemplate(p1): %v", err)
	}
	n2, _, err := controller.ResolveProfileTemplate("default", p2, "runner@sha256:r1")
	if err != nil {
		t.Fatalf("ResolveProfileTemplate(p2): %v", err)
	}
	if n1 != n2 {
		t.Fatalf("equivalent profiles yielded different template names: %q vs %q", n1, n2)
	}
	if !strings.HasPrefix(n1, "ax-") || len(n1) != 19 {
		t.Fatalf("unexpected template name format %q", n1)
	}
	if err := apivalidation.ValidateName(n1); err != nil {
		t.Fatalf("template name %q is not a valid RFC 1123 name: %v", n1, err)
	}
	if tmpl1.GetSnapshotConfig().GetPreferredFidelity() != ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_ROOTFS {
		t.Errorf("fidelity = %v, want SNAPSHOT_FIDELITY_ROOTFS", tmpl1.GetSnapshotConfig().GetPreferredFidelity())
	}

	// Omitted CPU/memory equivalent to Substrate's defaults (1000m, 512Mi) hash identically.
	nOmitted, _, err := controller.ResolveProfileTemplate("default", &v1alpha1.Profile{Image: "img@sha256:abc"}, "runner@sha256:r1")
	if err != nil {
		t.Fatal(err)
	}
	nExplicitDefault, _, err := controller.ResolveProfileTemplate("default", &v1alpha1.Profile{
		Image:     "img@sha256:abc",
		Resources: &v1alpha1.ResourcesSpec{Limits: &v1alpha1.ResourceList{Cpu: "1000m", Memory: "512Mi"}},
	}, "runner@sha256:r1")
	if err != nil {
		t.Fatal(err)
	}
	if nOmitted != nExplicitDefault {
		t.Errorf("omitted defaults (%q) != explicit defaults (%q)", nOmitted, nExplicitDefault)
	}

	// Changing runner volume image produces a distinct template hash.
	nRunner2, _, err := controller.ResolveProfileTemplate("default", p1, "runner@sha256:r2")
	if err != nil {
		t.Fatal(err)
	}
	if nRunner2 == n1 {
		t.Errorf("expected distinct template name when runnerImage differs, got %q", nRunner2)
	}

	// Explicit ate_actor_template returns that name with nil template spec.
	directName, directTmpl, err := controller.ResolveProfileTemplate("default", &v1alpha1.Profile{AteActorTemplate: "gpu-base"}, "runner@sha256:r1")
	if err != nil || directName != "gpu-base" || directTmpl != nil {
		t.Fatalf("ResolveProfileTemplate(AteActorTemplate) = (%q, %v, %v), want (\"gpu-base\", nil, nil)", directName, directTmpl, err)
	}
}
