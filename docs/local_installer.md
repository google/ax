# AX Local Installer & Multi-Cluster User Guide

This guide walks through the end-to-end developer lifecycle with the `ax` CLI: installing `ax` in one command (`install.sh`), bootstrapping a self-contained local Kubernetes + Agent Substrate + AX stack (`ax local start`), running and checkpointing sandboxed tasks locally, deploying AX onto a remote Kubernetes cluster (`ax ctx` + `ax install`), promoting a local task to it (`ax promote`), and cleaning up (`ax uninstall` + `ax local delete`).

---

## Quickstart (One-Line CLI Install)

```bash
# 1. Install the `ax` CLI (auto-detects macOS/Linux and arm64/amd64, verifies SHA-256)
curl -fsSL https://github.com/google/ax/releases/latest/download/install.sh | sh
export PATH="$HOME/.ax/bin:$PATH"

# 2. On macOS (local mode only): ensure OrbStack, Docker Desktop (>= 6 GiB RAM), or Colima is running
#    brew install docker colima
#    colima start --cpu 4 --memory 8 --disk 60 --vm-type vz --mount-type virtiofs

# 3. Stand up the local Kubernetes + Substrate v1.0.0 + AX stack (or switch to a remote cluster via `ax ctx`)
#    Note: Until official release images are published, specify --server-image and --default-task-image
#    (or export AX_SERVER_IMAGE and AX_DEFAULT_TASK_IMAGE).
export AX_SERVER_IMAGE="<registry>/ax-server:<tag>"
export AX_DEFAULT_TASK_IMAGE="<registry>/ax-task-runner:<tag>"
ax local start
ax local status
```

### Set Up with Your AI (Copyable Agent Setup Prompt)

Paste this prompt into Claude Code, Gemini CLI, Antigravity, or Cursor to let your local coding agent check prerequisites and set up AX automatically:

```text
Help me install AX on this computer and get a local isolated agent sandbox ready. Read docs/local_installer.md first. Check my operating system, CPU architecture, free disk space, and container runtime (on macOS, check whether OrbStack, Docker Desktop, or Colima with >= 6 GiB RAM and a Linux >= 6.8 kernel is running; if not, install/start Colima with `brew install docker colima && colima start --cpu 4 --memory 8 --disk 60 --vm-type vz --mount-type virtiofs`). Install or build `ax`, verify `ax version`, run `ax local start` (or let me choose a remote kubeconfig context with `ax ctx`), and verify `ax local status`. Let me handle any password or cloud sign-in prompts.
```

---

## Prerequisites & Automatic Tooling Bootstrap

The installer never modifies system directories (such as `/usr/local/bin`) and never requires `sudo`. It separates host-level runtime prerequisites from lightweight CLIs that it automatically downloads on demand into `~/.ax/bin/`:

| Category | `ax local start` (Local full stack) | `ax install` (Existing cluster, e.g. GKE) |
|---|---|---|
| **Required on host (not installed by `ax`)** | 1. **Running rootful container engine**: `docker` (on macOS: OrbStack, Docker Desktop >= 6 GiB RAM, or Colima via `brew install docker colima && colima start --cpu 4 --memory 8 --disk 60 --vm-type vz --mount-type virtiofs`) or rootful `podman`<br>2. **Linux kernel >= 6.8 & >= 4 GiB RAM (6 GiB+ recommended) on the container host**: required by Substrate's OverlayFS `lowerdir+` image cache and control-plane pods (`ax local start` automatically preflight-checks the Linux host or macOS VM's kernel and memory) | 1. **Kubernetes 1.36+ cluster running Agent Substrate `v1.0.0`** with valid `~/.kube/config` credentials (e.g. `gke-gcloud-auth-plugin` for GKE)<br>2. **Cluster RBAC permissions**: permissions to manage `ax-system` and update caller allowlists in `ate-system` |
| **Auto-downloaded to `~/.ax/bin/`** | - **`kind`**: prefers `kind >= v0.32.0` on `PATH`; otherwise downloads and SHA-256-verifies **`kind v0.32.0`**<br>- **`kubectl`**: prefers `kubectl` on `PATH`; otherwise downloads and SHA-256-verifies **`kubectl v1.36.0`** | - **`kubectl`**: prefers `kubectl` on `PATH`; otherwise downloads and SHA-256-verifies **`kubectl v1.36.0`** |
| **Legacy tools no longer needed** | No Go toolchain, `git clone substrate`, `ko`, `ate-setup`, `openssl`, `jq`, or `crane` | No Go, `docker`/`podman`, `make`, `openssl`, `sed`, `jq`, or `crane` |

