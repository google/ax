# Runners

A runner is the program that AX starts as PID 1 inside every task container. It is the bridge between the control plane and whatever your agent actually is: the control plane hands it the `Task` and `Workspace` specs, and the runner turns them into a prepared workspace, a running command, and a small HTTP surface that the rest of AX uses to observe the sandbox.

AX ships a default runner, `ax-task-runner`, baked into the default task image. You do not have to use it. Any binary that honors the contract below can be packaged into a container image and named in `spec.image`, and the control plane will treat it exactly like the default.

This page describes what a runner must do. For what the default runner exposes to your command once it is up, see [Sandbox](sandbox.md).

## How a runner is launched

The control plane does not run `spec.command` as the container entrypoint. It always starts the container with a fixed command and lets the runner take it from there:

| What AX sets | Value |
|---|---|
| Container image | `spec.image`, or the default `ax-task-runner` image when unset |
| Container command | `/usr/local/bin/ax-task-runner`, always |
| `AX_TASK_YAML` | The `Task` launch configuration as YAML, excluding status |
| `AX_WORKSPACE_YAML` | The bound `Workspace` resource as YAML; unset when the task binds none |
| `spec.env` entries | Each one set directly in the container environment |
| Volume | A durable directory mounted at `/workspace` |
| Readiness probe | `GET /readyz` on port 80 |

Two consequences follow from that table. Your image must contain an executable at `/usr/local/bin/ax-task-runner`, even if it is a symlink or a shell wrapper around something else. And `spec.command` reaches the runner only through `AX_TASK_YAML`; the runner is responsible for parsing it and starting it.

