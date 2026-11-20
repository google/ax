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

package server

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/ax/internal/apivalidation"
	"github.com/google/ax/internal/controller"
	"github.com/google/ax/internal/store"
	"github.com/google/ax/internal/substrate"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ForkTask creates a new suspended Task branched from a parked (SUSPENDED,
// IDLE, COMPLETED, or FAILED) source Task's durable snapshot. The forked task
// inherits the source's profile, workspace, gateway, debug setting, env, and
// session_id (unless overridden) and seeds its actor from a dedicated snapshot
// tag capturing the source's /workspace state.
func (s *Server) ForkTask(ctx context.Context, req *v1alpha1.ForkTaskRequest) (*v1alpha1.Task, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "missing request")
	}
	atespace := req.GetAtespace()
	if atespace == "" {
		atespace = v1alpha1.DefaultAtespace
	}
	sourceName := req.GetSourceTask()
	targetName := req.GetName()
	if sourceName == "" {
		return nil, status.Error(codes.InvalidArgument, "source_task is required")
	}
	if targetName == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if sourceName == targetName {
		return nil, status.Error(codes.InvalidArgument, "name must differ from source_task")
	}
	if err := apivalidation.ValidateObjectMeta(&v1alpha1.ObjectMeta{
		Atespace: atespace,
		Name:     targetName,
		Labels:   req.GetLabels(),
	}); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if strings.HasPrefix(targetName, apivalidation.SetupActorPrefix) {
		return nil, status.Errorf(codes.InvalidArgument, "name: prefix %q is reserved for workspace setup actors", apivalidation.SetupActorPrefix)
	}

	// Fast-fail if the target task already exists before taking a snapshot tag.
	if _, err := s.getStoredTask(ctx, atespace, targetName); err == nil {
		return nil, status.Errorf(codes.AlreadyExists, "task %s/%s already exists", atespace, targetName)
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, status.Errorf(codes.Internal, "checking target task: %v", err)
	}

	// Lock the source task while inspecting its state and creating its fork
	// snapshot tag so we never race with parkActorAsync or DeleteTask(source).
	child, ws, gw, tagName, err := s.snapshotSourceForFork(ctx, atespace, sourceName, targetName, req)
	if err != nil {
		return nil, err
	}

	// Now lock the target task to save and provision the forked actor. If saving
	// the target row fails (e.g., concurrent creation race), remove the orphan tag.
	unlockTarget, err := s.locker.Lock(ctx, "task", atespace, targetName)
	if err != nil {
		_ = s.reconciler.DeleteForkTag(ctx, atespace, tagName)
		return nil, status.Errorf(codes.Aborted, "locking task %s/%s: %v", atespace, targetName, err)
	}
	defer unlockTarget()

	rowSaved := false
	defer func() {
		if !rowSaved {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
			defer cancel()
			_ = s.reconciler.DeleteForkTag(cleanupCtx, atespace, tagName)
		}
	}()

	if _, err := s.getStoredTask(ctx, atespace, targetName); err == nil {
		return nil, status.Errorf(codes.AlreadyExists, "task %s/%s already exists", atespace, targetName)
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, status.Errorf(codes.Internal, "checking target task: %v", err)
	}

	key := store.TaskKey{Atespace: atespace, Name: targetName}
	written, err := s.applier.Apply(ctx, key, func(current *store.TaskRow, _ time.Time) (*store.TaskRow, error) {
		if current != nil {
			return nil, status.Errorf(codes.AlreadyExists, "task %s/%s already exists", atespace, targetName)
		}
		return &store.TaskRow{
			Task:     proto.Clone(child).(*v1alpha1.Task),
			Runnable: false,
		}, nil
	})
	if err != nil {
		if _, ok := status.FromError(err); ok {
			return nil, err
		}
		return nil, status.Errorf(codes.Internal, "saving forked task: %v", err)
	}
	rowSaved = true
	child = proto.Clone(written.Task).(*v1alpha1.Task)

	if err := s.reconciler.Suspend(ctx, child, ws, gw); err != nil {
		return nil, s.recordFailure(ctx, child, "forking", err)
	}
	if err := s.finalizeReconciledRow(ctx, child, s.fetchActorUID(ctx, atespace, targetName), false); err != nil {
		return nil, err
	}

	s.applyActor(ctx, child)
	return child, nil
}