> [!NOTE]
> `ax local`, `ax install`, and tunnel commands (`ax apply`, `ax get`, `ax ssh`) automatically use `~/.ax/bin/kubectl` when no system `kubectl` is in `PATH`, while `ax ctx` can list and switch contexts offline without `kubectl`. To invoke `ax` and `kubectl` directly in every new shell, add `export PATH="$HOME/.ax/bin:$PATH"` to your shell profile (`~/.zshrc` or `~/.bashrc`).

### Remote Cluster Prerequisites (for Step 5)

To run the remote cluster deployment step (**Step 5**) on GKE or another Kubernetes cluster, prepare a cluster that has **Agent Substrate `v1.0.0` installed**:

- **Cluster & Substrate**: GKE `1.37+` (or Kubernetes `1.36+`) with `ClusterTrustBundle` and `PodCertificateRequest` enabled, Agent Substrate `v1.0.0` deployed in `ate-system` (`--credential-provider='{"name":"k8s.io"}'`), and a ready gVisor `WorkerPool`.
- **Default StorageClass**: A default `StorageClass` (for example `standard-rwo` on GKE) capable of provisioning the `10Gi` `ReadWriteOnce` PVC for `ax-postgres`.
- **Snapshot Bucket**: Cluster-wide `--snapshot-location` (for example `gs://<snapshot-bucket>/`) configured on Substrate with Workload Identity transfer permissions.

---

## Step 1: Install (or Build) the `ax` CLI

### Option A: One-line release install (recommended)

```bash
curl -fsSL https://github.com/google/ax/releases/latest/download/install.sh | sh
export PATH="$HOME/.ax/bin:$PATH"
ax version
ax local --help
```

*(When an AX Studio release is available, `install.sh` also installs `~/.ax/bin/ax-studio` so you can launch the local web UI via `ax studio`; set `AX_SKIP_STUDIO=1` to install only `ax`.)*

### Option B: Build from a local repository checkout

```bash
export GOTOOLCHAIN=auto
go build -o "$PWD/bin/ax" ./cmd/ax
export PATH="$PWD/bin:$PATH"
ax local --help
```

### Expected Behavior
- Produces a single self-contained `ax` binary (with `@sha256`-pinned Agent Substrate `v1.0.0` manifests and canonical AX deployment manifests embedded via `//go:embed`).
- `ax local --help` lists the `start`, `status`, `stop`, and `delete` subcommands along with `--name`, `--workers`, `--server-image`, `--default-task-image`, and `-n`/`--namespace`.

---

## Step 2: Stand Up the Local Stack (`ax local start`)

```bash
ax local start \
  --server-image "$AX_SERVER_IMAGE" \
  --default-task-image "$AX_DEFAULT_TASK_IMAGE"
```

*(Note: `--workers` defaults to `1` to fit comfortably inside 6–8 GiB macOS container VMs; pass `--workers 2` if you need multi-worker concurrency.)*

### Expected Behavior (~2–3 minutes on cold start)
The installer prints progress (`==>`) across three stages:

1. **Tooling, Preflight & Kind Cluster (`ax-local`)**
   - Verifies that the container host / macOS Linux VM runs in rootful mode with Linux kernel `>= 6.8` and `>= 4 GiB` RAM, and checks that any pre-existing `ax-local` kubeconfig context belongs to `kind-ax-local`.
   - Uses `kind >= v0.32.0` and `kubectl` from `PATH` when available, or downloads and SHA-256-verifies `kind v0.32.0` and `kubectl v1.36.0` into `~/.ax/bin/`.
   - Creates the Kubernetes `v1.36.1` Kind cluster (`ax-local`) with `ClusterTrustBundle`, `ClusterTrustBundleProjection`, and `PodCertificateRequest` (`certificates.k8s.io/v1beta1=true`) enabled, configures `net.ipv4.conf.all.proxy_arp=1`, and switches your active kubeconfig context to **`ax-local`**.
2. **Agent Substrate `v1.0.0` Bootstrap**
   - Applies Substrate CRDs, generates `podcertificate-controller-system` (`Ed25519`) and `ate-system` (`ECDSA P-256` + `ES256` JWK) CA pool Secrets in pure Go, initializes `ate-system/postgres-0` over PodCertificate mTLS, and rolls out `ate-api-server`, `atenet-router`, `atenet-egress` (`--credential-provider={"name":"k8s.io"}`), `rustfs` (`gs://ate-snapshots/`), `k8s-credential-provider`, and the `ax-workers` gVisor `WorkerPool`.
