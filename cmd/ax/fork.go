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

	"github.com/google/ax/cmd/ax/internal/cliutil"
	"github.com/google/ax/internal/checkpoint"
	"github.com/google/ax/internal/config"
	"github.com/google/ax/proto"
	"github.com/spf13/cobra"
)

var (
	forkSourceConversation string
	forkSourceSeq          int32
	forkSourceSnapshot     string
	forkDestConversation   string
	forkServerAddr         string
	forkConfigFile         string
)

var forkCmd = &cobra.Command{
	Use:   "fork",
	Short: "Fork an event log from a specific checkpoint",
	Long: `Fork an existing agentic event log into a new conversation, optionally from a specific checkpoint.

Checkpoints may be specified as --src-seq or as a snapshot (--src-snapshot) in the
form "<conversation_id>:<seq>", matching the seq= value printed after ax exec.`,
	RunE: runFork,
}

func init() {
	forkCmd.Flags().StringVar(&forkSourceConversation, "src-conversation", "", "Source conversation ID to fork from (required)")
	forkCmd.Flags().Int32Var(&forkSourceSeq, "src-seq", 0, "Sequence number to fork from (optional, defaults to latest)")
	forkCmd.Flags().StringVar(&forkSourceSnapshot, "src-snapshot", "", "Snapshot checkpoint as <conversation_id>:<seq> (optional, overrides --src-seq)")
	forkCmd.Flags().StringVar(&forkDestConversation, "dest-conversation", "", "Destination conversation ID (required)")
	forkCmd.Flags().StringVar(&forkServerAddr, "server", "", "gRPC controller server address (if specified, connects to remote server; otherwise runs with a local built-in AX server)")
	forkCmd.Flags().StringVar(&forkConfigFile, "config", "ax.yaml", "Path to YAML configuration file (only used with a local built-in AX server)")

	forkCmd.MarkFlagRequired("src-conversation")
	forkCmd.MarkFlagRequired("dest-conversation")
}

func runFork(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	srcSeq, err := checkpoint.ResolveForkSeq(forkSourceConversation, forkSourceSnapshot, forkSourceSeq)
	if err != nil {
		return fmt.Errorf("invalid fork checkpoint: %w", err)
	}

	if forkServerAddr == "" {
		cfg, err := config.LoadFromFile(forkConfigFile)
		if err != nil {
			return fmt.Errorf("error loading config file '%s': %w", forkConfigFile, err)
		}

		c, err := cliutil.NewControllerFromConfig(ctx, cfg)
		if err != nil {
			return fmt.Errorf("error creating controller: %w", err)
		}
		defer c.Close()

		destID, err := c.Fork(ctx, forkSourceConversation, srcSeq, forkDestConversation)
		if err != nil {
			return fmt.Errorf("error forking conversation: %w", err)
		}

		fmt.Printf("Conversation forked successfully. New conversation ID: %s\n", destID)
		if srcSeq > 0 {
			fmt.Printf("Forked from snapshot: %s\n", checkpoint.At(forkSourceConversation, srcSeq))
		}
		return nil
	}

	conn, err := connect(forkServerAddr)
	if err != nil {
		return err
	}
	defer conn.Close()

	client := proto.NewConversationServiceClient(conn)

	resp, err := client.ForkConversation(ctx, &proto.ForkConversationRequest{
		SrcConversationId:  forkSourceConversation,
		SrcSeq:             srcSeq,
		DestConversationId: forkDestConversation,
	})
	if err != nil {
		return fmt.Errorf("error forking conversation: %w", err)
	}

	fmt.Printf("Conversation forked successfully. New conversation ID: %s\n", resp.ConversationId)
	return nil
}
