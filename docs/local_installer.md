# AX Local Installer & Multi-Cluster User Guide

This guide walks through the end-to-end developer lifecycle with the `ax` CLI: bootstrapping a self-contained local Kubernetes + Agent Substrate + AX stack in one command (`ax local up`), running and checkpointing sandboxed tasks locally, promoting the **exact same** `Task` manifest unchanged onto a remote Kubernetes cluster (`ax ctx` + `ax install`), and cleaning up (`ax uninstall` + `ax local delete`).

---

## Prerequisites & Automatic Tooling Bootstrap

The installer never modifies system directories (such as `/usr/local/bin`) and never requires `sudo`. It separates host-level runtime prerequisites from lightweight CLIs that it automatically downloads on demand into `~/.ax/bin/`:

| Category | `ax local up` (Local full stack) | `ax install` (Existing cluster, e.g. GKE) |
|---|---|---|
| **Required on host (not installed by `ax`)** | 1. **Running container engine**: `docker` or `podman` daemon<br>2. **Linux kernel >= 6.8 on the container host**: required by Substrate's OverlayFS `lowerdir+` image cache (on macOS, this refers to the Docker/Podman Linux VM kernel)<br>3. *(Only for `--from-source`)* **Go toolchain** (`GOTOOLCHAIN=auto` is set automatically during image builds) | 1. **Kubernetes 1.36+ cluster running Agent Substrate `v1.0.0-rc.1`** with valid `~/.kube/config` credentials (e.g. `gke-gcloud-auth-plugin` for GKE)<br>2. **Cluster RBAC permissions**: permissions to manage `ax-system` and update caller allowlists in `ate-system`<br>3. *(Only for `--from-source`)* **Go + `docker`/`podman`** authenticated to a pushable container registry |
| **Auto-downloaded to `~/.ax/bin/`** | - **`kind`**: downloads **`kind v0.32.0`** if missing or `< v0.32.0` (required for Kubernetes 1.36+ and `kindnetd` `NetworkPolicy` enforcement)<br>- **`kubectl`**: downloads **`kubectl v1.36.0`** if missing from `PATH` | - **`kubectl`**: downloads **`kubectl v1.36.0`** if missing from `PATH` |
| **Legacy tools no longer needed** | No `git clone substrate`, `ko`, `ate-setup`, `openssl`, `jq`, or `crane` | No `make`, `openssl`, `sed`, `jq`, or `crane` |

> [!NOTE]
> While `ax local` and `ax install` automatically fall back to `~/.ax/bin/kubectl`, everyday tunnel commands (`ax apply`, `ax get`, `ax ssh`) resolve `kubectl` from `PATH`. On a machine without a system `kubectl`, add `export PATH="$HOME/.ax/bin:$PATH"` to your shell profile.

### Remote Cluster Prerequisites (for Step 5)

To run the local-to-cloud promotion step (**Step 5**) on a remote cluster such as GKE, prepare a cluster that has **Agent Substrate `v1.0.0-rc.1` installed without AX**:

- **Cluster & Substrate**: GKE `1.37+` (or Kubernetes `1.36+`) with `ClusterTrustBundle` and `PodCertificateRequest` enabled, Agent Substrate `v1.0.0-rc.1` deployed in `ate-system` (`--credential-provider='{"name":"k8s.io"}'`), and a ready gVisor `WorkerPool`.
- **Default StorageClass**: A default `StorageClass` (for example `standard-rwo` on GKE) capable of provisioning the `10Gi` `ReadWriteOnce` PVC for `ax-postgres`.
- **Snapshot Bucket**: Cluster-wide `--snapshot-location` (for example `gs://<snapshot-bucket>/`) configured on Substrate with Workload Identity transfer permissions.
- **Container Registry** *(when installing with `--from-source`)*: A registry repository (for example Artifact Registry `<region>-docker.pkg.dev/<project>/<repo>`) that your workstation can push to and cluster nodes + `atelet` can pull from.

---

## Step 0: Clean Slate Reset (Optional)

If you have previous local AX clusters, cached tools, or background port-forward tunnels from earlier runs and want to verify a cold start from scratch:

