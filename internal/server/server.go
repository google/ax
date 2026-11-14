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
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/google/ax/internal/apivalidation"
	"github.com/google/ax/internal/controller"
	"github.com/google/ax/internal/lock"
	"github.com/google/ax/internal/store"
	"github.com/google/ax/internal/substrate"
	"github.com/google/ax/internal/telemetry"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"github.com/google/uuid"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Options configures the AX API Server.
type Options struct {
	// Locker serializes changes to a resource. It must be shared by all
	// ax-server replicas; it defaults to an in-process lock.MemoryLocker.
	Locker lock.Locker
}

// Server provides the gRPC API for AX.
type Server struct {
	v1alpha1.UnimplementedAXServer
	store      store.Store
	locker     lock.Locker
	reconciler *controller.TaskReconciler
	// substrate is the reconciler's client. It is read for the parts of a
	// task's status owned by its actor, which the store never holds.
	substrate  *substrate.Client
	grpcServer *grpc.Server
}

// NewServer creates a new AX API server.
func NewServer(s store.Store, reconciler *controller.TaskReconciler, opts ...Options) *Server {
	if reconciler == nil || reconciler.Client() == nil {
		panic("server: NewServer requires a non-nil TaskReconciler with a Substrate client")
	}
	var opt Options
	if len(opts) > 0 {
		opt = opts[0]
	}
	locker := opt.Locker
	if locker == nil {
		locker = lock.NewMemoryLocker()
	}

	srv := &Server{
		store:      s,
		locker:     locker,
		reconciler: reconciler,
		substrate:  reconciler.Client(),
	}
	srv.grpcServer = grpc.NewServer(grpc.StatsHandler(telemetry.ServerStatsHandler(func() map[string]grpc.ServiceInfo {
		return srv.grpcServer.GetServiceInfo()
	})))
	v1alpha1.RegisterAXServer(srv.grpcServer, srv)
	return srv
}

// GRPCServer returns the underlying gRPC server instance.
func (s *Server) GRPCServer() *grpc.Server {
	return s.grpcServer
}

// Handler returns the HTTP handler for the server, routing gRPC and HTTP health checks.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
			s.grpcServer.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok\n"))
			return
		}
		http.NotFound(w, r)
	})
}

// --- gRPC AXServer implementation ---

// --- Tasks ---

func (s *Server) GetTask(ctx context.Context, req *v1alpha1.GetTaskRequest) (*v1alpha1.Task, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "missing request")
	}
	atespace := req.Atespace
	if atespace == "" {
		atespace = v1alpha1.DefaultAtespace
	}
	task, err := s.getTask(ctx, atespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "task %q not found in atespace %q", req.Name, atespace)
		}
		return nil, status.Errorf(codes.Internal, "getting task: %v", err)
	}
	return task, nil
}

func (s *Server) ListTasks(ctx context.Context, req *v1alpha1.ListTasksRequest) (*v1alpha1.ListTasksResponse, error) {
	atespace := ""
	limit := int64(50)
	offset := int64(0)
	if req != nil {
		atespace = req.Atespace
		if req.Limit > 0 {
			limit = req.Limit
		}
		if req.Offset >= 0 {
			offset = req.Offset
		}
	}
	tasks, err := s.listTasks(ctx, atespace, limit, offset)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "listing tasks: %v", err)
	}
	return &v1alpha1.ListTasksResponse{Tasks: tasks}, nil
}

