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

package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/google/ax/cmd/ax/internal/cliutil"
	"github.com/google/ax/internal/server"
	"github.com/spf13/cobra"
)

var (
	serveConfigFile string
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run controller as a gRPC server",
	Long: `Run the AX controller as a gRPC server.
Loads configuration from a YAML file (default: ax.yaml).`,
	RunE: runServe,
}

func init() {
	serveCmd.Flags().StringVar(&serveConfigFile, "config", "ax.yaml", "Path to YAML configuration file")
}

func runServe(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	cfg, err := newConfig(cmd, serveConfigFile)
	if err != nil {
		return err
	}

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	c, err := cliutil.NewControllerFromConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("error creating controller: %w", err)
	}
	defer c.Close()

	// Create server
	srv := server.New(c)

	// Setup signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigChan
		log.Println("\nReceived interrupt, shutting down...")
		srv.GracefulStop()
	}()
	log.Printf("Starting AX server at %s...\n", cfg.Server.Address)
	if err := srv.Serve(cfg.Server.Address); err != nil {
		return fmt.Errorf("error serving: %w", err)
	}

	return nil
}
