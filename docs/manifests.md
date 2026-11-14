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
  labels:
    team: platform
spec:
  profile:
    image: "ghcr.io/my-org/my-agent-image@sha256:..."
    resources:
      limits:
        cpu: "2"
        memory: "4Gi"
    # Or use an existing Substrate ActorTemplate directly (mutually exclusive with image and resources;
    # not available yet: rejected until pull-based runner assignment is wired):
    # ateActorTemplate: "base-tmpl"

  commands: ["python", "agent.py"]
  env:
    - name: ENVIRONMENT
      value: "production"

  workspace:
    name: default-workspace
    path: "/workspace"   # optional; this is the default

  goal:
    spec: "Fix the failing unit tests."

  # retry:                         # not available yet: rejected until task retries are wired
  #   maxRetries: 2
  #   initialBackoff: 30s
  #   maxBackoff: 300s
  #   backoffMultiplier: 2

  sessionId: "session-42"   # optional; defaults to metadata.uid on creation
  debug: true               # serve guest services inside the sandbox so `ax ssh` works; off by default
```

### Actor identity

`status.actorUid` is the current actor's Substrate UID, also recorded as
`ate.actor.uid` in Substrate telemetry. Use it to correlate a task with
Substrate logs and events. `ax describe task <name>` shows it as `Actor UID`.
AX reads it from Substrate on each get or list and never stores it. It changes
if the actor is recreated and is empty when the actor is missing or Substrate
cannot be reached; `describe` shows `<none>` in those cases.

### Compute profile and image references

`spec.profile` configures the task's sandbox compute environment (`image` and `resources.limits`, or a mutually exclusive `ateActorTemplate` referencing an existing `ActorTemplate` in the same atespace; *not available yet: ax-server rejects `spec.profile.ateActorTemplate` until pull-based runner assignment is wired*).

When a task binds a `Workspace`, profile selection operates on the **whole `Profile`** rather than merging individual fields:

- **Omitted `Task.spec.profile` (warm path):** If `Task.spec.profile` is omitted (or empty), the task falls back to `Workspace.spec.profile` when set (or server defaults otherwise), allowing it to start directly from the Workspace's pre-warmed snapshot.
- **Explicit `Task.spec.profile` (independent override / cold-boot on mismatch):** `Workspace.spec.profile` exists to pre-warm a default workspace snapshot for fast warm startup—it never constrains or leaks fields into an explicit `Task.spec.profile`. Whenever `Task.spec.profile` sets any field (`image`, `resources.limits`, or `ateActorTemplate`), it is used **as-is** and `Workspace.spec.profile` is ignored; any unset fields within `Task.spec.profile` fall back to server defaults (`AX_DEFAULT_TASK_IMAGE`, `1` CPU / `1000m`, `512Mi` memory), never to `Workspace.spec.profile`. If the resulting profile differs from `Workspace.spec.profile`, the task simply misses the pre-warmed snapshot and cold-boots to run workspace setup in its own sandbox profile first.

Substrate requires `spec.profile.image` (and a Workspace's `spec.profile.image`) to be pinned by digest, e.g. `ghcr.io/my-org/my-agent-image@sha256:...`. A tag-only reference such as `my-agent:v1` is rejected when the ActorTemplate is created, because a tag can move and would silently invalidate the actor's snapshots. If that happens the task reports `Ready=False` with reason `TemplateCreationFailed` and the Substrate error as the message. Resolve a tag to its digest with `docker buildx imagetools inspect <image:tag>` or `crane digest <image:tag>`.

### Binding a workspace

A task binds at most one `Workspace` through `spec.workspace`. The binding needs a `name`; without a `path` the workspace is mounted at `/workspace`. That path is the working directory of `spec.commands`, and the task reports `WorkspaceReady` once the workspace is prepared. A task with no `workspace` gets an empty working directory at `/workspace`.

The bound `Workspace` must already exist in the task's atespace. Creating a task that binds a missing workspace fails with `FailedPrecondition` and stores nothing. `ax apply` creates documents in file order, so list a `Workspace` before the `Task` that binds it.

To work on content from several sources, put it all in one `Workspace`, for example by listing several repositories under `spec.setup.git` or adding files through `spec.setup.files`.

### Binding a gateway

A task binds at most one `Gateway` through `spec.gateway`, for example `gateway: {name: review-bot-gw}`, and can reach only the external hosts that gateway's rules allow. A task with no `gateway` has no external network access. Workspace setup runs in the task's sandbox, so the same rules apply to it: allow the hosts that the workspace's `spec.setup` reaches, such as its Git repositories.

A task gets a copy of the gateway's rules when it is created. Editing or deleting the `Gateway` afterwards does not change tasks that already exist; create a new task to pick up the new rules. The bound `Gateway` must already exist in the task's atespace, so list it before the `Task` that binds it.

If Agent Substrate refuses one of the rules, the task is marked `Failed` with reason `GatewayBindingFailed` and does not run.

### Sizing the sandbox

`spec.profile.resources.limits` caps the CPU and memory of the task's sandbox. The controller copies the limits onto the Substrate `ActorTemplate` it provisions for the task, using Kubernetes quantity syntax (`500m`, `2`, `4Gi`). Only `cpu` and `memory` are supported, each quantity must be greater than zero, and the CPU limit must be below 1000 cores. `ax apply` rejects values outside these rules, and a task whose limits Substrate refuses is marked `Failed` with reason `TemplateCreationFailed` rather than run without them. Because Substrate requires both CPU and memory limits on every `ActorTemplate`, any omitted dimension defaults to `1` CPU (`1000m`) and `512Mi` memory.

Substrate sizes sandboxes by limits alone, so `spec.profile.resources.requests` is not supported and `ax apply` rejects a manifest that sets it.

### Goal, retries, and session ID

- **`spec.goal.spec`** is an optional natural-language description of the task's objective, delivered as part of the task metadata.
- **`spec.retry`** configures retry limits and backoff (`maxRetries >= 0`, non-negative `initialBackoff` and `maxBackoff` with `initialBackoff <= maxBackoff`, and `backoffMultiplier >= 1.0` when set). *Not available yet: ax-server rejects `spec.retry` until task retries are wired.*
- **`spec.sessionId`** groups related tasks or multi-turn runs under one correlation ID. When omitted, `CreateTask` defaults it to the server-assigned `metadata.uid`.

### Runner configuration

`spec.config` is an arbitrary YAML object of per-task settings for the task's [runner](runner.md) or command. AX stores it and hands it over unchanged as part of the `Task`, both in `AX_TASK_YAML` and from the `/metadata/v1alpha1/ax/task` endpoint, so one runner image can serve many tasks that differ only in their settings. Nested maps and lists are kept as they are, instead of being flattened into `spec.env` strings.

The control plane does not interpret `spec.config`; its schema belongs to the runner. `ax apply` only rejects a config that is not an object or whose JSON encoding exceeds 16 KiB. The whole task, config included, must also stay under 31 KiB once encoded as YAML, because it reaches the runner in a single environment variable and Substrate caps each value at 32 KiB. Numbers are stored as JSON numbers (double precision), so quote large integers or IDs that must survive exactly. Like the rest of the spec, the config is immutable once the task is created: changing it means creating a new task.

`spec.config` is stored in plain text and shown by `ax get task` and `ax describe task`, so don't put credentials in it. Use secrets instead, for example `Model.spec.secretKey`.

Inside the task, the command can read its config through the metadata server:

```bash
curl -s "$AX_METADATA_URL/metadata/v1alpha1/ax/task" | yq .spec.config
```

## Workspace

Workspaces are **immutable** once created (`CreateWorkspace`): re-applying an identical manifest succeeds as `unchanged`, while modifying an existing Workspace fails with `FailedPrecondition`. To evolve an environment cleanly, create a new Workspace name (for example `backend-ws-v2`) and delete the old one once tasks drain.

```yaml
apiVersion: ax.io/v1alpha1
kind: Workspace
metadata:
  name: default-workspace
  atespace: default
