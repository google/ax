# AX v0.1 hero demo harness (`hack/hero-demo/`)

A development harness for the five-step AX v0.1 developer demo. It runs the steps in order and prints a scoreboard (`🟢 PASS` / `🟡 STUB` / `🔴 FAIL`), so the demo is tracked in one script on `main` while its pieces land. Steps that depend on work still in progress are `🟡 STUB`, each with a `TODO` in [`run.sh`](./run.sh).

## Running it

```bash
./hack/hero-demo/run.sh                                  # no cluster calls; always runs cleanly
RUN_LIVE_STEPS=1 ./hack/hero-demo/run.sh                 # run what works on main against the AX server of your kube context
./hack/hero-demo/run.sh --step 3                         # one step only
./hack/hero-demo/run.sh --remote-context <kube-context>  # Step 5
```

`./hack/hero-demo/run.sh --help` lists the environment variables.

## The five steps

| Step | Function in `run.sh` | What it verifies | Manifest |
| :--- | :--- | :--- | :--- |
| 1. Local Up & Studio | `step1_local_up` | AX runs locally (local Substrate 1.0, `ax-server`) and the AX Studio web UI is healthy | |
| 2. Warm Workspace Snapshot | `step2_warm_workspace` | The workspace's `spec.setup` is captured in a warm snapshot, and tasks attach to it without re-running setup | [`workspace.yaml`](./workspace.yaml) |
| 3. Starter Task, Session & Resume | `step3_starter_task_session` | A starter image (Claude Code) runs, takes a second Session turn, and survives suspend/resume | [`task-starter.yaml`](./task-starter.yaml) |
| 4. Custom Image Task | `step4_custom_image_task` | A second Task runs in the same workspace with a custom image and command, with no starter template | [`task-custom.yaml`](./task-custom.yaml) |
| 5. Remote Cluster Promotion | `step5_remote_promote` | The same manifests are applied unchanged to an existing remote Substrate 1.0 cluster | `workspace.yaml`, `task-starter.yaml` |

## What works on `main` today

With `RUN_LIVE_STEPS=1`, the harness calls:

- Step 1: `ax get workspaces`, to check `ax-server` is reachable.
- Step 2: `ax apply -f workspace.yaml`.
- Step 3: `ax apply -f task-starter.yaml`, then `ax resume`, `ax suspend` and `ax resume`.
- Step 5: `ax --context <remote> apply -f` for both manifests.

Still stubbed: local mode and the AX Studio health check (Step 1), the warm snapshot check (Step 2), the second Session turn (Step 3), the custom image task, which needs automatic `ax-task-runner` injection (Step 4), and promotion in one command (Step 5).

The manifests use the `v1alpha1` schema on `main`. The starter image is a placeholder until it is published; Substrate requires images pinned by digest.
