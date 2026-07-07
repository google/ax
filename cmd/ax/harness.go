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
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

var (
	harnessPort int
	harnessHost string
)

// harnessReadyzPort is the port for the HTTP /readyz readiness endpoint.
const harnessReadyzPort = 8081

var harnessCmd = &cobra.Command{
	Use:    "harness",
	Short:  "Run the harness gRPC server",
	Hidden: true,
	RunE:   runHarness,
}

func init() {
	harnessCmd.Flags().IntVar(&harnessPort, "port", 50053, "Port for the HarnessService to listen on")
	harnessCmd.Flags().StringVar(&harnessHost, "host", "127.0.0.1", "Host interface for the HarnessService to bind")
	rootCmd.AddCommand(harnessCmd)
}

// setHarnessWorkDir changes the process working directory to AX_HARNESS_WORKDIR
// when it is set. The forked Python sidecar inherits it, which scopes the agent's
// default workspace (os.getcwd()) away from its own source tree.
func setHarnessWorkDir() error {
	dir := os.Getenv("AX_HARNESS_WORKDIR")
	if dir == "" {
		return nil
	}
	if err := os.Chdir(dir); err != nil {
		return fmt.Errorf("set harness working directory %q: %w", dir, err)
	}
	log.Printf("harness working directory set to %s", dir)
	return nil
}

// runHarness forks the Antigravity Python sidecar server, which serves the
// HarnessService (and gRPC health) on the configured port. ax harness supervises
// the child: it forwards termination signals and exits with the child's status.
func runHarness(cmd *cobra.Command, args []string) error {
	if err := setHarnessWorkDir(); err != nil {
		return err
	}

	py := exec.Command("python3", "-m", "python.antigravity.harness_server",
		"--host", harnessHost,
		"--port", strconv.Itoa(harnessPort),
	)
	py.Stdin = os.Stdin
	py.Stdout = os.Stdout
	py.Stderr = os.Stderr
	py.Env = os.Environ()

	if err := py.Start(); err != nil {
		return fmt.Errorf("failed to start antigravity harness server: %w", err)
	}
	log.Printf("forked antigravity harness server (pid %d) on %s:%d", py.Process.Pid, harnessHost, harnessPort)

	// Serve the /readyz endpoint that substrate's readiness probe polls (during
	// golden snapshotting and per-actor Run/Restore).
	go serveReadyz(harnessReadyzPort, harnessPort)

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

// serveReadyz serves the HTTP /readyz endpoint on readyzPort that substrate's
// readiness probe polls (during golden snapshotting and per-actor Run/Restore).
// Each request forwards to the forked harness's gRPC health Check.
func serveReadyz(readyzPort, grpcPort int) {
	conn, err := grpc.NewClient(
		net.JoinHostPort("127.0.0.1", strconv.Itoa(grpcPort)),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		log.Printf("readyz: failed to create gRPC health client: %v", err)
		return
	}
	healthClient := healthpb.NewHealthClient(conn)

	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		resp, err := healthClient.Check(ctx, &healthpb.HealthCheckRequest{})
		if err != nil || resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	addr := net.JoinHostPort(harnessHost, strconv.Itoa(readyzPort))
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Printf("readyz server on %s exited: %v", addr, err)
	}
}