spec:
  # profile:                         # not available yet: rejected until snapshot pre-warming is wired
  #   image: "ghcr.io/my-org/my-agent-image@sha256:..."
  #   resources:
  #     limits:
  #       cpu: "2"
  #       memory: "4Gi"
  #   # Or use an existing Substrate ActorTemplate directly (mutually exclusive with image and resources):
  #   # ateActorTemplate: "base-tmpl"

  setup:
    git:
      - name: origin
        repo: "https://github.com/chalk/chalk.git"
        ref: "main"                  # branch, tag, or commit SHA (defaults to "main")
        dir: "."                     # relative to workspace root (defaults to repo basename)
        depth: 1                     # optional shallow clone depth (>= 0)
    files:
      - path: "AGENTS.md"
        content: |
          # Project Guidelines
          - Run `npm test` before submitting changes.
          - Keep dependencies minimal.
    skills:
      registries:
        - provider: google
          project: my-gcp-project
          query: "skills.tags:nodejs"
      path: "/workspace/.ax/skills"  # optional; defaults to <workspace>/.ax/skills
    commands:
      - "npm ci --prefer-offline"
    # gateway:                       # not available yet: rejected until workspace gateways are enforced
    #   name: build-egress

  mcp:
    registries:
      - provider: google
        project: my-gcp-project
        query: "mcp.tags:build"
    servers:
      - name: git-tools
        endpoint: "http://git-mcp.default.svc.cluster.local:8080/mcp"
      - name: local-stdio
        command: "git-mcp"
        args: ["--repo", "/workspace"]