func (s *Server) CreateTask(ctx context.Context, req *v1alpha1.CreateTaskRequest) (*v1alpha1.Task, error) {
	if req == nil || req.Task == nil {
		return nil, status.Error(codes.InvalidArgument, "task required")
	}
	task := req.Task
	if err := apivalidation.ValidateTask(task); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	// Tasks are immutable, so reject fields whose runtime enforcement has not
	// landed yet (ateActorTemplate requires pull-based runner assignment; retry
	// requires runner exit events and the retry loop) rather than silently
	// storing settings that nothing honors.
	if field := firstUnimplementedTaskField(task.GetSpec()); field != "" {
		return nil, status.Errorf(codes.Unimplemented, "%s is not supported yet", field)
	}
	if task.Metadata == nil {
		task.Metadata = &v1alpha1.ObjectMeta{}
	}
	atespace := task.Metadata.Atespace
	if atespace == "" {
		atespace = v1alpha1.DefaultAtespace
		task.Metadata.Atespace = atespace
	}
	taskName := task.Metadata.GetName()

	// Acquire exclusive lock for this task.
	unlock, err := s.locker.Lock(ctx, "task", atespace, taskName)
	if err != nil {
		return nil, status.Errorf(codes.Aborted, "locking task %s/%s: %v", atespace, taskName, err)
	}
	defer unlock()

	// Check the stored record (before overlaying live actor status) so only an
	// unfinished initial CreateTask (stored State == QUEUED with no provisioned
	// template yet) can be retried; an already-created task whose actor happens
	// to be resuming (ACTOR_STATE_RESUMING -> TASK_STATE_QUEUED on read) or that
	// hit a later transient ResumeTask error is rejected without touching the actor.
	existing, err := s.getStoredTask(ctx, atespace, taskName)
	if err == nil {
		if existing.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_QUEUED ||
			existing.GetStatus().GetAteActorTemplate() != "" ||
			existing.GetMetadata().GetDeleteTime() != nil ||
			!taskSpecMatchesExisting(existing, task.GetSpec()) {
			return nil, status.Errorf(codes.FailedPrecondition, "task %s/%s already exists and is immutable", atespace, taskName)
		}
		task = existing
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, status.Errorf(codes.Internal, "checking existing task: %v", err)
	}

	// Tasks are immutable, so a task created before its Workspace would keep
	// an empty workspace for its whole life. Reject it instead.
	var workspace *v1alpha1.Workspace
	if wsName := task.GetSpec().GetWorkspace().GetName(); wsName != "" {
		workspace, err = s.store.GetWorkspace(ctx, atespace, wsName)
		if errors.Is(err, store.ErrNotFound) {
			return nil, status.Errorf(codes.FailedPrecondition, "workspace %q not found in atespace %q", wsName, atespace)
		}
		if err != nil {
			return nil, status.Errorf(codes.Internal, "getting workspace %q: %v", wsName, err)
		}
	}
	// A task gets its Gateway's rules when it is created, so the Gateway must
	// already exist.
	var gateway *v1alpha1.Gateway
	if gwName := task.GetSpec().GetGateway().GetName(); gwName != "" {
		gateway, err = s.store.GetGateway(ctx, atespace, gwName)
		if errors.Is(err, store.ErrNotFound) {
			return nil, status.Errorf(codes.FailedPrecondition, "gateway %q not found in atespace %q", gwName, atespace)
		}
		if err != nil {
			return nil, status.Errorf(codes.Internal, "getting gateway %q: %v", gwName, err)
		}
	}

	effectiveProfile := controller.EffectiveProfile(workspace.GetSpec().GetProfile(), task.GetSpec().GetProfile())
	if effectiveProfile.GetAteActorTemplate() != "" {
		return nil, status.Error(codes.Unimplemented, "spec.profile.ateActorTemplate is not supported yet")
	}

	now := timestamppb.Now()
	if task.Metadata.CreateTime == nil {
		task.Metadata.CreateTime = now
	}
	if task.Metadata.Uid == "" {
		task.Metadata.Uid = uuid.NewString()
	}
	task.Metadata.UpdateTime = now
	if task.Spec == nil {
		task.Spec = &v1alpha1.TaskSpec{}
	}
	if task.Spec.SessionId == "" {
		task.Spec.SessionId = task.Metadata.Uid
	}
	if task.Status == nil {
		task.Status = &v1alpha1.TaskStatus{}
	}
	if task.Status.Attempt == 0 {
		task.Status.Attempt = 1
	}
	task.Status.Generation++
	if len(task.Spec.Commands) > 0 {
		task.Status.ResolvedArgv = slices.Clone(task.Spec.Commands)
	}
	// The task is Queued until its actor exists.
	task.Status.State = v1alpha1.TaskState_TASK_STATE_QUEUED
	if err := s.saveTask(ctx, task); err != nil {
		return nil, status.Errorf(codes.Internal, "saving task: %v", err)
	}

	// New tasks start suspended.
	if err := s.reconciler.Suspend(ctx, task, workspace, gateway); err != nil {
		return nil, s.recordFailure(ctx, task, "provisioning", err)
	}
	if err := s.clearStoredTaskState(ctx, task); err != nil {
		return nil, err
	}

	// Report the actor as it is now, as GetTask would.
	s.applyActor(ctx, task)
	return task, nil
}