```bash
# 1. Stop any lingering background port-forward tunnels to ax-server
pkill -f "[k]ubectl.*port-forward.*ax-server" || true

# 2. Delete previous local AX Kind clusters and kubeconfig context
~/.ax/bin/kind delete cluster --name local 2>/dev/null || true
kubectl config delete-context local        2>/dev/null || true

# 3. Remove cached binaries and tunnel state under ~/.ax
rm -rf ~/.ax/bin ~/.ax/local ~/.ax/tunnels

# 4. (Optional) Remove the loopback registry container to test fresh creation
docker rm -f kind-registry 2>/dev/null || true
```

### Expected Behavior
- `docker ps` shows no `local-control-plane` container, and `~/.ax` contains no `bin`, `local`, or `tunnels` directories.

---

## Step 1: Build the `ax` CLI

From the root of your `ax` repository checkout:

```bash
export GOTOOLCHAIN=auto
go build -o "$PWD/bin/ax" ./cmd/ax
alias ax="$PWD/bin/ax"
ax local --help
```

### Expected Behavior
- Produces a single self-contained `./bin/ax` binary (with `@sha256`-pinned Agent Substrate `v1.0.0-rc.1` manifests and canonical AX deployment manifests embedded via `//go:embed`) and aliases `ax` in the current shell.
- `ax local --help` lists the `up`, `status`, `stop`, and `delete` subcommands.

---

## Step 2: Stand Up the Local Stack (`ax local up`)

> [!TIP]
> **Live Demo Tip (Before vs. After):**
> - **Before running `ax local up`**: run `docker ps`, `ls -la ~/.ax`, and `kind version` to show a clean machine (and, if applicable, an older system `kind` binary that `ax local up` will automatically bypass by downloading `kind v0.32.0` to `~/.ax/bin/kind`).
> - **After `ax local up` finishes**: run `ax local status`, `docker ps` (showing `127.0.0.1:5001`), `kubectl get pods -A`, and `ax get tasks`.

```bash
ax local up --workers 1 --from-source .
```

*(Note: `--workers` defaults to `2`; `--workers 1` uses less memory for single-task local testing.)*

### Expected Behavior (~2.5–3.5 minutes on cold start)
The installer prints progress (`==>`) across three stages:

1. **Tooling & Loopback Registry + Kind Cluster (`local`)**
   - Downloads `kind v0.32.0` (if needed) and `kubectl v1.36.0` (if missing) into `~/.ax/bin/`.
   - Starts or reuses `kind-registry` bound strictly to **`127.0.0.1:5001`** (never `0.0.0.0`) and connects it to the `kind` Docker network.
   - Creates the Kubernetes `v1.36.1` Kind cluster (`local`) with `ClusterTrustBundle`, `ClusterTrustBundleProjection`, and `PodCertificateRequest` (`certificates.k8s.io/v1beta1=true`) enabled, configures `net.ipv4.conf.all.proxy_arp=1`, and switches your active kubeconfig context to **`local`**.
2. **Agent Substrate `v1.0.0-rc.1` Bootstrap**
   - Applies Substrate CRDs, generates `podcertificate-controller-system` (`Ed25519`) and `ate-system` (`ECDSA P-256` + `ES256` JWK) CA pool Secrets in pure Go, initializes `ate-system/postgres-0` over PodCertificate mTLS, and rolls out `ate-api-server`, `atenet-router`, `atenet-egress` (`--credential-provider={"name":"k8s.io"}`), `rustfs` (`gs://ate-snapshots/`), `k8s-credential-provider`, and the `ax-workers` gVisor `WorkerPool`.
3. **AX Build, `@sha256` Digest Pinning & Deployment**
   - Cross-compiles `ax-server` and `ax-task-runner`, pushes both to `localhost:5001`, and resolves their `@sha256:...` digests.
   - Labels `ax-system` (`ate.dev/api-access=true`) and registers `ax-server` in `ate-api-server`, `atenet-router`, and `k8s-credential-provider` (scoped strictly to Secrets carrying `ax.io/credential: "true"`).
   - Generates a one-time `ax-postgres` password Secret, deploys `ax-postgres` and `ax-server`, waits for readiness, and prints:
     ```text
     Local environment "local" is ready (active context: local).
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
  Cluster:         local (kind)
  State:           Running
  Kube Context:    local
  Local Registry:  127.0.0.1:5001
  Substrate:       ate-api-server (2/2 available)
  AX Server:       ax-server (1/1 available in ax-system)
  ```
