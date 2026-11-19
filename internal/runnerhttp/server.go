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

// Package runnerhttp serves ax-server's internal pull-based runner HTTP
// protocol (:8081) and implements the in-memory [controller.SetupCoordinator]
// used to drive temporary Workspace setup actors.
//
// Every request on :8081 arrives through Agent Substrate's egress gateway
// carrying a Substrate-issued Actor JWT in the Authorization header. Task
// actors additionally authenticate with a per-actor X-AX-Runner-Token minted on
// their initial assignment claim.
package runnerhttp

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/ax/internal/apivalidation"
	"github.com/google/ax/internal/controller"
	"github.com/google/ax/internal/lifecycle"
	"github.com/google/ax/internal/lock"
	"github.com/google/ax/internal/store"
	runnerv1 "github.com/google/ax/pkg/apis/runner/v1"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	// ClaimAssignmentPath is the HTTP POST path where ax-task-runner claims its
	// current assignment.
	ClaimAssignmentPath = "/runner/v1/assignment:claim"
	// ReportEventsPath is the HTTP POST path where ax-task-runner reports
	// ordered setup and command execution events.
	ReportEventsPath = "/runner/v1/events"
	// RunnerTokenHeader carries the per-actor bearer token minted on the
	// initial assignment claim.
	RunnerTokenHeader = "X-AX-Runner-Token"
	// DefaultParkTimeout bounds the background SuspendActor call issued after a
	// task command exits.
	DefaultParkTimeout = 30 * time.Second

	contentTypeProtobuf = "application/protobuf"
	contentTypeJSON     = "application/json"
	maxRequestBodyBytes = 1 << 20 // 1 MiB
)

var (
	errActorUIDMismatch = errors.New("actor UID does not match active task actor")
	jsonMarshalOpts     = protojson.MarshalOptions{}
	jsonUnmarshalOpts   = protojson.UnmarshalOptions{DiscardUnknown: true}
)

// WorkspaceReader loads the latest Workspace specification (such as dynamic
// spec.mcp servers) when building a RUN assignment. Satisfied by [store.Store].
type WorkspaceReader interface {
	GetWorkspace(ctx context.Context, atespace, name string) (*v1alpha1.Workspace, error)
}

// ActorParker suspends a task's sandbox actor after its command exits.
// Satisfied by [*github.com/google/ax/internal/substrate.Client].
type ActorParker interface {
	SuspendActor(ctx context.Context, atespace, actorName string) error
}

// Options configures a runner HTTP [Server].
type Options struct {
	// TaskApplier executes atomic per-task state transitions. Required for task
	// routing.
	TaskApplier store.TaskApplier
	// Store reads bound Workspaces outside TaskApplier transactions so RUN
	// assignments always see fresh Workspace.spec.mcp definitions.
	Store WorkspaceReader
	// Verifier authenticates the calling actor from the inbound HTTP request.
	// Required.
	Verifier ActorVerifier
	// ActorParker suspends finished task actors asynchronously after flushing
	// the HTTP 200 response for CommandExited. Optional in unit tests.
	ActorParker ActorParker
	// Locker serializes asynchronous actor parking against ResumeTask,
	// SuspendTask, and DeleteTask so a fast one-shot command does not park its
	// actor while ResumeTask is still polling /readyz?check=workspace.
	Locker lock.Locker
	// MintToken overrides token generation in tests; defaults to
	// lifecycle.DefaultMintToken.
	MintToken lifecycle.TokenMinter
	// ParkTimeout bounds background SuspendActor calls; defaults to
	// DefaultParkTimeout (30s).
	ParkTimeout time.Duration
}

type setupEntry struct {
	actorUID  string
	workspace *v1alpha1.Workspace
	done      chan struct{}
	err       error
	finished  bool
}

