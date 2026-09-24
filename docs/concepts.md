# Core concepts

Every AX resource lives in an **atespace**. The default atespace is `default`.

## Task

The smallest unit of isolated execution. A `Task` declares the container image and command, compute requests and limits, environment variables, and references to one or more `Workspace`s under `spec.workspaces`. Each workspace is mounted at its own path, and the first serves as the command's working directory.

The unit is deliberately small. An agent is not one process that runs to completion; over its lifetime it plans, delegates, retries, and fans work out. AX does not try to model that shape. It gives you one primitive that is cheap to create, isolate, suspend, and throw away, and lets the agent compose as many of them as its work demands. A single task may be the whole job, or it may be the root of a large tree of tasks spawned as the agent breaks the problem down. Either way each node gets the same sandbox, the same lifecycle, and the same tooling.

### Lifecycle

`status.phase` is a one-word summary of where the task is: `Running`, `Suspended`, `Failed`, `Terminating`, and so on. Conditions carry the detail. Watch them with `ax watch` or poll them with `ax describe`.

| Condition | True when |
|---|---|
| `WorkspaceReady` | Every workspace has finished setting up. Stays True afterwards. |
| `Ready` | The task is running and `WorkspaceReady` is True. This is the one to wait on. |

Two transitions are worth knowing. Suspending a task sets `Ready` to False with reason `TaskSuspended`; resuming sets it back. Deleting a task moves it to `Terminating` while the controller tears down the sandbox, then removes the record entirely. `ax delete` blocks until that has happened.

## Workspace

Getting an agent to the point where it can start working is tedious. Before the first useful action it needs its data sources in place, such as repositories cloned at the right revision or buckets mounted, the tools it is allowed to call, and the skills it should bring along. Every task that needs the same environment repeats that setup, and every agent framework reinvents it. A `Workspace` exists to make that work declarative and done once.

A `Workspace` populates the filesystem and tool landscape. Declare it once, bind it from as many tasks as you like, and the runner materializes it inside each sandbox before the command starts:

- **Git repositories** cloned into subdirectories of the workspace path.
- **MCP servers and registries** (Model Context Protocol) that the agent can call.
- **Skill registries** and the path where skills are materialized.

A binding can also carry a `goal`, a plain-language description of the environment the task needs. On first boot the runner hands that goal to an agent that finishes the setup, for example installing a toolchain or dependencies, so the task's own command starts in a ready environment.

## Model

A `Model` is not a model. It is a named model configuration: which provider to call, which model identifier to use, provider-specific generation parameters such as temperature, and a reference to the Kubernetes secret holding the API key.

Declaring it as a resource is what makes it manageable across the cluster. The configuration lives in one place instead of in every agent's environment, so rotating a key, pinning a new model version, or tightening a parameter is one `ax apply` rather than a hunt through task definitions. AX's own components read it too, for example when planning a workspace from a goal.