```

### Workspace fields

- **`spec.profile`** defines the compute profile pre-warmed upon Workspace creation for fast warm task startup (`image` and `resources.limits`, which synthesize an AX-managed `ActorTemplate` defaulting to `ROOTFS` snapshot fidelity, or a mutually exclusive `ateActorTemplate` referencing an existing `ActorTemplate` in the same atespace). Tasks that bind this Workspace and omit `Task.spec.profile` use `Workspace.spec.profile` as their default profile and start warm from the snapshot; tasks that set their own `Task.spec.profile` use it as-is without field-merging from `Workspace.spec.profile` (cold-booting workspace setup when the profiles differ). Because `ateActorTemplate` is used directly without `BuildActorTemplate`, a user-supplied template must provide the [sandbox contract](sandbox.md) itself (`ax-task-runner` entrypoint, HTTP `/readyz` wakeup probe on port 80, durable `/workspace` volume, and the `egress-trust-bundle` `SystemInfo` mount at `/run/ate/egress-trust-bundle/trust-bundle.pem` for Gateway HTTPS). *Not available yet: ax-server rejects `spec.profile` until snapshot pre-warming is wired.*
- **`spec.setup`** groups deterministic environment preparation steps executed once when initializing the workspace volume:
  - `git`: HTTPS repositories cloned under the workspace directory (`dir` must stay within the workspace root; `ref` accepts a branch, tag, or commit SHA and defaults to `"main"`; `depth` optionally configures `--depth`).
  - `files`: inline files written under the workspace directory (relative paths that traverse outside the workspace root are rejected).
  - `skills`: skill registries (`provider`, `project`, `query`) and optional destination `path` (defaults to `<workspace>/.ax/skills`).
  - `commands`: deterministic shell commands executed sequentially (`sh -c`) inside the workspace after git/files/skills complete (5m timeout per command). A non-zero exit or timeout aborts workspace setup without recording the initialization marker.
  - `gateway`: optional `Gateway` binding for setup-time network egress (*not available yet: ax-server rejects `spec.setup.gateway` until workspace gateways are enforced*).
- **`spec.mcp`** declares static `servers` (remote `endpoint` or local `command`/`args`) and `registries` (`provider`, `project`, `query`) read by agents through the in-sandbox metadata server.

## Gateway

A `Gateway` lists the external traffic a task may send. Traffic that matches no rule is refused.

```yaml
apiVersion: ax.io/v1alpha1
kind: Gateway
metadata:
  name: review-bot-gw
  atespace: default
