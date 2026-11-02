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

// Command claude-code-runner is a task runner that runs Claude Code
// configured from the Task itself. It is installed as
// /usr/local/bin/ax-task-runner in the image built from Dockerfile.claude-code.
//
// When the task leaves spec.command empty, the runner runs one headless
// `claude --print` session in the workspace. spec.config holds the prompt and
// claude's flags, named as in `claude --help` (see [claudeArgs]), and the
// session is connected to the MCP servers the bound Workspace declares.
// A task that sets spec.command runs it unchanged, exactly as with
// ax-task-runner. Everything else, from workspace setup to supervision and
// shutdown, is the standard runner lifecycle.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"github.com/google/ax/runner"
)

// claudeBinary is the Claude Code launcher installed by Dockerfile.claude-code.
const claudeBinary = "claude"

func main() {
	var (
		cfg      runner.Config
		taskFile string
		wsFile   string
	)
	flag.IntVar(&cfg.Port, "port", runner.DefaultPort, "Port for the probe and guest server")
	flag.IntVar(&cfg.MetadataPort, "metadata-port", runner.DefaultMetadataPort, "Loopback-only port for the metadata endpoints")
	flag.StringVar(&taskFile, "task-file", "", "Read the Task YAML from this file instead of AX_TASK_YAML")
	flag.StringVar(&wsFile, "workspace-file", "", "Read the Workspace YAML from this file instead of AX_WORKSPACE_YAML")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	var ws v1alpha1.Workspace
	if err := runner.LoadSpec(wsFile, "AX_WORKSPACE_YAML", &ws); err != nil {
		fatal(err)
	} else if ws.GetMetadata() != nil {
		cfg.Workspace = &ws
	}

	var task v1alpha1.Task
	if err := runner.LoadSpec(taskFile, "AX_TASK_YAML", &task); err != nil {
		fatal(err)
	} else if task.GetMetadata() != nil {
		t, err := withClaudeCommand(&task, cfg.Workspace)
		if err != nil {
			fatal(err)
		}
		cfg.Task = t
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := runner.Run(ctx, cfg); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	slog.Error("claude code runner failed", "error", err)
	os.Exit(1)
}
