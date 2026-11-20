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
	"errors"
	"io"
	"time"

	"github.com/google/ax/internal/apivalidation"
	"github.com/google/ax/internal/tasklogs"
	"github.com/google/ax/pkg/apis/v1alpha1"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// StreamTaskLogs follows one worker at a time, polling Substrate for moves.
// No request is sent to the actor, so a suspended actor is never woken.
func (s *Server) StreamTaskLogs(req *v1alpha1.StreamTaskLogsRequest, stream grpc.ServerStreamingServer[v1alpha1.StreamTaskLogsResponse]) error {
	if s.taskLogs == nil {
		return status.Error(codes.Unimplemented, "task logs are disabled; set --enable-task-logs")
	}
	if req == nil {
		return status.Error(codes.InvalidArgument, "missing request")
	}
	ns := req.Atespace
	if ns == "" {
		ns = v1alpha1.DefaultAtespace
	}
	if apivalidation.ValidateName(ns) != nil || apivalidation.ValidateName(req.Name) != nil {
		return status.Error(codes.InvalidArgument, "invalid task name or atespace")
	}
	ctx := stream.Context()
	if _, err := s.GetTask(ctx, &v1alpha1.GetTaskRequest{Atespace: ns, Name: req.Name}); err != nil {
		return err
	}
	actor, err := s.substrate.GetActor(ctx, ns, req.Name)
	if err != nil {
		return err
	}
	if actor == nil || actor.Status == nil || actor.Status.State != ateapipb.ActorState_ACTOR_STATE_RUNNING {
		return status.Error(codes.FailedPrecondition, "task actor must be running; requesting logs does not resume it")
	}
	if actor.Metadata == nil || actor.Metadata.Uid == "" {
		return status.Error(codes.FailedPrecondition, "actor has no UID")
	}
	uid := actor.Metadata.Uid
	var records <-chan tasklogs.Record
	var current *ateapipb.WorkerAssignment
	stop := func() {}
	defer func() { stop() }()
	open := func(a *ateapipb.Actor) error {
		assignment := a.GetStatus().GetWorkerAssignment()
		if assignment == nil || assignment.WorkerNamespace == "" || assignment.WorkerPod == "" {
			return status.Error(codes.FailedPrecondition, "running actor has no worker pod assignment")
		}
		body, err := s.taskLogs.Open(ctx, assignment.WorkerNamespace, assignment.WorkerPod, time.Now())
		if err != nil {
			return err
		}
		records, stop = tasklogs.Read(ctx, body, uid)
		current = assignment
		return nil
	}
	if err := open(actor); err != nil {
		return err
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		case record := <-records:
			if record.Err != nil {
				if !errors.Is(record.Err, io.EOF) && !errors.Is(record.Err, io.ErrUnexpectedEOF) {
					return status.Error(codes.Unavailable, "reading worker logs")
				}
				stop()
				stop = func() {}
				records, current = nil, nil
				continue
			}
			if err := stream.Send(&v1alpha1.StreamTaskLogsResponse{Line: record.Line}); err != nil {
				return err
			}
		case <-ticker.C:
			actor, err = s.substrate.GetActor(ctx, ns, req.Name)
			if err != nil {
				return err
			}
			if actor.GetMetadata().GetUid() != uid {
				return status.Error(codes.Aborted, "actor was recreated; reconnect for its logs")
			}
			assignment := actor.GetStatus().GetWorkerAssignment()
			running := actor.Status != nil && actor.Status.State == ateapipb.ActorState_ACTOR_STATE_RUNNING
			if current != nil && (!running || current.GetWorkerNamespace() != assignment.GetWorkerNamespace() || current.GetWorkerPod() != assignment.GetWorkerPod() || current.GetWorkerPodUid() != assignment.GetWorkerPodUid()) {
				stop()
				stop = func() {}
				records, current = nil, nil
			}
			if running && current == nil {
				if err := open(actor); err != nil && status.Code(err) != codes.Unavailable {
					return err
				}
			}
		}
	}
}
