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

// Package main: the `ax harness` command. It runs one harness in-process,
// selected by the optional [harness-id] argument: "antigravity" (default)
// or "antigravity-interactions"
package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/google/ax/internal/config"
	"github.com/google/ax/internal/harness/antigravityinteractions"
	"github.com/google/ax/internal/pythonsidecar"
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
	Use:    "harness [harness-id]",
	Short:  "Run the harness gRPC server",
	Args:   cobra.MaximumNArgs(1),
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

// runHarness runs the harness selected by the optional [harness-id] argument:
// "antigravity" (default) or "antigravity-interactions".
func runHarness(cmd *cobra.Command, args []string) error {
	harnessID := config.AntigravityHarnessID
	if len(args) > 0 {
		harnessID = args[0]
	}
	switch harnessID {
	case config.AntigravityInteractionsHarnessID:
		return runAntigravityInteractionsHarness(cmd.Context())
	case config.AntigravityHarnessID:
		return runAntigravityHarness(cmd)
	default:
		return fmt.Errorf("unknown harness %q (want %q or %q)",
			harnessID, config.AntigravityHarnessID, config.AntigravityInteractionsHarnessID)
	}
}

// runAntigravityHarness forks the Antigravity Python sidecar server, which serves
// the HarnessService (and gRPC health) on the configured port. ax harness
// supervises the child: it forwards termination signals and exits with its status.
func runAntigravityHarness(cmd *cobra.Command) error {
	if err := setHarnessWorkDir(); err != nil {
		return err
	}

	cfg := pythonsidecar.Config{
		Module: "python.antigravity.harness_server",
		Args: []string{
			"--host", harnessHost,
			"--port", strconv.Itoa(harnessPort),
		},
		Stdout:    os.Stdout,
		Stderr:    os.Stderr,
		ReadyFunc: pythonsidecar.TCPReady(net.JoinHostPort("127.0.0.1", strconv.Itoa(harnessPort))),
	}

	sidecar := pythonsidecar.New(cfg)
	if err := sidecar.Start(cmd.Context(), ""); err != nil {
		return fmt.Errorf("failed to start antigravity harness server: %w", err)
	}
	log.Printf("forked antigravity harness server (pid %d) on %s:%d", sidecar.Pid(), harnessHost, harnessPort)

	// Serve the /readyz endpoint that substrate's readiness probe polls (during
	// golden snapshotting and per-actor Run/Restore).
	go serveReadyz(harnessReadyzPort, harnessPort)

	// Forward termination signals to the child so substrate can stop the actor.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		_ = sidecar.Stop()
	}()

	if err := sidecar.Wait(); err != nil {
		return fmt.Errorf("antigravity harness server exited: %w", err)
	}
	return nil
}

// runAntigravityInteractionsHarness runs the Go Antigravity Interactions harness
// server (HarnessService + gRPC health + HTTP /readyz) on the configured ports.
func runAntigravityInteractionsHarness(ctx context.Context) error {
	if err := setHarnessWorkDir(); err != nil {
		return err
	}

	// Read harness config from ax.yaml handed to the actor via AX_CONFIG_CONTENT.
	// Fall back to built-in defaults.
	var hc config.AntigravityInteractionsHarnessConfig
	if raw := os.Getenv("AX_CONFIG_CONTENT"); raw != "" {
		if data, err := base64.StdEncoding.DecodeString(raw); err != nil {
			log.Printf("AX_CONFIG_CONTENT: base64 decode failed, using defaults: %v", err)
		} else if cfg, err := config.LoadFromBytes(data); err != nil {
			log.Printf("AX_CONFIG_CONTENT: parse failed, using defaults: %v", err)
		} else {
			hc = cfg.Harnesses.AntigravityInteractions
		}
	}
	agent := hc.Agent
	if agent == "" {
		agent = antigravityinteractions.DefaultAgent
	}
	// AX owns the resume-cursor path (a harness implementation detail), so it's
	// derived internally rather than read from ax.yaml.
	stateDir, err := antigravityinteractions.DefaultStateDir()
	if err != nil {
		return err
	}

	cfg := antigravityinteractions.AntigravityInteractionsConfig{
		Agent:    agent,
		StateDir: stateDir,
	}
	return antigravityinteractions.Serve(ctx, cfg, harnessHost, harnessPort, harnessReadyzPort)
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