// Server serves the internal :8081 runner HTTP endpoints and coordinates
// temporary Workspace setup actors in memory.
type Server struct {
	applier     store.TaskApplier
	store       WorkspaceReader
	verifier    ActorVerifier
	actorParker ActorParker
	locker      lock.Locker
	mintToken   lifecycle.TokenMinter
	parkTimeout time.Duration
	mux         *http.ServeMux

	setupMu sync.Mutex
	setups  map[string]*setupEntry

	bgWG sync.WaitGroup
}

var _ controller.SetupCoordinator = (*Server)(nil)

// NewServer constructs a runner control HTTP server from opts.
func NewServer(opts Options) *Server {
	if opts.Verifier == nil {
		panic("runnerhttp: NewServer requires a non-nil ActorVerifier")
	}
	mint := opts.MintToken
	if mint == nil {
		mint = lifecycle.DefaultMintToken
	}
	parkTimeout := opts.ParkTimeout
	if parkTimeout <= 0 {
		parkTimeout = DefaultParkTimeout
	}

	s := &Server{
		applier:     opts.TaskApplier,
		store:       opts.Store,
		verifier:    opts.Verifier,
		actorParker: opts.ActorParker,
		locker:      opts.Locker,
		mintToken:   mint,
		parkTimeout: parkTimeout,
		setups:      make(map[string]*setupEntry),
		mux:         http.NewServeMux(),
	}
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("POST "+ClaimAssignmentPath, s.handleClaimAssignment)
	s.mux.HandleFunc("POST "+ReportEventsPath, s.handleReportEvents)
	return s
}

// Handler returns the HTTP handler serving /healthz, /runner/v1/assignment:claim,
// and /runner/v1/events.
func (s *Server) Handler() http.Handler {
	return s.mux
}

// WaitBackground blocks until any in-flight asynchronous actor-parking
// goroutines spawned after CommandExited have completed.
func (s *Server) WaitBackground() {
	s.bgWG.Wait()
}

// RegisterSetup records an expected Workspace setup actor incarnation prior to
// ResumeActor so ClaimAssignment can immediately serve ASSIGNMENT_MODE_SETUP.
func (s *Server) RegisterSetup(_ context.Context, atespace, setupActor, actorUID string, ws *v1alpha1.Workspace) error {
	atespace = cmp.Or(atespace, v1alpha1.DefaultAtespace)
	if setupActor == "" || actorUID == "" {
		return errors.New("runnerhttp: setupActor and actorUID are required")
	}
	var cloned *v1alpha1.Workspace
	if ws != nil {
		cloned = proto.Clone(ws).(*v1alpha1.Workspace)
	}
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	s.setups[setupMapKey(atespace, setupActor)] = &setupEntry{
		actorUID:  actorUID,
		workspace: cloned,
		done:      make(chan struct{}),
	}
	return nil
}

