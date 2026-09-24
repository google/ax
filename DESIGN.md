# AX Design

## Architecture

Storing millions of short-lived tasks as Kubernetes CRDs pushes etcd past its comfort zone (single-digit GB storage limits, write-rate bottlenecks, control plane degradation). AX keeps its state in Redis and uses Redis Streams as the work queue between the API server and a horizontally scaled pool of controllers.

```
                      ax apply -f task.yaml
                                │
                                ▼
                            ax-server
                      (gRPC API + /healthz)
                                │
                     store & publish event
                                │
                                ▼
                              Redis
               (Task Hashes + Event Streams + PubSub)
                                │
                      XREADGROUP (Streams)
                                │
                                ▼
                          ax-controller
                   (Horizontally Scaled Workers)
                                │
                        gRPC (Control API)
                                │
                                ▼
                         Agent Substrate
                ┌───────────────────────────────┐
                │ • Atespace Provisioning       │
                │ • Actor Creation & Activation │
                │ • Worker Assignment           │
                │ • Egress Policy Filtering     │
                └───────────────────────────────┘
```

## Components

| Binary | Role |
|---|---|
| `ax` | Developer CLI. Applies manifests, inspects and watches resources, tunnels to the cluster. |
| `ax-server` | Stateless gRPC API on port 8080. Validates manifests, persists to Redis, publishes events. |
| `ax-controller` | Reconciliation workers. Consume the Redis stream, provision atespaces and actors on Agent Substrate, apply egress policy, and drive tasks toward desired state. Scale by adding replicas. |
| `ax-task-runner` | Entrypoint inside every task container. Bootstraps the workspace, serves metadata, and runs the agent command. A thin wrapper over the `runner` package, which custom images can embed directly. |

## API reference

The control plane exposes the `ax.v1alpha1.AX` gRPC service. Health checks are plain HTTP: `GET /healthz` on the same port returns `200 OK`.

**Tasks**

| RPC | Description |
|---|---|
| `CreateTask` | Create a task. Tasks are immutable once created. |
| `GetTask` | Get a task by atespace and name. |
| `ListTasks` | List tasks in an atespace, with pagination. |
| `DeleteTask` | Delete a task. |
| `SuspendTask` | Checkpoint actor state and pause the task. |
| `ResumeTask` | Resume a suspended task. |
| `WatchTask` | Server-streaming RPC that emits status and condition transitions as they happen. |

**Gateways**

| RPC | Description |
|---|---|
| `GetGateway` | Get a gateway by atespace and name. |
| `ListGateways` | List gateways in an atespace. |
| `UpdateGateway` | Create or update a gateway. |
| `DeleteGateway` | Delete a gateway. |

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
