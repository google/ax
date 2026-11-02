# Version-pinned installation

This source-install recipe uses the development baseline in the
[version table](#version-compatibility), not the v0.3.x deployment
layout. Build the CLI, server, and runner from the same checkout. The pins come
from source dependencies; running the checks below on your cluster establishes
whether that deployment works.

## Version compatibility

Build the CLI, control plane, and task runner from the **same AX ref**. In
particular, `go install ...@latest` selects a released CLI, which can use a
different RPC API from a server built from `main`.

These are the Substrate dependencies recorded in each AX ref's `go.mod`.
They identify source pins; they do not certify an end-to-end deployment or
compatibility with an entire Substrate release series. The historical
pseudo-versions below refer to commits; the development baseline uses the
published Substrate v0.6.0 tag.

| AX ref | Substrate module version | Substrate checkout |
|---|---|---|
| [v0.3.0](https://github.com/google/ax/blob/v0.3.0/go.mod) | `v0.0.0-20260911232748-672533541dbf` | [`672533541dbf`](https://github.com/agent-substrate/substrate/commit/672533541dbfcd29084e4de2475267088bda3651) |
| [v0.3.1](https://github.com/google/ax/blob/v0.3.1/go.mod) | `v0.0.0-20260911232748-672533541dbf` | [`672533541dbf`](https://github.com/agent-substrate/substrate/commit/672533541dbfcd29084e4de2475267088bda3651) |
| [Development baseline `a496fc1ff1cf`](https://github.com/google/ax/blob/a496fc1ff1cf9bea4c34924348e4119172cfea5b/go.mod) | `v0.6.0` | [`d3f2363590a1`](https://github.com/agent-substrate/substrate/commit/d3f2363590a11b2f50489971985ba033b4c9eff7) |

The development baseline uses ax-server and PostgreSQL. v0.3.0 and v0.3.1 use
Redis and a separate ax-controller; use their own checkouts' manifests and build
targets. For another AX ref, inspect its `go.mod` rather than substituting the
latest Substrate release. Update this table and the installation steps when
changing the documented baseline or publishing an AX release.

## Prerequisites

- Git, Make, Go **1.27.3** (matching the pinned Substrate toolchain), `kubectl`,
  OpenSSL, and Docker with a builder capable of producing `linux/amd64` images.
- Agent Substrate at the [pinned revision](#version-compatibility), with its
  Control API, atenet router, a `gvisor-default` SandboxConfig, ready gVisor
  WorkerPool capacity, and writable snapshot storage; see
  [Substrate setup](#substrate-setup) for Kubernetes and worker requirements.
- Kubernetes permissions to deploy AX, inspect services/pods and logs, and
  create `pods/portforward` connections in both `ax-system` and `ate-system`.
  For router mTLS, the CLI also needs to list ClusterTrustBundles and create,
  get, and delete PodCertificateRequests in `ax-system`.
- A default StorageClass that can provision the PostgreSQL **10 GiB ReadWriteOnce**
  volume, or an equivalent pre-provisioned volume.
- A container registry you can push to and the cluster can pull from. Configure
  credentials on both sides if the registry is private.

## 1. Select the sources and set up Substrate

Run all command blocks in this guide in the same Bash shell so their variables
remain available. These full commits match the development row in the version table:

```bash
set -euo pipefail
AX_REF=a496fc1ff1cf9bea4c34924348e4119172cfea5b
SUBSTRATE_REF=d3f2363590a11b2f50489971985ba033b4c9eff7
git clone https://github.com/google/ax.git ax-install
cd ax-install
git checkout --detach "$AX_REF"
export GOWORK=off

git clone https://github.com/agent-substrate/substrate.git ../substrate-ax
git -C ../substrate-ax checkout --detach "$SUBSTRATE_REF"
git rev-parse HEAD
git -C ../substrate-ax rev-parse HEAD
go list -m go.ate.dev/substrate github.com/agent-substrate/env
```

Confirm the two `git rev-parse` results match the selected refs and the Substrate
module version matches the table. The development module path is
`go.ate.dev/substrate`; historical releases used `github.com/agent-substrate/substrate`.

### Substrate setup

- A Linux/amd64 Kubernetes cluster configured for the pinned Substrate version.
  Enable ClusterTrustBundle, ClusterTrustBundleProjection, and
  PodCertificateRequest, including the `certificates.k8s.io/v1beta1` API used
  by the pinned manifests. Select its kube context before proceeding.
- Worker kernels with OverlayFS `lowerdir+` support through `fsconfig`, as used
  by the pinned [Substrate image cache](https://github.com/agent-substrate/substrate/blob/d3f2363590a11b2f50489971985ba033b4c9eff7/internal/imagecache/bundle_linux.go).
  This feature is available in
  [upstream Linux 6.8 and newer](https://docs.kernel.org/filesystems/overlayfs.html#multiple-lower-layers);
  vendor kernels may backport it. For kind, check the kernel of the Docker host
  or provider VM: changing the kind node image does not change that kernel.

Install Substrate from `../substrate-ax`, following [that revision's README](https://github.com/agent-substrate/substrate/blob/d3f2363590a11b2f50489971985ba033b4c9eff7/README.md)
and [operator installation guide](https://github.com/agent-substrate/substrate/blob/d3f2363590a11b2f50489971985ba033b4c9eff7/docs/operator-install.md), then return to the AX checkout.
Keep its source ref fixed when building or deploying Substrate components. AX's
`env` module is also pinned by `go.mod`; no separate checkout is needed.

Configure Substrate's cluster-wide `--snapshot-location` and access to its
[snapshot storage](https://github.com/agent-substrate/substrate/blob/d3f2363590a11b2f50489971985ba033b4c9eff7/docs/operator-install.md#snapshot-storage).
Keep `PerTemplateSnapshotLocation` disabled: this AX client does not opt in to
preview APIs. Substrate uses its cluster location instead of the template
location sent through `AX_SNAPSHOTS_BUCKET`. That template field is still
validated as a URI, so leave its default value; changing it does not configure
snapshot storage.

[Authorize](https://github.com/agent-substrate/substrate/blob/d3f2363590a11b2f50489971985ba033b4c9eff7/docs/authorization.md)
`system:serviceaccount:ax-system:ax-server` with global permission to create
atespaces and permission to manage their actors and templates. Also allow
`spiffe://cluster.local/ns/ax-system/sa/ax-server`
in the router's ingress policy (`--ingress-allowed-spiffe-ids` in `ate-setup`).
Adjust the trust domain if your cluster uses a different one.

Create a WorkerPool using the pinned [WorkerPool guide](https://github.com/agent-substrate/substrate/blob/d3f2363590a11b2f50489971985ba033b4c9eff7/docs/api-guide.md#1-workerpool-the-physical-capacity),
with `spec.sandboxClasses: [{name: gvisor}]`, and wait for its worker pods to be
ready. Installing the Substrate control plane alone does not create this pool.
This recipe uses gVisor, but AX sets no `workerSelector`: it does not restrict
selection to that sandbox class or to the task's atespace. Inspect the pools with:

```bash
kubectl get workerpools -A
```

For a historical AX release, use its own deployment files and instructions;
the remaining steps describe only the development baseline above.

## 2. Build the CLI and images

Install the CLI into this checkout's `bin` directory so an older `ax` on your
PATH cannot be selected accidentally:

```bash
GOBIN="$PWD/bin" make install
AX_BIN="$PWD/bin/ax"
export PATH="$PWD/bin:$PATH"
AX_INSTALL_DIR="$PWD/../ax-install-records"
mkdir -p "$AX_INSTALL_DIR"
go version -m "$AX_BIN" > "$AX_INSTALL_DIR/cli-build-info.txt"
AX_CONTEXT="$(kubectl config current-context)"
unset AX_SERVER
```

Replace the registry example below with your own repository and authenticate
Docker to it. `make images` builds and pushes both the server and production
runner. Explicit tag values let you repeat the build even after exporting digest
references for deployment:

```bash
export AX_IMAGE_REPO=registry.example.com/your-team
IMAGE_TAG="ax-${AX_REF:0:12}"
make images CONTAINER_CLI=docker IMAGE_ARCH=amd64 \
  AX_IMAGE_REPO="$AX_IMAGE_REPO" IMAGE_TAG="$IMAGE_TAG" \
  AX_SERVER_IMAGE="$AX_IMAGE_REPO/ax-server:$IMAGE_TAG" \
  AX_TASK_RUNNER_IMAGE="$AX_IMAGE_REPO/ax-task-runner:$IMAGE_TAG"

grep -Eq '^sha256:[0-9a-f]{64}$' bin/ax-server.digest
grep -Eq '^sha256:[0-9a-f]{64}$' bin/ax-task-runner.digest
AX_SERVER_DIGEST="$(cat bin/ax-server.digest)"
AX_TASK_RUNNER_DIGEST="$(cat bin/ax-task-runner.digest)"
export AX_SERVER_IMAGE="$AX_IMAGE_REPO/ax-server@$AX_SERVER_DIGEST"
export AX_DEFAULT_TASK_IMAGE="$AX_IMAGE_REPO/ax-task-runner@$AX_TASK_RUNNER_DIGEST"
cp bin/ax-server.digest bin/ax-task-runner.digest "$AX_INSTALL_DIR/"
printf 'AX_REF=%s\nSUBSTRATE_REF=%s\nAX_SERVER_IMAGE=%s\nAX_DEFAULT_TASK_IMAGE=%s\n' \
  "$AX_REF" "$SUBSTRATE_REF" "$AX_SERVER_IMAGE" "$AX_DEFAULT_TASK_IMAGE" \
  > "$AX_INSTALL_DIR/source-and-images.txt"
go list -m go.ate.dev/substrate github.com/agent-substrate/env \
  > "$AX_INSTALL_DIR/modules.txt"
```

Stop if either digest check fails. Use the registry digests recorded by
`make images`, not local image IDs or mutable tags. The server uses
`AX_DEFAULT_TASK_IMAGE` for tasks that omit `spec.image`. An explicit task image
overrides it: replace private image references in any adapted examples with your
own runner digest ([#424](https://github.com/google/ax/issues/424)).

## 3. Render and deploy the control plane

Render both image references into a saved copy of the production manifest:

```bash
sed -e "s|\${AX_SERVER_IMAGE}|$AX_SERVER_IMAGE|g" \
  -e "s|\${AX_DEFAULT_TASK_IMAGE}|$AX_DEFAULT_TASK_IMAGE|g" \
  deploy/ax-server.yaml > "$AX_INSTALL_DIR/ax-server.yaml"
```

**Inspect and adjust the generated manifest before applying it:**

- Confirm the Substrate endpoint `api.ate-system.svc.cluster.local:443`, token
  audience `api.ate-system.svc`, and router
  `https://atenet-router.ate-system.svc.cluster.local:443` match your installation.
- Confirm the projected Service DNS trust bundle exists: signer
  `servicedns.podcert.ate.dev/identity` and label `podcert.ate.dev/canarying=live`.
  Keep TLS verification and the projected service-account token configured.
- Keep the PodCertificate projection for signer
  `podidentity.podcert.ate.dev/identity`; the server uses it for router mTLS.
- Leave `AX_SNAPSHOTS_BUCKET` unchanged; configure storage in Substrate as described in
  [Substrate setup](#substrate-setup).

Inspect the trust bundles and their signer names:

```bash
kubectl --context "$AX_CONTEXT" get clustertrustbundles \
  -l podcert.ate.dev/canarying=live -o yaml
```

Deploy PostgreSQL using the same steps as `make deploy-postgres`, with an
explicit context. Create its password only on the first deployment: PostgreSQL
reads it when initializing an empty volume, so replacing the Secret while
retaining the volume would break authentication.

```bash
kubectl --context "$AX_CONTEXT" create namespace ax-system --dry-run=client -o yaml \
  | kubectl --context "$AX_CONTEXT" apply -f -
kubectl --context "$AX_CONTEXT" -n ax-system get secret ax-postgres >/dev/null 2>&1 \
  || kubectl --context "$AX_CONTEXT" -n ax-system create secret generic ax-postgres \
    --from-literal=password="$(openssl rand -hex 24)"
kubectl --context "$AX_CONTEXT" apply -f deploy/postgres.yaml
kubectl --context "$AX_CONTEXT" -n ax-system rollout status statefulset/ax-postgres --timeout=180s
kubectl --context "$AX_CONTEXT" apply -f "$AX_INSTALL_DIR/ax-server.yaml"
kubectl --context "$AX_CONTEXT" -n ax-system rollout status deployment/ax-server --timeout=120s
```

Use the edited file rather than `make deploy`, which renders the checked-in
manifest again. The `postgres:17-alpine` tag and OS/Python build dependencies
remain mutable. Retain the resolved manifest, source refs, module versions, and
both AX image digests with your deployment records. The saved
`cli-build-info.txt` includes `vcs.revision`; `ax version` does not report an AX
commit. The records directory is outside the checkout so `make clean` does not
remove it.

## 4. Check a task

Control-plane rollout alone does not verify a sandbox. This task exercises the
runner and guest exec without requiring a Workspace, Model, or model API key.
It omits `spec.image` to check the server's configured default:

```bash
AX_CHECK_TASK="ax-install-$(date +%s)"
cat > "$AX_INSTALL_DIR/task.yaml" <<EOF
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: ${AX_CHECK_TASK}
  atespace: default
spec:
  debug: true
  command: ["sleep", "infinity"]
EOF
"$AX_BIN" --context "$AX_CONTEXT" -n ax-system apply -f "$AX_INSTALL_DIR/task.yaml"
"$AX_BIN" --context "$AX_CONTEXT" -n ax-system describe task "$AX_CHECK_TASK"
```

A newly created task should be `Suspended`. In the server log, confirm the
`reconciling task` entry for this task has an `image` matching
`$AX_DEFAULT_TASK_IMAGE`. If creation fails or conditions report
`TemplateCreationFailed`, diagnose the error before proceeding:

```bash
kubectl --context "$AX_CONTEXT" -n ax-system logs deployment/ax-server --since=10m
```

To recreate a `Suspended` or `Failed` check task, delete it before applying it
again; those tasks are immutable. Once creation succeeds, resume it:

```bash
"$AX_BIN" --context "$AX_CONTEXT" -n ax-system resume task "$AX_CHECK_TASK"
"$AX_BIN" --context "$AX_CONTEXT" -n ax-system describe task "$AX_CHECK_TASK"
```

Continue when the task is `Running` with `Ready=True`. Substrate first waits for
the runner's wakeup probe; a failure can produce `ActorResumeFailed`. Check the
reported error, worker capacity, image pulls, snapshot storage, and worker logs,
then retry `resume` after fixing the cause.

After the actor resumes, AX polls workspace readiness for up to 15 seconds.
If it reports `Running` with `Ready=False` / `WorkspaceInitializing`, check
ax-server's connectivity to the guest and router, then repeat `resume` and
`describe`. `describe` reads the actor's live phase but does not repeat this
readiness poll. If `resume` fails while creating an overlay superblock, inspect the
worker's kernel log: `unrecognized mount option "lowerdir+=..."` indicates
missing `lowerdir+` support; use a worker kernel that meets the
[Substrate setup requirements](#substrate-setup).
Then verify guest exec:

```bash
"$AX_BIN" --context "$AX_CONTEXT" -n ax-system ssh "$AX_CHECK_TASK" -- echo installation-ok
```

Require `installation-ok` and exit status 0. If guest exec on a `Ready` task
fails with a gRPC transport error such as `error reading server preface`, check
the route from the CLI host to the worker IP, including Pod CIDR overlap or
proxies. At this baseline, `ax ssh` first probes the worker IP over TCP (port 80
by default). If that fails, it tries router mTLS on port 443. It tries port 80
only if TLS configuration or the 443 port-forward could not be established;
a later TLS or RPC failure does not trigger that fallback. Likewise, a
successful direct TCP probe followed by an RPC failure does not retry through
the router.

After the check succeeds, remove the task and stop the tunnel:

```bash
"$AX_BIN" --context "$AX_CONTEXT" -n ax-system delete task "$AX_CHECK_TASK"
"$AX_BIN" tunnel stop "$AX_CONTEXT"
```

When upgrading, select a new AX ref and its recorded Substrate dependency,
rebuild all AX components, and repeat these checks. Keep release and development
components together at the chosen ref.