**Runner-specific settings** belong in `spec.config`, a free-form object that AX passes through unchanged inside `AX_TASK_YAML` (and the task metadata endpoint). The runner owns its schema; the control plane only checks that it is an object of at most 16 KiB and that the whole encoded task fits in 31 KiB. Decode `AX_TASK_YAML` leniently (the Go package offers `v1alpha1.UnmarshalYAMLLenient`) so the runner keeps working when a newer control plane adds fields. See [Runner configuration](manifests.md#runner-configuration).

The `/workspace` volume is what survives suspend and resume. Agent Substrate snapshots it when a task is suspended and restores it into a fresh container when the task is resumed, so the runner will see the same files but a new process tree.

## What a runner must do

**Serve HTTP on port 80.** Both Agent Substrate and the AX server probe the container on this port. The paths that matter:

| Path | Behavior |
|---|---|
| `/healthz` | Return `200` as soon as the runner is alive. |
| `/readyz` | Return `503` until the workspace is prepared, then `200`. AX polls this to set the task's `WorkspaceReady` condition, which `ax describe` shows. |

**Serve metadata on loopback.** These endpoints are for your command, so serve them on a loopback address (the default runner uses `127.0.0.1:8099`) and pass that base URL as `AX_METADATA_URL`. Don't serve them on port 80.

| Path | Behavior |
|---|---|
| `/metadata/v1alpha1/ax/task` | Return the `Task` as `application/yaml`. Optional, but your command and `ax` tooling may expect it. |
| `/metadata/v1alpha1/ax/workspace` | Return the bound `Workspace` as `application/yaml`, or `404` when there is none. Optional, as above. |

**Prepare the workspace once.** A task binds at most one workspace through `spec.workspace`. At the binding's path, `/workspace` unless it sets one, clone the Git repos from `spec.git`, create the skills path, and write any MCP configuration. A task with no binding gets an empty `/workspace`. Record that setup happened somewhere on the durable volume or in a known location, then skip the work on later boots. Resume restarts the container, and re-cloning into a restored workspace would destroy the agent's state. The default runner writes a marker file under `/ax`.

**Run the command and supervise it.** Start `spec.command` as a child process with the workspace path as its working directory. Give it `AX_METADATA_URL` pointing at your loopback metadata server plus every `spec.env` entry. Put it in its own process group so you can signal everything it spawns.

**Stay up after the command exits.** The runner is PID 1, and the container lives as long as it does. If the runner exits when the command does, the metadata server goes with it and `ax ssh` stops working. Log the exit status and keep serving until you are told to stop. The control plane does not currently read the command's exit status back from the container.

**Shut down cleanly on `SIGTERM`.** Stop and suspend both deliver `SIGTERM` to PID 1. Forward it to the command's process group, wait a bounded grace period, then `SIGKILL` whatever is left. Flush anything the agent needs to survive a resume before you exit.

**Serve guest services only when asked.** When `spec.debug` is true, the runner should also serve the [Agent Substrate guest services](https://github.com/agent-substrate/env) over gRPC on port 80, multiplexed with the HTTP endpoints using `h2c`. This is what `ax ssh` connects to. When `spec.debug` is false, leave them off. They allow arbitrary process execution and file access inside the sandbox, and `ax ssh` refuses to connect to a task that has not opted in.

## The default runner

`ax-task-runner` lives in `cmd/ax-task-runner` and is a thin wrapper over the `runner` Go package. It implements everything above and is documented from the inside in [Sandbox](sandbox.md). Its image, built from `Dockerfile.task-runner`, is Python 3.12 with `git`, `curl`, and `openssh-client` installed.

```bash
make images                # cross-compile for linux/amd64, build, and push to $AX_IMAGE_REPO
```

## Replacing the default runner

There are three levels of customization. Pick the shallowest one that solves your problem.

### 1. Extend the default image

If the runner behavior is fine and you only need different tools in the sandbox, build on top of the default image and keep its entrypoint:

The image below is private. Build and push your own runner from the same AX
checkout as the CLI and control plane, then replace `FROM` with its digest
reference. The [installation guide](installation.md) shows the build steps for
its pinned baseline.

```dockerfile
# Replace this with the digest of the runner built from the same AX checkout.
FROM gcr.io/ax-substrate/ate-images/ax-task-runner@sha256:69b764607ec7f1e433d83d2eca17dccfa04f663b43f071fd376e2dd716a57f8c

RUN apt-get update && apt-get install -y --no-install-recommends nodejs npm \
    && rm -rf /var/lib/apt/lists/*
RUN npm install -g my-agent
```

The runner binary stays at `/usr/local/bin/ax-task-runner`, so nothing else changes.


### 2. Embed the runner package in your own binary

If you want the standard lifecycle but need to run code around it, import `github.com/google/ax/runner` and call `runner.Run` yourself. This gives you a hook for the command's exit and a place to do your own setup before or after the metadata server starts:

```go
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"github.com/google/ax/runner"
	"gopkg.in/yaml.v3"
)

func main() {
	var task v1alpha1.Task
	_ = yaml.Unmarshal([]byte(os.Getenv("AX_TASK_YAML")), &task)

	// AX_WORKSPACE_YAML is unset when the task binds no workspace.
	var ws *v1alpha1.Workspace
	if raw := os.Getenv("AX_WORKSPACE_YAML"); raw != "" {
		ws = &v1alpha1.Workspace{}
		_ = yaml.Unmarshal([]byte(raw), ws)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := runner.Run(ctx, runner.Config{
		Task:      &task,
		Workspace: ws,
		OnCommandExit: func(exit runner.CommandExit) {
			slog.Info("agent finished", "exitCode", exit.ExitCode)
			// Upload artifacts, notify a webhook, and so on.
		},
	})
	if err != nil {
		slog.Error("runner failed", "error", err)
		os.Exit(1)
	}
}
```

Cross-compile it for `linux/amd64` with `CGO_ENABLED=0` and copy it into your image at `/usr/local/bin/ax-task-runner`.

For a working example, see [`cmd/runners/claude-code-runner`](../cmd/runners/claude-code-runner), which builds a Claude Code invocation from `spec.config` and passes it to `runner.Run` as the command. [Claude Code runner](../examples/claude-code/README.md) documents it.

### 3. Write a runner from scratch

If the default lifecycle does not fit, for example because your agent framework already supervises processes or you want a different workspace layout, write your own in any language. Read `AX_TASK_YAML` and `AX_WORKSPACE_YAML`, satisfy the contract in the previous section, and install the result at `/usr/local/bin/ax-task-runner`. The `ax.io/v1alpha1` schema is defined in `pkg/apis/v1alpha1/ax.proto`, and [Manifests](manifests.md) walks through every field.

### Ship it

Whichever route you take, push the image to a registry the cluster can pull from and reference it in the task:

```yaml
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: custom-runner
spec:
  image: "ghcr.io/my-org/my-runner@sha256:..."
  command: ["my-agent", "--goal", "fix the flaky test"]
  debug: true
```

AX provisions a dedicated Agent Substrate actor template for each distinct image and environment, so different tasks can run different runners side by side in the same atespace.

## Testing a runner locally

The default runner accepts its specs from files as well as the environment, which makes it easy to run outside a cluster. Your own runner should offer something similar:

```bash
ax-task-runner --task-file task.yaml --workspace-file code.yaml --port 8080 --metadata-port 8099

# In another shell:
curl -i http://127.0.0.1:8080/readyz
curl -s http://127.0.0.1:8099/metadata/v1alpha1/ax/task
```

Once the image is built, the fastest end-to-end check is a task with `debug: true` and `ax ssh` into it to confirm the workspace, the command, and the environment look the way you expect.

## Checklist

- Executable present at `/usr/local/bin/ax-task-runner`
- Reads `AX_TASK_YAML` and `AX_WORKSPACE_YAML`
- Serves `/healthz` and `/readyz` on port 80, with `/readyz` returning `503` until the workspace is ready
- Serves the metadata endpoints on loopback only, at `AX_METADATA_URL`
- Prepares the workspace exactly once across restarts and resumes, at its path
- Starts `spec.command` in the workspace with `AX_METADATA_URL` and `spec.env`
- Keeps running after the command exits
- Forwards `SIGTERM` to the command's process group and exits after a grace period
- Serves guest gRPC services on port 80 only when `spec.debug` is true