3. **AX `@sha256` Digest Resolution & Deployment**
   - Resolves multi-arch (`linux/amd64` + `linux/arm64`) `@sha256:...` digests for `--server-image` and `--default-task-image`.
   - Labels `ax-system` (`ate.dev/api-access=true`) and registers `ax-server` in `ate-api-server`, `atenet-router`, and `k8s-credential-provider` (scoped strictly to Secrets carrying `ax.io/credential: "true"`).
   - Generates a one-time `ax-postgres` password Secret, deploys `ax-postgres` and `ax-server`, waits for readiness, and prints:
     ```text
     Local environment "ax-local" is ready (active context: ax-local).
     ```

---

## Step 3: Inspect Cluster Health and Contexts (`ax local status` & `ax ctx`)

```bash
ax local status
ax ctx
```

### Expected Behavior
- `ax local status` reports all three layers healthy:
  ```text
  Cluster:         ax-local (kind)
  State:           Running
  Kube Context:    ax-local
  Substrate:       ate-api-server (1/1 available)
  AX Server:       ax-server (1/1 available in ax-system)
  ```
- `ax ctx` lists your kubeconfig contexts with `*` on `ax-local`. `AX TUNNEL` shows `-` until the first workload command lazily opens a background tunnel.

---

## Step 4: Local Sandbox Lifecycle (`apply`, `resume`, `ssh`, and Snapshot Restore)

### 4.1 Create a Task (`ax apply`)

```bash
cat <<'EOF' > /tmp/demo-task.yaml
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: demo-task
  atespace: default
spec:
  debug: true
  commands: ["sleep", "infinity"]
  env:
    - name: GREETING
      value: "hello-from-ax"
EOF

ax apply -f /tmp/demo-task.yaml
ax get tasks
ax ctx
```

*(Note: if `AX_SERVER` is exported in your shell from an earlier manual session, run `unset AX_SERVER` first so `ax` uses automatic per-context tunneling.)*

