# AX v0.1 hero demo harness (`hack/hero-demo/`)

A development harness for the five-step AX v0.1 developer demo. It runs the steps in order and prints a scoreboard (`🟢 PASS` / `🟡 STUB` / `🔴 FAIL`), so the demo is tracked in one script on `main` while its pieces land. Steps that depend on work still in progress are `🟡 STUB`, each with a `TODO` in [`run.sh`](./run.sh).

## Running it

```bash
./hack/hero-demo/run.sh                                  # no cluster calls; always runs cleanly
RUN_LIVE_STEPS=1 ./hack/hero-demo/run.sh                 # run what works on main against the AX server of your kube context
./hack/hero-demo/run.sh --step 3                         # one step only
RUN_LIVE_STEPS=1 ./hack/hero-demo/run.sh --remote-context <kube-context>  # and promote the starter task there (Step 5)
```

`./hack/hero-demo/run.sh --help` lists the environment variables.

## The five steps

| Step | Function in `run.sh` | What it verifies | Manifest |
| :--- | :--- | :--- | :--- |
| 1. Local Up & Studio | `step1_local_up` | AX runs locally (local Substrate 1.0, `ax-server`) and the AX Studio web UI is healthy | |
| 2. Warm Workspace Snapshot | `step2_warm_workspace` | The workspace's `spec.setup` is captured in a warm snapshot, and tasks attach to it without re-running setup | [`workspace.yaml`](./workspace.yaml) |
| 3. Starter Task, Session & Resume | `step3_starter_task_session` | A starter image (Antigravity) runs, takes a second Session turn, and survives suspend/resume | [`gateway.yaml`](./gateway.yaml), [`task-starter.yaml`](./task-starter.yaml) |
| 4. Custom Image Task | `step4_custom_image_task` | A second Task runs in the same workspace with a custom image and command, with no starter template, and survives suspend/resume | [`task-custom.yaml`](./task-custom.yaml) |
| 5. Remote Cluster Promotion | `step5_remote_promote` | The starter task, with its Gateway and Workspace, is promoted in one command to an existing remote Substrate 1.0 cluster | |

## What works on `main` today

With `RUN_LIVE_STEPS=1`, the harness calls:

- Step 1: `ax get workspaces`, to check `ax-server` is reachable.
- Step 2: `ax apply -f workspace.yaml`, which waits for the pre-warm, then `ax describe workspace` to check `SnapshotReady`.
- Step 3: check that the cluster has the `hero-demo-gemini` Secret with the `ax.io/credential=true` label (with `kubectl`, when it is installed), `ax apply -f gateway.yaml` and `ax apply -f task-starter.yaml`, then `ax resume`, and wait until the agent has finished with output.
- Step 4: `ax apply -f task-custom.yaml`, then `ax resume`; once the task's command has started, `ax suspend` and `ax resume`, and wait until the command has finished with output. The command waits on its first run and runs the unit tests after the resume; the step fails unless they pass.
- Step 5: `ax promote task hero-demo-starter-task --to <remote> --replace --suspend-source`, then wait until the promoted task's agent has finished with output on the remote cluster.

AX leaves command completion and output on the data plane ([#756](https://github.com/google/ax/pull/756)): the task stays `Running` once the sandbox is up. So both tasks set `spec.commands` that write their exit status to `/workspace/.ax/hero-demo-exit-code` when they finish, and the harness reads it, and the output in `/workspace/.ax/output` (`$AX_OUTPUT_FILE`), over `ax ssh` (both tasks set `spec.debug`). The starter task's command runs the image's launcher, as `ax-runner` does for a task with a goal and no commands, and does not run the agent again once it has recorded a status.

`task-starter.yaml` pins the published Antigravity CLI starter image (`ghcr.io/agentexecutor/agy-cli`, `v0.0.0-rc.4`, pinned by digest as Substrate requires); pass `STARTER_IMAGE=<image>@sha256:<digest>` to override it for local testing. Because the starter image and the plain image of Step 4 have no `ax-runner` baked in, `ax-server` also needs a runner volume image (`ax install --runner-volume-image` or `AX_RUNNER_VOLUME_IMAGE`, which is empty by default); Step 2 needs it too, because the workspace is pre-warmed in the same image.

Still stubbed: local mode and the AX Studio health check (Step 1), and the second Session turn on the same task's workspace (Step 3). Step 4 shows `ax suspend` and `ax resume`.

## Checking the warm snapshot by hand

Step 2 waits for the pre-warm, so the first run takes as long as the image pull, `spec.setup`, and the snapshot upload. Later runs find the workspace unchanged and return at once.

```bash
ax apply -f hack/hero-demo/workspace.yaml
ax describe workspace hero-demo-ws         # Phase: Ready; SnapshotReady names the tag ws-<workspace>-<template>-r1
ax describe task hero-demo-starter-task    # WorkspaceReady: "Workspace seeded from snapshot tag <the same tag>"
ax ssh hero-demo-starter-task -- ls /workspace/app   # after `ax resume`: the files spec.setup wrote
```

While the apply runs, `kubectl -n ax-system logs deploy/ax-server -f` shows `provisioning workspace setup actor`. Workspaces are immutable, so to pre-warm again, or to change `STARTER_IMAGE`, delete the tasks and then `ax delete workspace hero-demo-ws`. A cluster that Step 5 promoted to has its own copy of the workspace, to delete the same way.

The manifests use the `v1alpha1` schema on `main`. The starter task reaches the Gemini API through the Gateway in [`gateway.yaml`](./gateway.yaml), which names the Secret that holds the API key.