// TODO: Drop these checks once pull-based runner assignment and task retries are wired.
func firstUnimplementedTaskField(spec *v1alpha1.TaskSpec) string {
	if spec == nil {
		return ""
	}
	if spec.GetProfile().GetAteActorTemplate() != "" {
		return "spec.profile.ateActorTemplate"
	}
	if spec.GetRetry() != nil {
		return "spec.retry"
	}
	return ""
}

// taskSpecMatchesExisting compares an incoming TaskSpec against an existing
// Queued task's spec, accounting for session_id defaulting to metadata.uid on
// the initial CreateTask call.
func taskSpecMatchesExisting(existing *v1alpha1.Task, incoming *v1alpha1.TaskSpec) bool {
	want := incoming
	if want == nil {
		want = &v1alpha1.TaskSpec{}
	} else {
		want = proto.Clone(incoming).(*v1alpha1.TaskSpec)
	}
	if want.SessionId == "" && existing.GetMetadata().GetUid() != "" {
		want.SessionId = existing.GetMetadata().GetUid()
	}
	have := existing.GetSpec()
	if have == nil {
		have = &v1alpha1.TaskSpec{}
	}
	return proto.Equal(have, want)
}

func (s *Server) DeleteTask(ctx context.Context, req *v1alpha1.DeleteTaskRequest) (*v1alpha1.DeleteTaskResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "missing request")
	}
	atespace := req.Atespace
	if atespace == "" {
		atespace = v1alpha1.DefaultAtespace
	}
	taskName := req.Name

	// Acquire exclusive lock for this task.
	unlock, err := s.locker.Lock(ctx, "task", atespace, taskName)
	if err != nil {
		return nil, status.Errorf(codes.Aborted, "locking task %s/%s: %v", atespace, taskName, err)
	}
	defer unlock()

	task, err := s.getTask(ctx, atespace, taskName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "task %q not found in atespace %q", taskName, atespace)
		}
		return nil, status.Errorf(codes.Internal, "getting task: %v", err)
	}

	// Mark the task with delete_time while actor cleanup is in progress.
	deleteTime := timestamppb.Now()
	if err := s.setTaskDeleteTime(ctx, atespace, taskName, deleteTime); err != nil {
		return nil, status.Errorf(codes.Internal, "updating task delete_time: %v", err)
	}
	if task.Metadata == nil {
		task.Metadata = &v1alpha1.ObjectMeta{Atespace: atespace, Name: taskName}
	}
	task.Metadata.DeleteTime = deleteTime

	if err := s.deleteTask(ctx, task); err != nil {
		return nil, err
	}
	return &v1alpha1.DeleteTaskResponse{}, nil
}

