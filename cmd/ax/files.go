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
	"errors"
	"fmt"
	"os"

	"github.com/google/ax/cmd/ax/internal/cliutil"
	"github.com/google/ax/proto"
	"github.com/spf13/cobra"
)

var (
	filesConversationID string
	filesServerAddr     string
	filesAXConfigFile   string
)

var filesCmd = &cobra.Command{
	Use:   "files <path>",
	Short: "Read a file from a particular conversation / actor workspace",
	Long: `Read a file from a particular conversation or actor workspace.
Positional arguments:
  path  Path to the file to read in the actor workspace`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE:         runFiles,
}

func init() {
	filesCmd.Flags().StringVar(&filesConversationID, "conversation", "", "Conversation ID")
	filesCmd.Flags().StringVar(&filesServerAddr, "server", "", "gRPC controller server address")
	filesCmd.Flags().StringVar(&filesAXConfigFile, "ax-config", "ax.yaml", "Path to YAML configuration file (only used with a local built-in AX server)")
}

func runFiles(cmd *cobra.Command, args []string) error {
	if filesConversationID == "" {
		return errors.New("--conversation flag is required")
	}
	if len(args) == 0 || args[0] == "" {
		return errors.New("file path argument is required")
	}
	path := args[0]
	conversationID := filesConversationID

	var content []byte

	if filesServerAddr != "" {
		conn, err := connect(filesServerAddr)
		if err != nil {
			return err
		}
		defer conn.Close()

		client := proto.NewFileServiceClient(conn)
		resp, err := client.ReadFile(cmd.Context(), &proto.ReadFileRequest{
			ConversationId: conversationID,
			Path:           path,
		})
		if err != nil {
			return fmt.Errorf("failed to read file: %w", err)
		}
		content = resp.GetContent()
	} else {
		cfg, err := newConfig(cmd, filesAXConfigFile)
		if err != nil {
			return err
		}
		c, err := cliutil.NewControllerFromConfig(cmd.Context(), cfg)
		if err != nil {
			return fmt.Errorf("failed to create controller: %w", err)
		}
		defer c.Close()

		content, err = c.ReadFile(cmd.Context(), conversationID, path)
		if err != nil {
			return fmt.Errorf("failed to read file: %w", err)
		}
	}

	_, err := os.Stdout.Write(content)
	return err
}