// AwaitSetup blocks until the named Workspace setup actor reports
// setup_finished or ctx is canceled.
func (s *Server) AwaitSetup(ctx context.Context, atespace, setupActor string) error {
	atespace = cmp.Or(atespace, v1alpha1.DefaultAtespace)
	s.setupMu.Lock()
	entry, ok := s.setups[setupMapKey(atespace, setupActor)]
	s.setupMu.Unlock()
	if !ok {
		return fmt.Errorf("workspace setup %s/%s is not registered", atespace, setupActor)
	}
	select {
	case <-entry.done:
		s.setupMu.Lock()
		err := entry.err
		s.setupMu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// DeleteSetup removes the in-memory setup coordination entry for setupActor.
func (s *Server) DeleteSetup(_ context.Context, atespace, setupActor string) error {
	atespace = cmp.Or(atespace, v1alpha1.DefaultAtespace)
	s.setupMu.Lock()
	delete(s.setups, setupMapKey(atespace, setupActor))
	s.setupMu.Unlock()
	return nil
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) handleClaimAssignment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := s.verifier.VerifyActorRequest(ctx, r)
	if err != nil {
		http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
		return
	}

	var req runnerv1.ClaimAssignmentRequest
	if err := decodeProtoRequest(w, r, &req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}

	if strings.HasPrefix(id.ActorName, apivalidation.SetupActorPrefix) {
		s.handleSetupClaim(w, r, id)
		return
	}

	if s.applier == nil {
		http.Error(w, "task applier not configured", http.StatusInternalServerError)
		return
	}

	key := store.TaskKey{Atespace: id.Atespace, Name: id.ActorName}.Normalized()
	// Pre-fetch the bound Workspace outside Apply so no external store I/O runs
	// while holding the task row lock, and Workspace.spec.mcp stays fresh.
	var ws *v1alpha1.Workspace
	if existing, err := s.applier.GetTaskRow(ctx, key); err == nil && s.store != nil {
		if wsName := existing.Task.GetSpec().GetWorkspace().GetName(); wsName != "" {
			ws, _ = s.store.GetWorkspace(ctx, key.Atespace, wsName)
		}
	}

	presentedToken := r.Header.Get(RunnerTokenHeader)
	var resp *runnerv1.ClaimAssignmentResponse
	_, err = s.applier.Apply(ctx, key, func(current *store.TaskRow, now time.Time) (*store.TaskRow, error) {
		if current == nil {
			return nil, store.ErrNotFound
		}
		if current.ActorUID == "" || current.ActorUID != id.ActorUID {
			return nil, errActorUIDMismatch
		}
		next, claimResp, claimErr := lifecycle.Claim(current, &req, ws, presentedToken, s.mintToken, now)
		if claimErr != nil {
			return nil, claimErr
		}
		resp = claimResp
		return next, nil
	})
	if err != nil {
		writeTaskApplyError(w, err)
		return
	}
	writeProtoResponse(w, r, resp)
}

func (s *Server) handleSetupClaim(w http.ResponseWriter, r *http.Request, id ActorIdentity) {
	s.setupMu.Lock()
	entry, ok := s.setups[setupMapKey(id.Atespace, id.ActorName)]
	if !ok || entry.actorUID != id.ActorUID {
		s.setupMu.Unlock()
		http.Error(w, "unrecognized workspace setup actor", http.StatusUnauthorized)
		return
	}
	finished := entry.finished
	var ws *v1alpha1.Workspace
	if entry.workspace != nil {
		ws = proto.Clone(entry.workspace).(*v1alpha1.Workspace)
	}
	s.setupMu.Unlock()

	if finished {
		writeProtoResponse(w, r, &runnerv1.ClaimAssignmentResponse{
			Mode: runnerv1.AssignmentMode_ASSIGNMENT_MODE_IDLE,
		})
		return
	}
	writeProtoResponse(w, r, &runnerv1.ClaimAssignmentResponse{
		Mode:      runnerv1.AssignmentMode_ASSIGNMENT_MODE_SETUP,
		Workspace: ws,
	})
}

func (s *Server) handleReportEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := s.verifier.VerifyActorRequest(ctx, r)
	if err != nil {
		http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
		return
	}

	var req runnerv1.ReportEventsRequest
	if err := decodeProtoRequest(w, r, &req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}

	if strings.HasPrefix(id.ActorName, apivalidation.SetupActorPrefix) {
		s.handleSetupEvents(w, r, id, &req)
		return
	}

	if s.applier == nil {
		http.Error(w, "task applier not configured", http.StatusInternalServerError)
		return
	}

	key := store.TaskKey{Atespace: id.Atespace, Name: id.ActorName}.Normalized()
	presentedToken := r.Header.Get(RunnerTokenHeader)
	var (
		resp      *runnerv1.ReportEventsResponse
		parkActor bool
	)
	_, err = s.applier.Apply(ctx, key, func(current *store.TaskRow, now time.Time) (*store.TaskRow, error) {
		if current == nil {
			return nil, store.ErrNotFound
		}
		if current.ActorUID == "" || current.ActorUID != id.ActorUID {
			return nil, errActorUIDMismatch
		}
		next, eventsResp, shouldPark, applyErr := lifecycle.ApplyEvents(current, &req, presentedToken, now)
		if applyErr != nil {
			return nil, applyErr
		}
		resp = eventsResp
		parkActor = shouldPark
		return next, nil
	})
	if err != nil {
		writeTaskApplyError(w, err)
		return
	}

	writeProtoResponse(w, r, resp)
	// Flush HTTP 200 before parking so Substrate does not freeze the container
	// while the runner is still reading the response.
	_ = http.NewResponseController(w).Flush()

	if parkActor && s.actorParker != nil {
		s.parkActorAsync(ctx, key.Atespace, key.Name)
	}
}