func (s *Server) SuspendTask(ctx context.Context, req *v1alpha1.SuspendTaskRequest) (*v1alpha1.Task, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "missing request")
	}
	atespace := req.Atespace
	if atespace == "" {
		atespace = v1alpha1.DefaultAtespace
	}
	taskName := req.Name

	// Acquire exclusive lock for this task.
	unlock, err := s.locker.Lock(ctx, "task", atespace, taskName)
	if err != nil {
		return nil, status.Errorf(codes.Aborted, "locking task %s/%s: %v", atespace, taskName, err)
	}
	defer unlock()

	task, err := s.getTask(ctx, atespace, taskName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "task %q not found in atespace %q", taskName, atespace)
		}
		return nil, status.Errorf(codes.Internal, "getting task: %v", err)
	}
	if task.Status == nil {
		task.Status = &v1alpha1.TaskStatus{}
	}

	if err := s.reconciler.Suspend(ctx, task, s.fetchWorkspace(ctx, atespace, task), s.fetchGateway(ctx, atespace, task)); err != nil {
		// Store nothing: the actor keeps its state on Substrate, and a stored
		// Queued or Failed would hide it.
		if substrate.IsTransientError(err) {
			return nil, status.Errorf(codes.Unavailable, "suspending task on substrate: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "suspending task on substrate: %v", err)
	}
	if err := s.clearStoredTaskState(ctx, task); err != nil {
		return nil, err
	}

	// Report the actor as it is now, as GetTask would.
	s.applyActor(ctx, task)
	return task, nil
}

func (s *Server) ResumeTask(ctx context.Context, req *v1alpha1.ResumeTaskRequest) (*v1alpha1.Task, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "missing request")
	}
	atespace := req.Atespace
	if atespace == "" {
		atespace = v1alpha1.DefaultAtespace
	}
	taskName := req.Name

	// Acquire exclusive lock for this task.
	unlock, err := s.locker.Lock(ctx, "task", atespace, taskName)
	if err != nil {
		return nil, status.Errorf(codes.Aborted, "locking task %s/%s: %v", atespace, taskName, err)
	}
	defer unlock()

	task, err := s.getTask(ctx, atespace, taskName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "task %q not found in atespace %q", taskName, atespace)
		}
		return nil, status.Errorf(codes.Internal, "getting task: %v", err)
	}
	if task.Status == nil {
		task.Status = &v1alpha1.TaskStatus{}
	}

	if err := s.reconciler.Resume(ctx, task, s.fetchWorkspace(ctx, atespace, task), s.fetchGateway(ctx, atespace, task)); err != nil {
		return nil, s.recordFailure(ctx, task, "resuming", err)
	}
	if err := s.clearStoredTaskState(ctx, task); err != nil {
		return nil, err
	}

	// Report the actor as it is now, as GetTask would.
	s.applyActor(ctx, task)
	return task, nil
}

// clearStoredTaskState transitions task back to actor-derived status after a
// Substrate reconcile (CreateTask, SuspendTask, or ResumeTask) succeeds:
//   - Clears metadata.delete_time in the store and in memory if set by an
//     earlier failed DeleteTask, canceling that pending deletion.
//   - Resets status.state to UNSPECIFIED and clears status.status_detail so
//     subsequent reads derive the task's state from its live Substrate actor
//     rather than keeping a stored Queued or Failed state.
//   - Increments status.generation and persists the updated status via
//     updateTask, returning a gRPC Internal status error if any store write
//     fails.
func (s *Server) clearStoredTaskState(ctx context.Context, task *v1alpha1.Task) error {
	atespace := task.GetMetadata().GetAtespace()
	if atespace == "" {
		atespace = v1alpha1.DefaultAtespace
	}
	taskName := task.GetMetadata().GetName()
	if task.GetMetadata().GetDeleteTime() != nil {
		if err := s.setTaskDeleteTime(ctx, atespace, taskName, nil); err != nil {
			return status.Errorf(codes.Internal, "clearing task delete_time: %v", err)
		}
		task.Metadata.DeleteTime = nil
	}
	if task.Status == nil {
		task.Status = &v1alpha1.TaskStatus{}
	}
	task.Status.State = v1alpha1.TaskState_TASK_STATE_UNSPECIFIED
	task.Status.StatusDetail = nil
	task.Status.Generation++
	if err := s.updateTask(ctx, atespace, taskName, task.Status); err != nil {
		return status.Errorf(codes.Internal, "updating task status: %v", err)
	}
	return nil
}

