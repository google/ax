# AX Design

## Architecture

Storing millions of short-lived tasks as Kubernetes CRDs pushes etcd past its comfort zone (single-digit GB storage limits, write-rate bottlenecks, control plane degradation). AX stores its state in PostgreSQL and reconciles directly with Agent Substrate under fine-grained distributed locks.

PostgreSQL holds resources as protojson documents alongside the leases that back AX's distributed locks. Lock waiters poll; nothing depends on `LISTEN/NOTIFY`, so any connection pooler works. `ax-server` creates any missing tables when it starts; there are no versioned migrations yet. The bundled manifest runs a single-instance StatefulSet with a persistent volume; any reachable PostgreSQL works. Storage sits behind the `store.Store` and `lock.Locker` interfaces. The store holds only what AX owns: specs, metadata, and the parts of a task's status that describe AX's own work (`Pending`, `Failed`, `Terminating`, the task ID, workspace setup). Everything that belongs to the task's actor (its phase while provisioned, `actor`, `workerIP`, and the `Ready` condition) is read from Agent Substrate whenever a task is fetched or listed, so AX never keeps a copy that can drift from the actor. The split lives in a few helpers in `internal/server/server.go` (`getTask`, `listTasks`, `saveTask`, `updateTask`, `deleteTask`), so storage implementations never need to know about Substrate.

```
                      ax apply -f task.yaml
                                │
                                ▼
                            ax-server
                      (gRPC API + /healthz)
                                │
         ┌──────────────────────┴──────────────────────┐
         ▼                                             ▼
     PostgreSQL                                 Agent Substrate
 (Resource Store,                       ┌───────────────────────────────┐
  Locks)                                │ • Atespace Provisioning       │
                                        │ • Actor Creation & Activation │
                                        │ • Worker Assignment           │
                                        └───────────────────────────────┘
```

## Components

| Binary | Role |
|---|---|
| `ax` | Developer CLI. Applies manifests, inspects resources, tunnels to the cluster. |
| `ax-server` | Direct-execution gRPC API on port 8080. Validates manifests, manages distributed locks, reconciles directly with Agent Substrate, and persists state to PostgreSQL. |
| `ax-task-runner` | Entrypoint inside every task container. Bootstraps the workspace, serves metadata, and runs the agent command. A thin wrapper over the `runner` package, which custom images can embed directly. |

## API reference

The control plane exposes the `ax.v1alpha1.AX` gRPC service. Health checks are plain HTTP: `GET /healthz` on the same port returns `200 OK`.

**Tasks**

| RPC | Description |
|---|---|
| `GetTask` | Get a task by atespace and name. |
| `ListTasks` | List tasks in an atespace, with pagination. |
| `CreateTask` | Create a task (tasks are immutable once created). |
| `DeleteTask` | Delete a task. |
| `SuspendTask` | Checkpoint actor state and pause the task. |
| `ResumeTask` | Resume a suspended task. |

**Workspaces**

| RPC | Description |
|---|---|
| `GetWorkspace` | Get a workspace by atespace and name. |
| `ListWorkspaces` | List workspaces in an atespace. |
| `UpdateWorkspace` | Create or update a workspace. |
| `DeleteWorkspace` | Delete a workspace. |

**Models**

| RPC | Description |
|---|---|
| `GetModel` | Get a model configuration by atespace and name. |
| `ListModels` | List model configurations in an atespace. |
| `UpdateModel` | Create or update a model configuration. |
| `DeleteModel` | Delete a model configuration. |

Request and response types follow the `<Method>Request` / `<Method>Response` convention. Generated Go types live in [`pkg/apis/v1alpha1`](pkg/apis/v1alpha1).
