<h1>
  <img src="assets/axel.svg" width="70" align="absmiddle" alt="AX axolotl">
  AX
</h1>

> [!WARNING]
> AX and several of its features are in heavy development. We are actively
> refining our core concepts, protocols, and specifications, and will likely
> introduce major breaking changes prior to a stable release.

**Declare an agentic task with workspaces and model specifications. AX sandboxes it, wires up its workspace, and helps running it at scale.**

AX is a highly efficient, declarative orchestrator to run millions of autonomous agent workloads in a cluster. It runs on top of [Agent Substrate](https://github.com/agent-substrate/substrate) for sandboxed execution and is built to run millions of tasks per cluster. If you have used Kubernetes, `ax` will feel similar.

```yaml
# task.yaml
apiVersion: ax.io/v1alpha1
kind: Workspace
metadata:
  name: golang
spec:
  profile:          # tasks that bind this workspace and omit spec.profile use it
    image: "ghcr.io/my-org/my-coding-agent@sha256:..."
  setup:
    git:
      - repo: https://github.com/golang/go.git
        ref: "my-fix"
    files:
      - path: "AGENTS.md"
        content: |
          - Run `go test` to ensure that tests are passing.
          - Do not modify the public API.
---
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: test
spec:
  goal:
    text: "Fix what's broken please."
  workspace:
    name: golang
  debug: true   # enables `ax ssh`
```

Then apply it, resume it, and look over the agent's shoulder:

```bash
ax apply -f task.yaml
ax resume task test
ax describe task test
ax ssh test -- ls -al /workspace
```

## Why?

Agents are a new kind of workload. They are neither stateless microservices nor run-to-completion batch jobs. They accumulate state, need strict isolation, call out to model APIs and tool servers, and can burn money in a loop if nobody is watching. AX gives you three small primitives that handle all of that declaratively:

| You want to... | AX gives you |
|---|---|
| Run untrusted agent code in an isolated sandbox with CPU/memory limits | **`Task`** |
| Pre-wire Git repos, MCP servers, and skill packages so every agent starts warm | **`Workspace`** |
| Configure which LLM the platform itself uses, with credentials from a Kubernetes secret | **`Model`** |
| Decide which hosts an agent can reach, and inject credentials without the agent ever seeing them | **`Gateway`** |
| Pause an idle agent and pick up exactly where it left off | `ax suspend` / `ax resume` |
| Shell into a running agent to see what it is doing | `ax ssh` |

Everything is expressed as `ax.io/v1alpha1` manifests and applied with a single command.

## Quick start

### 1. Install the CLI

```bash
curl -fsSL https://github.com/google/ax/releases/latest/download/install.sh | sh
export PATH="$HOME/.ax/bin:$PATH"
```

> [!TIP]
> Prefer a UI? [AX Studio](https://github.com/agentexecutor/studio#readme) is a local web app that sets up AX on your computer and starts tasks for you. It deploys its own AX images, so you can skip steps 2 and 3. Install it as its README describes, then run `ax studio`.

### 2. Build the images

Until release images are published, build and push your own from an ax checkout and pin them by digest:

```bash
export AX_IMAGE_REPO=<your-registry>   # e.g. gcr.io/my-project
make images                            # prints the digest of each pushed image
export AX_SERVER_IMAGE="$AX_IMAGE_REPO/ax-server@<digest>"
```

For `ax local start` you can use a registry on `localhost` instead of a remote one: see [Using Locally Built Images](docs/local_installer.md#using-locally-built-images).

### 3. Install AX

**Locally**, on a new Kind cluster with Substrate and a WorkerPool:

```bash
ax local start --server-image "$AX_SERVER_IMAGE"
```

**On an existing cluster** with Kubernetes 1.37+ and [Agent Substrate](https://github.com/agent-substrate/substrate#readme):

```bash
ax install --server-image "$AX_SERVER_IMAGE"
```

Then create a WorkerPool from [`examples/workerpool.yaml`](examples/workerpool.yaml). Without one, tasks fail to resume with `no free workers available`.

See the [Local Installer guide](docs/local_installer.md) for prerequisites, `ax uninstall`, and more.

### Run your first task

From your ax checkout:

```bash
ax apply -f examples/simple.yaml         # the smallest useful Task
ax resume task simple-task               # new tasks start Suspended; resume schedules the actor and starts it
ax get tasks
# NAME          ATESPACE   STATE     ACTOR         AGE
# simple-task   default    Working   simple-task   1m

ax ssh simple-task -- ls -la /workspace  # poke around inside the sandbox
ax suspend task simple-task              # checkpoint and pause
ax resume task simple-task               # pick up where it left off
```

The first resume also waits for the workspace to initialize (cloning its git repos), so it can take a while; if the CLI gives up first, the task keeps coming up in the background — `ax get tasks` tells you where it is.

Every Task runs an image pinned by digest, named by `spec.profile.image` or `spec.profile.ateTemplate.name` or inherited from its Workspace's `spec.profile`; replace the image in `examples/simple.yaml` with `$AX_TASK_IMAGE` if you built your own.

Want to see the whole lifecycle end to end? Run `AX_TASK_IMAGE=... ./demo.sh` ([`demo.sh`](demo.sh)). It applies a custom workspace, resumes the task, runs commands over `ax ssh`, and suspends it again.

## Documentation

| Guide | Read it to... |
|---|---|
| [Local Installer](docs/local_installer.md) | Walk through `ax local`, `ax studio`, `ax ctx`, `ax install`, and promoting tasks unchanged from local Kind to a remote GKE cluster. |
| [AX Studio](https://github.com/agentexecutor/studio#readme) | Set up AX on your computer and start, watch, and shell into tasks from a local web app (`ax studio`). |
| [Concepts](docs/concepts.md) | Learn what a `Task`, `Workspace`, and `Model` each do, and how a task moves through phases and conditions. |
| [Manifests](docs/manifests.md) | Write your own YAML, with an annotated example of every kind. |
| [Sandbox](docs/sandbox.md) | See what the runner does on boot and what your command can rely on: metadata server, guest services, environment. |
| [Runners](docs/runner.md) | Understand the contract between the control plane and the task container, and build your own runner image to replace the default. |
| [Networking](docs/networking.md) | Reach a running task through the atenet router from the cluster, your laptop, or a gRPC client. |
| [Observability](docs/observability.md) | Collect ax-server's logs, metrics, traces, and events, and configure where they go. |
| [Architecture](DESIGN.md) | Understand how the control plane fits together, plus the [API reference](DESIGN.md#api-reference). |
| [Development](docs/development.md) | Build, test, and ship changes to AX itself. |
| [Roadmap](docs/roadmap.md) | See planned milestones across core specs, actor architecture, agentic environments, and governance. |

## CLI usage

`ax` talks to the control plane over gRPC. It is deliberately `kubectl`-shaped: `apply`, `get`, `describe`, `delete`, plus a few agent-specific verbs.

### Everyday commands

```bash
# Apply anything (multi-document YAML, file or stdin)
ax apply -f examples/simple.yaml

# Tasks
ax get tasks                          # list
ax get tasks -a my-atespace           # list in another atespace
ax get task task123                   # full spec + live status as YAML
ax describe task task123              # human-readable detail
ax suspend task task123               # checkpoint actor state and pause
ax resume task task123                # resume a suspended task
ax promote task task123 --to prod     # copy the task and what it binds to the cluster of kube context prod, and resume it there
ax delete task task123

# Shell into the running sandbox
ax ssh task123                        # interactive shell (task needs spec.debug: true)
ax ssh task123 -- ls -la /workspace   # one-off command
ax ssh task123 -- python3 main.py

# Workspaces, gateways, and models follow the same pattern

ax get workspaces
# NAME                ATESPACE   GIT-REPOS   MCP-SERVERS
# default-workspace   default    1           1
ax describe workspace default-workspace
ax delete workspace default-workspace

ax get gateways
# NAME            ATESPACE   EGRESS-RULES
# review-bot-gw   default    3
ax describe gateway review-bot-gw
ax delete gateway review-bot-gw

ax get models
# NAME            ATESPACE   PROVIDER   MODEL
# default-model   default    google     gemini-3.8-flash
ax describe model default-model
ax delete model default-model

# Environment & connection plumbing
ax local start --server-image <img>   # create/resume local Kind + Substrate + AX cluster
ax local status                       # check local environment, Substrate, and AX health
ax local stop                         # stop local environment while keeping state
ax local delete                       # destroy local environment and clean up owned context
ax install / ax uninstall             # install or remove AX on the active Kubernetes cluster
ax studio                             # launch the AX Studio local Web UI on 127.0.0.1:8080 (needs the ax-studio binary)
ax ctx                                # list kube contexts and active AX tunnel status
ax ctx <name>                         # switch active Kubernetes context in kubeconfig
ax tunnel list                        # background tunnels (state lives in ~/.ax/tunnels)
ax tunnel stop
ax version
```

### Works with `kubectx`

`ax` follows your active Kubernetes context. Switch clusters and `ax` resolves and tunnels to that cluster's control plane in the background.

```bash
kubectx staging-cluster
ax get tasks

kubectx prod-cluster
ax get tasks

# Or target a context without switching
ax --context=dev-cluster get tasks
```

### Global flags

| Flag | Description | Default |
|---|---|---|
| `-a`, `--atespace` | Atespace scope for the command | `default` |
| `-n`, `--namespace` | Kubernetes namespace where AX is installed | `ax-system` |
| `--context` | Kubernetes context to target | active `kubectx` / `current-context` |
| `--server` | Control plane address, bypassing auto-detection | derived from kube context, or `$AX_SERVER` |
| `--trace` | Trace the command and print its trace ID to stderr. Its RPCs send a sampled W3C `traceparent`, and CLI spans are exported over OTLP when `OTEL_EXPORTER_OTLP_ENDPOINT` is set. Without it, the CLI sends no trace context and ax-server samples on its own. See [CLI traces](docs/observability.md#cli-traces) | off |

## License

Apache License 2.0. See [LICENSE](LICENSE) for details.
