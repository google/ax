# AX Deployment on Kubernetes

This directory contains Kubernetes manifests and configurations to deploy
and verify the AX on Kubernetes using Agent Substrate.

The target Kubernetes cluster is assumed to have
[Agent Substrate](https://github.com/agent-substrate/substrate) installed.

---

## 🚀 Deploying to Agent Substrate

This option deploys AX as isolated, warm-standby actors. Workers are live-snapshotted on boot and instantly restored from GCS when a new conversation starts. Actors are automatically suspended when conversations stop emitting all of their outputs.

### 1. Build and Deploy

> [!NOTE]
> Do not manually edit `manifests/ax-deployment.yaml.tmpl`. The installation script automatically injects your `${GEMINI_API_KEY}` and `${BUCKET_NAME}` environment variables during deployment.

Use the core installation script to build the images and apply the resolved manifests to your cluster:

```bash
export GEMINI_API_KEY="your-api-key"
export BUCKET_NAME="your-gcs-bucket"
./hack/ax-dev.sh cloud deploy
```

This command will:
- Build the AX server and proxy images using `ko`.
- Create the `ax` namespace.
- Create the `WorkerPool` and `ActorTemplate` for AX.

Wait until the template is ready:
```bash
kubectl wait --for=condition=Ready actortemplate/ax-template -n ax --timeout=5m
```

### 2. Port-Forward Services

To interact with the router locally:

```bash
# Port-forward the Ax Router
kubectl port-forward -n ax svc/ax-router 8001:443
```

### 3. Test End-to-End

Run an execution targeting the deployed server using the external IP:

```bash
ax exec --server=localhost:8001 --input="hello"
```
*Envoy will intercept the request and route traffic using the conversation ID.*

## 🧹 How to Uninstall

To remove AX resources from your cluster, run:

```bash
./hack/ax-dev.sh cloud delete
```

---

## 🛠️ Inspection & Diagnostics

Use the **`kubectl ate`** CLI tool to inspect the live states of
active actors and allocated standby worker pool instances:

```bash
kubectl ate get actors

kubectl ate get workers
```
