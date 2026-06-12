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

---

## Harness types

AX serves two kinds of harnesses:

- **Built-in** (e.g. Antigravity): implementation
  and container image are provided by AX. You configure only behavior; AX owns
  deployment. A built-in runs **locally** or as a **SubstrATE actor** depending
  on the `AX_SUBSTRATE` environment variable (`1` = substrate). Built-in actors
  run in the reserved `ax` namespace.
- **Custom** (the `substrate` config key): implementation and container image are
  provided by you via your own `ActorTemplate`. Custom harnesses always run on
  SubstrATE, in **your own namespace** (the `ax` namespace is reserved for
  built-ins), and require `AX_SUBSTRATE=1`.

---

## 🚀 Deploying to Agent Substrate

This deploys the AX `harness` path: a built-in harness `WorkerPool` and `ActorTemplate` (the `antigravity` example, in the reserved `ax` namespace), a custom harness `WorkerPool` and `ActorTemplate` (the `hello-world` example, in the `custom-harness` namespace) — provisioned as isolated, warm-standby actors that are live-snapshotted on boot and instantly restored from GCS when a new conversation starts — together with an `ax-server` controller front-end (a `ReplicaSet`) in the `ax` namespace.

### 1. Build and Deploy

> [!NOTE]
> Do not manually edit `internal/manifests/ax-deployment2.yaml`. The installation script automatically injects your `${GEMINI_API_KEY}`, `${BUCKET_NAME}`, and the built `${ANTIGRAVITY_IMAGE}` reference during deployment.

The installation script builds two images and applies the resolved manifests to
your cluster: the AX control-plane (Go) image, built with `ko` using the
`harness` build tag, and the built-in **antigravity harness** image, built from
`python/antigravity/Dockerfile` with Docker or Podman.

#### Build prerequisites

The antigravity image bundles the antigravity SDK and its `localharness` binary,
installed offline from a pre-downloaded linux/amd64 wheel cache. Fetch it once
(re-run after dependency changes):

```bash
./internal/hack/install-ax.sh --fetch-wheels
```

> [!NOTE]
> `--fetch-wheels` resolves the **linux/amd64 + CPython 3.13** wheels regardless
> of your host OS/Python, so Mac and Linux produce the same set. It uses your
> host pip index configuration, which must reach the private antigravity registry
> (override the primary index with `PIP_INDEX_URL`). Customize the cache location
> with `WHEELS_DIR` and the interpreter with `PYTHON`.

You also need a container engine to build and push the harness image. The script
auto-detects one (preferring a **running** docker, then podman); force a choice
with `CONTAINER_ENGINE=docker` or `CONTAINER_ENGINE=podman`. The engine must
support `--build-context` and `RUN --mount`:

- **Docker** — Docker Desktop (macOS; cross-builds linux/amd64 via emulation) or
  Docker Engine (Linux; native). Requires BuildKit (default since Docker 23; on
  older Docker use `docker buildx`). Authenticate to your registry with
  `gcloud auth configure-docker <region>-docker.pkg.dev` or `docker login`.
- **Podman** — on macOS, start a machine first with `podman machine init &&
  podman machine start` (cross-builds linux/amd64 via emulation); on Linux it
  runs natively (podman/buildah >= 4.0). Authenticate with a credential helper
  or `podman login`.

Unlike `ko`, the container engine's `push` is not auto-authenticated, so make
sure you are logged in to `$KO_DOCKER_REPO` first.

#### Deploy

```bash
export PROJECT_ID="ax-substrate" # Your GCP project ID
export GEMINI_API_KEY="your-api-key"
export BUCKET_NAME="snapshot-substrate-test-$PROJECT_ID"
export KO_DOCKER_REPO="gcr.io/$PROJECT_ID/ate-images"
export KO_DEFAULTPLATFORMS="linux/amd64"

./internal/hack/install-ax.sh --deploy-ax-server
```

This command will:
- Build the AX control-plane image with `ko` (`harness` build tag) and the
  antigravity harness image with the detected container engine, pushing both to
  `$KO_DOCKER_REPO`. Both are referenced by **digest** (`repo@sha256:...`) in the
  `ActorTemplate`, which Substrate requires because a moving tag would invalidate
  the actor's live snapshots.
- Create the `ax` namespace (AX control plane + built-in harnesses) and the
  `custom-harness` namespace (the example custom harness).
- Create a shared `ax-harness-workerpool` `WorkerPool` and the built-in
  `antigravity-template` `ActorTemplate` in `ax` (all built-in harnesses share
  this pool).
- Create a shared `custom-harness-workerpool` `WorkerPool` and the
  `hello-world-template` `ActorTemplate` in `custom-harness` (custom harnesses
  there share this pool).
- Create the `ax-server` `ReplicaSet` (the controller front-end) in `ax`.
- Create the `ax-server-config` `ConfigMap` that tells the `ax-server` which
  harnesses to serve (mounted at `/etc/ax/ax.yaml`).

The harness registry lives in that `ConfigMap`. It registers the built-in
`antigravity` harness (AX-managed, in `ax`; the antigravity image built
above) and a custom substrate harness (`hello-world`, in `custom-harness`), with
`antigravity` marked as the default via `harnesses.default`.

Wait until the templates are ready:
```bash
kubectl wait --for=condition=Ready actortemplate/antigravity-template -n ax --timeout=5m
kubectl wait --for=condition=Ready actortemplate/hello-world-template -n custom-harness --timeout=5m
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

To remove AX resources from your cluster, run:

```bash
./internal/hack/install-ax.sh --delete-ax-server
```

---

## 🛠️ Inspection & Diagnostics

Use the **`kubectl ate`** CLI tool to inspect the live states of
active actors and allocated standby worker pool instances:

```bash
kubectl ate get actors

kubectl ate get workers
```
