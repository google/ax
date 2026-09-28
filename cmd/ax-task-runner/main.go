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

// Command ax-task-runner is the entrypoint of every AX task container. It
// loads the Task and Workspace specs and hands them to the runner package,
// which does everything else.
//
// AX delivers the Task as YAML in AX_TASK_YAML and the bound
// Workspaces as a multi-document YAML stream in AX_WORKSPACES_YAML. For local
// runs the specs can be read from files instead with --task-file and one or
// more --workspace-file flags.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"github.com/google/ax/runner"
	"gopkg.in/yaml.v3"
)

// stringList collects a repeatable flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

func main() {
	var (
		cfg      runner.Config
		taskFile string
		wsFiles  stringList
	)
	flag.IntVar(&cfg.Port, "port", runner.DefaultPort, "Port for the metadata and guest server")
	flag.StringVar(&taskFile, "task-file", "", "Read the Task YAML from this file instead of AX_TASK_YAML")
	flag.Var(&wsFiles, "workspace-file", "Read Workspace YAML from this file instead of the environment; repeatable, and each file may hold several documents")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	var task v1alpha1.Task
	if err := loadSpec(taskFile, "AX_TASK_YAML", &task); err != nil {
		fatal(err)
	} else if task.GetMetadata() != nil {
		cfg.Task = &task
	}

	workspaces, err := loadWorkspaces(wsFiles)
	if err != nil {
		fatal(err)
	}
	cfg.Workspaces = workspaces

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := runner.Run(ctx, cfg); err != nil {
		fatal(err)
	}
}

// loadSpec decodes YAML into out from file when set, otherwise from the named
// environment variable. It is not an error for neither to be present.
func loadSpec(file, envVar string, out any) error {
	var raw []byte
	switch {
	case file != "":
		data, err := os.ReadFile(file)
		if err != nil {
			return fmt.Errorf("reading %s: %w", file, err)
		}
		raw = data
	case os.Getenv(envVar) != "":
		raw = []byte(os.Getenv(envVar))
	default:
		return nil
	}
	if err := yaml.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("parsing %T: %w", out, err)
	}
	return nil
}

// loadWorkspaces reads Workspace documents from the given files, or when none
// are given from AX_WORKSPACES_YAML. Empty documents are skipped. It is not an
// error for no source to be present.
func loadWorkspaces(files []string) ([]*v1alpha1.Workspace, error) {
	var sources [][]byte
	switch {
	case len(files) > 0:
		for _, f := range files {
			data, err := os.ReadFile(f)
			if err != nil {
				return nil, fmt.Errorf("reading %s: %w", f, err)
			}
			sources = append(sources, data)
		}
	case os.Getenv("AX_WORKSPACES_YAML") != "":
		sources = append(sources, []byte(os.Getenv("AX_WORKSPACES_YAML")))
	}

	var workspaces []*v1alpha1.Workspace
	for _, src := range sources {
		dec := yaml.NewDecoder(strings.NewReader(string(src)))
		for {
			var ws v1alpha1.Workspace
			err := dec.Decode(&ws)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("parsing workspace yaml: %w", err)
			}
			if ws.GetMetadata() != nil {
				workspaces = append(workspaces, &ws)
			}
		}
	}
	return workspaces, nil
}

func fatal(err error) {
	slog.Error("task runner failed", "error", err)
	os.Exit(1)
}