// recordFailure stores the status of a task that could not be provisioned or
// resumed: state Queued if err is a transient Substrate error, which a retry
// may fix, and Failed otherwise. It returns the gRPC error for action.
func (s *Server) recordFailure(ctx context.Context, task *v1alpha1.Task, action string, err error) error {
	atespace, name := task.GetMetadata().GetAtespace(), task.GetMetadata().GetName()
	slog.Error(action+" task on substrate failed", "atespace", atespace, "task", name, "error", err)
	code, state := codes.Internal, v1alpha1.TaskState_TASK_STATE_FAILED
	var detail *v1alpha1.StatusDetail
	if substrate.IsTransientError(err) {
		code, state = codes.Unavailable, v1alpha1.TaskState_TASK_STATE_QUEUED
	} else {
		detail = &v1alpha1.StatusDetail{
			Reason: v1alpha1.TaskStatusReason_TASK_STATUS_REASON_PROVISIONING_FAILED,
			Detail: err.Error(),
		}
	}
	task.Status.State = state
	task.Status.StatusDetail = detail
	task.Status.Generation++
	_ = s.updateTask(ctx, atespace, name, task.Status)
	return status.Errorf(code, "%s task on substrate: %v", action, err)
}

// Task storage and Substrate.
//
// A task's actor on Agent Substrate is the source of truth for whether the task
// is running and whether it is healthy. The store keeps only what AX itself knows.
//
// Handlers never call s.store for tasks; they use the methods below. saveTask
// and updateTask drop the actor-owned status before writing. getTask and
// listTasks fill it in from Substrate.

// getStoredTask returns a task directly from the store without overlaying live
// actor status from Substrate.
func (s *Server) getStoredTask(ctx context.Context, atespace, name string) (*v1alpha1.Task, error) {
	return s.store.GetTask(ctx, atespace, name)
}

// getTask returns a task with its Substrate-owned status filled in.
func (s *Server) getTask(ctx context.Context, atespace, name string) (*v1alpha1.Task, error) {
	task, err := s.getStoredTask(ctx, atespace, name)
	if err != nil {
		return nil, err
	}
	s.applyActor(ctx, task)
	return task, nil
}

// listTasks returns a page of tasks with their Substrate-owned status filled
// in, using one ListActors call per atespace.
func (s *Server) listTasks(ctx context.Context, atespace string, limit, offset int64) ([]*v1alpha1.Task, error) {
	tasks, err := s.store.ListTasks(ctx, atespace, limit, offset)
	if err != nil {
		return tasks, err
	}
	byAtespace := make(map[string][]*v1alpha1.Task)
	for _, t := range tasks {
		ns := t.GetMetadata().GetAtespace()
		byAtespace[ns] = append(byAtespace[ns], t)
	}
	for ns, group := range byAtespace {
		actors, err := s.substrate.ListActors(ctx, ns)
		byName := make(map[string]*ateapipb.Actor, len(actors))
		for _, a := range actors {
			byName[a.GetMetadata().GetName()] = a
		}
		for _, t := range group {
			setActorStatus(t, byName[t.GetMetadata().GetName()], err)
		}
	}
	return tasks, nil
}

// saveTask stores task without its Substrate-owned status. task is not modified.
func (s *Server) saveTask(ctx context.Context, task *v1alpha1.Task) error {
	stored := proto.Clone(task).(*v1alpha1.Task)
	stored.Status = storedStatus(task.Status)
	return s.store.SaveTask(ctx, stored)
}

// updateTask stores a task's status without its Substrate-owned parts. st is
// not modified.
func (s *Server) updateTask(ctx context.Context, atespace, name string, st *v1alpha1.TaskStatus) error {
	return s.store.UpdateTaskStatus(ctx, atespace, name, storedStatus(st))
}

// setTaskDeleteTime sets or clears metadata.delete_time on a task record.
func (s *Server) setTaskDeleteTime(ctx context.Context, atespace, name string, deleteTime *timestamppb.Timestamp) error {
	return s.store.SetTaskDeleteTime(ctx, atespace, name, deleteTime)
}

// deleteTask removes a task's actor and templates from Substrate, then its
// record from the store. It returns a gRPC status error.
func (s *Server) deleteTask(ctx context.Context, task *v1alpha1.Task) error {
	atespace := task.GetMetadata().GetAtespace()
	name := task.GetMetadata().GetName()
	if err := s.reconciler.DeleteTask(ctx, task); err != nil {
		if errors.Is(err, context.Canceled) {
			return status.Error(codes.Canceled, err.Error())
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return status.Error(codes.DeadlineExceeded, err.Error())
		}
		if st, ok := status.FromError(err); ok && st.Code() != codes.Unknown {
			return err
		}
		return status.Errorf(codes.Internal, "cleaning up substrate resources for task %s/%s: %v", atespace, name, err)
	}
	if err := s.store.DeleteTask(ctx, atespace, name); err != nil {
		return status.Errorf(codes.Internal, "deleting task record: %v", err)
	}
	return nil
}