spec:
  egress:
    - https:
        hostnames: ["api.anthropic.com"]
        effects:
          replaceHeaders:
            - header: x-api-key
              credentialUri: "ate-secret://k8s.io/default/default/anthropic-creds/api-key"
    - https:
        hostnames: ["github.com", "api.github.com", "*.githubusercontent.com"]
    - tlsPassthrough:
        hostnames: ["smtp.example.com"]
        ports:
          numbers: [465]
```

Each entry of `spec.egress` is an Agent Substrate `EgressRule` and sets exactly one of:

- `http`: cleartext HTTP to the named hosts, on port 80 unless `ports` is set.
- `https`: HTTPS to the named hosts, on port 443 unless `ports` is set. The egress gateway intercepts these connections so that it can apply `effects` to each request.
- `tlsPassthrough`: TLS to the named hosts, forwarded without decryption. `ports` is required. The connection must begin with TLS: a protocol that starts in cleartext and upgrades, such as PostgreSQL's, does not match.

`hostnames` are lowercase DNS names. `*` can stand for the whole leftmost label: `*.example.com` matches `api.example.com`, but not `example.com` or `a.b.example.com`. `ports` is either `numbers: [443, 8443]` or `all: {}`.

### Injecting credentials

`effects.replaceHeaders` adds a credential to matching requests outside the sandbox, so the task never holds it. The task sends the header with any placeholder value, and the egress gateway replaces that value with `prefix` followed by the credential that `credentialUri` names. A request that does not send the header is forwarded unchanged. Only an `https` rule whose hostnames have no wildcard can inject a credential.

A tool sends the header only when it has a key, so give it a placeholder: set the variable it reads its key from to any value, in the task's `spec.env` or in the image. For the gateway above:

```yaml
spec:
  gateway:
    name: review-bot-gw
  env:
    - name: ANTHROPIC_API_KEY
      value: placeholder   # replaced with the real key outside the sandbox
```

`credentialUri` has the form `ate-secret://<provider>/<path>` and is resolved by a credential provider registered with Agent Substrate. For the bundled Kubernetes Secrets provider it is `ate-secret://k8s.io/default/<namespace>/<secret>/<key>`. That provider denies by default: matching requests get a 403 until a cluster admin grants the task's atespace the Secret's namespace in the `k8s-credential-provider-namespace-policy` ConfigMap in `ate-system`. A grant can be limited to Secrets with given labels, such as `ax.io/credential: "true"`, so that other Secrets in that namespace stay out of reach. See Substrate's [egress credential injection](https://github.com/agent-substrate/substrate/blob/v1.0.0/docs/guides/egress-credential-injection.md) guide.

### What `ax apply` rejects

`ax apply` rejects a rule that:

- uses `*` alone as a hostname, which would match every host, including hosts inside the cluster;
- names an IP address;
- names `localhost` or a name under it, which would be the egress gateway's own: a task reaches its own localhost without a rule;
- replaces headers in an `http` rule, where the credential would travel in cleartext, or for a wildcard hostname: name each host that gets the credential;
- is a `tlsPassthrough` rule without `ports`.

A hostname can name a service inside the cluster, such as `git-mcp.default.svc.cluster.local`: the egress gateway resolves it and connects from inside the cluster. Name only the hosts the task should reach.

Substrate checks the rest of a rule's syntax when a task that binds the gateway is created.

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
