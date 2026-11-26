---
name: ax
description: >
  How to drive the ax CLI: start a cluster, apply manifests, run, inspect, shell into, and delete tasks.
  Load when running tasks against a cluster, writing Task/Workspace/Gateway/Model YAML, or debugging a task that will not start.
---

# Using AX

Reference: `docs/manifests.md` (every field), `docs/concepts.md` (lifecycle),
`docs/sandbox.md` (what runs inside), `examples/simple.yaml` (smallest Task).

## Loop

```bash
ax local start                          # Kind + Substrate + AX + WorkerPool
ax apply -f examples/simple.yaml        # creates the task Suspended
ax resume task simple-task              # runs it; returns once Ready=True
ax describe task simple-task            # state, conditions, Actor UID
ax ssh simple-task -- ls -la /workspace # needs spec.debug: true
ax delete task simple-task              # blocks until the sandbox is gone
```

On an existing cluster: `ax install`, then
`kubectl apply -f examples/workerpool.yaml`.

Global flags: `-a <atespace>` (default `default`), `--context <kubectx>`,
`--server <addr>`, `--trace`. `ax ctx` lists and switches contexts.

## Rules that bite

- **Apply does not run.** New tasks start `Suspended`; `ax resume task` starts them.
- **Digest-pinned images only.** `image: repo@sha256:...`; a tag fails the apply.
  Resolve with `crane digest <image:tag>`.
- **No WorkerPool, no workers.** Resume fails with `no free workers available`.
- **Order in the file matters.** A `Workspace` or `Gateway` must come before the `Task` that binds it.
- **Immutable.** Task specs and Workspaces cannot change; create a new name
  (`backend-ws-v2`) or delete and re-apply.
- **No gateway, no egress.** That includes `spec.setup.git` clones during workspace setup.
- **Gateway edits don't reach existing tasks**; tasks copy the rules at creation.
- **Prebuilt agent images** need `ax local start --runner-volume-image <ax-runner-volume digest>`.
- **Names** are RFC 1123 labels: lowercase, `-`, at most 63 characters.

## Debugging

| Symptom | Look at |
|---|---|
| Apply fails | The error is from validation or Substrate; nothing was stored |
| Task never `Ready` | `ax describe task`: `WorkspaceReady` and `Ready` reasons |
| Workspace `Failed` | `ax describe workspace`: `Last Error`; delete and re-create |
| `ax ssh` refused | Task not `Running`, or `spec.debug` unset |
| Network denied inside the task | The bound Gateway's `spec.egress` hostnames |

There is no `ax logs`; use `ax ssh` and `ax describe`. `hack/tour.sh` runs
every feature against the current context.
