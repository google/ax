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

// Package main: the `ax harness` command. It supervises the Antigravity Python
// sidecar server (which serves the HarnessService and gRPC health), forking it
// as a child process and forwarding termination signals.
package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/spf13/cobra"
)

var (
	harnessPort                 int
	harnessHost                 string
	harnessAntigravityAgentFile string
)

var harnessCmd = &cobra.Command{
	Use:    "harness",
	Short:  "Run the harness gRPC server",
	Hidden: true,
	RunE:   runHarness,
}

func init() {
	harnessCmd.Flags().IntVar(&harnessPort, "port", 50053, "Port for the HarnessService to listen on")
	harnessCmd.Flags().StringVar(&harnessHost, "host", "127.0.0.1", "Host interface for the HarnessService to bind")
	harnessCmd.Flags().StringVar(&harnessAntigravityAgentFile, "antigravity-agent-file", "examples/antigravity_agent/agent.py", "Path to the agent config file the Python sidecar serves")
	rootCmd.AddCommand(harnessCmd)
}

// runHarness forks the Antigravity Python sidecar server, which serves the
// HarnessService (and gRPC health) on the configured port. ax harness supervises
// the child: it forwards termination signals and exits with the child's status.
func runHarness(cmd *cobra.Command, args []string) error {
	py := exec.Command("python3", "-m", "python.antigravity.harness_server",
		"--host", harnessHost,
		"--port", strconv.Itoa(harnessPort),
		"--agent_file", harnessAntigravityAgentFile,
	)
	py.Stdin = os.Stdin
	py.Stdout = os.Stdout
	py.Stderr = os.Stderr
	py.Env = os.Environ()

	if err := py.Start(); err != nil {
		return fmt.Errorf("failed to start antigravity harness server: %w", err)
	}
	log.Printf("forked antigravity harness server (pid %d) on %s:%d", py.Process.Pid, harnessHost, harnessPort)

	// Forward termination signals to the child so substrate can stop the actor.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		for sig := range sigChan {
			_ = py.Process.Signal(sig)
		}
	}()

	if err := py.Wait(); err != nil {
		return fmt.Errorf("antigravity harness server exited: %w", err)
	}
	return nil
}