// applyActor fills in the Substrate-owned status of task from its actor.
func (s *Server) applyActor(ctx context.Context, task *v1alpha1.Task) {
	actor, err := s.substrate.GetActor(ctx, task.GetMetadata().GetAtespace(), task.GetMetadata().GetName())
	if status.Code(err) == codes.NotFound {
		actor, err = nil, nil
	}
	setActorStatus(task, actor, err)
}

// setActorStatus overlays the actor's state on the task's status: actor name,
// actorUID, and, unless the state is one of AX's own or deletion is in progress,
// the state and Ready condition. actor is nil when the task has no actor; err is
// set when the actor could not be looked up, in which case Ready is reported as
// Unknown.
func setActorStatus(task *v1alpha1.Task, actor *ateapipb.Actor, err error) {
	if task.Status == nil {
		task.Status = &v1alpha1.TaskStatus{}
	}
	st := task.Status
	st.Actor = ""
	st.ActorUid = ""
	terminating := task.GetMetadata().GetDeleteTime() != nil

	switch {
	case err != nil:
		slog.Warn("could not read task actor", "atespace", task.GetMetadata().GetAtespace(), "task", task.GetMetadata().GetName(), "error", err)
		if terminating {
			setReady(task, "False", v1alpha1.ReasonTerminating, "Task is being deleted")
			return
		}
		if !v1alpha1.IsAXTaskState(st.State) {
			st.State = v1alpha1.TaskState_TASK_STATE_UNSPECIFIED
			setReady(task, "Unknown", v1alpha1.ReasonSubstrateUnavailable, err.Error())
		}
	case actor == nil:
		if terminating {
			setReady(task, "False", v1alpha1.ReasonTerminating, "Task is being deleted")
			return
		}
		// A task that finished provisioning always has an actor, so a missing one
		// was removed outside AX.
		if !v1alpha1.IsAXTaskState(st.State) {
			st.State = v1alpha1.TaskState_TASK_STATE_FAILED
			setReady(task, "False", v1alpha1.ReasonActorNotFound, "The task's actor no longer exists on Substrate")
		}
	default:
		st.Actor = actor.GetMetadata().GetName()
		st.ActorUid = actor.GetMetadata().GetUid()
		if terminating {
			if !v1alpha1.IsAXTaskState(st.State) {
				st.State, _ = actorTaskState(actor.GetStatus().GetState())
			}
			setReady(task, "False", v1alpha1.ReasonTerminating, "Task is being deleted")
			return
		}
		if v1alpha1.IsAXTaskState(st.State) {
			return
		}
		actorState := actor.GetStatus().GetState()
		taskState, readyReason := actorTaskState(actorState)
		st.State = taskState
		switch readyReason {
		case v1alpha1.ReasonTaskRunning:
			if conditionTrue(task, v1alpha1.ConditionWorkspaceReady) {
				setReady(task, "True", v1alpha1.ReasonTaskRunning, "Task is running and its workspace is ready")
			} else {
				setReady(task, "False", v1alpha1.ReasonWorkspaceInitializing, "Waiting for workspace setup to complete")
			}
		case v1alpha1.ReasonUnknown:
			setReady(task, "Unknown", v1alpha1.ReasonUnknown, "Actor is in state "+actorState.String())
		default:
			message := actor.GetStatus().GetCrash().GetMessage()
			if message == "" {
				message = "Actor is " + strings.ToLower(strings.TrimPrefix(actorState.String(), "ACTOR_STATE_"))
			}
			if actorState == ateapipb.ActorState_ACTOR_STATE_CRASHED && st.StatusDetail == nil {
				st.StatusDetail = &v1alpha1.StatusDetail{
					Reason: v1alpha1.TaskStatusReason_TASK_STATUS_REASON_SANDBOX_CRASHED,
					Detail: message,
				}
			}
			setReady(task, "False", readyReason, message)
		}
	}
}

