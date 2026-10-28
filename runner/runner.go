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

// Package runner implements the logic that runs inside every AX task container.
//
// Given a Task and its Workspace, a run starts the metadata and guest server,
// performs the maiden-run workspace setup, and then starts the task's command
// as a supervised child process. The runner stays up as the container's main
// process until it is told to stop, so the sandbox remains inspectable after
// the command has finished. The ax-task-runner binary is a thin wrapper around
// [Run]; custom images can embed the same behavior by calling it directly.
package runner

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/google/ax/internal/metadata"
	"github.com/google/ax/internal/workspace"
	"github.com/google/ax/pkg/apis/v1alpha1"
)

const (
	// DefaultPort is where the probe and guest server listens when Config.Port
	// is zero. It matches the port Agent Substrate probes for readiness.
	DefaultPort = 80
	// DefaultMetadataPort is the loopback port for the metadata endpoints when
	// Config.MetadataPort is zero.
	DefaultMetadataPort = 8099
	// DefaultWorkspacePath is the command's working directory unless the task's
	// workspace binding sets a path.
	DefaultWorkspacePath = v1alpha1.DefaultWorkspacePath

	// stopGracePeriod is how long a running task command gets to exit after
	// SIGTERM before it is killed during shutdown.
	stopGracePeriod = 10 * time.Second
)

// Config controls a single Run.
type Config struct {
	// Port is where the probe endpoints and guest gRPC services listen.
	// Zero means DefaultPort.
	Port int
	// MetadataPort is the loopback-only port for the metadata endpoints the task
	// command reads through AX_METADATA_URL. Zero means DefaultMetadataPort.
	MetadataPort int
	// Task is the task to run. It supplies the command, its environment, and
	// the workspace binding. Nil runs with no command and default settings.
	Task *v1alpha1.Task
	// Workspace is the Workspace resource the task's spec.workspace refers to.
	// Nil sets up an empty working directory instead.
	Workspace *v1alpha1.Workspace
	// OnCommandExit, when set, is called once the task command has exited.
	OnCommandExit func(CommandExit)
}

// CommandExit describes how the task command finished.
type CommandExit struct {
	// Pid of the command's process.
	Pid int
	// ExitCode is the command's exit status, or -1 if it was killed by a signal.
	ExitCode int
	// Err is nil when the command exited with status zero.
	Err error
}

// Run executes the task-runner lifecycle and blocks until ctx is cancelled.
//
// The task's workspace is set up at its path, /workspace unless the binding
// says otherwise. The task's spec.command, if any, is then started as a child
// process in its own process group with that path as its working directory and
// AX_METADATA_URL plus spec.env in its environment. The metadata and guest
// server keeps serving whether or not the command is still running. When ctx is
// cancelled a running command is sent SIGTERM, given a grace period, and then
// killed.
//
// Workspace setup failures are logged but do not abort the run; the task simply
// never reports ready. Run returns an error only when the runner itself cannot
// start, for example when the metadata server or the command fails to launch.
func Run(ctx context.Context, cfg Config) error {
	port := cfg.Port
	if port <= 0 {
		port = DefaultPort
	}
	metadataPort := cfg.MetadataPort
	if metadataPort <= 0 {
		metadataPort = DefaultMetadataPort
	}
	ref := cfg.Task.GetSpec().GetWorkspace()
	wsPath := cfg.Task.GetSpec().WorkspacePath()
	slog.Info("initializing AX task runner", "port", port, "workspace", ref.GetName(), "wsPath", wsPath)

	metaServer := metadata.NewServer(port, cfg.Task, cfg.Workspace, metadata.ServerOptions{
		WorkspacePath: wsPath,
		MetadataPort:  metadataPort,
	})
	if err := metaServer.Start(); err != nil {
		return fmt.Errorf("starting metadata server: %w", err)
	}
	metadataURL := metaServer.MetadataURL()
	_ = os.Setenv("AX_METADATA_URL", metadataURL)
	for _, e := range cfg.Task.GetSpec().GetEnv() {
		_ = os.Setenv(e.GetName(), e.GetValue())
	}
	defer func() {
		slog.Info("shutting down metadata server")
		_ = metaServer.Stop(context.Background())
	}()

	if _, err := workspace.SetupWorkspace(ctx, cfg.Workspace, wsPath); err != nil {
		slog.Error("workspace maiden run setup failed", "workspace", ref.GetName(), "path", wsPath, "error", err)
	} else {
		metaServer.SetWorkspaceReady(true)
		slog.Info("workspace maiden run setup marked ready", "path", wsPath)
	}

	cmdArgs := cfg.Task.GetSpec().GetCommand()
	if len(cmdArgs) == 0 {
		slog.Info("no task command specified; serving metadata until stopped")
		<-ctx.Done()
		return nil
	}

	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Dir = wsPath
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = commandEnv(cfg.Task, metadataURL)
	// A dedicated process group lets shutdown signal the command together with
	// anything it spawned.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting task command %v: %w", cmdArgs, err)
	}
	slog.Info("started task command", "pid", cmd.Process.Pid, "command", cmdArgs)

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	select {
	case err := <-exited:
		reportExit(cfg, cmd, err)
		// Keep the sandbox up and inspectable until told to stop.
		<-ctx.Done()
	case <-ctx.Done():
		reportExit(cfg, cmd, stopCommand(cmd, exited))
	}
	return nil
}

// stopCommand asks the command's process group to terminate, kills it if it has
// not exited within the grace period, and returns the command's exit result.
func stopCommand(cmd *exec.Cmd, exited <-chan error) error {
	pid := cmd.Process.Pid
	slog.Info("stopping task command", "pid", pid)
	_ = syscall.Kill(-pid, syscall.SIGTERM)

	timer := time.NewTimer(stopGracePeriod)
	defer timer.Stop()
	select {
	case err := <-exited:
		return err
	case <-timer.C:
		slog.Warn("task command did not exit within grace period; killing", "pid", pid, "gracePeriod", stopGracePeriod)
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		return <-exited
	}
}

// reportExit logs how the command finished and notifies the OnCommandExit hook.
func reportExit(cfg Config, cmd *exec.Cmd, err error) {
	exit := CommandExit{Pid: cmd.Process.Pid, ExitCode: -1, Err: err}
	if cmd.ProcessState != nil {
		exit.ExitCode = cmd.ProcessState.ExitCode()
	}
	if err != nil {
		slog.Error("task command exited with error", "pid", exit.Pid, "exitCode", exit.ExitCode, "error", err)
	} else {
		slog.Info("task command completed successfully", "pid", exit.Pid)
	}
	if cfg.OnCommandExit != nil {
		cfg.OnCommandExit(exit)
	}
}

// commandEnv builds the environment for the task command: the runner's own
// environment, the metadata URL, and the Task's spec.env entries.
func commandEnv(task *v1alpha1.Task, metadataURL string) []string {
	env := append(os.Environ(), "AX_METADATA_URL="+metadataURL)
	for _, e := range task.GetSpec().GetEnv() {
		env = append(env, fmt.Sprintf("%s=%s", e.GetName(), e.GetValue()))
	}
	return env
}
