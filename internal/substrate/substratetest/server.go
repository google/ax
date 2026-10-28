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

// Package substratetest provides an in-memory Agent Substrate control API for
// tests.
package substratetest

import (
	"context"
	"net"
	"sync"
	"testing"

	"github.com/google/ax/internal/substrate"
	"go.ate.dev/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Errors makes calls to a Server fail. Each non-nil field is returned by the
// matching call.
type Errors struct {
	CreateAtespace error
	CreateTemplate error
	CreateActor    error
	SuspendActor   error
	ResumeActor    error
	DeleteActor    error
}

// Server is an in-memory Agent Substrate control API. It keeps atespaces,
// actor templates and actors, and moves actors between states as Substrate
// does, without running anything. Calls it does not implement return
// Unimplemented.
type Server struct {
	ateapipb.UnimplementedControlServer

	client *substrate.Client

	mu               sync.Mutex
	atespaces        map[string]*ateapipb.Atespace
	templates        map[string]*ateapipb.ActorTemplate // by atespace/name
	actors           map[string]*ateapipb.Actor         // by atespace/name
	workerIP         string                             // empty: resumed actors get no worker
	errs             Errors
	onDeleteActor    func()
	deleteActorCalls int
}

// NewServer starts a Server with nothing in it. It stops when the test ends.
func NewServer(tb testing.TB) *Server {
	tb.Helper()
	s := &Server{
		atespaces: make(map[string]*ateapipb.Atespace),
		templates: make(map[string]*ateapipb.ActorTemplate),
		actors:    make(map[string]*ateapipb.Actor),
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("listen: %v", err)
	}
	grpcServer := grpc.NewServer()
	ateapipb.RegisterControlServer(grpcServer, s)
	go func() { _ = grpcServer.Serve(lis) }()
	tb.Cleanup(grpcServer.Stop)
	c, err := substrate.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		tb.Fatalf("substrate client: %v", err)
	}
	tb.Cleanup(func() { _ = c.Close() })
	s.client = c
	return s
}

// Client returns a client connected to s.
func (s *Server) Client() *substrate.Client {
	return s.client
}

// SetWorkerIP sets the IP of the worker ResumeActor places actors on. Until it
// is called, resumed actors have no worker, so a TaskReconciler sends no
// workspace readiness probe; point it at a local httptest.Server to test one.
func (s *Server) SetWorkerIP(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workerIP = ip
}

// SetErrors replaces the errors calls fail with.
func (s *Server) SetErrors(errs Errors) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errs = errs
}

// SetOnDeleteActor makes fn run at the start of every DeleteActor call.
func (s *Server) SetOnDeleteActor(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onDeleteActor = fn
}

// SetActor stores a copy of actor, replacing any actor of the same name, as
// Substrate might change an actor on its own.
func (s *Server) SetActor(actor *ateapipb.Actor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.actors[key(actor.GetMetadata().GetAtespace(), actor.GetMetadata().GetName())] = proto.Clone(actor).(*ateapipb.Actor)
}

// Actor returns a copy of the named actor, if it exists.
func (s *Server) Actor(atespace, name string) (*ateapipb.Actor, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	actor, ok := s.actors[key(atespace, name)]
	if !ok {
		return nil, false
	}
	return proto.Clone(actor).(*ateapipb.Actor), true
}

// Template returns a copy of the named actor template, if it exists.
func (s *Server) Template(atespace, name string) (*ateapipb.ActorTemplate, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tmpl, ok := s.templates[key(atespace, name)]
	if !ok {
		return nil, false
	}
	return proto.Clone(tmpl).(*ateapipb.ActorTemplate), true
}

// DeleteActorCalls returns how many DeleteActor calls s has received.
func (s *Server) DeleteActorCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleteActorCalls
}

func key(atespace, name string) string {
	return atespace + "/" + name
}

// lookupActor returns the actor ref names. s.mu must be held.
func (s *Server) lookupActor(ref *ateapipb.ObjectRef) (*ateapipb.Actor, error) {
	actor, ok := s.actors[key(ref.GetAtespace(), ref.GetName())]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "actor %s/%s not found", ref.GetAtespace(), ref.GetName())
	}
	return actor, nil
}

func (s *Server) CreateAtespace(_ context.Context, req *ateapipb.CreateAtespaceRequest) (*ateapipb.Atespace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.errs.CreateAtespace != nil {
		return nil, s.errs.CreateAtespace
	}
	name := req.GetAtespace().GetMetadata().GetName()
	if _, ok := s.atespaces[name]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "atespace %s already exists", name)
	}
	atespace := proto.Clone(req.GetAtespace()).(*ateapipb.Atespace)
	s.atespaces[name] = atespace
	return proto.Clone(atespace).(*ateapipb.Atespace), nil
}

func (s *Server) GetActorTemplate(_ context.Context, req *ateapipb.GetActorTemplateRequest) (*ateapipb.ActorTemplate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ref := req.GetActorTemplate()
	tmpl, ok := s.templates[key(ref.GetAtespace(), ref.GetName())]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "actor template %s/%s not found", ref.GetAtespace(), ref.GetName())
	}
	return proto.Clone(tmpl).(*ateapipb.ActorTemplate), nil
}

