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
	"strings"
	"testing"

	"github.com/google/ax/internal/harness/harnesstest"
	"github.com/google/ax/proto"
)

var antigravityHarnessConfig = []byte(`{"system_instructions":"be terse"}`)

func TestAntigravityHarness_Run_Success(t *testing.T) {
	srv := &harnesstest.MockHarnessServer{
		Outputs: []*proto.Message{harnesstest.ThoughtText("Analyzing"), harnesstest.AssistantText("Hello world")},
	}
	harnessClient := New(harnesstest.StartHarnessServer(t, srv))

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

func TestAntigravityHarness_Run_ErrorFrame(t *testing.T) {
	srv := &harnesstest.MockHarnessServer{FailConnect: true, ErrMessage: "internal mock server crash"}
	harnessClient := New(harnesstest.StartHarnessServer(t, srv))

	exec, _ := harnessClient.Start(context.Background(), "conv-test", antigravityHarnessConfig)
	defer exec.Close(context.Background())

	if err := exec.Queue(context.Background(), harnesstest.UserText("Hi")); err != nil {
		t.Fatalf("failed to queue message: %v", err)
	}

	err := exec.Run(context.Background(), &harnesstest.MockHandler{})
	if err == nil {
		t.Fatal("expected error from Run(), got nil")
	}
	if !strings.Contains(err.Error(), "internal mock server crash") {
		t.Errorf("unexpected error message: %v", err)
	}
}