- `ax ctx` lists your kubeconfig contexts with `*` on `local`. `AX TUNNEL` shows `-` until the first workload command lazily opens a background tunnel.

---

## Step 4: Local Sandbox Lifecycle (`apply`, `resume`, `ssh`, and Snapshot Restore)

### 4.1 Create a Task (`ax apply`)

```bash
cat <<'EOF' > /tmp/poc-demo-task.yaml
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: poc-demo
  atespace: default
spec:
  debug: true
  command: ["sleep", "infinity"]
  env:
    - name: GREETING
      value: "hello-from-ax"
EOF

ax apply -f /tmp/poc-demo-task.yaml
ax get tasks
ax ctx
```

*(Note: if `AX_SERVER` is exported in your shell from an earlier manual session, run `unset AX_SERVER` first so `ax` uses automatic per-context tunneling.)*

#### Expected Behavior
- Outputs `task.ax.io/poc-demo created` (using the server's `@sha256`-pinned default `ax-task-runner` image since `spec.image` is omitted).
- `ax get tasks` shows `poc-demo` in the **`Suspended`** phase (new tasks start checkpointed without consuming worker capacity).
- `ax ctx` now shows `127.0.0.1:<port> -> ax-system/ax-server (Healthy)` under `AX TUNNEL` for `local`.

### 4.2 Resume the Sandbox (`ax resume`) and Execute Commands (`ax ssh`)

```bash
ax resume task poc-demo
ax describe task poc-demo
ax ssh poc-demo -- sh -c 'uname -a && echo "ENV=$GREETING"'
```

#### Expected Behavior
- `ax resume` starts the gVisor sandbox, waits for `WorkspaceReady=True`, and prints `task.ax.io/poc-demo resumed`.
- `ax ssh` mints an ephemeral client mTLS certificate via Kubernetes `PodCertificateRequest`, connects through `atenet-router:443`, and prints:
  ```text
  Linux actor 4.19.0-gvisor ...
  ENV=hello-from-ax
  ```

### 4.3 Verify Local `/workspace` Snapshot Persistence (`suspend` -> `resume`)

```bash
ax ssh poc-demo -- sh -c 'echo "durable-across-suspend" > /workspace/durable.txt'
ax suspend task poc-demo
ax get tasks

ax resume task poc-demo
ax ssh poc-demo -- cat /workspace/durable.txt
```

#### Expected Behavior
- `ax suspend` checkpoints `/workspace` to the in-cluster `rustfs` object store (`gs://ate-snapshots/`) and releases the worker (`PHASE: Suspended`, `WORKER-IP` empty).
- Resuming and reading `/workspace/durable.txt` outputs **`durable-across-suspend`**.

---

## Step 5: Promote Unchanged to a Remote GKE + Substrate Cluster (`ax ctx` & `ax install`)

Once validated locally, switch to your remote Kubernetes + Agent Substrate cluster, install AX with `ax install`, and apply the **exact same** `/tmp/poc-demo-task.yaml` without changing a single line.

### 5.1 Switch Context and Run `ax install`

```bash
export GKE_CTX="<your-gke-kube-context>"
export GAR_REPO="<region>-docker.pkg.dev/<project>/<repo>"

# 1. Switch active context to the remote cluster (confirm AX is not installed yet)
ax ctx "$GKE_CTX"
kubectl get ns ax-system 2>&1 || true

# 2. Build/push from source, wire Substrate allowlists, and deploy AX
ax install --from-source . --image-repo "$GAR_REPO"
```

*(Once official pre-built AX images are published to `ghcr.io/google/ax`, end users who are not building local source changes can run bare `ax install` with no flags.)*

#### Expected Behavior
- Cross-compiles `ax-server` and `ax-task-runner`, pushes them to `$GAR_REPO` using your Docker credential helper, and extracts their `@sha256:...` digests locally.
- Creates `ax-system` with `ate.dev/api-access=true` and registers `ax-server` in `ate-api-server`, `atenet-router`, and `k8s-credential-provider-namespace-policy`, waiting for rollouts to finish.
- Provisions the `ax-postgres` PVC and rolls out `ax-postgres` and `ax-server`.

### 5.2 Run the Same Task Manifest on GKE and Switch Between Clusters

```bash
# 1. Apply the exact same Task YAML and verify ax resume + ax ssh on GKE
#    (Reaching this point confirms ax install wired AX + Substrate end-to-end)
ax apply -f /tmp/poc-demo-task.yaml
ax resume task poc-demo
ax ssh poc-demo -- sh -c 'uname -a && echo "ENV=$GREETING"'

# 2. Verify cloud GCS snapshot persistence:
#    suspend tears down the sandbox and uploads /workspace to GCS;
#    resume starts a fresh container and restores /workspace from GCS
ax ssh poc-demo -- sh -c 'echo "saved-in-gcs" > /workspace/gcs.txt'
ax suspend task poc-demo
ax resume task poc-demo
ax ssh poc-demo -- cat /workspace/gcs.txt

# 3. Switch seamlessly between local and GKE (each keeps its own background tunnel)
ax ctx
ax ctx local && ax get tasks
ax ctx "$GKE_CTX" && ax get tasks
```

#### Expected Behavior
- `/tmp/poc-demo-task.yaml` runs unmodified on GKE; `ax suspend` -> `ax resume` round-trips `/workspace/gcs.txt` through GCS (`saved-in-gcs`).
- `ax ctx` displays independent `(Healthy)` background tunnels for both `local` and `$GKE_CTX`, allowing instant context switching without manual `kubectl port-forward` management.

---

## Step 6: Uninstall AX and Clean Up (`ax uninstall` & `ax local delete`)

> [!TIP]
> If you also want to verify idempotent re-runs (**Optional A**) or local cluster pause/resume (**Optional B**), run the appendix steps below before deleting the clusters.

```bash
# 1. Uninstall AX from the remote cluster (revokes Substrate allowlists; keeps PVCs by default)
ax ctx "$GKE_CTX"
ax delete task poc-demo || true
ax uninstall
kubectl -n ax-system get all,pvc,secret

# 2. Destroy the local Kind cluster and remove its kubeconfig context
ax local delete
ax local status
```

### Expected Behavior
- `ax uninstall` removes `ax-server` and `ax-postgres` workloads and cleanly revokes `ax-server` from `ate-api-server`, `atenet-router`, and `k8s-credential-provider`, while preserving `pvc/data-ax-postgres-0` and `secret/ax-postgres` by default to prevent accidental data loss.
- `ax local delete` destroys the `local` Kind cluster and deletes the `local` kubeconfig context (`ax local status` reports `State: NotFound`).

---

## Appendix: Optional Verification

### Optional A: Zero-Restart Idempotency on Repeated `ax install`

```bash
ax ctx local
kubectl --context local -n ate-system get deploy ate-api-server atenet-router k8s-credential-provider \
  -o custom-columns=NAME:.metadata.name,GEN:.metadata.generation

ax install --image-repo localhost:5001 --image-tag local

kubectl --context local -n ate-system get deploy ate-api-server atenet-router k8s-credential-provider \
  -o custom-columns=NAME:.metadata.name,GEN:.metadata.generation
```

#### Expected Behavior
- `ax install` finishes in a few seconds without rotating the existing `ax-postgres` password or duplicating Substrate allowlist flags.
- All three `GEN` (`metadata.generation`) numbers remain identical before and after (**zero unnecessary rollouts**).

### Optional B: Local Pause and Fast Resume (`ax local stop` -> `ax local up`)

```bash
ax local stop
ax local status

# Resume using the images already stored in the local 127.0.0.1:5001 registry:
ax local up --workers 1 --image-repo localhost:5001 --image-tag local
ax get tasks
```

#### Expected Behavior
- `ax local stop` halts the `local-control-plane` container, stops its background tunnel, and clears cached router certificates (`State: Stopped`).
- `ax local up` restarts the existing node container in ~15–30 seconds; `ax get tasks` still lists `poc-demo` with all Postgres records and `rustfs` snapshots intact.
