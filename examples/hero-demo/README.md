# AX v0.1 hero demo (`examples/hero-demo/`)

This folder holds the single 5-step developer hero demo for AX v0.1, and a harness that runs it end to end.

## How we use it

1. **One script, no branch drift.** Instead of five demos on five local branches, each step's DRI replaces the `🟡 STUB` in their step function in [`run.sh`](./run.sh) on `main`.
2. **Check before standup.** Pull `main` and run:

   ```bash
   ./examples/hero-demo/run.sh
   ```

   It runs Steps 1 to 5 in order and prints the 5-step scoreboard (`🟢 PASS` / `🟡 STUB` / `🔴 FAIL`). By default it makes no calls to a cluster, so it always runs cleanly. To run the steps that already work on `main` against the AX server of your current kube context:

   ```bash
   RUN_LIVE_STEPS=1 ./examples/hero-demo/run.sh
   ./examples/hero-demo/run.sh --step 3                        # one step only
   ./examples/hero-demo/run.sh --remote-context <kube-context> # Step 5
   ```

3. **Kind E2E gate.** Planned: the Kind E2E suite runs this same flow against a tagged v0.1 release candidate (gmccloskey).

## The 5 steps and DRIs

| Step | Function in `run.sh` | What it verifies | Manifest | DRIs |
| :--- | :--- | :--- | :--- | :--- |
| 1. Local Up & Studio | `step1_local_up` | AX runs locally (local Substrate 1.0, `ax-server`) and the AX Studio web UI is healthy | | yczhou, gmccloskey |
| 2. Warm Workspace Snapshot | `step2_warm_workspace` | The workspace's `spec.setup` is captured in a warm snapshot, and tasks attach to it without re-running setup | [`workspace.yaml`](./workspace.yaml) | sizhang |
| 3. Starter Task, Session & Resume | `step3_starter_task_session` | A versioned Claude Code or Antigravity starter image runs, takes a second Session turn, and survives suspend/resume | [`task-starter.yaml`](./task-starter.yaml) | aprajitaarora, luluke, simrankaurk, rajithal |
| 4. Custom Image Task | `step4_custom_image_task` | A second Task runs in the same workspace with a custom image and command, with no starter template | [`task-custom.yaml`](./task-custom.yaml) | simrankaurk, rajithal, sizhang |
| 5. Remote Cluster Promotion | `step5_remote_promote` | The same manifests are promoted unchanged to an existing remote GKE Substrate 1.0 cluster | `workspace.yaml`, `task-starter.yaml` | yczhou, sizhang, gmccloskey |

## What works on `main` today

With `RUN_LIVE_STEPS=1`, the harness already calls:

- Step 1: `ax get workspaces`, to check `ax-server` is reachable.
- Step 2: `ax apply -f workspace.yaml`.
- Step 3: `ax apply -f task-starter.yaml`, then `ax resume`, `ax suspend` and `ax resume`.
- Step 5: `ax --context <remote> apply -f` for both manifests.

Still stubbed, each with a `TODO(owner)` in `run.sh`: `ax local start` and the Studio health check (Step 1), the warm snapshot check (Step 2), the second Session turn (Step 3), the custom image task, which needs automatic `ax-task-runner` injection (Step 4), and promotion in one command (Step 5).

The manifests use the `v1alpha1` schema on `main`. The images are placeholders until they are published and pinned by digest; Substrate rejects tag-only images.
