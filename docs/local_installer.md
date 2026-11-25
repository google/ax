# AX Local Installer

Install the `ax` CLI, run AX on a local Kind cluster, run a task, and move it to a remote cluster.

## 1. Install `ax`

```bash
export AX_VERSION=v0.4.0-rc.2
curl -fsSL "https://github.com/google/ax/releases/download/${AX_VERSION}/install.sh" | sh
export PATH="$HOME/.ax/bin:$PATH"
ax version
```

`install.sh` installs the `ax` and `ax-studio` of release `AX_VERSION` into `~/.ax/bin`, verifying their SHA-256 digests, and never needs `sudo`. Without `AX_VERSION` it installs the latest stable release. To build from source instead (Go 1.27+), run `go install github.com/google/ax/cmd/ax@main`.

## 2. Start a local cluster

Requirements: a running rootful `docker` or `podman` whose host (or macOS VM) has Linux kernel 6.8+ and at least 4 GiB RAM (6 GiB recommended). On macOS, use OrbStack, Docker Desktop, or Colima:

```bash
brew install docker colima
colima start --cpu 4 --memory 8 --disk 60 --vm-type vz --mount-type virtiofs
```

Then:

```bash
ax local start     # ~3 minutes: Kind cluster "ax-local", Agent Substrate, AX
ax local status
```

`ax` downloads `kind` and `kubectl` into `~/.ax/bin` if they are not on `PATH`, and switches your kubeconfig context to `ax-local`.

- Release builds deploy their matching `ax-server` image. Source builds need `--server-image` (or `$AX_SERVER_IMAGE`).
- Prebuilt agent images (such as Antigravity) have no runner baked in: pass `--runner-volume-image` (or `$AX_RUNNER_VOLUME_IMAGE`) with the `ax-runner-volume` digest from the release notes.
- Images must be public; the local cluster has no registry credentials.

Or run `ax studio` to do this, and create tasks, from a web page on `http://127.0.0.1:8080`. See the [AX Studio README](https://github.com/agentexecutor/studio#readme).

### Using Locally Built Images

The local cluster can pull images from a registry container named `kind-registry` on `localhost`:

```bash
docker run -d -p 127.0.0.1:5001:5000 --name kind-registry registry:2
make images AX_IMAGE_REPO=localhost:5001 CONTAINER_CLI=docker   # prints the AX_SERVER_IMAGE export to run
ax local start --server-image "$AX_SERVER_IMAGE"
```

From a checkout, `make build-local-cluster` does all of this in one step and prints the `AX_TASK_IMAGE` to use; rerun it after a change to roll out a new `ax-server`.

## 3. Run a task

Set `AX_TASK_IMAGE` to the digest-pinned `ax-runner` image from the release notes (or your own image built on it):

```bash
export AX_TASK_IMAGE="ghcr.io/google/ax/ax-runner@sha256:<digest>"
cat <<EOF > /tmp/demo-task.yaml
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: demo-task
  atespace: default
spec:
  debug: true
  profile:
    image: "${AX_TASK_IMAGE}"
  commands: ["sleep", "infinity"]
EOF

ax apply -f /tmp/demo-task.yaml   # created suspended
ax resume task demo-task
ax ssh demo-task -- sh -c 'echo hello > /workspace/hello.txt'
ax suspend task demo-task         # checkpoints /workspace
ax resume task demo-task
ax ssh demo-task -- cat /workspace/hello.txt
```

`ax` opens a background tunnel to `ax-server` per kube context; `ax ctx` lists contexts and their tunnels. See [manifests.md](manifests.md) for the full API.

## 4. Deploy to a remote cluster

The cluster needs Kubernetes 1.36+ (GKE 1.37+) with `ClusterTrustBundle` and `PodCertificateRequest` enabled, Agent Substrate `v1.0.0` in `ate-system` with a gVisor `WorkerPool` and a snapshot location, and a default `StorageClass`.

```bash
ax ctx <remote-context>
ax install                        # same image flags as ax local start
ax apply -f /tmp/demo-task.yaml   # the same manifest, unchanged
```

To copy a running local task (with its Workspace and Gateway) instead of re-applying it:

```bash
ax ctx ax-local
ax promote task demo-task --to <remote-context> --suspend-source
```

`ax promote` does not copy the sandbox's state or the Secrets a Gateway names; `--preflight-only` shows what it would do. Run `ax promote --help` for the other flags.

## 5. Clean up

```bash
ax ctx <remote-context> && ax uninstall   # keeps the ax-postgres PVC and Secret
ax local delete                           # deletes the ax-local cluster and context
rm -rf ~/.ax                              # binaries, tunnels, certificates
```

`ax local stop` pauses the local cluster instead; `ax local start` resumes it. Suspend tasks first so their `/workspace` is saved.