// actorTaskState maps an actor's state to the task's TaskState and Ready condition reason.
func actorTaskState(state ateapipb.ActorState) (v1alpha1.TaskState, string) {
	switch state {
	case ateapipb.ActorState_ACTOR_STATE_RUNNING:
		return v1alpha1.TaskState_TASK_STATE_WORKING, v1alpha1.ReasonTaskRunning
	case ateapipb.ActorState_ACTOR_STATE_RESUMING, ateapipb.ActorState_ACTOR_STATE_REVERTING:
		return v1alpha1.TaskState_TASK_STATE_QUEUED, v1alpha1.ReasonResuming
	case ateapipb.ActorState_ACTOR_STATE_SUSPENDING:
		return v1alpha1.TaskState_TASK_STATE_SUSPENDING, v1alpha1.ReasonSuspending
	case ateapipb.ActorState_ACTOR_STATE_SUSPENDED:
		return v1alpha1.TaskState_TASK_STATE_SUSPENDED, v1alpha1.ReasonSuspended
	case ateapipb.ActorState_ACTOR_STATE_CRASHED:
		return v1alpha1.TaskState_TASK_STATE_FAILED, v1alpha1.ReasonFailed
	case ateapipb.ActorState_ACTOR_STATE_DELETING:
		return v1alpha1.TaskState_TASK_STATE_UNSPECIFIED, v1alpha1.ReasonTerminating
	default:
		return v1alpha1.TaskState_TASK_STATE_UNSPECIFIED, v1alpha1.ReasonUnknown
	}
}

// conditionTrue reports whether the task has the given condition with status
// True.
func conditionTrue(task *v1alpha1.Task, condType string) bool {
	for _, c := range task.GetStatus().GetConditions() {
		if c.GetType() == condType {
			return c.GetStatus() == "True"
		}
	}
	return false
}

// setReady sets the task's Ready condition. Its update time changes only when
// the condition does, so that repeated reads report a stable timestamp.
func setReady(task *v1alpha1.Task, condStatus, reason, message string) {
	for _, c := range task.Status.Conditions {
		if c.GetType() != v1alpha1.ConditionReady {
			continue
		}
		if c.Status != condStatus || c.Reason != reason || c.Message != message {
			c.Status, c.Reason, c.Message = condStatus, reason, message
			c.UpdateTime = timestamppb.Now()
		}
		return
	}
	task.Status.Conditions = append(task.Status.Conditions, &v1alpha1.Condition{
		Type:       v1alpha1.ConditionReady,
		Status:     condStatus,
		Reason:     reason,
		Message:    message,
		UpdateTime: timestamppb.Now(),
	})
}

// storedStatus returns the part of a task's status that AX owns and stores. The
// actor name, actor UID, and the state, status detail, and Ready condition that
// follow from the actor's live state are dropped. st is not modified.
func storedStatus(st *v1alpha1.TaskStatus) *v1alpha1.TaskStatus {
	out := &v1alpha1.TaskStatus{}
	if st != nil {
		out = proto.Clone(st).(*v1alpha1.TaskStatus)
	}
	out.Actor = ""
	out.ActorUid = ""
	if !v1alpha1.IsAXTaskState(out.State) {
		out.State = v1alpha1.TaskState_TASK_STATE_UNSPECIFIED
		out.StatusDetail = nil
		conds := out.Conditions[:0]
		for _, c := range out.Conditions {
			if c.GetType() != v1alpha1.ConditionReady {
				conds = append(conds, c)
			}
		}
		out.Conditions = conds
	}
	return out
}

// fetchWorkspace loads the Workspace the task binds, or nil when the task
// binds none or the Workspace does not exist.
func (s *Server) fetchWorkspace(ctx context.Context, atespace string, task *v1alpha1.Task) *v1alpha1.Workspace {
	name := task.GetSpec().GetWorkspace().GetName()
	if name == "" {
		return nil
	}
	ws, err := s.store.GetWorkspace(ctx, atespace, name)
	if err != nil {
		return nil
	}
	return ws
}

