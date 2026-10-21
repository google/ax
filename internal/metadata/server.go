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

package metadata

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/agent-substrate/env/guest"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"gopkg.in/yaml.v3"
)

// Server is the cloud-style metadata server running inside the task actor container.
// It runs two listeners. The probe listener on all interfaces serves /healthz and
// /readyz, multiplexed with the guest gRPC services when debug is enabled. The
// metadata listener serves the /metadata endpoints on loopback only, so they are
// reachable from inside the container and nowhere else.
type Server struct {
	server         *http.Server
	metadataServer *http.Server
	probeAddr      string // bound address of the probe listener, set by Start
	metadataAddr   string // bound address of the metadata listener, set by Start
	grpcServer     *grpc.Server
	grpcCleanup    func()
	mu             sync.RWMutex
	task           *v1alpha1.Task
	workspace      *v1alpha1.Workspace
	workspaceReady bool
}

// ServerOptions configures optional settings for the metadata and guest server.
type ServerOptions struct {
	WorkspacePath string
	LogDir        string
	// MetadataPort is the loopback port for the /metadata endpoints. Zero picks
	// a free port; read the result from MetadataURL after Start.
	MetadataPort int
}

// NewServer creates a new metadata and guest server serving the task and its
// bound workspace, which may be nil. port is the probe and guest port; the
// metadata endpoints use ServerOptions.MetadataPort on loopback. A zero port
// picks a free one; read the result from ProbeAddr or MetadataURL after Start.
func NewServer(port int, task *v1alpha1.Task, ws *v1alpha1.Workspace, opts ...ServerOptions) *Server {
	var opt ServerOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	s := &Server{
		task:      task,
		workspace: ws,
	}

	// Guest services expose process execution and file access inside the container,
	// so they are only served when the task opts in via spec.debug.
	if task.GetSpec().GetDebug() {
		guestCfg := guest.DefaultConfig()
		if opt.WorkspacePath != "" {
			guestCfg.Workspace = opt.WorkspacePath
		}
		if opt.LogDir != "" {
			guestCfg.LogDir = opt.LogDir
		}

		grpcServer, grpcCleanup, err := guest.NewServer(guestCfg)
		if err != nil {
			slog.Error("failed to initialize guest gRPC server", "error", err)
		} else {
			s.grpcServer = grpcServer
			s.grpcCleanup = grpcCleanup
			slog.Info("guest services enabled (spec.debug is true)")
		}
	} else {
		slog.Info("guest services disabled; set spec.debug: true on the Task to enable ax ssh")
	}

	// Probe endpoints, reachable from outside the container.
	probeMux := http.NewServeMux()
	probeMux.HandleFunc("/healthz", s.handleHealthz)
	probeMux.HandleFunc("/readyz", s.handleReadyz)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.grpcServer != nil && r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
			s.grpcServer.ServeHTTP(w, r)
			return
		}
		probeMux.ServeHTTP(w, r)
	})

	s.server = &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: handler,
	}
	s.server.Protocols = new(http.Protocols)
	s.server.Protocols.SetHTTP1(true)
	s.server.Protocols.SetUnencryptedHTTP2(true)

	// Metadata endpoints, for the task command only:
	// /metadata/v1alpha1/ax/task        the Task
	// /metadata/v1alpha1/ax/workspace   the bound Workspace, as YAML
	metadataMux := http.NewServeMux()
	metadataMux.HandleFunc("/metadata/v1alpha1/ax/task", s.handleTask)
	metadataMux.HandleFunc("/metadata/v1alpha1/ax/workspace", s.handleWorkspace)

	s.metadataServer = &http.Server{
		Addr:    net.JoinHostPort("127.0.0.1", fmt.Sprint(opt.MetadataPort)),
		Handler: metadataMux,
	}

	return s
}

// ProbeAddr is the address the probe and guest listener is bound to. It is
// empty until Start succeeds.
func (s *Server) ProbeAddr() string {
	return s.probeAddr
}

// MetadataURL is the base URL of the metadata endpoints, as handed to the task
// command in AX_METADATA_URL. It is empty until Start succeeds.
func (s *Server) MetadataURL() string {
	if s.metadataAddr == "" {
		return ""
	}
	return "http://" + s.metadataAddr
}

// Start begins serving the probe, guest, and metadata endpoints in background goroutines.
func (s *Server) Start() error {
	lis, err := net.Listen("tcp", s.server.Addr)
	if err != nil {
		return fmt.Errorf("probe and guest server listening on %s: %w", s.server.Addr, err)
	}
	metadataLis, err := net.Listen("tcp", s.metadataServer.Addr)
	if err != nil {
		_ = lis.Close()
		return fmt.Errorf("metadata server listening on %s: %w", s.metadataServer.Addr, err)
	}

	s.probeAddr = lis.Addr().String()
	s.metadataAddr = metadataLis.Addr().String()
	slog.Info("probe and guest server started", "addr", s.probeAddr)
	slog.Info("metadata server started", "addr", s.metadataAddr)

	go func() {
		if err := s.server.Serve(lis); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("probe and guest server error", "error", err)
		}
	}()
	go func() {
		if err := s.metadataServer.Serve(metadataLis); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("metadata server error", "error", err)
		}
	}()

	return nil
}

// Stop gracefully stops the probe, guest, and metadata servers.
func (s *Server) Stop(ctx context.Context) error {
	if s.grpcCleanup != nil {
		s.grpcCleanup()
	}
	// Shut both servers down concurrently so one draining its connections does
	// not hold the other open or use up ctx's deadline.
	var wg sync.WaitGroup
	var probeErr, metadataErr error
	wg.Go(func() { probeErr = s.server.Shutdown(ctx) })
	wg.Go(func() { metadataErr = s.metadataServer.Shutdown(ctx) })
	wg.Wait()
	return errors.Join(probeErr, metadataErr)
}

// UpdateState allows dynamic updates to the current Task and Workspace.
func (s *Server) UpdateState(task *v1alpha1.Task, ws *v1alpha1.Workspace) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.task = task
	s.workspace = ws
}

// SetWorkspaceReady updates whether the maiden run workspace setup has completed.
func (s *Server) SetWorkspaceReady(ready bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workspaceReady = ready
}

// IsWorkspaceReady returns whether the workspace setup has completed.
func (s *Server) IsWorkspaceReady() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.workspaceReady
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	ready := s.workspaceReady
	s.mu.RUnlock()

	if !ready {
		http.Error(w, "workspace initializing", http.StatusServiceUnavailable)
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) handleTask(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	task := s.task
	s.mu.RUnlock()

	if task == nil {
		http.Error(w, "task metadata not available", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/yaml")
	if err := yaml.NewEncoder(w).Encode(task); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// handleWorkspace serves the bound workspace as YAML, or 404 when the task
// binds none.
func (s *Server) handleWorkspace(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	ws := s.workspace
	s.mu.RUnlock()

	if ws == nil {
		http.Error(w, "workspace metadata not available", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/yaml")
	if err := yaml.NewEncoder(w).Encode(ws); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
