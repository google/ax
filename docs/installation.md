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
compatibility with an entire Substrate release series. The pseudo-versions below
refer to commits, not published Substrate release tags.

| AX ref | Substrate module version | Substrate checkout |
|---|---|---|
| [v0.3.0](https://github.com/google/ax/blob/v0.3.0/go.mod) | `v0.0.0-20260911232748-672533541dbf` | [`672533541dbf`](https://github.com/agent-substrate/substrate/commit/672533541dbfcd29084e4de2475267088bda3651) |
| [v0.3.1](https://github.com/google/ax/blob/v0.3.1/go.mod) | `v0.0.0-20260911232748-672533541dbf` | [`672533541dbf`](https://github.com/agent-substrate/substrate/commit/672533541dbfcd29084e4de2475267088bda3651) |
| [Development baseline `8d2ccf36f194`](https://github.com/google/ax/blob/8d2ccf36f194f6f58c57b90753bd3986639b562f/go.mod) | `v0.4.1-0.20261009152331-f6a1dc72a35d` | [`f6a1dc72a35d`](https://github.com/agent-substrate/substrate/commit/f6a1dc72a35d9875302e2998b2aa23b67dbc732a) |

The development baseline uses ax-server and Redis. v0.3.0 and v0.3.1 also
deploy a separate ax-controller; use their own checkouts' manifests and build
targets. For another AX ref, inspect its `go.mod` rather than substituting the
latest Substrate release. Update this table and the installation steps when
changing the documented baseline or publishing an AX release.

## Prerequisites

- Git, Make, Go **1.27.1**, `kubectl`, [ko](https://ko.build/install/), and Docker
  with a builder capable of producing `linux/amd64` images.
- A Linux/amd64 Kubernetes cluster configured for the pinned Substrate version.
  It must support ClusterTrustBundle projection. Select its kube context before
  proceeding.
- Worker kernels with OverlayFS `lowerdir+` support through `fsconfig`, as used
  by the pinned [Substrate image cache](https://github.com/agent-substrate/substrate/blob/f6a1dc72a35d9875302e2998b2aa23b67dbc732a/internal/imagecache/bundle_linux.go#L159-L176).
  This feature is available in
  [upstream Linux 6.8 and newer](https://docs.kernel.org/filesystems/overlayfs.html#multiple-lower-layers);
  vendor kernels may backport it. For kind, check the kernel of the Docker host
  or provider VM: changing the kind node image does not change that kernel.
- Substrate's Control API and atenet router, a `gvisor-default` SandboxConfig,
  and at least one gVisor WorkerPool with ready worker capacity. Follow the
  pinned Substrate checkout's README and sandbox/WorkerPool documentation for
  provisioning, cluster API requirements, and authentication.
- Kubernetes permissions to deploy AX, inspect services/pods and logs, and
  create `pods/portforward` connections in both `ax-system` and `ate-system`.
  The CLI connects to ax-server; guest exec can also port-forward to the atenet
  router when its TCP connection to the worker IP fails.
- A snapshot storage location writable by Substrate, and a container registry
  you can push to and the cluster can pull from. Configure credentials on both
  sides if the registry is private.

## 1. Select the sources

Run all command blocks in this guide in the same shell so their variables remain
available. These full commits match the development row in the version table:

```bash
AX_REF=8d2ccf36f194f6f58c57b90753bd3986639b562f
SUBSTRATE_REF=f6a1dc72a35d9875302e2998b2aa23b67dbc732a
git clone https://github.com/google/ax.git ax-install
cd ax-install
git checkout --detach "$AX_REF"
export GOWORK=off

git clone https://github.com/agent-substrate/substrate.git ../substrate-ax
git -C ../substrate-ax checkout --detach "$SUBSTRATE_REF"
git rev-parse HEAD
git -C ../substrate-ax rev-parse HEAD
go list -m github.com/agent-substrate/substrate github.com/agent-substrate/env
```

Confirm the two `git rev-parse` results match the selected refs and the Substrate
module version matches the table. Install Substrate from
`../substrate-ax`, following [that revision's README](https://github.com/agent-substrate/substrate/blob/f6a1dc72a35d9875302e2998b2aa23b67dbc732a/README.md)
and its local instructions, then return to the AX checkout. Keep its source ref
fixed when building or deploying Substrate components. AX's `env` module is also
pinned by `go.mod`; no separate checkout is needed.

Create a WorkerPool using the pinned [WorkerPool guide](https://github.com/agent-substrate/substrate/blob/f6a1dc72a35d9875302e2998b2aa23b67dbc732a/docs/api-guide.md#1-workerpool-the-physical-capacity),
with `spec.sandboxClasses: [{name: gvisor}]`, and wait for its worker pods to be
ready. Installing the Substrate control plane alone does not create this pool.
AX sets no `workerSelector`, so any pool matching the gVisor class is eligible;
pool selection is not scoped by the task's atespace. Inspect the pools with:

```bash
kubectl get workerpools -A
```

For a historical AX release, use its own deployment files and instructions;
the remaining steps describe only the development baseline above.

## 2. Build the CLI and runner

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
Docker to it. The default task image is not publicly accessible
([#424](https://github.com/google/ax/issues/424)), so build the production runner
with `Dockerfile.task-runner` through the existing Make target:

```bash
TASK_RUNNER_REPO=registry.example.com/your-team/ax-task-runner
make build-task-runner CONTAINER_CLI=docker TASK_RUNNER_REPO="$TASK_RUNNER_REPO"
docker push "$TASK_RUNNER_REPO:latest"
docker image inspect "$TASK_RUNNER_REPO:latest" \
  --format '{{range .RepoDigests}}{{println .}}{{end}}'
```

Copy the `repository@sha256:...` entry for **your** repository into
`AX_TASK_IMAGE` below. Use the registry digest, not a local image ID or `:latest`:

```bash
AX_TASK_IMAGE='registry.example.com/your-team/ax-task-runner@sha256:REPLACE_WITH_PUSHED_DIGEST'
```

Set this image explicitly on every Task, including any adapted examples. This
recipe uses `docker push` directly because `make push-task-runner` uses a
GCR-specific digest lookup. The runner is built for `linux/amd64`; building it
with `ko` would use a different base image without the production Python tools.

## 3. Render and deploy the control plane

Set your server image repository. [ko resolve](https://ko.build/features/k8s/)
builds and pushes the server from this checkout and replaces `ko://` with a
digest reference in the generated manifest:

```bash
export KO_DOCKER_REPO=registry.example.com/your-team/ax
ko resolve --platform=linux/amd64 -f deploy/ax-server.yaml \
  > "$AX_INSTALL_DIR/ax-server.yaml"
```

**Edit the generated manifest before applying it:**

- Set `AX_SNAPSHOTS_BUCKET` to your Substrate snapshot location. The committed
  value, `gs://dberkov-gke-dev3/ate-env/`, is project-specific; removing or emptying
  the variable falls back to the same value.
- Confirm the Substrate endpoint `api.ate-system.svc.cluster.local:443`, token
  audience `api.ate-system.svc`, and router
  `atenet-router.ate-system.svc.cluster.local:80` match your installation.
- Confirm the projected Service DNS trust bundle exists: signer
  `servicedns.podcert.ate.dev/identity` and label `podcert.ate.dev/canarying=live`.
  Keep TLS verification and the projected service-account token configured.

Inspect the trust bundles and their signer names:

```bash
kubectl --context "$AX_CONTEXT" get clustertrustbundles \
  -l podcert.ate.dev/canarying=live -o yaml
```

Then deploy Redis and your edited server manifest into `ax-system`:

```bash
kubectl --context "$AX_CONTEXT" apply -f deploy/redis.yaml
kubectl --context "$AX_CONTEXT" apply -f "$AX_INSTALL_DIR/ax-server.yaml"
kubectl --context "$AX_CONTEXT" -n ax-system rollout status deployment/ax-redis --timeout=120s
kubectl --context "$AX_CONTEXT" -n ax-system rollout status deployment/ax-server --timeout=120s
```

Use the edited file rather than `make deploy`, which applies the committed
defaults. The `redis:7-alpine` tag and OS/Python build dependencies remain mutable.
Retain the resolved manifest, source refs, module versions, and runner digest
with your deployment records. The saved `cli-build-info.txt` includes
`vcs.revision`; `ax version` does not report an AX commit. The records directory
is outside the checkout so `make clean` does not remove it.

## 4. Check a task

Control-plane rollout alone does not verify a sandbox. This task exercises the
runner and guest exec without requiring a Workspace, Model, or model API key:

```bash
AX_CHECK_TASK="ax-install-$(date +%s)"
cat > "$AX_INSTALL_DIR/task.yaml" <<EOF
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: ${AX_CHECK_TASK}
  atespace: default
spec:
  image: "${AX_TASK_IMAGE}"
  debug: true
  command: ["sleep", "infinity"]
EOF
"$AX_BIN" --context "$AX_CONTEXT" -n ax-system apply -f "$AX_INSTALL_DIR/task.yaml"
"$AX_BIN" --context "$AX_CONTEXT" -n ax-system describe task "$AX_CHECK_TASK"
```

A newly created task should be `Suspended`. Inspect its conditions and the
ax-server log before proceeding, even if `apply` succeeded:

```bash
kubectl --context "$AX_CONTEXT" -n ax-system logs deployment/ax-server --since=10m
```

Require `using custom ActorTemplate for actor` with a `templateName` beginning
with your `${AX_CHECK_TASK}-tmpl-`; widen the log window if needed to include
creation. In this baseline, a custom template creation
error can fall back to `default-template` while still returning a `Suspended`
task. If the log reports `falling back to default template` for this creation,
or you cannot confirm its custom template, stop here and diagnose the failure.
Do not resume or SSH into the task. After fixing a failed creation or template
fallback, delete this check task before applying it again; both `Suspended` and
`Failed` tasks are immutable.

Once its custom template is confirmed, resume the task:

```bash
"$AX_BIN" --context "$AX_CONTEXT" -n ax-system resume task "$AX_CHECK_TASK"
"$AX_BIN" --context "$AX_CONTEXT" -n ax-system describe task "$AX_CHECK_TASK"
```

Continue when the task is `Running` with `Ready=True`. The readiness check waits
up to 15 seconds during `resume`; `describe` only reads the stored status. If the
task remains `Running` with `Ready=False` / `WorkspaceInitializing`, check worker
capacity, image pulls, snapshot storage, and the reported conditions. Once the
guest has initialized, run the `resume` and `describe` commands again to refresh
readiness. If `resume` fails while creating an overlay superblock, inspect the
worker's kernel log: `unrecognized mount option "lowerdir+=..."` indicates
missing `lowerdir+` support; use a worker kernel that meets the prerequisites.
Then verify guest exec:

```bash
"$AX_BIN" --context "$AX_CONTEXT" -n ax-system ssh "$AX_CHECK_TASK" -- echo installation-ok
```

Require `installation-ok` and exit status 0. If guest exec on a `Ready` task
fails with a gRPC transport error such as `error reading server preface`, check
the route from the CLI host to the worker IP, including Pod CIDR overlap or
proxies. At this baseline, `ax ssh` first probes the worker IP over TCP (port 80
by default) and port-forwards to the atenet router only if that connection
fails. A successful TCP probe does not verify guest RPC connectivity, and a
later RPC failure does not trigger the router fallback.

After the check succeeds, remove the task and stop the tunnel:

```bash
"$AX_BIN" --context "$AX_CONTEXT" -n ax-system delete task "$AX_CHECK_TASK"
"$AX_BIN" tunnel stop "$AX_CONTEXT"
```

When upgrading, select a new AX ref and its recorded Substrate dependency,
rebuild all AX components, and repeat these checks. Keep release and development
components together at the chosen ref.
