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

func TestEffectiveProfile(t *testing.T) {
	empty := controller.EffectiveProfile(nil, nil)
	if empty.GetImage() != "" || empty.GetResources() != nil || empty.GetAteActorTemplate() != "" {
		t.Fatalf("EffectiveProfile(nil, nil) = %+v, want empty Profile", empty)
	}

	wsImageProfile := &v1alpha1.Profile{
		Image: "ws/image@sha256:aaa",
		Resources: &v1alpha1.ResourcesSpec{
			Limits: &v1alpha1.ResourceList{Cpu: "4", Memory: "8Gi"},
		},
	}

	// Scenario 1: nil or empty taskProfile falls back to workspaceProfile as a whole.
	for _, emptyTaskProfile := range []*v1alpha1.Profile{
		nil,
		{},
		{Resources: &v1alpha1.ResourcesSpec{}},
		{Resources: &v1alpha1.ResourcesSpec{Limits: &v1alpha1.ResourceList{}}},
	} {
		got := controller.EffectiveProfile(wsImageProfile, emptyTaskProfile)
		if got.GetImage() != "ws/image@sha256:aaa" ||
			got.GetResources().GetLimits().GetCpu() != "4" ||
			got.GetResources().GetLimits().GetMemory() != "8Gi" {
			t.Fatalf("EffectiveProfile(wsImageProfile, %+v) = %+v, want wsImageProfile", emptyTaskProfile, got)
		}
		// Mutating returned profile must not mutate workspaceProfile.
		got.Resources.Limits.Cpu = "8"
		if wsImageProfile.Resources.Limits.Cpu != "4" {
			t.Fatal("EffectiveProfile mutated workspaceProfile.Resources")
		}
	}

	// Workspace AteActorTemplate is also inherited when taskProfile is empty.
	inheritedTpl := controller.EffectiveProfile(&v1alpha1.Profile{AteActorTemplate: "gpu-base"}, &v1alpha1.Profile{})
	if inheritedTpl.GetAteActorTemplate() != "gpu-base" {
		t.Fatalf("inherited AteActorTemplate = %+v, want gpu-base", inheritedTpl)
	}

	// Scenario 2: taskProfile sets resources only -> used as-is; does NOT inherit
	// workspaceProfile's image or CPU limit (omitted fields fall back to server defaults).
	taskResourcesOnly := &v1alpha1.Profile{
		Resources: &v1alpha1.ResourcesSpec{
			Limits: &v1alpha1.ResourceList{Memory: "16Gi"},
		},
	}
	gotResourcesOnly := controller.EffectiveProfile(wsImageProfile, taskResourcesOnly)
	if gotResourcesOnly.GetImage() != "" {
		t.Errorf("image = %q, want empty (must not inherit workspace image)", gotResourcesOnly.GetImage())
	}
	if gotResourcesOnly.GetResources().GetLimits().GetCpu() != "" {
		t.Errorf("cpu = %q, want empty (must not inherit workspace cpu)", gotResourcesOnly.GetResources().GetLimits().GetCpu())
	}
	if gotResourcesOnly.GetResources().GetLimits().GetMemory() != "16Gi" {
		t.Errorf("memory = %q, want 16Gi", gotResourcesOnly.GetResources().GetLimits().GetMemory())
	}
	// Mutating returned profile must not mutate taskProfile.
	gotResourcesOnly.Resources.Limits.Memory = "32Gi"
	if taskResourcesOnly.Resources.Limits.Memory != "16Gi" {
		t.Fatal("EffectiveProfile mutated taskProfile.Resources")
	}

	// Scenario 3: taskProfile sets image only -> used as-is; does NOT inherit
	// workspaceProfile's CPU/memory limits.
	gotImageOnly := controller.EffectiveProfile(wsImageProfile, &v1alpha1.Profile{Image: "task/image@sha256:bbb"})
	if gotImageOnly.GetImage() != "task/image@sha256:bbb" || gotImageOnly.GetResources() != nil {
		t.Errorf("EffectiveProfile(wsImageProfile, image-only) = %+v, want image only with nil resources", gotImageOnly)
	}

	// Scenario 4: workspaceProfile uses AteActorTemplate, while taskProfile sets
	// resources only, image, or another AteActorTemplate -> taskProfile wins cleanly
	// without conflicting with workspaceProfile's AteActorTemplate.
	wsTemplateProfile := &v1alpha1.Profile{AteActorTemplate: "gpu-base"}
	gotResourcesOverTpl := controller.EffectiveProfile(wsTemplateProfile, taskResourcesOnly)
	if gotResourcesOverTpl.GetAteActorTemplate() != "" ||
		gotResourcesOverTpl.GetImage() != "" ||
		gotResourcesOverTpl.GetResources().GetLimits().GetMemory() != "16Gi" {
		t.Errorf("EffectiveProfile(wsTemplateProfile, taskResourcesOnly) = %+v, want taskResourcesOnly as-is", gotResourcesOverTpl)
	}

	switchedTpl := controller.EffectiveProfile(wsTemplateProfile, &v1alpha1.Profile{AteActorTemplate: "tpu-base"})
	if switchedTpl.GetAteActorTemplate() != "tpu-base" {
		t.Errorf("switched AteActorTemplate = %+v, want tpu-base", switchedTpl)
	}

	gotTplOverWsImage := controller.EffectiveProfile(wsImageProfile, &v1alpha1.Profile{AteActorTemplate: "tpu-base"})
	if gotTplOverWsImage.GetAteActorTemplate() != "tpu-base" || gotTplOverWsImage.GetImage() != "" || gotTplOverWsImage.GetResources() != nil {
		t.Errorf("EffectiveProfile(wsImageProfile, tpu-base) = %+v, want tpu-base only", gotTplOverWsImage)
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
