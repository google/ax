# Development

## Prerequisites

- Go 1.27+
- [Podman](https://podman.io) (or Docker) to run tasks locally and to build the task runner image
- For cluster work: [`ko`](https://ko.build/), a Kubernetes cluster with Agent Substrate, and a kubeconfig

## Build

```bash
make build                 # bin/ax, bin/ax-server
make install               # install the ax CLI into $(go env GOPATH)/bin
make build-local-image     # build the runner for this machine and tag it as the default task image
make build-task-runner     # cross-compile the runner for linux/amd64 and build its image
make push-task-runner      # ...and push it (set TASK_RUNNER_REPO)
```

## Local mode

Unless it's pointed at a cluster (`ax config use-context kube`, `--context`, `--server`), `ax` runs the whole control plane on your machine. You need no cluster, Redis, or daemon:

```bash
podman machine start           # macOS/Windows; Linux runs Podman natively
make build-local-image
ax apply -f examples/local.yaml
ax resume task hello
ax ssh hello -- ls /workspace
```

Each `ax` command starts the regular AX API server in-process and talks to it over an in-memory gRPC connection. The pieces it plugs in differ from a cluster deployment:

| Cluster | Local |
|---|---|
| Redis store | SQLite at `~/.ax/db.sqlite` ([`internal/store/sqlite`](../internal/store/sqlite)) |
| Redis locks | `flock` on files in `~/.ax/locks` ([`lock.FileLocker`](../internal/lock/file.go)) |
| Substrate reconciler | Container reconciler ([`internal/container`](../internal/container)) |

The container reconciler maps the Substrate actor lifecycle onto containers:

| Task operation | Agent Substrate | Local container |
|---|---|---|
| create | ActorTemplate + Actor, suspended | image pull if missing, `create` (not started) |
| resume | resume actor on a worker | `start`, or `unpause` if paused |
| suspend | snapshot and suspend actor | `pause` (processes frozen in memory) |
| delete | delete actor and templates | `rm --force` plus the workspace volume |

Each task container runs the image with `/usr/local/bin/ax-task-runner` as its entrypoint, the same command Substrate uses. It gets `AX_TASK_YAML`, `AX_WORKSPACE_YAML`, the task's `spec.env`, and a named volume `ax.<atespace>.<task>.workspace` at `/workspace`. `spec.resources.limits` become `--cpus` and `--memory`. The runner's port 80 is published on an ephemeral `127.0.0.1` port and recorded as the task's worker address, so readiness checks and `ax ssh` connect to it directly.

Some notes:

- **Runtime.** `ax` uses `podman`, then `docker`, then `nerdctl`, whichever it finds first on `PATH`. Set `AX_CONTAINER_RUNTIME` to choose one.
- **Secrets.** `GEMINI_API_KEY` is taken from your shell instead of a Kubernetes secret. It and the task/workspace YAML reach the container through the CLI's environment (`-e NAME`), never as command-line arguments.
- **Images.** The default task image (`gcr.io/ax-substrate/ate-images/ax-task-runner`) is published for linux/amd64 only. `make build-local-image` builds a native image under that name, so tasks start without a registry pull or emulation. Images pinned by digest, as in `examples/task.yaml`, are pulled from their registry.
- **State.** Set `AX_HOME` to keep the database, locks, and config somewhere other than `~/.ax`. Containers keep running between commands. If the Podman machine restarts, `ax resume` starts a task's container again.
- **Debugging.** Set `AX_DEBUG=1` to see the embedded server's logs. `podman logs ax.<atespace>.<task>` shows the runner's.
- **Isolation.** Local tasks are ordinary containers. They don't get the gVisor sandboxing or snapshot-based suspend that Agent Substrate provides, so only run workloads you'd run in a container.

## Test

Runs everything, including the mock Substrate gRPC server, a fake container CLI for the local reconciler, the store implementations, and API server tests:

```bash
make test        # or: go test -v ./...
```

## Contributing

See [CONTRIBUTING.md](../CONTRIBUTING.md) for contribution guidelines, CLA requirements, and PR workflows.