func (s *Server) CreateActorTemplate(_ context.Context, req *ateapipb.CreateActorTemplateRequest) (*ateapipb.ActorTemplate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.errs.CreateTemplate != nil {
		return nil, s.errs.CreateTemplate
	}
	meta := req.GetActorTemplate().GetMetadata()
	k := key(meta.GetAtespace(), meta.GetName())
	if _, ok := s.templates[k]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "actor template %s already exists", k)
	}
	tmpl := proto.Clone(req.GetActorTemplate()).(*ateapipb.ActorTemplate)
	s.templates[k] = tmpl
	return proto.Clone(tmpl).(*ateapipb.ActorTemplate), nil
}

func (s *Server) ListActorTemplates(_ context.Context, req *ateapipb.ListActorTemplatesRequest) (*ateapipb.ListActorTemplatesResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := &ateapipb.ListActorTemplatesResponse{}
	for _, tmpl := range s.templates {
		if req.GetAtespace() == "" || tmpl.GetMetadata().GetAtespace() == req.GetAtespace() {
			resp.ActorTemplates = append(resp.ActorTemplates, proto.Clone(tmpl).(*ateapipb.ActorTemplate))
		}
	}
	return resp, nil
}

func (s *Server) DeleteActorTemplate(_ context.Context, req *ateapipb.DeleteActorTemplateRequest) (*ateapipb.ActorTemplate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ref := req.GetActorTemplate()
	k := key(ref.GetAtespace(), ref.GetName())
	tmpl, ok := s.templates[k]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "actor template %s not found", k)
	}
	delete(s.templates, k)
	return tmpl, nil
}

// CreateActor creates the actor suspended.
func (s *Server) CreateActor(_ context.Context, req *ateapipb.CreateActorRequest) (*ateapipb.Actor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.errs.CreateActor != nil {
		return nil, s.errs.CreateActor
	}
	meta := req.GetActor().GetMetadata()
	k := key(meta.GetAtespace(), meta.GetName())
	if _, ok := s.actors[k]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "actor %s already exists", k)
	}
	actor := proto.Clone(req.GetActor()).(*ateapipb.Actor)
	actor.Status = &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_SUSPENDED}
	s.actors[k] = actor
	return proto.Clone(actor).(*ateapipb.Actor), nil
}

func (s *Server) SuspendActor(_ context.Context, req *ateapipb.SuspendActorRequest) (*ateapipb.SuspendActorResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.errs.SuspendActor != nil {
		return nil, s.errs.SuspendActor
	}
	actor, err := s.lookupActor(req.GetActor())
	if err != nil {
		return nil, err
	}
	actor.Status = &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_SUSPENDED}
	return &ateapipb.SuspendActorResponse{}, nil
}

func (s *Server) ResumeActor(_ context.Context, req *ateapipb.ResumeActorRequest) (*ateapipb.ResumeActorResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.errs.ResumeActor != nil {
		return nil, s.errs.ResumeActor
	}
	actor, err := s.lookupActor(req.GetActor())
	if err != nil {
		return nil, err
	}
	actor.Status = &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING}
	if s.workerIP != "" {
		actor.Status.WorkerAssignment = &ateapipb.WorkerAssignment{WorkerPod: "worker-pod-1", WorkerPodIps: []string{s.workerIP}}
	}
	return &ateapipb.ResumeActorResponse{Actor: proto.Clone(actor).(*ateapipb.Actor), Resumed: true}, nil
}

// RevertActor leaves the actor suspended, as if restored from its last
// snapshot.
func (s *Server) RevertActor(_ context.Context, req *ateapipb.RevertActorRequest) (*ateapipb.RevertActorResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	actor, err := s.lookupActor(req.GetActor())
	if err != nil {
		return nil, err
	}
	actor.Status = &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_SUSPENDED}
	return &ateapipb.RevertActorResponse{Actor: proto.Clone(actor).(*ateapipb.Actor)}, nil
}

// DeleteActor removes the actor at once, so the client's follow-up GetActor
// finds it gone without polling. The SetOnDeleteActor callback runs without
// s.mu held, so it may call s or block.
func (s *Server) DeleteActor(_ context.Context, req *ateapipb.DeleteActorRequest) (*ateapipb.Actor, error) {
	s.mu.Lock()
	s.deleteActorCalls++
	onDelete, delErr := s.onDeleteActor, s.errs.DeleteActor
	s.mu.Unlock()
	if onDelete != nil {
		onDelete()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if delErr != nil {
		return nil, delErr
	}
	ref := req.GetActor()
	actor, err := s.lookupActor(ref)
	if err != nil {
		return nil, err
	}
	delete(s.actors, key(ref.GetAtespace(), ref.GetName()))
	return actor, nil
}

func (s *Server) GetActor(_ context.Context, req *ateapipb.GetActorRequest) (*ateapipb.Actor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	actor, err := s.lookupActor(req.GetActor())
	if err != nil {
		return nil, err
	}
	return proto.Clone(actor).(*ateapipb.Actor), nil
}

func (s *Server) ListActors(_ context.Context, req *ateapipb.ListActorsRequest) (*ateapipb.ListActorsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := &ateapipb.ListActorsResponse{}
	for _, actor := range s.actors {
		if req.GetAtespace() == "" || actor.GetMetadata().GetAtespace() == req.GetAtespace() {
			resp.Actors = append(resp.Actors, proto.Clone(actor).(*ateapipb.Actor))
		}
	}
	return resp, nil
}