#### Expected Behavior
- Outputs `task.ax.io/demo-task created` (using `ax-server`'s `@sha256`-pinned default task image since `spec.profile.image` is omitted).
- `ax get tasks` displays `NAME`, `ATESPACE`, `STATE`, `ACTOR`, and `AGE`, with `demo-task` in `STATE: Suspended` (new tasks start checkpointed without consuming worker capacity).
- `ax ctx` now shows `127.0.0.1:<port> -> ax-system/ax-server (Healthy)` under `AX TUNNEL` for `ax-local`.

### 4.2 Resume the Sandbox (`ax resume`) and Execute Commands (`ax ssh`)

```bash
ax resume task demo-task
ax describe task demo-task
ax ssh demo-task -- sh -c 'uname -a && echo "ENV=$GREETING"'
```

#### Expected Behavior
- `ax resume` starts the gVisor sandbox and prints `task.ax.io/demo-task resumed` (`ax get tasks` now reports `STATE: Working`).
- `ax ssh` mints an ephemeral client mTLS certificate via Kubernetes `PodCertificateRequest` (cached per cluster fingerprint under `~/.ax/router-tls/`), connects through `atenet-router:443`, and prints:
  ```text
  Linux ... gvisor ...
  ENV=hello-from-ax
  ```

### 4.3 Verify Local `/workspace` Snapshot Persistence (`suspend` -> `resume`)

```bash
ax ssh demo-task -- sh -c 'echo "durable-across-suspend" > /workspace/durable.txt'
ax suspend task demo-task
ax get tasks

ax resume task demo-task
ax ssh demo-task -- cat /workspace/durable.txt
```

#### Expected Behavior
- `ax suspend` checkpoints `/workspace` to the in-cluster `rustfs` object store (`gs://ate-snapshots/`) and releases the worker (`STATE: Suspended`).
- Resuming and reading `/workspace/durable.txt` outputs **`durable-across-suspend`**.

---

## Step 5: Deploy to a Remote GKE + Substrate Cluster (`ax ctx` & `ax install`)

Once validated locally, switch to your remote Kubernetes + Agent Substrate cluster, install AX with `ax install`, and apply the **exact same** `/tmp/demo-task.yaml` without changing a single line.

### 5.1 Switch Context and Run `ax install`

```bash
export GKE_CTX="<your-gke-kube-context>"

# 1. Switch active context to the remote cluster
ax ctx "$GKE_CTX"

# 2. Resolve @sha256 multi-arch digests, wire Substrate allowlists, and deploy AX
ax install \
  --server-image "$AX_SERVER_IMAGE" \
  --default-task-image "$AX_DEFAULT_TASK_IMAGE"
```

*(Pass `--server-image <ref>` and `--default-task-image <ref>`, or set `$AX_SERVER_IMAGE` and `$AX_DEFAULT_TASK_IMAGE`.)*

#### Expected Behavior
- Resolves `@sha256:...` multi-arch OCI digests for `--server-image` and `--default-task-image` via `crane`.
- Creates `ax-system` with `ate.dev/api-access=true` and registers `ax-server` in `ate-api-server`, `atenet-router`, and `k8s-credential-provider-namespace-policy`, waiting for rollouts to finish.
- Provisions the `ax-postgres` PVC and rolls out `ax-postgres` and `ax-server`.

### 5.2 Run the Same Task Manifest on GKE and Switch Between Clusters

```bash
# 1. Apply the exact same Task YAML and verify ax resume + ax ssh on GKE
ax apply -f /tmp/demo-task.yaml
ax resume task demo-task
ax ssh demo-task -- sh -c 'uname -a && echo "ENV=$GREETING"'

# 2. Verify cloud GCS snapshot persistence
ax ssh demo-task -- sh -c 'echo "saved-in-gcs" > /workspace/gcs.txt'
ax suspend task demo-task
ax resume task demo-task
ax ssh demo-task -- cat /workspace/gcs.txt

# 3. Switch seamlessly between ax-local and GKE (each keeps its own background tunnel)
ax ctx
ax ctx ax-local && ax get tasks
ax ctx "$GKE_CTX" && ax get tasks
```

#### Expected Behavior
- `/tmp/demo-task.yaml` runs unmodified on GKE; `ax suspend` -> `ax resume` round-trips `/workspace/gcs.txt` through GCS (`saved-in-gcs`).
- `ax ctx` displays independent `(Healthy)` background tunnels for both `ax-local` and `$GKE_CTX`, allowing instant context switching without manual `kubectl port-forward` management.

### 5.3 Promote a Local Task to GKE (`ax promote`)

`ax promote` copies a Task that already exists on one cluster, with the Workspace and Gateway it binds, to another cluster and resumes it there, so you do not have to re-apply its manifests.

```bash
# 1. From the local cluster, check what promoting demo-task to GKE would do
ax ctx ax-local
ax promote task demo-task --to "$GKE_CTX" --replace --preflight-only

# 2. Promote it and suspend the local one (--replace recreates the demo-task that Step 5.2 applied on GKE)
ax promote task demo-task --to "$GKE_CTX" --replace --suspend-source

# 3. The task runs on GKE and is suspended locally
ax --context "$GKE_CTX" get tasks
ax get tasks
```

#### Expected Behavior
- `--preflight-only` prints what would change on GKE, such as `task.ax.io/demo-task replaced (preflight only)`, and changes nothing. Without it, the same lines are printed as each change is made, ending with `task.ax.io/demo-task resumed`.
- The promoted Task has the same `spec` as the local one, including `spec.sessionId`. GKE assigns its `metadata.uid`, timestamps, and `status`.
- `--suspend-source` suspends the local `demo-task` once GKE has created the Task and before GKE resumes it (`task.ax.io/demo-task suspended on the source`), so the Task never runs on both clusters. If GKE then cannot resume it, the local Task stays suspended; resume it on either cluster. Without `--suspend-source` the local cluster is only read and the local Task keeps running.
- A Workspace or Gateway the Task binds is created on GKE when it is missing. A Gateway that GKE already has is reused as it is (`reused`), so each cluster keeps its own egress rules and credentials. A Workspace that GKE already has must have the same spec, because Workspaces are immutable.
- The sandbox is not copied: on GKE the Task starts as a new Task does, in a workspace built from the Workspace's `spec.setup`. Kubernetes Secrets that a Gateway names in `credentialUri` are not copied either: when `ax promote` creates a Gateway that injects credentials, it prints a note that names them. Create those Secrets on GKE (see [Injecting credentials](manifests.md#injecting-credentials)); until they exist, the Task's requests through the Gateway get a `403`.
- Without `--replace`, promoting a Task that GKE already has fails with `task default/demo-task already exists on target; pass --replace to recreate it`.
- Other flags: `--no-resume` creates the Task on the target without resuming it, `--to-atespace` promotes into another atespace, `--to-namespace` names the namespace AX runs in on the target (default `ax-system`), and `--to-server` addresses the target by its AX server address instead of a kube context. The global flags (`--context`, `--server`, `-n`, `-a`) select the source.

---

## Step 6: Uninstall AX and Clean Up

> [!TIP]
> If you also want to verify idempotent re-runs (**Optional A**) or local cluster pause/resume (**Optional B**), run the appendix steps below before deleting the clusters.

### 6.1 Remove AX from a Remote Cluster (`ax uninstall`)

```bash
ax ctx "$GKE_CTX"
ax delete task demo-task
ax uninstall
kubectl -n ax-system get all,pvc,secret
```

- `ax uninstall` removes `ax-server` and `ax-postgres` workloads and revokes `ax-server` from `ate-api-server`, `atenet-router`, and `k8s-credential-provider`, while preserving `pvc/data-ax-postgres-0` and `secret/ax-postgres` by default to prevent accidental data loss.
- To permanently delete the PostgreSQL data volume and namespace as well, run `kubectl --context "$GKE_CTX" delete ns ax-system`.

### 6.2 Destroy the Local Sandbox Cluster & Background Tunnels (`ax local delete`)

```bash
ax local delete
ax local status
```

- `ax local delete` stops any background tunnel for `ax-local`, destroys the `ax-local` Kind cluster, deletes the `ax-local` kubeconfig context (and unsets `current-context` if `ax-local` was active), and reports `State: NotFound` on `ax local status`.
- If you also opened tunnels to remote clusters during the session, stop any remaining background `kubectl port-forward` processes with `ax tunnel stop <context>` (inspect active tunnels via `ax tunnel list`).

### 6.3 Remove Local AX Binaries & Caches (`~/.ax`)

All host-side binaries and runtime caches created by `install.sh` and `ax` live exclusively under `~/.ax/` (no files are written to `/usr/local/bin` or system directories):

- `~/.ax/bin/`: `ax`, `ax-studio`, and any auto-bootstrapped `kind` / `kubectl` binaries (if your `PATH` already provided `kubectl` and `kind >= v0.32.0`, `ax` reused them and did not download duplicates into `~/.ax/bin/`)
- `~/.ax/tunnels/`: port-forward metadata and logs
- `~/.ax/router-tls/`: short-lived `ax ssh` client certificates

To completely remove AX and AX Studio from your machine after running `ax local delete`:

```bash
rm -rf ~/.ax
```

Finally, if you added `export PATH="$HOME/.ax/bin:$PATH"` to `~/.zshrc` or `~/.bashrc`, remove that line. Pre-existing host tools (`docker`, `podman`, Homebrew/system `kubectl` and `kind`) and other `~/.kube/config` contexts remain untouched.

---

## Appendix: Optional Verification

### Optional A: Zero-Restart Idempotency on Repeated `ax install`

```bash
ax ctx ax-local
kubectl --context ax-local -n ate-system get deploy ate-api-server atenet-router k8s-credential-provider \
  -o custom-columns=NAME:.metadata.name,GEN:.metadata.generation

ax install \
  --server-image "$AX_SERVER_IMAGE" \
  --default-task-image "$AX_DEFAULT_TASK_IMAGE"

kubectl --context ax-local -n ate-system get deploy ate-api-server atenet-router k8s-credential-provider \
  -o custom-columns=NAME:.metadata.name,GEN:.metadata.generation
```

#### Expected Behavior
- `ax install` finishes in a few seconds without rotating the existing `ax-postgres` password or duplicating Substrate allowlist flags.
- All three `GEN` (`metadata.generation`) numbers remain identical before and after (**zero unnecessary rollouts**).

### Optional B: Local Pause and Fast Resume (`ax local stop` -> `ax local start`)

Checkpoint active tasks first (`ax suspend`) so their `/workspace` state is saved to `rustfs` before stopping the Kind node container:

```bash
ax ctx ax-local
ax suspend task demo-task

ax local stop
ax local status

ax local start \
  --server-image "$AX_SERVER_IMAGE" \
  --default-task-image "$AX_DEFAULT_TASK_IMAGE"
ax get tasks
ax resume task demo-task
ax ssh demo-task -- cat /workspace/durable.txt
```

#### Expected Behavior
- `ax local stop` halts the `ax-local-control-plane` container and stops its background tunnel (`State: Stopped`).
- `ax local start` restarts the existing node container, refreshes its API server port and `proxy_arp=1` sysctl in ~15–30 seconds, skips re-bootstrapping Substrate once pods report Ready, and restores `demo-task`'s `/workspace/durable.txt` (`durable-across-suspend`) on `ax resume`.
