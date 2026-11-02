# Claude Code runner

`claude-code-runner` is a [runner](../../docs/runner.md) that runs [Claude Code](https://claude.com/claude-code) headlessly, configured from the `Task` itself. It embeds the standard `runner` package, so workspace setup, supervision, probes, and shutdown work exactly as they do with the default runner. The only difference is where the command comes from. When a task leaves `spec.command` empty, the runner builds a `claude --print` command from `spec.config` and connects it to the MCP servers of the task's Workspace.

A task that sets `spec.command` runs that command unchanged, so the same image also works for tasks that call `claude` themselves.

## Build the image

The image is built from [`Dockerfile.claude-code`](../../Dockerfile.claude-code). It is the default task image with Claude Code installed and `claude-code-runner` at `/usr/local/bin/ax-task-runner`:

```bash
make push-claude-code-image CLAUDE_CODE_REPO=<your-registry>/<image>
make push-claude-code-image CLAUDE_CODE_VERSION=2.1.310   # pin a Claude Code version
```

The target cross-compiles the runner, builds and pushes the image, and prints the digest to pin in `spec.image`. Build on an amd64 host: the Claude Code installer runs the binary it downloads, and that binary aborts under qemu emulation.

## Configure a task

`spec.config` holds the prompt and `claude`'s command-line flags. Keys are camelCase: take a flag from `claude --help`, drop its leading dashes, and capitalize each word after the first. So `--max-turns` becomes `maxTurns` and `--allowed-tools` becomes `allowedTools`. The runner always passes `--print`, then one flag per key in sorted order, then the prompt:

```yaml
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: fix-tests
spec:
  image: "gcr.io/my-project/claude-code@sha256:..."
  config:
    prompt: "Run the test suite and fix any failing tests."
    model: claude-opus-5-5
    maxTurns: 50
    permissionMode: bypassPermissions
    allowedTools: ["Bash", "Edit", "Write"]
    settings:
      includeCoAuthoredBy: false
  env:
    - name: ANTHROPIC_API_KEY
      value: "${ANTHROPIC_API_KEY}"
  workspace:
    name: my-repo
```

This config runs:

```
claude --print --allowed-tools=Bash --allowed-tools=Edit --allowed-tools=Write \
  --max-turns=50 --model=claude-opus-5-5 --permission-mode=bypassPermissions \
  --settings={"includeCoAuthoredBy":false} -- "Run the test suite and fix any failing tests."
```

`prompt` is the positional `[prompt]` argument and is required. Every other key is a flag, and its value becomes arguments according to its type:

| Value | Arguments |
|---|---|
| string or number | `--flag=value` |
| `true` | `--flag` |
| `false` | nothing |
| list of strings or numbers | `--flag=value` once per element |
| object | `--flag=<JSON>`, for flags such as `--settings` that accept a JSON string |

The runner has no list of valid flags. `claude` checks them itself, so an unknown flag or a bad value fails the session with an error in the container log.

### Permissions

The task runs as root with nobody there to approve tool use, so a useful headless session needs `permissionMode: bypassPermissions` (or `dangerouslySkipPermissions: true`). As root, Claude Code accepts these only when `IS_SANDBOX=1` is set, which the image does. The sandbox is the isolation boundary. Use `allowedTools` and `disallowedTools` to narrow what the agent may do, for example `["Bash(pytest:*)", "Edit"]`.

### Output

Claude Code's output goes to the container log. To keep a report in the workspace, ask for one in the prompt, for example "write a summary to /workspace/claude-output.md", and read it with `ax ssh <task> -- cat /workspace/claude-output.md`. Set `outputFormat: json` or `outputFormat: stream-json` (with `verbose: true`) for machine-readable output.

## MCP servers

MCP servers are declared on the Workspace, not in `spec.config`, so every agent bound to a workspace sees the same tools. The runner converts the Workspace's `spec.mcp.servers`, plus any servers listed under `spec.mcp.registries`, into Claude Code's MCP config and passes it as JSON with `--mcp-config`:

```yaml
apiVersion: ax.io/v1alpha1
kind: Workspace
metadata:
  name: my-repo
spec:
  mcp:
    servers:
      - name: docs
        endpoint: "http://docs-mcp.default.svc.cluster.local:8080/mcp"
      - name: git-tools
        command: "git-mcp"
        args: ["--repo", "/workspace"]
```

This Workspace produces:

```json
{"mcpServers": {
  "docs": {"type": "http", "url": "http://docs-mcp.default.svc.cluster.local:8080/mcp"},
  "git-tools": {"type": "stdio", "command": "git-mcp", "args": ["--repo", "/workspace"]}
}}
```

A server with an `endpoint` is reached over streamable HTTP. A server with a `command` is started over stdio inside the sandbox, so that command must exist in the image. A server with neither, or whose name is already taken, is skipped with a warning in the container log. If names collide, static servers win over registry servers. Registry queries are not resolved yet; only servers already listed under a registry are used. MCP servers in a `.mcp.json` checked into the repository are still picked up by Claude Code as usual.

## Credentials

`spec.config` is stored in plain text and shown by `ax get task`, so keep credentials out of it, including in `settings.env`. Pass `ANTHROPIC_API_KEY` through `spec.env`, substituting it at apply time instead of committing it:

```bash
ANTHROPIC_API_KEY=sk-ant-... envsubst < examples/claude-code/task.yaml | ax apply -f -
```

## Debugging

The runner writes the command it builds into `spec.command` of the `Task` it serves from the metadata endpoint. Inside the sandbox, this shows exactly what ran:

```bash
curl -s "$AX_METADATA_URL/metadata/v1alpha1/ax/task"
```

To try the runner outside a cluster, pass the specs as files. Set `spec.workspace.path` in the task to a local directory, since `/workspace` is usually not writable on a laptop:

```bash
go build -o claude-code-runner ./cmd/runners/claude-code-runner
./claude-code-runner --task-file task.yaml --workspace-file workspace.yaml --port 8080
```

See [`task.yaml`](task.yaml) for a complete Task and Workspace.
