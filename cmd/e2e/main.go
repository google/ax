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
//
// TO RUN THIS E2E DEMONSTRATION:
//
// Step 1: Start the Python gRPC Harness Server (in a separate terminal or background):
//   PYTHONPATH=python:. /Users/anjalisridhar/.gemini/jetski/worktrees/harness-interface-3/implement-agy-sdk-streaming-20260528/.venv/bin/python python/antigravity/harness_server.py --port 50053
//
// Step 2: Run this Go E2E client:
//   go run cmd/e2e/main.go
package main


import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"time"

	"github.com/google/ax/internal/controller2/eventlog"
	"github.com/google/ax/internal/controller2/eventlog/eventlogtest"
	"github.com/google/ax/internal/controller2"
	"github.com/google/ax/internal/harness"
	"github.com/google/ax/proto"
)

func main() {
	ctx := context.Background()
	fmt.Println("==================================================")
	fmt.Println("AX Controller V2 - E2E Harness Demonstration")
	fmt.Println("==================================================")

	// -------------------------------------------------------------------------
	// Demo 1: Unregistered Harness (Should fail)
	// -------------------------------------------------------------------------
	fmt.Println("\n--- Demo 1: Unregistered Harness ---")
	fmt.Println("Requesting 'unregistered-agent'. Exec should fail since no harness is registered.")
	runDemo(ctx, "unregistered-agent", func(reg *controller2.Registry) {
		// Do not register any harness
	})

	// -------------------------------------------------------------------------
	// Demo 2: Antigravity Execution (Requires google-antigravity & GEMINI_API_KEY)
	// -------------------------------------------------------------------------
	fmt.Println("\n--- Demo 2: Antigravity Execution ---")
	fmt.Println("Registering 'antigravity' with real script. Attempting execution.")
	if os.Getenv("GEMINI_API_KEY") == "" {
		fmt.Println("WARNING: GEMINI_API_KEY is not set. Execution will likely fail if dependencies are missing, but we will try anyway.")
	}
	runDemo(ctx, "antigravity", func(reg *controller2.Registry) {
		// With the new stateful gRPC-based streaming harness, connectivity checks on the
		// server address replace the build-time checks for local script file presence.
		address := "localhost:50053"
		conn, err := net.DialTimeout("tcp", address, 1*time.Second)
		if err != nil {
			log.Fatalf("Antigravity harness server not active at %s: %v", address, err)
		}
		conn.Close()
		fmt.Printf("Connected to Antigravity gRPC harness server at %s\n", address)
		harness := harness.NewAntigravityHarness(address)
		reg.RegisterHarness("antigravity", harness)
	})
}

func runDemo(ctx context.Context, agentID string, setupRegistry func(reg *controller2.Registry)) {
	reg := controller2.NewRegistry()
	setupRegistry(reg)

	log := &eventlogtest.MemoryEventLog{}
	c, err := controller2.New(ctx, controller2.Config{
		Registry: reg,
		EventLogBuilder: func() (eventlog.EventLog, error) {
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
			} else if toolCall := out.GetContent().GetToolCall(); toolCall != nil {
				fmt.Printf("Agent Triggered Tool Call: %s (ID: %s)\n", toolCall.GetFunctionCall().Name, toolCall.Id)
			}
		}
		return nil
	})

	inputs := []*proto.Message{
		{
			Role: "user",
			Content: &proto.Content{
				Type: &proto.Content_Text{
					Text: &proto.TextContent{Text: "What is the weather in New York?"},
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
