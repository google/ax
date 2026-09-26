# AX Design

## Architecture

Storing millions of short-lived tasks as Kubernetes CRDs pushes etcd past its comfort zone (single-digit GB storage limits, write-rate bottlenecks, control plane degradation). AX stores its state in Redis and reconciles directly with Agent Substrate under fine-grained distributed locks.

```
                      ax apply -f task.yaml
                                │
                                ▼
                            ax-server
                      (gRPC API + /healthz)
                                │
         ┌──────────────────────┴──────────────────────┐
         ▼                                             ▼
       Redis                                    Agent Substrate
 (Resource Store,                       ┌───────────────────────────────┐
  Locks, PubSub)                        │ • Atespace Provisioning       │
                                        │ • Actor Creation & Activation │
                                        │ • Worker Assignment           │
                                        └───────────────────────────────┘
```

## Components

| Binary | Role |
|---|---|
| `ax` | Developer CLI. Applies manifests, inspects and watches resources, tunnels to the cluster. |
| `ax-server` | Direct-execution gRPC API on port 8080. Validates manifests, manages distributed locks, reconciles directly with Agent Substrate, and persists state to Redis. |
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
| `WatchTask` | Server-streaming RPC that emits status and condition transitions as they happen. |

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
