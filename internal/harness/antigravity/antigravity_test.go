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

package antigravity

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/ax/internal/harness/harnesstest"
	"github.com/google/ax/proto"
)

var antigravityHarnessConfig = []byte(`{"system_instructions":"be terse"}`)

func TestRun_AutoStartFalse_ServerOK_Succeeds(t *testing.T) {
	srv := &harnesstest.MockHarnessServer{
		Outputs: []*proto.Message{harnesstest.ThoughtText("Analyzing"), harnesstest.AssistantText("Hello world")},
	}
	harnessClient, err := New(context.Background(), harnesstest.StartHarnessServer(t, srv), false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	exec, err := harnessClient.Start(context.Background(), "conv-test", antigravityHarnessConfig)
	if err != nil {
		t.Fatalf("failed to start execution: %v", err)
	}
	defer exec.Close(context.Background())

	if err := exec.Queue(context.Background(), harnesstest.UserText("Hi")); err != nil {
		t.Fatalf("failed to queue message: %v", err)
	}

	handler := &harnesstest.MockHandler{}
	if err := exec.Run(context.Background(), handler); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if !handler.IsDone() {
		t.Error("expected OnComplete to be called")
	}
	msgs := handler.Collected()
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if got := msgs[0].GetContent().GetThought().GetSummary()[0].GetText().GetText(); got != "Analyzing" {
		t.Errorf("expected 'Analyzing', got %q", got)
	}
	if got := msgs[1].GetContent().GetText().GetText(); got != "Hello world" {
		t.Errorf("expected 'Hello world', got %q", got)
	}
	// The harness propagated the conversation id and config to the server.
	convID, _, harnessConfig, _ := srv.Received()
	if convID != "conv-test" {
		t.Errorf("server got convID=%q, want conv-test", convID)
	}
	if !bytes.Equal(harnessConfig, antigravityHarnessConfig) {
		t.Errorf("server got harnessConfig=%q, want %q", harnessConfig, antigravityHarnessConfig)
	}
}

func TestRun_AutoStartFalse_ServerErrorFrame_Fails(t *testing.T) {
	srv := &harnesstest.MockHarnessServer{FailConnect: true, ErrMessage: "internal mock server crash"}
	harnessClient, err := New(context.Background(), harnesstest.StartHarnessServer(t, srv), false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	exec, _ := harnessClient.Start(context.Background(), "conv-test", antigravityHarnessConfig)
	defer exec.Close(context.Background())

	if err := exec.Queue(context.Background(), harnesstest.UserText("Hi")); err != nil {
		t.Fatalf("failed to queue message: %v", err)
	}

	err = exec.Run(context.Background(), &harnesstest.MockHandler{})
	if err == nil {
		t.Fatal("expected error from Run(), got nil")
	}
	if !strings.Contains(err.Error(), "internal mock server crash") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestNew_AutoStartFalse_NilSidecar: autoStart=false leaves sidecar nil.
func TestNew_AutoStartFalse_NilSidecar(t *testing.T) {
	h, err := New(context.Background(), "127.0.0.1:1", false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if h.sidecar != nil {
		t.Errorf("expected sidecar to be nil, got %v", h.sidecar)
	}
}

// TestNew_AutoStartTrue_StubServer_ForksSidecar drives the real fork +
// TCPReady path with a tiny stub python.antigravity.harness_server on
// PYTHONPATH, so we don't need the AGY SDK installed to test the wiring.
// sidecar.Stop stops the forked process.
func TestNew_AutoStartTrue_StubServer_ForksSidecar(t *testing.T) {
	stubPythonAntigravityModule(t)

	addr := fmt.Sprintf("127.0.0.1:%d", getFreePort(t))
	h, err := New(context.Background(), addr, true)
	if err != nil {
		t.Fatalf("New(autoStart=true): %v", err)
	}
	if h.sidecar == nil {
		t.Fatal("expected sidecar to be forked, got nil")
	}
	if !h.sidecar.IsRunning() {
		t.Fatal("expected sidecar to be running after New")
	}
	if err := h.sidecar.Stop(); err != nil {
		t.Errorf("sidecar.Stop: %v", err)
	}
	if h.sidecar.IsRunning() {
		t.Error("expected sidecar to be stopped")
	}
}

// stubPythonAntigravityModule writes a minimal python/antigravity/harness_server.py on PYTHONPATH that runs a stdlib socketserver.TCPServer so pythonsidecar.TCPReady succeeds without needing the real AGY SDK.
func stubPythonAntigravityModule(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	pkg := filepath.Join(root, "python", "antigravity")
	if err := os.MkdirAll(pkg, 0755); err != nil {
		t.Fatalf("mkdir stub: %v", err)
	}
	src := `import socketserver, sys
port = int(sys.argv[sys.argv.index("--port")+1])
socketserver.TCPServer.allow_reuse_address = True
socketserver.TCPServer(("127.0.0.1", port), socketserver.BaseRequestHandler).serve_forever()
`
	if err := os.WriteFile(filepath.Join(pkg, "harness_server.py"), []byte(src), 0644); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("PYTHONPATH", root)
}

func getFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to get free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}
