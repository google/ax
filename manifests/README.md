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

## ☁️ GKE SubstrATE vs. Open-Source SubstrATE Compatibility

AX supports both self-managed open-source SubstrATE (`ate.dev/v1alpha1`) and managed GKE SubstrATE (`ate.gke.io/v1alpha1`) clusters.

### Key Architectural & Schema Differences:
* **Container Configuration**: In open-source, containers (like AX server or harnesses) are defined inside the `ActorTemplate` resource. In GKE, the managed sandboxing engine requires containers to be declared inside the `WorkerPool` resource's `spec.containers` instead.
* **Snapshot Storage (`snapshotsConfig.location`)**: Open-source takes a single string URI (`gs://bucket/folder/`), whereas GKE validates this as a structured object containing separate `bucket` and `folder` string keys.
* **Envoy Router Cert Signer**: The SPIFFE certificate signer name is `servicedns.podcert.ate.dev/identity` in open-source, but is `servicedns.podcert.gke.io/identity` in GKE.

### Automatic Routing:
The unified deploy script (`./hack/ax-dev.sh`) dynamically detects if your active cluster runs GKE's managed CRD endpoints (`workerpools.ate.gke.io`). If GKE is found, it automatically applies GKE-specific manifest files (`-gke.yaml`), bypassing manual configuration changes.

---

## 🛠️ Inspection & Diagnostics

Use the **`kubectl ate`** CLI tool to inspect the live states of
active actors and allocated standby worker pool instances:

```bash
kubectl ate get actors

kubectl ate get workers
```
