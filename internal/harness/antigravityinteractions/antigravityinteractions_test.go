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

package antigravityinteractions

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/ax/internal/harness/harnesstest"
	"golang.org/x/oauth2"
)

// fakeInteractions is a fake Interactions API: an http.RoundTripper that records
// the decoded request body of every POST and replies with a canned SSE stream.
// It lets the harness's real Start/Run/cursorStore code run end to end while the
// network is faked, so we can assert exactly what previous_interaction_id the
// harness sends on each turn.
type fakeInteractions struct {
	mu sync.Mutex
	// requests holds the decoded body of each interaction request, in order.
	requests []interactionRequest
	// interactionIDs are returned (in order) as the completed interaction id for
	// successive turns; the i-th request gets interactionIDs[i].
	interactionIDs []string
}

func (f *fakeInteractions) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	var decoded interactionRequest
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("fake server: decoding request: %w", err)
	}

	f.mu.Lock()
	idx := len(f.requests)
	f.requests = append(f.requests, decoded)
	id := fmt.Sprintf("INT-%d", idx+1)
	if idx < len(f.interactionIDs) {
		id = f.interactionIDs[idx]
	}
	f.mu.Unlock()

	// A minimal completed turn: no tool calls, so Run finishes in one turn.
	sse := "" +
		"event: interaction.created\n" +
		`data: {"interaction":{"id":"` + id + `","status":"in_progress"},"event_type":"interaction.created"}` + "\n\n" +
		"event: interaction.completed\n" +
		`data: {"interaction":{"id":"` + id + `","status":"completed"},"event_type":"interaction.completed"}` + "\n\n" +
		"event: done\n" +
		"data: [DONE]\n\n"

	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(sse)),
		Request:    req,
	}, nil
}

func (f *fakeInteractions) recorded() []interactionRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]interactionRequest(nil), f.requests...)
}

// newTestHarness builds a harness wired to the fake server, a static token (no
// ADC), and the given state dir. It also sets the project env so the request URL
// and X-Goog-User-Project header are well-formed.
func newTestHarness(t *testing.T, fake *fakeInteractions, stateDir string) *AntigravityInteractionsHarness {
	t.Helper()
	t.Setenv(envCloudProject, "test-project")
	h, err := New(AntigravityInteractionsConfig{
		Agent:       "test-agent",
		StateDir:    stateDir,
		HTTPClient:  &http.Client{Transport: fake},
		TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "fake-token"}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

// runOneTurn starts an Execution for conversationID, queues a single user
// message, and runs it to completion.
func runOneTurn(t *testing.T, h *AntigravityInteractionsHarness, conversationID, prompt string) {
	t.Helper()
	ctx := context.Background()
	exec, err := h.Start(ctx, conversationID, nil)
	if err != nil {
		t.Fatalf("Start(%q): %v", conversationID, err)
	}
	if err := exec.Queue(ctx, harnesstest.UserText(prompt)); err != nil {
		t.Fatalf("Queue: %v", err)
	}
	if err := exec.Run(ctx, &harnesstest.MockHandler{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := exec.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestResumeAcrossRestart is the core CUJ: a first Execution starts a new
// interaction chain (empty previous_interaction_id) and persists the returned
// interaction id; then a brand-new harness over the SAME state dir (a simulated
// restart / snapshot restore) loads that cursor and sends it as
// previous_interaction_id on the next request.
func TestResumeAcrossRestart(t *testing.T) {
	fake := &fakeInteractions{interactionIDs: []string{"INT-1", "INT-2"}}
	stateDir := t.TempDir()

	// First Execution: starts the chain.
	h1 := newTestHarness(t, fake, stateDir)
	runOneTurn(t, h1, "conv-1", "hello")

	// Simulated restart: a brand-new harness over the same state dir, so any
	// resumed state must come from disk, not h1's memory.
	h2 := newTestHarness(t, fake, stateDir)
	runOneTurn(t, h2, "conv-1", "again")

	reqs := fake.recorded()
	if len(reqs) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(reqs))
	}
	if reqs[0].PreviousInteractionID != "" {
		t.Errorf("turn 1: previous_interaction_id = %q, want empty (new chain)", reqs[0].PreviousInteractionID)
	}
	if got, want := reqs[1].PreviousInteractionID, "INT-1"; got != want {
		t.Errorf("turn 2 (after restart): previous_interaction_id = %q, want %q (resumed from persisted cursor)", got, want)
	}
}

// TestNewRequiresStateDir verifies that the constructor rejects an empty
// StateDir: resume-cursor persistence is required.
func TestNewRequiresStateDir(t *testing.T) {
	t.Setenv(envCloudProject, "test-project")
	_, err := New(AntigravityInteractionsConfig{
		Agent:       "test-agent",
		StateDir:    "", // missing
		TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "fake-token"}),
	})
	if err == nil {
		t.Fatal("New with empty StateDir: got nil error, want error")
	}
}

// TestSameHarnessSecondTurnResumes checks that even without a "restart", a second
// Execution on the same harness/conversation continues the chain via the cursor.
func TestSameHarnessSecondTurnResumes(t *testing.T) {
	fake := &fakeInteractions{interactionIDs: []string{"INT-1", "INT-2"}}
	h := newTestHarness(t, fake, t.TempDir())

	runOneTurn(t, h, "conv-1", "hello")
	runOneTurn(t, h, "conv-1", "again")

	reqs := fake.recorded()
	if len(reqs) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(reqs))
	}
	if got, want := reqs[1].PreviousInteractionID, "INT-1"; got != want {
		t.Errorf("turn 2: previous_interaction_id = %q, want %q", got, want)
	}
}

// TestCursorStoreLoadSave is a focused unit test of the harness-local cursor
// store round-trip.
func TestCursorStoreLoadSave(t *testing.T) {
	cs, err := newCursorStore(t.TempDir())
	if err != nil {
		t.Fatalf("newCursorStore: %v", err)
	}

	// Missing key: found is false, no error.
	if _, found, err := cs.load("missing"); err != nil || found {
		t.Fatalf("load(missing) = found=%v err=%v, want found=false err=nil", found, err)
	}

	// Round-trip.
	if err := cs.save("conv-1", resumeCursor{PrevInteractionID: "INT-7"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	cur, found, err := cs.load("conv-1")
	if err != nil || !found {
		t.Fatalf("load(conv-1) = found=%v err=%v, want found=true err=nil", found, err)
	}
	if cur.PrevInteractionID != "INT-7" {
		t.Errorf("loaded PrevInteractionID = %q, want %q", cur.PrevInteractionID, "INT-7")
	}

	// Last-write-wins overwrite.
	if err := cs.save("conv-1", resumeCursor{PrevInteractionID: "INT-8"}); err != nil {
		t.Fatalf("save (overwrite): %v", err)
	}
	cur, _, err = cs.load("conv-1")
	if err != nil {
		t.Fatalf("load after overwrite: %v", err)
	}
	if cur.PrevInteractionID != "INT-8" {
		t.Errorf("after overwrite PrevInteractionID = %q, want %q", cur.PrevInteractionID, "INT-8")
	}
}

// TestDefaultStateDir returns ~/.ax/antigravityinteractions/cursors under the
// user's home directory.
func TestDefaultStateDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	got, err := DefaultStateDir()
	if err != nil {
		t.Fatalf("DefaultStateDir: %v", err)
	}
	if want := filepath.Join(home, ".ax", "antigravityinteractions", "cursors"); got != want {
		t.Errorf("DefaultStateDir() = %q, want %q", got, want)
	}
}
