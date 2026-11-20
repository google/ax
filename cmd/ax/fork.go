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
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/google/ax/pkg/apis/v1alpha1"
)

const forkUsage = `Usage:
  ax fork task <source-task> <new-task> [flags] [-- command...]

Fork a suspended or completed Task onto a new Task seeded from the source's
workspace snapshot. The forked task inherits the source's profile, workspace,
gateway, environment, and sessionId unless overridden, and is resumed unless
--no-resume is set.

Flags:
  --goal string         Override spec.goal.text on the forked task
  --session-id string   Override spec.sessionId (defaults to the source task's sessionId)
  --env KEY=VALUE       Set or override an environment variable (repeatable)
  --label KEY=VALUE     Set or override a metadata label (repeatable)
  --no-resume           Create the forked task suspended without resuming it`

type keyValueList struct {
	entries [][2]string
}

func (k *keyValueList) String() string { return "" }

func (k *keyValueList) Set(raw string) error {
	name, val, ok := strings.Cut(raw, "=")
	if !ok || name == "" {
		return fmt.Errorf("expected KEY=VALUE, got %q", raw)
	}
	k.entries = append(k.entries, [2]string{name, val})
	return nil
}

type forkFlags struct {
	sourceName string
	targetName string
	goal       string
	goalSet    bool
	sessionID  string
	env        []*v1alpha1.EnvVar
	labels     map[string]string
	commands   []string
	noResume   bool
	help       bool
}

// parseForkArgs parses the arguments of `ax fork`: `<source> <new>` (with an
// optional leading `task` kind), flags in any order, and optional `-- cmd...`.
func parseForkArgs(args []string) (forkFlags, error) {
	var cmdArgs []string
	flagArgs := args
	for i, a := range args {
		if a == "--" {
			flagArgs = args[:i]
			cmdArgs = args[i+1:]
			break
		}
	}

	fs := flag.NewFlagSet("ax fork", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var (
		out    forkFlags
		envs   keyValueList
		labels keyValueList
	)
	fs.StringVar(&out.goal, "goal", "", "override spec.goal.text")
	fs.StringVar(&out.sessionID, "session-id", "", "override spec.sessionId")
	fs.Var(&envs, "env", "set or override an environment variable (KEY=VALUE)")
	fs.Var(&labels, "label", "set or override a metadata label (KEY=VALUE)")
	fs.BoolVar(&out.noResume, "no-resume", false, "create the forked task suspended without resuming it")
	fs.BoolVar(&out.help, "h", false, "help")
	fs.BoolVar(&out.help, "help", false, "help")

	var names []string
	for rest := flagArgs; ; {
		if err := fs.Parse(rest); err != nil {
			return forkFlags{}, err
		}
		if fs.NArg() == 0 {
			break
		}
		names = append(names, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	if out.help {
		return out, nil
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "goal" {
			out.goalSet = true
		}
	})
	if len(names) > 0 && (names[0] == "task" || names[0] == "tasks") {
		names = names[1:]
	}
	if len(names) != 2 {
		return forkFlags{}, fmt.Errorf("expected <source-task> <new-task>, got %d positional argument(s)", len(names))
	}
	out.sourceName = names[0]
	out.targetName = names[1]
	if out.sourceName == out.targetName {
		return forkFlags{}, errors.New("new task name must differ from source task name")
	}
	for _, kv := range envs.entries {
		out.env = append(out.env, &v1alpha1.EnvVar{Name: kv[0], Value: kv[1]})
	}
	if len(labels.entries) > 0 {
		out.labels = make(map[string]string, len(labels.entries))
		for _, kv := range labels.entries {
			out.labels[kv[0]] = kv[1]
		}
	}
	if len(cmdArgs) > 0 {
		out.commands = cmdArgs
	}
	return out, nil
}

func runFork(serverURL, atespace string, args []string) error {
	flags, err := parseForkArgs(args)
	if err != nil {
		return err
	}

	axClient, conn, err := getAXClient(serverURL)
	if err != nil {
		return err
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*lifecycleRPCTimeout)
	defer cancel()

	var overrides *v1alpha1.ForkOverrides
	if flags.goalSet || flags.sessionID != "" || len(flags.env) > 0 || len(flags.commands) > 0 {
		overrides = &v1alpha1.ForkOverrides{
			Commands:  flags.commands,
			Env:       flags.env,
			SessionId: flags.sessionID,
		}
		if flags.goalSet {
			overrides.Goal = &v1alpha1.Goal{Text: flags.goal}
		}
	}

	if _, err := axClient.ForkTask(ctx, &v1alpha1.ForkTaskRequest{
		Atespace:   atespace,
		SourceTask: flags.sourceName,
		Name:       flags.targetName,
		Labels:     flags.labels,
		Overrides:  overrides,
	}); err != nil {
		return fmt.Errorf("forking task %q: %w", flags.sourceName, err)
	}
	fmt.Printf("task.ax.io/%s forked from %s\n", flags.targetName, flags.sourceName)

	if !flags.noResume {
		if _, err := axClient.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{
			Atespace: atespace,
			Name:     flags.targetName,
		}); err != nil {
			return fmt.Errorf("resuming forked task %q: %w", flags.targetName, err)
		}
		fmt.Printf("task.ax.io/%s resumed\n", flags.targetName)
	}
	return nil
}
