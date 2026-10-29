# Writing manifests

All four kinds can live in one multi-document YAML file. The examples below show every kind; [`examples/simple.yaml`](../examples/simple.yaml) is the smallest working Task. [`examples/claude-code/task.yaml`](../examples/claude-code/task.yaml) shows a Task that runs Claude Code headlessly against a cloned repository.

`metadata.name` and `metadata.atespace` become Substrate resource names, so they must be lowercase RFC 1123 labels: at most 63 lowercase alphanumeric characters or `-`, starting and ending with an alphanumeric character. `ax apply` rejects anything else up front rather than letting the task fail later with `ActorCreationFailed`.

When `metadata.atespace` is omitted, `ax apply -a team-a -f manifest.yaml` uses `team-a`. Without `-a` or `--atespace`, the manifest's atespace is preserved, or defaults to `default` if omitted. If an explicit flag disagrees with `metadata.atespace`, that resource is rejected before it is submitted.

## Task

```yaml
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: task123
  atespace: default
spec:
  image: "ghcr.io/my-org/my-agent-image"
  command: ["python", "agent.py"]
  env:
    - name: ENVIRONMENT
      value: "production"

  resources:
    limits:
      cpu: "2"
      memory: "4Gi"

  workspace:
    name: default-workspace
    path: "/workspace"   # optional; this is the default

  debug: true   # serve guest services inside the sandbox so `ax ssh` works; off by default
```

### Image references

Substrate requires `spec.image` to be pinned by digest, e.g. `ghcr.io/my-org/my-agent-image@sha256:...`. A tag-only reference such as `my-agent:v1` is rejected when the task's ActorTemplate is created, because a tag can move and would silently invalidate the actor's snapshots. If that happens the task reports `Ready=False` with reason `TemplateCreationFailed` and the Substrate error as the message. Resolve a tag to its digest with `docker buildx imagetools inspect <image:tag>` or `crane digest <image:tag>`.

### Binding a workspace

A task binds at most one `Workspace` through `spec.workspace`. The binding needs a `name`; without a `path` the workspace is mounted at `/workspace`. That path is the working directory of `spec.command`, and the task reports `WorkspaceReady` once the workspace is prepared. A task with no `workspace` gets an empty working directory at `/workspace`.

To work on content from several sources, put it all in one `Workspace`, for example by listing several repositories under `spec.git` or adding files through `spec.files`.

### Sizing the sandbox

`spec.resources.limits` caps the CPU and memory of the task's sandbox. The controller copies the limits onto the Substrate `ActorTemplate` it provisions for the task, using Kubernetes quantity syntax (`500m`, `2`, `4Gi`). Only `cpu` and `memory` are supported, each quantity must be greater than zero, and the CPU limit must be below 1000 cores. `ax apply` rejects values outside these rules, and a task whose limits Substrate refuses is marked `Failed` with reason `TemplateCreationFailed` rather than run without them. Because Substrate requires both CPU and memory limits on every `ActorTemplate`, any omitted dimension defaults to `1` CPU (`1000m`) and `512Mi` memory.

Substrate sizes sandboxes by limits alone, so `spec.resources.requests` is not supported and `ax apply` rejects a manifest that sets it.

### Runner configuration

`spec.config` is an arbitrary YAML object of per-task settings for the task's [runner](runner.md) or command. AX stores it and hands it over unchanged as part of the `Task`, both in `AX_TASK_YAML` and from the `/metadata/v1alpha1/ax/task` endpoint, so one runner image can serve many tasks that differ only in their settings. Nested maps and lists are kept as they are, instead of being flattened into `spec.env` strings.

The control plane does not interpret `spec.config`; its schema belongs to the runner. `ax apply` only rejects a config that is not an object or whose JSON encoding exceeds 16 KiB. The whole task, config included, must also stay under 31 KiB once encoded as YAML, because it reaches the runner in a single environment variable and Substrate caps each value at 32 KiB. Numbers are stored as JSON numbers (double precision), so quote large integers or IDs that must survive exactly. Like the rest of the spec, the config is immutable once the task is created: changing it means creating a new task.

`spec.config` is stored in plain text and shown by `ax get task` and `ax describe task`, so don't put credentials in it. Use secrets instead, for example `Model.spec.secretKey`.

Inside the task, the command can read its config through the metadata server:

```bash
curl -s "$AX_METADATA_URL/metadata/v1alpha1/ax/task" | yq .spec.config
```

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

For Google models, store the API key and set `provider: google`.

```bash
kubectl create secret generic gemini-api-secret --from-literal=GEMINI_API_KEY="AIzaSy..."
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

For Anthropic models, store the key the same way and set `provider: anthropic`.

```bash
kubectl create secret generic anthropic-api-secret --from-literal=ANTHROPIC_API_KEY="sk-ant-..."
```

```yaml
apiVersion: ax.io/v1alpha1
kind: Model
metadata:
  name: claude-model
  atespace: default
spec:
  provider: anthropic
  model: claude-opus-5
  secretKey:
    name: anthropic-api-secret
    key: ANTHROPIC_API_KEY
  parameters:
    maxTokens: 16000
```
