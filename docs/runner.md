# Runners

A runner is the program that AX starts as PID 1 inside every task container. It is the bridge between the control plane and whatever your agent actually is: the control plane hands it the `Task` and `Workspace` specs, and the runner turns them into a prepared workspace, a running command, and a small HTTP surface that the rest of AX uses to observe the sandbox.

AX ships a default runner, `ax-runner`, baked into the `ax-runner` image. You do not have to use it. Any binary that honors the contract below can be packaged into a container image and named in `spec.profile.image`, and the control plane will treat it exactly like the default.

This page describes what a runner must do. For what the default runner exposes to your command once it is up, see [Sandbox](sandbox.md).

## How a runner is launched

The control plane does not run `spec.commands` as the container entrypoint. It always starts the container with a fixed command and lets the runner take it from there:

| What AX sets | Value |
|---|---|
| Container image | `spec.profile.image`, or the default `ax-runner` image when unset |
| Container command | `/usr/local/bin/ax-runner`, always |
| `spec.env` entries | Each one set directly in the container environment |
| Volume | A durable directory mounted at `/workspace` |
| CA bundle | `/run/ate/egress-trust-bundle/trust-bundle.pem`: the CA certificates to trust for outbound TLS, including the egress gateway's |
| Readiness probe | `GET /readyz` on port 80 |

Your image must contain an executable at `/usr/local/bin/ax-runner`, even if it is a symlink or a shell wrapper around something else.

## How the task reaches the runner

Tasks that share a profile share one Agent Substrate template, so the container starts without knowing which task it runs. Once the actor is running, the AX server pushes the work to port 80:

| Request | Body | Sent to |
|---|---|---|
| `POST /runner/v1alpha1/task` | The `Task` as YAML, excluding status, followed by the bound `Workspace` as a second YAML document when the task binds one | Task actors, on every resume |
| `POST /runner/v1alpha1/setup` | The `Workspace` as YAML | Setup actors, which prepare a workspace snapshot once and are then suspended |

Reply `202` when you accept the work, `200` when you already hold the same object (same atespace, name, and uid; the server repeats the push on every resume), `409` when you hold different work, and `400` when the body cannot be decoded. The server retries anything else until it times out, and then fails the resume with a transient error, because a task that was never pushed is not running; `ax resume` repeats the push. `spec.commands` reaches the runner only through this push; the runner is responsible for parsing it and starting it.

The default runner also accepts the task as `AX_TASK_YAML` and `AX_WORKSPACE_YAML` environment variables, or with `--task-file` and `--workspace-file`, which is handy when you run it outside AX. It waits for a push only when it is given no task.

