<h1>
  <img src="assets/axolotl.svg" width="70" align="absmiddle" alt="AX axolotl">
  AX
</h1>

> [!WARNING]
> AX and several of its features are in heavy development. We are actively
> refining our core concepts, protocols, and specifications, and will likely
> introduce major breaking changes prior to a stable release.

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
  image: "YOUR_RUNNER_REPOSITORY@sha256:YOUR_DIGEST"
  workspaces:
    - name: golang
      goal: "Ensure that Go tool chain is available and is built from source"
  debug: true   # lets you `ax ssh` into the sandbox
```

After [installing AX](docs/installation.md), replace the image with your runner's
digest reference, then apply it and look over the agent's shoulder:

```bash
ax apply -f task.yaml
ax resume task test
ax watch task test                  # Ctrl-C to stop watching
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

## Version compatibility

Build the CLI, control plane, and task runner from the **same AX ref**. In
particular, `go install ...@latest` selects a released CLI, which can use a
different RPC API from a server built from `main`.

These are the Substrate dependencies recorded in each AX ref's `go.mod`.
They identify source pins; they do not certify an end-to-end deployment or
compatibility with an entire Substrate release series. The pseudo-versions below
refer to commits, not published Substrate release tags.

| AX ref | Substrate module version | Substrate checkout |
|---|---|---|
| [v0.3.0](https://github.com/google/ax/blob/v0.3.0/go.mod) | `v0.0.0-20260911232748-672533541dbf` | [`672533541dbf`](https://github.com/agent-substrate/substrate/commit/672533541dbfcd29084e4de2475267088bda3651) |
| [v0.3.1](https://github.com/google/ax/blob/v0.3.1/go.mod) | `v0.0.0-20260911232748-672533541dbf` | [`672533541dbf`](https://github.com/agent-substrate/substrate/commit/672533541dbfcd29084e4de2475267088bda3651) |
| [Development baseline `8d2ccf36f194`](https://github.com/google/ax/blob/8d2ccf36f194f6f58c57b90753bd3986639b562f/go.mod) | `v0.4.1-0.20261009152331-f6a1dc72a35d` | [`f6a1dc72a35d`](https://github.com/agent-substrate/substrate/commit/f6a1dc72a35d9875302e2998b2aa23b67dbc732a) |

The development baseline uses ax-server and Redis. v0.3.0 and v0.3.1 also
deploy a separate ax-controller; use their own checkouts' manifests and build
targets. For another AX ref, inspect its `go.mod` rather than substituting the
latest Substrate release. Update this table and the installation guide when
changing the documented baseline or publishing an AX release.

<a id="2-deploy-the-control-plane"></a>

## Quick start

Follow the [version-pinned installation guide](docs/installation.md) to build
the matching CLI and images, configure Substrate, and run a minimal task. It
uses your registry and digest-pinned AX images while public release images are
being addressed in [#487](https://github.com/google/ax/issues/487).

## Documentation

| Guide | Read it to... |
|---|---|
| [Installation](docs/installation.md) | Choose matching AX/Substrate sources and deploy your own digest-pinned AX images. |
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

Before applying `examples/task.yaml`, replace its image with your runner digest
from the [installation guide](docs/installation.md).

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
