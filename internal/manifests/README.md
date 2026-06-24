# AX Harness Deployment on Kubernetes

> [!WARNING]
> 🚧 **The `harness` deployment path is under active development.**
>
> This path is experimental and incomplete: the manifests, scripts, and
> runtime behavior will change and may break without notice.

This directory contains Kubernetes manifests and configurations to deploy
and verify the AX `harness` configuration path on Kubernetes using Agent
Substrate.

The target Kubernetes cluster is assumed to have
[Agent Substrate](https://github.com/agent-substrate/substrate) installed.

### Substrate compatibility

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

---

## 🚀 Deploying to Agent Substrate

### 1. Build and Deploy

> [!NOTE]
> Do not manually edit `internal/manifests/ax-deployment2.yaml`. The installation script automatically injects your `${GEMINI_API_KEY}`, `${BUCKET_NAME}`, and the built `${AX_IMAGE}` and `${ATEOM_IMAGE}` references during deployment.

The installation script builds the required images and applies the resolved
manifests to your cluster:

- the comprehensive **ax** image, built from `cmd/ax/Dockerfile` with Docker or
  Podman (the `harness`-tagged Go `ax` binary plus the Antigravity Python sidecar
  on a Debian base). The ax-server runs `ax serve`; the harness actor runs
  `ax harness`, which forks the sidecar;
- the **ateom-gvisor** worker image, built with `ko` from the `go.mod` pinned
  substrate module.

#### Build prerequisites

The ax image bundles the antigravity SDK and its `localharness` binary,
installed from PyPI at build time. The image targets the cluster's **linux/amd64**
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
gcloud auth login              # authenticate gcloud
gcloud auth configure-docker   # set up the gcr.io credential helper
```

#### Deploy

```bash
export PROJECT_ID="ax-substrate" # Your GCP project ID
export GEMINI_API_KEY="your-api-key"
export BUCKET_NAME="snapshot-substrate-test-$PROJECT_ID"

./internal/hack/install-ax.sh --deploy-ax-server
```

### 2. Port-Forward Services

The `harness` path has no Envoy router or `Service`; connect directly to the `ax-server` `ReplicaSet`:

```bash
# Port-forward the ax-server ReplicaSet
kubectl port-forward -n ax rs/ax-server 8494:8494
```

### 3. Test End-to-End

Run an execution targeting the port-forwarded server. The default `antigravity`
harness serves the example `examples/antigravity_agent/agent.py`, which exposes
a `get_weather` tool.

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

## 🧹 How to Uninstall

To remove the AX server and its components, run:

```bash
./internal/hack/install-ax.sh --delete-ax-server
```

> [!NOTE]
> The event-log database is preserved by default. If you want to
> delete everything including the data, after the command above, be careful and
> run:
>
> ```bash
> kubectl delete namespace ax
> ```

---

## 🛠️ Inspection & Diagnostics

Use the **`kubectl ate`** CLI tool to inspect the live states of
active actors and allocated standby worker pool instances:

```bash
kubectl ate get actors

kubectl ate get workers
```