func (s *Server) snapshotSourceForFork(ctx context.Context, atespace, sourceName, targetName string, req *v1alpha1.ForkTaskRequest) (*v1alpha1.Task, *v1alpha1.Workspace, *v1alpha1.Gateway, string, error) {
	unlockSource, err := s.locker.Lock(ctx, "task", atespace, sourceName)
	if err != nil {
		return nil, nil, nil, "", status.Errorf(codes.Aborted, "locking source task %s/%s: %v", atespace, sourceName, err)
	}
	defer unlockSource()

	source, err := s.getTask(ctx, atespace, sourceName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil, nil, "", status.Errorf(codes.NotFound, "source task %q not found in atespace %q", sourceName, atespace)
		}
		return nil, nil, nil, "", status.Errorf(codes.Internal, "getting source task: %v", err)
	}
	if source.GetMetadata().GetDeleteTime() != nil {
		return nil, nil, nil, "", status.Errorf(codes.FailedPrecondition, "source task %q is being deleted", sourceName)
	}
	if !isForkableTaskState(source.GetStatus().GetState()) {
		return nil, nil, nil, "", status.Errorf(codes.FailedPrecondition, "source task %q is in state %v; suspend it or wait for completion before forking", sourceName, source.GetStatus().GetState())
	}
	if source.GetStatus().GetActor() == "" || source.GetStatus().GetAteTemplate().GetName() == "" {
		return nil, nil, nil, "", status.Errorf(codes.FailedPrecondition, "source task %q has no provisioned actor snapshot to fork", sourceName)
	}

	now := timestamppb.Now()
	childUID := uuid.NewString()
	tagName := controller.ForkTagName(childUID)
	child := buildForkedTask(source, atespace, targetName, childUID, tagName, req, now)

	if err := apivalidation.ValidateTask(child); err != nil {
		return nil, nil, nil, "", status.Error(codes.InvalidArgument, err.Error())
	}
	if field := firstUnimplementedTaskField(child.GetSpec()); field != "" {
		return nil, nil, nil, "", status.Errorf(codes.Unimplemented, "%s is not supported yet", field)
	}

	var gw *v1alpha1.Gateway
	if gwName := child.GetSpec().GetGateway().GetName(); gwName != "" {
		gw, err = s.store.GetGateway(ctx, atespace, gwName)
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil, nil, "", status.Errorf(codes.FailedPrecondition, "gateway %q not found in atespace %q", gwName, atespace)
		}
		if err != nil {
			return nil, nil, nil, "", status.Errorf(codes.Internal, "getting gateway %q: %v", gwName, err)
		}
	}
	ws := s.fetchWorkspace(ctx, atespace, child)

	if err := s.reconciler.CreateForkTag(ctx, atespace, sourceName, tagName); err != nil {
		if substrate.IsTransientError(err) {
			return nil, nil, nil, "", status.Errorf(codes.Unavailable, "snapshotting source task %q: %v", sourceName, err)
		}
		return nil, nil, nil, "", status.Errorf(codes.Internal, "snapshotting source task %q: %v", sourceName, err)
	}
	return child, ws, gw, tagName, nil
}

func isForkableTaskState(state v1alpha1.TaskState) bool {
	switch state {
	case v1alpha1.TaskState_TASK_STATE_SUSPENDED,
		v1alpha1.TaskState_TASK_STATE_IDLE,
		v1alpha1.TaskState_TASK_STATE_COMPLETED,
		v1alpha1.TaskState_TASK_STATE_FAILED:
		return true
	default:
		return false
	}
}

func buildForkedTask(source *v1alpha1.Task, atespace, targetName, childUID, tagName string, req *v1alpha1.ForkTaskRequest, now *timestamppb.Timestamp) *v1alpha1.Task {
	labels := map[string]string(nil)
	if len(source.GetMetadata().GetLabels()) > 0 || len(req.GetLabels()) > 0 {
		labels = make(map[string]string, len(source.GetMetadata().GetLabels())+len(req.GetLabels()))
		maps.Copy(labels, source.GetMetadata().GetLabels())
		maps.Copy(labels, req.GetLabels())
	}

	var spec *v1alpha1.TaskSpec
	if source.GetSpec() != nil {
		spec = proto.Clone(source.GetSpec()).(*v1alpha1.TaskSpec)
	} else {
		spec = &v1alpha1.TaskSpec{}
	}
	if spec.SessionId == "" {
		spec.SessionId = source.GetMetadata().GetUid()
	}

	if ov := req.GetOverrides(); ov != nil {
		if len(ov.GetCommands()) > 0 {
			spec.Commands = slices.Clone(ov.GetCommands())
		}
		if len(ov.GetEnv()) > 0 {
			spec.Env = mergeEnvVars(spec.Env, ov.GetEnv())
		}
		if ov.GetGoal() != nil {
			spec.Goal = proto.Clone(ov.GetGoal()).(*v1alpha1.Goal)
		}
		if ov.GetConfig() != nil {
			spec.Config = proto.Clone(ov.GetConfig()).(*structpb.Struct)
		}
		if ov.GetSessionId() != "" {
			spec.SessionId = ov.GetSessionId()
		}
	}

	var resolvedArgv []string
	if len(spec.Commands) > 0 {
		resolvedArgv = slices.Clone(spec.Commands)
	}

	return &v1alpha1.Task{
		ApiVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindTask,
		Metadata: &v1alpha1.ObjectMeta{
			Name:       targetName,
			Atespace:   atespace,
			Uid:        childUID,
			Labels:     labels,
			Owner:      source.GetMetadata().GetOwner(),
			CreateTime: now,
			UpdateTime: now,
		},
		Spec: spec,
		Status: &v1alpha1.TaskStatus{
			State:        v1alpha1.TaskState_TASK_STATE_QUEUED,
			Attempt:      1,
			AteTemplate:  proto.Clone(source.GetStatus().GetAteTemplate()).(*v1alpha1.ObjectRef),
			ResolvedArgv: resolvedArgv,
			ParentTask:   source.GetMetadata().GetName(),
			ParentTag:    tagName,
		},
	}
}

func mergeEnvVars(base, overrides []*v1alpha1.EnvVar) []*v1alpha1.EnvVar {
	out := make([]*v1alpha1.EnvVar, 0, len(base)+len(overrides))
	idx := make(map[string]int, len(base))
	for _, e := range base {
		c := proto.Clone(e).(*v1alpha1.EnvVar)
		idx[c.GetName()] = len(out)
		out = append(out, c)
	}
	for _, e := range overrides {
		c := proto.Clone(e).(*v1alpha1.EnvVar)
		if i, ok := idx[c.GetName()]; ok {
			out[i] = c
		} else {
			idx[c.GetName()] = len(out)
			out = append(out, c)
		}
	}
	return out
}