**Runner-specific settings** belong in `spec.config`, a free-form object that AX passes through unchanged inside the pushed task (and the task metadata endpoint). The runner owns its schema; the control plane only checks that it is an object of at most 16 KiB and that the whole encoded task fits in 31 KiB. Decode the task leniently (the Go package offers `v1alpha1.UnmarshalYAMLLenient`) so the runner keeps working when a newer control plane adds fields. See [Runner configuration](manifests.md#runner-configuration).

The `/workspace` volume is what survives suspend and resume. Agent Substrate snapshots it when a task is suspended and restores it into a fresh container when the task is resumed, so the runner will see the same files but a new process tree.

## What a runner must do

**Serve HTTP on port 80.** Both Agent Substrate and the AX server probe the container on this port. The paths that matter:

| Path | Behavior |
|---|---|
| `/healthz` | Return `200` as soon as the runner is alive. |
| `/readyz` | Return `200` once the runner can take a push. Agent Substrate probes this. |
| `/readyz?check=workspace` | Return `503` until the pushed work has prepared the workspace and, for a task, started its command, `200` once it has, and `500` with the reason as the body when setup failed or the command could not be started. AX polls this on every resume to set the task's `WorkspaceReady` condition, which `ax describe` shows, and to know when a setup actor can be snapshotted. A `500` for a task whose workspace was already set up fails the resume. |

**Serve metadata on loopback.** These endpoints are for your command, so serve them on a loopback address (the default runner uses `127.0.0.1:8099`) and pass that base URL as `AX_METADATA_URL`. Don't serve them on port 80.

| Path | Behavior |
|---|---|
| `/metadata/v1alpha1/ax/task` | Return the `Task` as `application/yaml`. Optional, but your command and `ax` tooling may expect it. |
| `/metadata/v1alpha1/ax/workspace` | Return the bound `Workspace` as `application/yaml`, or `404` when there is none. Optional, as above. |

**Prepare the workspace once.** A task binds at most one workspace through `spec.workspace`. At the binding's path, `/workspace` unless it sets one, materialize `spec.setup` (clone Git repos from `spec.setup.git` at their `ref`, write `spec.setup.files`, create `spec.setup.skills.path`, and run `spec.setup.commands`). A task with no binding gets an empty `/workspace`. Record that setup happened on the durable volume, then skip the work on later boots. Resume restarts the container, and re-cloning into a restored workspace would destroy the agent's state. The default runner writes a marker file under `<workspace>/.ax/initialized` (or `<workspace>/.git/.ax/initialized` for root Git checkouts).

**Make outbound TLS and credentials work.** The egress gateway intercepts HTTPS that a `Gateway` rule allows, so point the CA variables your tools read (`SSL_CERT_FILE`, `CURL_CA_BUNDLE`, `REQUESTS_CA_BUNDLE`, `NODE_EXTRA_CA_CERTS`, `GIT_SSL_CAINFO`) at the CA bundle when it exists. It replaces a credential header only on requests that already carry it. Tasks set placeholders for their tools in `spec.env`; the default runner adds one for git (see [Outbound TLS and credentials](sandbox.md#outbound-tls-and-credentials)).

**Run the command and supervise it.** Start `spec.commands` as a child process with the workspace path as its working directory. Give it `AX_METADATA_URL` pointing at your loopback metadata server plus every `spec.env` entry. Put it in its own process group so you can signal everything it spawns.

**Stay up after the command exits.** The runner is PID 1, and the container lives as long as it does. If the runner exits when the command does, the metadata server goes with it and `ax ssh` stops working. Log the exit status and keep serving until you are told to stop. The control plane does not currently read the command's exit status back from the container.

**Shut down cleanly on `SIGTERM`.** Stop and suspend both deliver `SIGTERM` to PID 1. Forward it to the command's process group, wait a bounded grace period, then `SIGKILL` whatever is left. Flush anything the agent needs to survive a resume before you exit.

**Serve guest services only when asked.** When `spec.debug` is true, the runner should also serve the [Agent Substrate guest services](https://github.com/agent-substrate/env) over gRPC on port 80, multiplexed with the HTTP endpoints using `h2c`. This is what `ax ssh` connects to. When `spec.debug` is false, leave them off. They allow arbitrary process execution and file access inside the sandbox, and `ax ssh` refuses to connect to a task that has not opted in.

## The default runner

`ax-runner` lives in `cmd/ax-runner` and is a thin wrapper over the `runner` Go package. It implements everything above and is documented from the inside in [Sandbox](sandbox.md). Its image, built from `Dockerfile.runner`, is Python 3.12 with `git`, `curl`, and `openssh-client` installed.

```bash
make images                # cross-compile for linux/amd64, build, and push to $AX_IMAGE_REPO
```

## Replacing the default runner

There are three levels of customization. Pick the shallowest one that solves your problem.

### 1. Extend the default image

If the runner behavior is fine and you only need different tools in the sandbox, build on top of the default image and keep its entrypoint:

Use the `ax-runner` digest from the release notes of the AX version you run; the one below is from v0.4.0-rc.2.

```dockerfile
FROM ghcr.io/google/ax/ax-runner@sha256:0937ebe5ad2d8db6bb25e88ded44d2d65de1b1bd8f0a9a97c6bb3787407d9947

RUN apt-get update && apt-get install -y --no-install-recommends nodejs npm \
    && rm -rf /var/lib/apt/lists/*
RUN npm install -g my-agent
```

The runner binary stays at `/usr/local/bin/ax-runner`, so nothing else changes.


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

	"github.com/google/ax/runner"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// AwaitPush serves port 80 and waits for the AX server to push the task.
	err := runner.Run(ctx, runner.Config{
		AwaitPush: true,
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

Cross-compile it for `linux/amd64` with `CGO_ENABLED=0` and copy it into your image at `/usr/local/bin/ax-runner`.

### 3. Write a runner from scratch

If the default lifecycle does not fit, for example because your agent framework already supervises processes or you want a different workspace layout, write your own in any language. Accept the pushes, satisfy the contract in the previous section, and install the result at `/usr/local/bin/ax-runner`. The `ax.io/v1alpha1` schema is defined in the `.proto` files in `pkg/apis/v1alpha1/`, and [Manifests](manifests.md) walks through every field.

### Ship it

Whichever route you take, push the image to a registry the cluster can pull from and reference it in the task:

```yaml
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: custom-runner
spec:
  profile:
    image: "ghcr.io/my-org/my-runner@sha256:..."
  commands: ["my-agent", "--goal", "fix the flaky test"]
  debug: true
```

AX provisions a dedicated Agent Substrate actor template for each distinct image and environment, so different tasks can run different runners side by side in the same atespace.

## Testing a runner locally

The default runner accepts its specs from files as well as the environment, which makes it easy to run outside a cluster. Your own runner should offer something similar:

```bash
ax-runner --task-file task.yaml --workspace-file code.yaml --port 8080 --metadata-port 8099

# In another shell:
curl -i http://127.0.0.1:8080/readyz
curl -s http://127.0.0.1:8099/metadata/v1alpha1/ax/task
```

Once the image is built, the fastest end-to-end check is a task with `debug: true` and `ax ssh` into it to confirm the workspace, the command, and the environment look the way you expect.

## Checklist

- Executable present at `/usr/local/bin/ax-runner`
- Reads `AX_TASK_YAML` and `AX_WORKSPACE_YAML`
- Serves `/healthz` and `/readyz` on port 80, with `/readyz` returning `503` until the workspace is ready
- Serves the metadata endpoints on loopback only, at `AX_METADATA_URL`
- Prepares the workspace exactly once across restarts and resumes, at its path
- Starts `spec.commands` in the workspace with `AX_METADATA_URL` and `spec.env`
- Keeps running after the command exits
- Forwards `SIGTERM` to the command's process group and exits after a grace period
- Serves guest gRPC services on port 80 only when `spec.debug` is true
