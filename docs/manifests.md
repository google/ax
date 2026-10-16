# Writing manifests

The three supported kinds, `Task`, `Workspace`, and `Model`, can live in one multi-document YAML file. See [`examples/task.yaml`](../examples/task.yaml) for a complete set.

`metadata.name` and `metadata.atespace` become Substrate resource names, so they must be lowercase RFC 1123 labels: at most 63 lowercase alphanumeric characters or `-`, starting and ending with an alphanumeric character. `ax apply` rejects anything else up front rather than letting the task fail later with `ActorCreationFailed`.

## Task

```yaml
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: task123
  atespace: default
spec:
  image: "ghcr.io/my-org/my-agent-image@sha256:<digest>"
  command: ["python", "agent.py"]
  env:
    - name: ENVIRONMENT
      value: "production"

  resources:
    requests:
      cpu: "500m"
      memory: "1Gi"
    limits:
      cpu: "2"
      memory: "4Gi"

  workspaces:
    - name: default-workspace
      path: "/workspace"
      goal: "Install dependencies and run the test suite"   # Antigravity prepares the workspace to this goal on first run

  debug: true   # serve guest services inside the sandbox so `ax ssh` works; off by default
```

Replace `<digest>` with your image's SHA-256 digest; Substrate requires digest-pinned images. The image must include `/usr/local/bin/ax-task-runner`; see [the runner contract](runner.md).

### Binding several workspaces

`spec.workspaces` takes as many entries as you like, so a task can compose reusable `Workspace` resources, for example the code to work on plus a shared set of tools:

```yaml
spec:
  workspaces:
    - name: my-service          # mounted at /workspace/my-service, the command's working directory
      goal: "Install dependencies and run the test suite"
    - name: team-tools
      path: "/workspace/tools"  # explicit mount path
```

Each entry is set up independently at its own path, in order. Every entry needs a `name`; without a `path` it lands at `/workspace/<name>`, and paths must be unique. The first entry is the working directory of `spec.command`, and the task reports `WorkspaceReady` only once all of them are prepared. See [`examples/task.yaml`](../examples/task.yaml) for complete resource definitions with a single workspace.

## Workspace

```yaml
apiVersion: ax.io/v1alpha1
kind: Workspace
metadata:
  name: default-workspace
  atespace: default
spec:
  git:
    - name: origin
      repo: "https://github.com/chalk/chalk.git"
      branch: "main"
  files:
    - path: "AGENTS.md"
      content: |
        # Project Guidelines
        - Run `go test ./...` before submitting changes.
        - Keep dependencies minimal.
  mcp:
    registries:
      - provider: google
        query: "mcp.tags:build"
    servers:
      - name: git-tools
        endpoint: "http://git-mcp.default.svc.cluster.local:8080"
  skills:
    registries:
      - provider: google
        query: "skills.tags:nodejs"
    path: "/.agents/skills"
```

## Model

Only `google` is implemented as a model provider. Other providers are accepted when applying a `Model`, but generation fails with an `unsupported provider` error.

`Model` resources configure AX's model client and workspace planner. The planner loads `default-model` from the workspace's atespace. These resources do not configure the provider or model used by arbitrary agent frameworks inside a Task image.

Store the Google API key in a Kubernetes Secret in the same namespace as the Model's `metadata.atespace`, and set `provider: google`.

```bash
kubectl create secret generic gemini-api-secret --namespace default --from-literal=GEMINI_API_KEY="<your-api-key>"
```

Then reference it from the `Model`:

```yaml
apiVersion: ax.io/v1alpha1
kind: Model
metadata:
  name: default-model
  atespace: default
spec:
  provider: google
  model: gemini-3.8-flash
  secretKey:
    name: gemini-api-secret
    key: GEMINI_API_KEY
```
