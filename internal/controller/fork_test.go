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

	"github.com/google/ax/internal/controller"
	"github.com/google/ax/internal/substrate/substratetest"
	"github.com/google/ax/pkg/apis/v1alpha1"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
)

func TestTaskReconciler_ForkTagAndActorSeed(t *testing.T) {
	ctx := t.Context()
	sub := substratetest.NewServer(t)
	rec := controller.NewTaskReconciler(sub.Client(), "default-template", v1alpha1.DefaultSystemNamespace, "")

	source := &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "turn1", Atespace: "default"},
		Spec:     &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "ghcr.io/example/agent@sha256:1111"}},
	}
	if err := rec.Suspend(ctx, source, nil, nil); err != nil {
		t.Fatalf("Suspend source: %v", err)
	}

	tagName := controller.ForkTagName("child-uid-1")
	if !strings.HasPrefix(tagName, "fork-") || len(tagName) > 63 {
		t.Fatalf("unexpected ForkTagName(%q) = %q", "child-uid-1", tagName)
	}

	// CreateForkTag suspends the source actor and captures a completed Tag.
	if err := rec.CreateForkTag(ctx, "default", "turn1", tagName); err != nil {
		t.Fatalf("CreateForkTag: %v", err)
	}
	tag, ok := sub.Tag("default", tagName)
	if !ok {
		t.Fatalf("expected tag %q on Substrate", tagName)
	}
	if tag.GetSourceActor().GetName() != "turn1" {
		t.Errorf("tag source actor = %q, want turn1", tag.GetSourceActor().GetName())
	}

	// Retrying CreateForkTag replaces an existing tag cleanly.
	if err := rec.CreateForkTag(ctx, "default", "turn1", tagName); err != nil {
		t.Fatalf("retry CreateForkTag: %v", err)
	}

	// Changing RunnerVolumeImage on the reconciler must not break a forked task:
	// the fork reuses its parent's exact ActorTemplate and seeds from ParentTag.
	rec.RunnerVolumeImage = "ghcr.io/example/runner@sha256:2222"
	child := &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "turn2", Atespace: "default"},
		Spec:     source.Spec,
		Status: &v1alpha1.TaskStatus{
			AteTemplate: source.GetStatus().GetAteTemplate(),
			ParentTask:  "turn1",
			ParentTag:   tagName,
		},
	}
	if err := rec.Suspend(ctx, child, nil, nil); err != nil {
		t.Fatalf("Suspend forked child: %v", err)
	}

	actor, ok := sub.Actor("default", "turn2")
	if !ok {
		t.Fatal("expected forked actor turn2 on Substrate")
	}
	if actor.GetSourceTag().GetName() != tagName {
		t.Errorf("forked actor source_tag = %q, want %q", actor.GetSourceTag().GetName(), tagName)
	}
	if actor.GetActorTemplate().GetName() != source.GetStatus().GetAteTemplate().GetName() {
		t.Errorf("forked actor template = %q, want %q", actor.GetActorTemplate().GetName(), source.GetStatus().GetAteTemplate().GetName())
	}
	assertCondition(t, child, v1alpha1.ConditionWorkspaceReady, "True", v1alpha1.ReasonSetupComplete)

	// Deleting the forked task removes its actor and cleans up its fork Tag,
	// leaving the source actor untouched.
	if err := rec.DeleteTask(ctx, child); err != nil {
		t.Fatalf("DeleteTask(child): %v", err)
	}
	if _, ok := sub.Actor("default", "turn2"); ok {
		t.Error("expected child actor turn2 to be deleted")
	}
	if _, ok := sub.Tag("default", tagName); ok {
		t.Errorf("expected fork tag %q to be deleted with child task", tagName)
	}
	if _, ok := sub.Actor("default", "turn1"); !ok {
		t.Error("expected source actor turn1 to remain intact")
	}

	// Ensure CreateForkTag suspends a running source actor before tagging.
	if _, _, err := sub.Client().ResumeActor(ctx, "default", "turn1"); err != nil {
		t.Fatalf("ResumeActor: %v", err)
	}
	tag2 := controller.ForkTagName("child-uid-2")
	if err := rec.CreateForkTag(ctx, "default", "turn1", tag2); err != nil {
		t.Fatalf("CreateForkTag from running actor: %v", err)
	}
	srcActor, _ := sub.Actor("default", "turn1")
	if srcActor.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_SUSPENDED {
		t.Errorf("source actor state = %v, want SUSPENDED", srcActor.GetStatus().GetState())
	}
	if err := rec.DeleteForkTag(ctx, "default", tag2); err != nil {
		t.Fatalf("DeleteForkTag: %v", err)
	}
}
