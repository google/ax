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

// Package main implements an end-to-end demonstration of the Antigravity harness
// integration with AX Controller V2.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/gorilla/websocket"

	"github.com/google/ax/internal/controller/executor"
	"github.com/google/ax/internal/controller/executor/executortest"
	"github.com/google/ax/internal/controller2"
	"github.com/google/ax/internal/harness"
	"github.com/google/ax/internal/harness/harnesstest"
	"github.com/google/ax/proto"
)

func main() {
	ctx := context.Background()
	fmt.Println("==================================================")
	fmt.Println("AX Controller V2 - E2E Harness Demonstration")
	fmt.Println("==================================================")

	// -------------------------------------------------------------------------
	// Demo 1: Runtime Fallback (No harness registered)
	// -------------------------------------------------------------------------
	fmt.Println("\n--- Demo 1: Runtime Fallback ---")
	fmt.Println("Requesting 'unregistered-agent'. Should fallback to Test Harness (Hello World).")
	runDemo(ctx, "unregistered-agent", func(reg *controller2.Registry) {
		// Do not register any harness
	})

	// -------------------------------------------------------------------------
	// Demo 2: Build-time Fallback (Antigravity with bad script path)
	// -------------------------------------------------------------------------
	fmt.Println("\n--- Demo 2: Build-time Fallback ---")
	fmt.Println("Registering 'antigravity' with non-existent script. Should fallback to Test Harness.")
	runDemo(ctx, "antigravity", func(reg *controller2.Registry) {
		// Build harness with bad path, manually implementing fallback check
		var badHarness harness.Harness
		scriptPath := "non-existent-script.py"
		if _, err := os.Stat(scriptPath); err != nil {
			fmt.Printf("WARNING: Antigravity agent script not found at %s, falling back to test harness: %v\n", scriptPath, err)
			badHarness = harnesstest.New()
		} else {
			badHarness = harness.NewAntigravityHarness("ws://localhost:50054/ws")
		}
		reg.RegisterHarness("antigravity", badHarness)
	})

	// -------------------------------------------------------------------------
	// Demo 3: Antigravity Execution (Requires google-antigravity & GEMINI_API_KEY)
	// -------------------------------------------------------------------------
	fmt.Println("\n--- Demo 3: Antigravity Execution ---")
	fmt.Println("Registering 'antigravity' with real script. Attempting execution.")
	if os.Getenv("GEMINI_API_KEY") == "" {
		fmt.Println("WARNING: GEMINI_API_KEY is not set. Execution will likely fail if dependencies are missing, but we will try anyway.")
	}
	runDemo(ctx, "antigravity", func(reg *controller2.Registry) {
		// Check if Python WebSocket server is active, otherwise fallback
		var realHarness harness.Harness
		address := "ws://localhost:50053/ws"
		dialer := websocket.DefaultDialer
		conn, _, err := dialer.Dial(address, nil)
		if err != nil {
			fmt.Printf("WARNING: Antigravity harness server not active at %s, falling back to test harness: %v\n", address, err)
			realHarness = harnesstest.New()
		} else {
			conn.Close()
			fmt.Printf("Connected to Antigravity harness server at %s\n", address)
			realHarness = harness.NewAntigravityHarness(address)
		}
		reg.RegisterHarness("antigravity", realHarness)
	})
}

func runDemo(ctx context.Context, agentID string, setupRegistry func(reg *controller2.Registry)) {
	reg := controller2.NewRegistry()
	setupRegistry(reg)

	log := &executortest.MemoryEventLog{}
	c, err := controller2.New(ctx, controller2.Config{
		Registry: reg,
		EventLogBuilder: func() (executor.EventLog, error) {
			return log, nil
		},
	})
	if err != nil {
		fmt.Printf("Error creating controller: %v\n", err)
		return
	}
	defer c.Close()

	handler := controller2.ExecHandler(func(resp *proto.ExecResponse) error {
		for _, out := range resp.Outputs {
			if textContent := out.GetContent().GetText().GetText(); textContent != "" {
				fmt.Printf("Agent Output: %s\n", textContent)
			}
		}
		return nil
	})

	inputs := []*proto.Message{
		{
			Role: "user",
			Content: &proto.Content{
				Type: &proto.Content_Text{
					Text: &proto.TextContent{Text: "Who are you?"},
				},
			},
		},
	}

	err = c.Exec(ctx, &proto.ExecRequest{
		ConversationId: "e2e-conv",
		Inputs:         inputs,
		AgentId:        agentID,
	}, handler)

	if err != nil {
		fmt.Printf("Execution Failed (as expected if environment is not ready): %v\n", err)
	} else {
		fmt.Println("Execution Succeeded!")
	}
}
