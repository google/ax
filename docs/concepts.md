# Core concepts

Every AX resource lives in an **atespace**. The default atespace is `default`.

## Task

The smallest unit of isolated execution. A `Task` declares the container image and command, compute limits, environment variables, and optionally a reference to a `Workspace` under `spec.workspace`. The workspace is mounted at `/workspace` unless the binding sets a path, and that path is the command's working directory.

The unit is deliberately small. An agent is not one process that runs to completion; over its lifetime it plans, delegates, retries, and fans work out. AX does not try to model that shape. It gives you one primitive that is cheap to create, isolate, suspend, and throw away, and lets the agent compose as many of them as its work demands. A single task may be the whole job, or it may be the root of a large tree of tasks spawned as the agent breaks the problem down. Either way each node gets the same sandbox, the same lifecycle, and the same tooling.

### Lifecycle

`status.phase` is a one-word summary of where the task is: `Running`, `Suspended`, `Failed`, `Terminating`, and so on. Conditions carry the detail. Check them with `ax describe`.

| Condition | True when |
|---|---|
| `WorkspaceReady` | The workspace has finished setting up. Stays True afterwards. |
| `Ready` | The task is running and `WorkspaceReady` is True. This is the one to wait on. |

`status.residency` says where the task's actor is on Agent Substrate: `queued` (no actor yet), `resuming`, `running`, `suspending`, `suspended`, `reverting`, `crashed`, `deleting`, or `deleted`. Unlike the phase, it tells a crashed or reverting actor apart from a task AX failed to provision. `status.actorUid` is the actor's Substrate UID, the `ate.actor.uid` that Substrate's logs and telemetry carry. `status.outcome` will say how the command ended (`succeeded`, `failed`, or `cancelled`); AX does not detect command exit yet ([#346](https://github.com/google/ax/issues/346)), so it is empty for now. `ax get tasks` shows the residency and outcome; `ax describe task` also shows the actor UID.

Two transitions are worth knowing. Suspending a task sets `Ready` to False with reason `Suspended`; resuming sets it back. Deleting a task tears down the sandbox on Agent Substrate and removes the record. `ax delete` blocks until that has completed.

## Workspace

Getting an agent to the point where it can start working is tedious. Before the first useful action it needs its data sources in place, such as repositories cloned at the right revision or buckets mounted, the tools it is allowed to call, and the skills it should bring along. Every task that needs the same environment repeats that setup, and every agent framework reinvents it. A `Workspace` exists to make that work declarative and done once.

A `Workspace` populates the filesystem and tool landscape. Declare it once, bind it from as many tasks as you like, and the runner materializes it inside each sandbox before the command starts:

- **Git repositories** cloned into subdirectories of the workspace path.
- **MCP servers and registries** (Model Context Protocol) that the agent can call.
- **Skill registries** and the path where skills are materialized.

## Model

A configuration for a model provider and its parameters: which provider to call, the model identifier, provider-specific generation parameters such as max_tokens, and references to the Kubernetes secret holding the credentials.

Declaring models as cluster resources makes them manageable and reusable across tasks. The configuration lives in one place instead of in every agent's environment, so rotating a key, pinning a new model version, or tuning parameters is a single `ax apply` rather than a hunt through task definitions. AX's own components read `Model` resources as well.