func (s *Server) handleSetupEvents(w http.ResponseWriter, r *http.Request, id ActorIdentity, req *runnerv1.ReportEventsRequest) {
	s.setupMu.Lock()
	entry, ok := s.setups[setupMapKey(id.Atespace, id.ActorName)]
	if !ok || entry.actorUID != id.ActorUID {
		s.setupMu.Unlock()
		http.Error(w, "unrecognized workspace setup actor", http.StatusUnauthorized)
		return
	}

	var maxSeq int64
	for _, ev := range req.GetEvents() {
		if ev.GetSeq() > maxSeq {
			maxSeq = ev.GetSeq()
		}
		sf := ev.GetSetupFinished()
		if sf == nil || entry.finished {
			continue
		}
		if !sf.GetOk() {
			msg := cmp.Or(sf.GetMessage(), "workspace setup failed")
			entry.err = errors.New(msg)
		}
		entry.finished = true
		close(entry.done)
	}
	s.setupMu.Unlock()

	writeProtoResponse(w, r, &runnerv1.ReportEventsResponse{
		AckSeq: maxSeq,
		Next: &runnerv1.ClaimAssignmentResponse{
			Mode: runnerv1.AssignmentMode_ASSIGNMENT_MODE_IDLE,
		},
	})
}

func (s *Server) parkActorAsync(ctx context.Context, atespace, actorName string) {
	s.bgWG.Add(1)
	go func() {
		defer s.bgWG.Done()
		parkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.parkTimeout)
		defer cancel()
		if s.locker != nil {
			// Wait for any active ResumeTask/SuspendTask/DeleteTask lock on
			// this task to finish so SuspendActor does not race with
			// ResumeTask's /readyz?check=workspace probe via atenet-router.
			unlock, err := s.locker.Lock(parkCtx, "task", atespace, actorName)
			if err != nil {
				slog.Warn("failed locking task before parking finished actor", "atespace", atespace, "actor", actorName, "error", err)
				return
			}
			defer unlock()
			row, err := s.applier.GetTaskRow(parkCtx, store.TaskKey{Atespace: atespace, Name: actorName})
			if err != nil || row == nil || row.Runnable || !lifecycle.IsExecutionTerminal(row.Task) {
				return
			}
		}
		if err := s.actorParker.SuspendActor(parkCtx, atespace, actorName); err != nil {
			slog.Warn("failed to park finished task actor", "atespace", atespace, "actor", actorName, "error", err)
		}
	}()
}

func setupMapKey(atespace, actorName string) string {
	return atespace + "/" + actorName
}

func writeTaskApplyError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, lifecycle.ErrUnauthorized), errors.Is(err, errActorUIDMismatch):
		http.Error(w, err.Error(), http.StatusUnauthorized)
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func decodeProtoRequest(w http.ResponseWriter, r *http.Request, msg proto.Message) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	ct := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]))
	if ct == contentTypeProtobuf {
		return proto.Unmarshal(body, msg)
	}
	return jsonUnmarshalOpts.Unmarshal(body, msg)
}

func writeProtoResponse(w http.ResponseWriter, r *http.Request, msg proto.Message) {
	accept := strings.ToLower(r.Header.Get("Accept"))
	if strings.Contains(accept, contentTypeProtobuf) {
		raw, err := proto.Marshal(msg)
		if err != nil {
			http.Error(w, "marshaling response: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", contentTypeProtobuf)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
		return
	}

	raw, err := jsonMarshalOpts.Marshal(msg)
	if err != nil {
		http.Error(w, "marshaling response: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}
