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

package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gopkg.in/yaml.v3"
)

const (
	// GuestCommand is the entrypoint every task container runs, as on Agent
	// Substrate, so any image that ships the runner works unchanged.
	GuestCommand = "/usr/local/bin/ax-task-runner"
	// GuestPort is where the runner serves probes and guest services.
	GuestPort = 80

	geminiKeyEnv = "GEMINI_API_KEY"

	defaultWorkspaceReadyTimeout = 30 * time.Second
	workspaceReadyPollInterval   = 250 * time.Millisecond

	condReady          = "Ready"
	condWorkspaceReady = "WorkspaceReady"

	labelManaged  = "ax.io/managed"
	labelAtespace = "ax.io/atespace"
	labelTask     = "ax.io/task"
)

// ContainerName is the name of the container running a task. Atespace and
// task names are DNS labels, so the dots keep the mapping unambiguous.
func ContainerName(atespace, task string) string {
	if atespace == "" {
		atespace = "default"
	}
	return "ax." + atespace + "." + task
}

// VolumeName is the named volume holding a task's /workspace, the local
// counterpart of the durable workspace volume on Agent Substrate.
func VolumeName(atespace, task string) string {
	return ContainerName(atespace, task) + ".workspace"
}

// Options configures a Reconciler.
type Options struct {
	// Runtime is the container CLI to use. Nil detects one on first use, so
	// commands that never touch a container do not require a runtime.
	Runtime *Runtime
	// Progress receives image pull progress. Nil discards it.
	Progress io.Writer
	// WorkspaceReadyTimeout bounds how long a resume waits for the runner to
	// report its workspace ready. Zero means 30s.
	WorkspaceReadyTimeout time.Duration
}

// Reconciler runs tasks as local containers. It implements the same contract
// as the Agent Substrate reconciler, mapping actor operations onto container
// ones: create -> create, resume -> start/unpause, suspend -> pause,
// delete -> rm.
type Reconciler struct {
	progress              io.Writer
	httpClient            *http.Client
	workspaceReadyTimeout time.Duration

	mu      sync.Mutex
	runtime *Runtime
}

// NewReconciler returns a Reconciler.
func NewReconciler(opts Options) *Reconciler {
	if opts.WorkspaceReadyTimeout <= 0 {
		opts.WorkspaceReadyTimeout = defaultWorkspaceReadyTimeout
	}
	if opts.Progress == nil {
		opts.Progress = io.Discard
	}
	return &Reconciler{
		progress:              opts.Progress,
		httpClient:            &http.Client{Timeout: 2 * time.Second},
		workspaceReadyTimeout: opts.WorkspaceReadyTimeout,
		runtime:               opts.Runtime,
	}
}

func (r *Reconciler) rt() (*Runtime, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runtime == nil {
		rt, err := Detect()
		if err != nil {
			return nil, err
		}
		r.runtime = rt
	}
	return r.runtime, nil
}

