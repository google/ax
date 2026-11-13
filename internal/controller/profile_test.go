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
)

func TestMergeProfile(t *testing.T) {
	empty, err := controller.MergeProfile(nil, nil)
	if err != nil {
		t.Fatalf("MergeProfile(nil, nil): %v", err)
	}
	if empty.GetImage() != "" || empty.GetResources() != nil || empty.GetAteActorTemplate() != "" {
		t.Fatalf("MergeProfile(nil, nil) = %+v, want empty Profile", empty)
	}

	// Scenario 1a: base uses image, override sets resources only -> inherits base image.
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

	merged, err := controller.MergeProfile(base, override)
	if err != nil {
		t.Fatalf("MergeProfile(image base, resources override): %v", err)
	}
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

	// Scenario 1b: base omits image/ateActorTemplate, override sets resources only -> allowed (uses server default image).
	defaultImgWithLimits, err := controller.MergeProfile(nil, override)
	if err != nil || defaultImgWithLimits.GetResources().GetLimits().GetMemory() != "16Gi" {
		t.Fatalf("MergeProfile(nil, resources override) = (%+v, %v), want 16Gi with nil err", defaultImgWithLimits, err)
	}

	// Scenario 2: base uses AteActorTemplate, override is empty or switches AteActorTemplate.
	inheritedTpl, err := controller.MergeProfile(&v1alpha1.Profile{AteActorTemplate: "gpu-base"}, &v1alpha1.Profile{})
	if err != nil || inheritedTpl.GetAteActorTemplate() != "gpu-base" {
		t.Fatalf("inherited AteActorTemplate = (%+v, %v), want gpu-base", inheritedTpl, err)
	}
	switchedTpl, err := controller.MergeProfile(
		&v1alpha1.Profile{AteActorTemplate: "gpu-base"},
		&v1alpha1.Profile{AteActorTemplate: "tpu-base"},
	)
	if err != nil || switchedTpl.GetAteActorTemplate() != "tpu-base" {
		t.Fatalf("switched AteActorTemplate = (%+v, %v), want tpu-base", switchedTpl, err)
	}

	// Scenario 3: base uses AteActorTemplate, override sets explicit image (+ optional resources) -> replaces template.
	overriddenByImage, err := controller.MergeProfile(
		&v1alpha1.Profile{AteActorTemplate: "gpu-base"},
		&v1alpha1.Profile{
			Image:     "task/image@sha256:bbb",
			Resources: &v1alpha1.ResourcesSpec{Limits: &v1alpha1.ResourceList{Memory: "8Gi"}},
		},
	)
	if err != nil {
		t.Fatalf("MergeProfile(template base, image override): %v", err)
	}
	if overriddenByImage.GetAteActorTemplate() != "" || overriddenByImage.GetImage() != "task/image@sha256:bbb" ||
		overriddenByImage.GetResources().GetLimits().GetMemory() != "8Gi" {
		t.Errorf("overriddenByImage = %+v", overriddenByImage)
	}

	// Scenario 4: base uses AteActorTemplate, override sets resources only -> rejected instead of silently dropping template.
	if _, err := controller.MergeProfile(
		&v1alpha1.Profile{AteActorTemplate: "gpu-base"},
		&v1alpha1.Profile{Resources: &v1alpha1.ResourcesSpec{Limits: &v1alpha1.ResourceList{Memory: "8Gi"}}},
	); err == nil {
		t.Fatal("expected error when overriding resources without image on an ateActorTemplate base, got nil")
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
	if tmpl1.GetSnapshotConfig().GetPreferredFidelity() != controller.DefaultProfileSnapshotFidelity {
		t.Errorf("fidelity = %v, want %v", tmpl1.GetSnapshotConfig().GetPreferredFidelity(), controller.DefaultProfileSnapshotFidelity)
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