// fetchGateway loads the Gateway the task binds, or nil when the task binds
// none or the Gateway cannot be loaded.
func (s *Server) fetchGateway(ctx context.Context, atespace string, task *v1alpha1.Task) *v1alpha1.Gateway {
	name := task.GetSpec().GetGateway().GetName()
	if name == "" {
		return nil
	}
	gw, err := s.store.GetGateway(ctx, atespace, name)
	if err != nil {
		return nil
	}
	return gw
}

// --- Models ---

func (s *Server) GetModel(ctx context.Context, req *v1alpha1.GetModelRequest) (*v1alpha1.Model, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "missing request")
	}
	atespace := req.Atespace
	if atespace == "" {
		atespace = v1alpha1.DefaultAtespace
	}
	model, err := s.store.GetModel(ctx, atespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "model %q not found in atespace %q", req.Name, atespace)
		}
		return nil, status.Errorf(codes.Internal, "getting model: %v", err)
	}
	return model, nil
}

func (s *Server) ListModels(ctx context.Context, req *v1alpha1.ListModelsRequest) (*v1alpha1.ListModelsResponse, error) {
	atespace := ""
	if req != nil {
		atespace = req.Atespace
	}
	models, err := s.store.ListModels(ctx, atespace)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "listing models: %v", err)
	}
	return &v1alpha1.ListModelsResponse{Models: models}, nil
}

func (s *Server) UpdateModel(ctx context.Context, req *v1alpha1.UpdateModelRequest) (*v1alpha1.Model, error) {
	if req == nil || req.Model == nil {
		return nil, status.Error(codes.InvalidArgument, "model required")
	}
	if err := apivalidation.ValidateModel(req.Model); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	req.Model.Metadata = defaultMetadata(req.Model.Metadata, func(atespace, name string) *v1alpha1.ObjectMeta {
		existing, err := s.store.GetModel(ctx, atespace, name)
		if err != nil {
			return nil
		}
		return existing.GetMetadata()
	})

	atespace := req.Model.Metadata.Atespace
	modelName := req.Model.Metadata.Name

	// Acquire exclusive lock for this model.
	unlock, err := s.locker.Lock(ctx, "model", atespace, modelName)
	if err != nil {
		return nil, status.Errorf(codes.Aborted, "locking model %s/%s: %v", atespace, modelName, err)
	}
	defer unlock()

	if err := s.store.SaveModel(ctx, req.Model); err != nil {
		return nil, status.Errorf(codes.Internal, "saving model: %v", err)
	}
	return req.Model, nil
}

func (s *Server) DeleteModel(ctx context.Context, req *v1alpha1.DeleteModelRequest) (*v1alpha1.DeleteModelResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "missing request")
	}
	atespace := req.Atespace
	if atespace == "" {
		atespace = v1alpha1.DefaultAtespace
	}
	modelName := req.Name

	// Acquire exclusive lock for this model.
	unlock, err := s.locker.Lock(ctx, "model", atespace, modelName)
	if err != nil {
		return nil, status.Errorf(codes.Aborted, "locking model %s/%s: %v", atespace, modelName, err)
	}
	defer unlock()

	if err := s.store.DeleteModel(ctx, atespace, modelName); err != nil {
		return nil, status.Errorf(codes.Internal, "deleting model: %v", err)
	}
	return &v1alpha1.DeleteModelResponse{}, nil
}

func defaultMetadata(meta *v1alpha1.ObjectMeta, existing func(atespace, name string) *v1alpha1.ObjectMeta) *v1alpha1.ObjectMeta {
	if meta == nil {
		meta = &v1alpha1.ObjectMeta{}
	}
	if meta.Atespace == "" {
		meta.Atespace = v1alpha1.DefaultAtespace
	}
	if meta.CreateTime == nil {
		var prev *v1alpha1.ObjectMeta
		if existing != nil {
			prev = existing(meta.Atespace, meta.Name)
		}
		if prev.GetCreateTime() != nil {
			meta.CreateTime = prev.GetCreateTime()
		} else {
			meta.CreateTime = timestamppb.Now()
		}
	}
	return meta
}