// Reconcile drives the task's container to the state its phase asks for:
// created and paused (or never started) for Suspended, running for anything
// else. It reports the outcome in the task's status.
func (r *Reconciler) Reconcile(ctx context.Context, task *v1alpha1.Task, ws *v1alpha1.Workspace) (*v1alpha1.Task, error) {
	if task.Metadata == nil {
		task.Metadata = &v1alpha1.ObjectMeta{}
	}
	if task.Status == nil {
		task.Status = &v1alpha1.TaskStatus{}
	}
	if task.Metadata.Atespace == "" {
		task.Metadata.Atespace = "default"
	}
	if task.Spec == nil {
		task.Spec = &v1alpha1.TaskSpec{}
	}
	if task.Spec.Image == "" {
		task.Spec.Image = v1alpha1.DefaultTaskImage
	}
	atespace, name := task.Metadata.Atespace, task.Metadata.Name
	now := time.Now()

	task.Status.Actor = name
	if task.Status.Id == "" {
		task.Status.Id = fmt.Sprintf("task-%s-%d", name, now.Unix())
	}

	if err := v1alpha1.ValidateResources(task.Spec.Resources); err != nil {
		setCondition(task, condReady, "False", "InvalidResources", err.Error(), now)
		task.Status.Phase = "Failed"
		return task, fmt.Errorf("validating resources: %w", err)
	}

	rt, err := r.rt()
	if err != nil {
		setCondition(task, condReady, "False", "RuntimeUnavailable", err.Error(), now)
		task.Status.Phase = "Failed"
		return task, err
	}

	cname := ContainerName(atespace, name)
	state, err := rt.State(ctx, cname)
	if errors.Is(err, ErrNotFound) {
		if err := r.create(ctx, rt, task, ws, cname); err != nil {
			setCondition(task, condReady, "False", "ContainerCreationFailed", err.Error(), now)
			task.Status.Phase = "Failed"
			return task, fmt.Errorf("creating container: %w", err)
		}
		state = StateCreated
	} else if err != nil {
		setCondition(task, condReady, "False", "RuntimeUnavailable", err.Error(), now)
		task.Status.Phase = "Pending"
		return task, fmt.Errorf("inspecting container: %w", err)
	}

	if task.Status.Phase == "Suspended" || task.Status.Phase == "" {
		if state == StateRunning {
			slog.Info("pausing task container", "container", cname)
			if err := rt.Pause(ctx, cname); err != nil {
				return task, fmt.Errorf("pausing container: %w", err)
			}
		}
		task.Status.WorkerIp = ""
		task.Status.Phase = "Suspended"
		setCondition(task, condReady, "False", "TaskSuspended", "Task is suspended", now)
		return task, nil
	}

	switch state {
	case StateRunning:
	case StatePaused:
		slog.Info("unpausing task container", "container", cname)
		err = rt.Unpause(ctx, cname)
	default:
		slog.Info("starting task container", "container", cname, "state", state)
		err = rt.Start(ctx, cname)
	}
	if err != nil {
		setCondition(task, condReady, "False", "ContainerStartFailed", err.Error(), now)
		task.Status.Phase = "Failed"
		return task, fmt.Errorf("starting container: %w", err)
	}

	addr, err := rt.HostPort(ctx, cname, GuestPort)
	if err != nil {
		setCondition(task, condReady, "False", "ContainerStartFailed", err.Error(), now)
		task.Status.Phase = "Failed"
		return task, fmt.Errorf("finding published port: %w", err)
	}
	task.Status.WorkerIp = addr
	task.Status.Phase = "Running"

	// Workspace setup happens once per task; after it succeeds WorkspaceReady
	// stays True across suspend and resume, as on Substrate.
	workspaceReady := conditionTrue(task, condWorkspaceReady)
	if !workspaceReady {
		workspaceReady = r.waitReady(ctx, addr)
	}
	if workspaceReady {
		if !conditionTrue(task, condWorkspaceReady) {
			setCondition(task, condWorkspaceReady, "True", "SetupComplete", fmt.Sprintf("Workspace setup completed at %s", addr), time.Now())
		}
		setCondition(task, condReady, "True", "TaskRunning", "Task is running and its workspace is ready", time.Now())
		return task, nil
	}

	// A runner that died during startup will never become ready; say so
	// rather than leaving the task "initializing" forever.
	if st, err := rt.State(ctx, cname); err == nil && st != StateRunning {
		msg := fmt.Sprintf("container %s is %s", cname, st)
		if logs := rt.Logs(ctx, cname, 20); logs != "" {
			msg += ":\n" + logs
		}
		setCondition(task, condReady, "False", "ContainerExited", msg, time.Now())
		task.Status.Phase = "Failed"
		task.Status.WorkerIp = ""
		return task, errors.New(msg)
	}
	setCondition(task, condWorkspaceReady, "False", "Initializing", fmt.Sprintf("Workspace is initializing at %s", addr), time.Now())
	setCondition(task, condReady, "False", "WorkspaceInitializing", "Waiting for workspace setup to complete", time.Now())
	return task, nil
}

