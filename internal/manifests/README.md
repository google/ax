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

## 🚀 Deploying to Agent Substrate

This deploys the AX `harness` path: the AX harness `WorkerPool` and `ActorTemplate` — provisioned as isolated, warm-standby actors that are live-snapshotted on boot and instantly restored from GCS when a new conversation starts — together with an `ax-server` controller front-end (a `ReplicaSet`).

### 1. Build and Deploy

> [!NOTE]
> Do not manually edit `internal/manifests/ax-deployment2.yaml`. The installation script automatically injects your `${GEMINI_API_KEY}` and `${BUCKET_NAME}` environment variables during deployment.

Use the installation script to build the images (with the `harness` build tag) and apply the resolved manifests to your cluster:

```bash
export PROJECT_ID="ax-substrate" # Your GCP project ID
export GEMINI_API_KEY="your-api-key"
export BUCKET_NAME="snapshot-substrate-test-$PROJECT_ID"
export KO_DOCKER_REPO="gcr.io/$PROJECT_ID/ate-images"
export KO_DEFAULTPLATFORMS="linux/amd64"

./internal/hack/install-ax.sh --deploy-ax-server
```

This command will:
- Build the AX images using `ko` with the `harness` build tag.
- Create the `ax` namespace.
- Create the `WorkerPool` and `ActorTemplate` for the AX harness.
- Create the `ax-server` `ReplicaSet` (the controller front-end).
- Create the `ax-server-config` `ConfigMap` that tells the `ax-server` which
  harnesses to serve (mounted at `/etc/ax/ax.yaml`).

The harness registry lives in that `ConfigMap`. By default it registers a
substrate harness (`hello-world`) backed by the `ax-harness-template`, marked as
the default via `harnesses.default`.

Wait until the template is ready:
```bash
kubectl wait --for=condition=Ready actortemplate/ax-harness-template -n ax --timeout=5m
```

### 2. Port-Forward Services

The `harness` path has no Envoy router or `Service`; connect directly to the `ax-server` `ReplicaSet`:

```bash
# Port-forward the ax-server ReplicaSet
kubectl port-forward -n ax rs/ax-server 8494:8494
```

### 3. Test End-to-End

Run an execution targeting the port-forwarded server:

```bash
ax exec --server=localhost:8494 --input="hello"
```

The server should respond with:
```text
Conversation: fb344a18-3720-4c4f-8a6e-2ce34db975b3

⏺ hello

hello world
```
*The request is served by the harness actor running on Substrate.*

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
