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

// AX is a server for managing agent orchestrator tasks.
// It provides commands to execute tasks, resume from checkpoints,
// register agents, and run the controller server.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/google/ax/cmd/ax/internal/cliutil"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

var rootCmd = &cobra.Command{
	Use:   "ax",
	Short: "AX - Agent eXecutor",
	Long: `ax provides a server and CLI tools for managing agent orchestrator tasks.
It provides commands to execute tasks, resume from checkpoints,
and run the controller server.`,
}

func init() {
	rootCmd.AddCommand(execCmd)
	rootCmd.AddCommand(serveCmd)

	rootCmd.AddCommand(dashboardCmd)
}

func connect(server string) (*grpc.ClientConn, error) {
	conn, err := grpc.NewClient(server,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to server: %w", err)
	}
	return conn, nil
}

func newConfig(cmd *cobra.Command, configFile string) (*cliutil.Config, error) {
	cfg, err := cliutil.LoadFromFile(configFile)
	if errors.Is(err, os.ErrNotExist) && !cmd.Flags().Changed("config") {
		return cliutil.DefaultConfig(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("error loading config file '%s': %w", configFile, err)
	}
	return cfg, nil
}