// create pulls the image if needed and creates the task's container with the
// same entrypoint, environment, workspace mount, probe port and limits as the
// ActorTemplate the Substrate reconciler would provision.
func (r *Reconciler) create(ctx context.Context, rt *Runtime, task *v1alpha1.Task, ws *v1alpha1.Workspace, cname string) error {
	image := task.Spec.Image
	exists, err := rt.ImageExists(ctx, image)
	if err != nil {
		return err
	}
	if !exists {
		fmt.Fprintf(r.progress, "Pulling %s with %s...\n", image, rt.Name())
		if err := rt.Pull(ctx, image, r.progress); err != nil {
			return err
		}
	}

	env := make(map[string]string)
	for _, e := range task.Spec.Env {
		if e.GetName() != "" {
			env[e.GetName()] = e.GetValue()
		}
	}

	secret := make(map[string]string)
	// Launch configuration only: status must not leak into the container.
	launch := proto.Clone(task).(*v1alpha1.Task)
	launch.Status = nil
	taskYAML, err := yaml.Marshal(launch)
	if err != nil {
		return fmt.Errorf("encoding task: %w", err)
	}
	secret["AX_TASK_YAML"] = string(taskYAML)
	if ws != nil {
		wsYAML, err := yaml.Marshal(ws)
		if err != nil {
			return fmt.Errorf("encoding workspace: %w", err)
		}
		secret["AX_WORKSPACE_YAML"] = string(wsYAML)
	}
	// Locally the developer's own environment stands in for the cluster
	// secret the Substrate reconciler reads.
	if key := os.Getenv(geminiKeyEnv); key != "" {
		if _, set := env[geminiKeyEnv]; !set {
			secret[geminiKeyEnv] = key
		}
	}

	cpus, mem, err := limits(task.Spec.Resources)
	if err != nil {
		return err
	}

	atespace, name := task.Metadata.Atespace, task.Metadata.Name
	slog.Info("creating task container", "container", cname, "image", image)
	return rt.Create(ctx, CreateOptions{
		Name:       cname,
		Image:      image,
		Entrypoint: GuestCommand,
		Labels: map[string]string{
			labelManaged:  "true",
			labelAtespace: atespace,
			labelTask:     name,
		},
		Env:         env,
		SecretEnv:   secret,
		Publish:     []int{GuestPort},
		Volumes:     map[string]string{VolumeName(atespace, name): v1alpha1.DefaultWorkspacePath},
		CPUs:        cpus,
		MemoryBytes: mem,
	})
}

// waitReady polls the runner's workspace readiness probe until it answers 200
// or the timeout passes.
func (r *Reconciler) waitReady(ctx context.Context, addr string) bool {
	ctx, cancel := context.WithTimeout(ctx, r.workspaceReadyTimeout)
	defer cancel()
	url := fmt.Sprintf("http://%s/readyz?check=workspace", addr)
	ticker := time.NewTicker(workspaceReadyPollInterval)
	defer ticker.Stop()
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if resp, err := r.httpClient.Do(req); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

// ReconcileDelete removes the task's container and workspace volume.
func (r *Reconciler) ReconcileDelete(ctx context.Context, atespace, taskName string) error {
	rt, err := r.rt()
	if err != nil {
		return err
	}
	slog.Info("removing task container", "container", ContainerName(atespace, taskName))
	if err := rt.Remove(ctx, ContainerName(atespace, taskName)); err != nil {
		return err
	}
	return rt.RemoveVolume(ctx, VolumeName(atespace, taskName))
}

// limits converts spec.resources.limits into container CLI flags: cores as a
// decimal and memory in bytes.
func limits(reqs *v1alpha1.ResourcesSpec) (cpus string, memBytes int64, err error) {
	l := reqs.GetLimits()
	if c := l.GetCpu(); c != "" {
		q, err := v1alpha1.ParseQuantity(c)
		if err != nil {
			return "", 0, fmt.Errorf("spec.resources.limits.cpu: %w", err)
		}
		cpus = q.FloatString(3)
	}
	if m := l.GetMemory(); m != "" {
		q, err := v1alpha1.ParseQuantity(m)
		if err != nil {
			return "", 0, fmt.Errorf("spec.resources.limits.memory: %w", err)
		}
		memBytes = new(big.Int).Quo(q.Num(), q.Denom()).Int64()
	}
	return cpus, memBytes, nil
}

func conditionTrue(task *v1alpha1.Task, condType string) bool {
	for _, c := range task.GetStatus().GetConditions() {
		if c.Type == condType {
			return c.Status == "True"
		}
	}
	return false
}

func setCondition(task *v1alpha1.Task, condType, status, reason, message string, t time.Time) {
	ts := timestamppb.New(t)
	for _, c := range task.Status.Conditions {
		if c.Type == condType {
			c.Status, c.Reason, c.Message, c.UpdateTime = status, reason, message, ts
			return
		}
	}
	task.Status.Conditions = append(task.Status.Conditions, &v1alpha1.Condition{
		Type: condType, Status: status, Reason: reason, Message: message, UpdateTime: ts,
	})
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
