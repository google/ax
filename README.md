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
  git:
    - repo: https://github.com/golang/go.git
      branch: "my-fix"
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
  image: "ghcr.io/my-org/my-coding-agent@sha256:..."
  config:
    goal: "Fix what's broken please."
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
| Pause an idle agent and pick up exactly where it left off | `ax suspend` / `ax resume` |
| Shell into a running agent to see what it is doing | `ax ssh` |

Everything is expressed as `ax.io/v1alpha1` manifests and applied with a single command.

## Quick start

### Local environment in one command (`ax local`)

With Docker or Podman running on your machine (and a Linux 6.8+ kernel on the container host), `ax local up` provisions a local Kubernetes 1.36+ Kind cluster, bootstraps Agent Substrate (`v1.0.0-rc.1`), builds and deploys AX, and selects the `local` kubeconfig context:

```bash
GOTOOLCHAIN=auto go build -o ./bin/ax ./cmd/ax
./bin/ax local up --from-source .
./bin/ax local status
```

To install AX onto an existing Kubernetes 1.36+ cluster that already runs Agent Substrate:

```bash
./bin/ax install --context <kube-ctx> --from-source . --image-repo <your-registry>
```

### Manual cluster deployment (`make images` / `ax install`)

If you prefer deploying manually to an existing cluster (or see [docs/installation.md](docs/installation.md) for the full version-pinned audit recipe), you need:

- A Kubernetes 1.36+ cluster with **Agent Substrate** installed (see the [Substrate README](https://github.com/agent-substrate/substrate#readme))
- [Go](https://go.dev/doc/install) and `kubectl`
- [Podman](https://podman.io/) or [Docker](https://docs.docker.com/get-docker/), authenticated to a container registry your cluster can pull from

Substrate lands in the `ate-system` namespace and exposes its Control API at `api.ate-system.svc.cluster.local:443`. Verify it is up before moving on:

```bash
kubectl get svc api -n ate-system
```

#### 1. Build the CLI

```bash
make install   # installs `ax` from this checkout into $(go env GOPATH)/bin
```

#### 2. Build and push the images

Point `AX_IMAGE_REPO` at your registry, then build the `ax-server` and `ax-task-runner` images and push them to it:

```bash
export AX_IMAGE_REPO=<your-registry>   # e.g. gcr.io/my-project
make images
```

#### 3. Deploy the control plane

Use `ax install` so `ax-system` is labeled (`ate.dev/api-access=true`) and `ax-server` is registered in Substrate's caller allowlists before deploying PostgreSQL and `ax-server`:

```bash
ax install --image-repo "$AX_IMAGE_REPO" --image-tag latest
```

#### 4. Create a WorkerPool

Every task runs as a Substrate actor, and an actor needs a free worker from a `WorkerPool`. (`ax local up` creates one automatically; on an existing cluster neither `ax install` nor the Substrate installer creates one for you, so without a pool the first `ax resume` fails with `ResourceExhausted: no free workers available`.)

Create a small gVisor pool from [`examples/workerpool.yaml`](examples/workerpool.yaml) (its header explains how to apply it), or follow the WorkerPool step in the [installation guide](docs/installation.md#substrate-setup). Then wait until `kubectl get workerpools -A` shows the workers ready.

### Run your first task

From your ax checkout:

```bash
ax apply -f examples/simple.yaml         # the smallest useful Task
ax resume task simple-task               # new tasks start Suspended; resume schedules the actor and starts it
ax get tasks
# NAME          ATESPACE   PHASE     ACTOR         WORKER-IP    AGE
# simple-task   default    Running   simple-task   10.20.3.67   1m

ax ssh simple-task -- ls -la /workspace  # poke around inside the sandbox
ax suspend task simple-task              # checkpoint and pause
ax resume task simple-task               # pick up where it left off
```

The first resume also waits for the workspace to initialize (cloning its git repos), so it can take a while; if the CLI gives up first, the task keeps coming up in the background — `ax get tasks` tells you where it is.

Want to see the whole lifecycle end to end? Run [`./demo.sh`](demo.sh). It applies a custom workspace, resumes the task, runs commands over `ax ssh`, and suspends it again.

## Documentation

| Guide | Read it to... |
|---|---|
| [Local Installer](docs/local_installer.md) | Walk through `ax local`, `ax ctx`, `ax install`, and promoting tasks unchanged from local Kind to a remote GKE cluster. |
| [Concepts](docs/concepts.md) | Learn what a `Task`, `Workspace`, and `Model` each do, and how a task moves through phases and conditions. |
| [Manifests](docs/manifests.md) | Write your own YAML, with an annotated example of every kind. |
| [Sandbox](docs/sandbox.md) | See what the runner does on boot and what your command can rely on: metadata server, guest services, environment. |
| [Runners](docs/runner.md) | Understand the contract between the control plane and the task container, and build your own runner image to replace the default. |
| [Networking](docs/networking.md) | Reach a running task through the atenet router from the cluster, your laptop, or a gRPC client. |
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

# Environment & connection plumbing
ax local up --from-source .           # create/resume local Kind + Substrate + AX cluster
ax local status                       # check local cluster, Substrate, and AX health
ax local stop                         # stop local cluster containers while keeping state
ax local delete                       # destroy local cluster and clean up context
ax install / ax uninstall             # install or remove AX on the active Kubernetes cluster
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

## License

Apache License 2.0. See [LICENSE](LICENSE) for details.
