<h1>
  <img src="assets/axolotl.svg" width="70" align="absmiddle" alt="AX axolotl">
  AX
</h1>

> [!WARNING]
> We are still actively refining our core concepts, protocols,
> and specifications. We will likely to introduce major breaking
> changes prior to a stable release.

**Declare an agentic task with workspaces and model specifications. AX sandboxes it, wires up its workspace, and helps running it at scale.**

AX is a high-throughput, declarative orchestrator to run billions of autonomous agent workloads in a cluster. It runs on top of [Agent Substrate](https://github.com/agent-substrate/substrate) for sandboxed execution and is built to run billions of tasks per cluster. If you have used Kubernetes, `ax` will feel similar.

```yaml
# task.yaml
apiVersion: ax.io/v1alpha1
kind: Workspace
metadata:
  name: golang
spec:
  git:
    - repo: https://github.com/golang/go.git
      branch: "my-fix"
---
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: test
spec:
  workspaces:
    - name: golang
      goal: "Ensure that Go tool chain is available and is built from source"
  debug: true   # lets you `ax ssh` into the sandbox
```

Then apply it, watch it come up, and look over the agent's shoulder:

```bash
ax apply -f task.yaml
ax watch task test
ax ssh test -- ls -al /workspace
```

## Why?

Agents are a new kind of workload. They are neither stateless microservices nor run-to-completion batch jobs. They accumulate state, need strict isolation, call out to model APIs and tool servers, and can burn money in a loop if nobody is watching. AX gives you three small primitives that handle all of that declaratively:

| You want to... | AX gives you |
|---|---|
| Run untrusted agent code in an isolated sandbox with CPU/memory limits | **`Task`** |
| Pre-wire Git repos, MCP servers, and skill packages so every agent starts warm | **`Workspace`** |
| Configure which LLM the platform itself uses, with credentials from a Kubernetes secret | **`Model`** |
| Pause an idle agent and pick up exactly where it left off | `ax suspend` / `ax resume` |
| Shell into a running agent to see what it is doing | `ax ssh` |

Everything is expressed as `ax.io/v1alpha1` manifests and applied with a single command.

## Quick start

### Prerequisites

AX schedules every task as a sandboxed actor on [Agent Substrate](https://github.com/agent-substrate/substrate), so Substrate must be running in your cluster before you deploy AX. You need:

- A Kubernetes cluster with **Agent Substrate** installed (see below)
- [Go](https://go.dev/doc/install) and `kubectl`
- [`ko`](https://ko.build/) (`brew install ko`) and a container registry your cluster can pull from

To install Agent Substrate, follow the instructions in the [Substrate README](https://github.com/agent-substrate/substrate#readme). Substrate lands in the `ate-system` namespace and exposes its Control API at `api.ate-system.svc.cluster.local:443`, which is where AX expects to find it. Verify it is up before moving on:

```bash
kubectl get svc api -n ate-system
```

### 1. Install the CLI

```bash
go install github.com/google/ax/cmd/ax@latest
```

This puts the `ax` binary in `$(go env GOPATH)/bin`. Make sure that directory is on your `PATH`.

### 2. Deploy the control plane

With the [prerequisites](#prerequisites) in place — most importantly a reachable Agent Substrate Control API — deploy the AX control plane:

```bash
make deploy AX_IMAGE_REPO=<your-registry>
```

This deploys Redis, then builds and deploys the control plane images with `ko`. Everything lands in the `ax-system` namespace.

### 3. Run your first task

```bash
ax apply -f examples/task.yaml       # Task + Workspace + Model in one file
ax get tasks
# NAME      ATESPACE   PHASE     ACTOR           WORKER-IP    AGE
# task123   default    Running   task123         10.20.3.67   1m

ax watch task task123                # stream phase and condition changes live
ax ssh task123 -- ls -la /workspace  # poke around inside the sandbox
ax suspend task task123              # checkpoint and pause
ax resume task task123               # pick up where it left off
```

Want to see the whole lifecycle end to end? Run [`./demo.sh`](demo.sh). It applies a custom workspace, waits for readiness, runs commands over `ax ssh`, and suspends the task.

## Documentation

| Guide | Read it to... |
|---|---|
| [Concepts](docs/concepts.md) | Learn what a `Task`, `Workspace`, and `Model` each do, and how a task moves through phases and conditions. |
| [Manifests](docs/manifests.md) | Write your own YAML, with an annotated example of every kind. |
| [Sandbox](docs/sandbox.md) | See what the runner does on boot and what your command can rely on: metadata server, guest services, environment. |
| [Runners](docs/runner.md) | Understand the contract between the control plane and the task container, and build your own runner image to replace the default. |
| [Networking](docs/networking.md) | Reach a running task through the atenet router from the cluster, your laptop, or a gRPC client. |
| [Architecture](DESIGN.md) | Understand how the control plane fits together, plus the [API reference](DESIGN.md#api-reference). |
| [Development](docs/development.md) | Build, test, and ship changes to AX itself. |
| [Roadmap](docs/roadmap.md) | See planned milestones across core specs, actor architecture, agentic environments, and governance. |

## CLI usage

`ax` talks to the control plane over gRPC. It is deliberately `kubectl`-shaped: `apply`, `get`, `describe`, `watch`, `delete`, plus a few agent-specific verbs.

### Everyday commands

```bash
# Apply anything (multi-document YAML, file or stdin)
ax apply -f examples/task.yaml

# Tasks
ax get tasks                          # list
ax get tasks -a my-atespace           # list in another atespace
ax get task task123                   # full spec + live status as YAML
ax describe task task123              # human-readable detail
ax watch task task123                 # stream status and condition transitions
ax suspend task task123               # checkpoint actor state and pause
ax resume task task123                # resume a suspended task
ax delete task task123

# Shell into the running sandbox
ax ssh task123                        # interactive shell (task needs spec.debug: true)
ax ssh task123 -- ls -la /workspace   # one-off command
ax ssh task123 -- python3 main.py

# Workspaces and models follow the same pattern

ax get workspaces
# NAME                ATESPACE   GIT-REPOS   MCP-SERVERS
# default-workspace   default    1           1
ax describe workspace default-workspace
ax delete workspace default-workspace

ax get models
# NAME            ATESPACE   PROVIDER   MODEL
# default-model   default    google     gemini-3.8-flash
ax describe model default-model
ax delete model default-model

# Connection plumbing
ax ctx                                # active kube context and how ax is reaching the control plane
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

## License

Apache License 2.0. See [LICENSE](LICENSE) for details.
