# Deploying Agent Substrate + AX on Kubernetes

> [!WARNING]
>
> It is experimental and incomplete: the manifests, scripts, and runtime
> behavior will change and may break without notice.

This directory contains Kubernetes manifests and configurations to deploy
and verify the AX on Kubernetes using Agent Substrate.

The target Kubernetes cluster is assumed to have
[Agent Substrate](https://github.com/agent-substrate/substrate) installed.

---

## 🚀 Deploying to Agent Substrate

### 1. Build and Deploy

> [!NOTE]
> Do not manually edit `manifests/ax-deployment.yaml`. The installation script automatically injects your `${GEMINI_API_KEY}`, `${BUCKET_NAME}`, and the built `${AX_IMAGE}` and `${ATEOM_IMAGE}` references during deployment.

The installation script builds the required images and applies the resolved
manifests to your cluster:

- the comprehensive **ax** image, built from `cmd/ax/Dockerfile`,
- the **ateom-gvisor** worker image, built with `ko` from the `go.mod` pinned
  substrate module.

#### Build prerequisites

The ax image bundles the antigravity SDK, installed from PyPI at build time.
The image targets the cluster's **linux/amd64**
nodes and is built with `--platform linux/amd64`.

You also need a container engine to build and push the ax image. The script
auto-detects one (preferring a **running** docker, then podman); force a choice
with `CONTAINER_ENGINE=docker` or `CONTAINER_ENGINE=podman`:

- **Docker** — Docker Desktop (macOS; cross-builds linux/amd64 via emulation) or
  Docker Engine (Linux; native).
- **Podman** — on macOS, start a machine first with `podman machine init &&
  podman machine start` (cross-builds linux/amd64 via emulation); on Linux it
  runs natively (podman/buildah >= 4.0).

#### Registry authentication

`PROJECT_ID` sets `KO_DOCKER_REPO=gcr.io/$PROJECT_ID`. The deploy pushes two
images — the **ax** image (via your container engine) and the **ateom** image
(via `ko`) — and both authenticate through the gcloud credential helper:

```bash
./hack/install-ax.sh --deploy-all
```

This builds the images and deploys the substrate control plane followed by the
AX server. It is **idempotent and re-runnable**: re-run it after every code
change. It only rolls pods when an image digest or manifest actually changes, so
re-deploying an unchanged version is a no-op.

Re-deploy a single layer:

```bash
export PROJECT_ID="ax-substrate" # Your GCP project ID
export GEMINI_API_KEY="your-api-key"
export BUCKET_NAME="snapshot-substrate-test-$PROJECT_ID"

./hack/install-ax.sh --deploy-ax-server
```

### 2. Port-Forward Services

```bash
kubectl port-forward -n ax rs/ax-server 8494:8494
```

### 3. Test End-to-End

Run an execution targeting the port-forwarded server. The default `antigravity`
harness has an embedded weather agent that exposes a `get_weather` tool.

```bash
ax exec --server=localhost:8494 --input="what's the weather in NYC?"
```

The server should respond with something like:

```text
Conversation: fb344a18-3720-4c4f-8a6e-2ce34db975b3

⏺ what's the weather in NYC?

The weather in New York is sunny with a temperature of 25 degrees Celsius (77 degrees Fahrenheit).
```

*The request is served by the antigravity harness actor running on Substrate.*

## Uninstall

Remove the in-cluster workloads (the event-log database is preserved):

```bash
./hack/install-ax.sh --delete-all          # AX server + substrate control plane
# or just one layer:
./hack/install-ax.sh --delete-ax-server    # AX only; preserves the event-log DB
./hack/install-ax.sh --delete-ate-system   # substrate control plane only
```

To delete everything in the namespace, including the event-log data:

```bash
kubectl delete namespace ax
```

To tear down the GCP resources created by `--create-cluster` (cluster, bucket,
and IAM):

```bash
./hack/install-ax.sh --delete-cluster
```

> [!WARNING]
> `--delete-cluster` removes shared GCP infrastructure. Be careful on a shared
> project.

## Inspection & diagnostics

Use the **`kubectl ate`** CLI tool to inspect the live states of active actors
and allocated standby worker pool instances:

```bash
kubectl ate get actors

kubectl ate get workers
```

List the pods running in the `ax` namespace:

```bash
# Add `-o wide` to see node/IP assignments, or `-w` to watch status changes.
kubectl get pods -n ax
```

## Substrate compatibility

AX pins [Agent Substrate](https://github.com/agent-substrate/substrate) in
`go.mod`, and the **ateom** worker image is built from that pinned version. The
cluster's substrate **CRDs and control plane** must be compatible with the
manifest AX applies.

When installing substrate, keep three things aligned: the ax `go.mod` pin = your
local substrate checkout = the cluster's installed substrate.

```bash
# Get AX's pinned substrate commit:
commit=$(go list -m -f '{{.Version}}' github.com/agent-substrate/substrate | sed 's/.*-//')
echo "$commit"   # e.g. fe93d160a1df

# Check it out on a normal branch in your substrate clone (avoids a detached HEAD):
git -C <substrate> fetch origin
git -C <substrate> switch -C ax-pinned "$commit"
```
