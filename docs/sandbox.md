# Inside the sandbox

Every task container starts with `ax-task-runner` as PID 1. On boot it:

1. Loads the `Task` and every bound `Workspace` spec.
2. Starts a metadata and guest-management daemon on port 80.
3. On the first run, prepares each workspace at its own path, in binding order: clones Git repos, sets up the skills path, and, if the binding has a goal, hands that goal to an Antigravity agent to finish environment setup. The agent needs `GEMINI_API_KEY` in the container and gets 10 minutes by default; set `AX_BOOTSTRAP_TIMEOUT` to a Go duration to change that. The task reports not-ready until every workspace, including any agent run, has finished.
4. Starts `spec.command` as a child process, with the first workspace as its working directory and `AX_METADATA_URL` plus `spec.env` in its environment, and supervises it.

The runner stays up as PID 1 whether or not the command is still running, so the metadata server keeps answering and `ax ssh` still works after the command has exited. Its exit code is logged. When the sandbox is stopped or suspended, the runner sends the command's process group `SIGTERM`, waits ten seconds, and then kills whatever is left.

## Metadata server

The daemon speaks HTTP/1.1 and `h2c` on the same port. Your agent can introspect its own configuration without any SDK:

| Endpoint | Method | Returns | Description |
|---|---|---|---|
| `/healthz` | `GET` | `text/plain` | Liveness. Always `200 OK`. |
| `/readyz` | `GET` | `text/plain` | Readiness. `503` while the workspace is initializing, `200` once clones, MCP config, and skills are in place. |
| `/metadata/v1alpha1/ax/task` | `GET` | `application/yaml` | Task launch configuration, excluding status. |
| `/metadata/v1alpha1/ax/workspaces` | `GET` | `application/yaml` | Every bound `Workspace`, as a multi-document stream in binding order. |

```bash
# From inside a task:
curl -s "$AX_METADATA_URL/metadata/v1alpha1/ax/task"
```

## Guest services

When a task sets `spec.debug: true`, the same port also serves the [Agent Substrate guest services](https://github.com/agent-substrate/env) over gRPC. They are off by default because they allow arbitrary process execution and file access inside the sandbox, and `ax ssh` refuses to connect to a task that has not enabled them.

- **Process service**: start, inspect, stream output from, and kill processes. This is what powers `ax ssh`.
- **File system service**: streaming file reads and writes inside the workspace.

## Environment provided to your command

| Variable | Value |
|---|---|
| `AX_METADATA_URL` | Base URL of the metadata server, e.g. `http://127.0.0.1:80` |
