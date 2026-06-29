# AX Harness Deployment on Kubernetes

> [!WARNING]
>
> This path is experimental and incomplete: the manifests, scripts, and
> runtime behavior will change and may break without notice.

This directory contains Kubernetes manifests and configurations to deploy
and verify the AX on Kubernetes using Agent Substrate.

There are two phases for the full deployment:

- **One-time setup** — provision the GKE cluster and GCP resources
  (`--create-cluster`). Run this once per cluster.
- **Deploy** — build and roll out substrate + AX. Re-run this
  every time you change your code and want to deploy the changes to the cluster.

> [!NOTE]
> Do not manually edit `manifests/ax-deployment.yaml`. The installation script automatically injects your `${GEMINI_API_KEY}`, `${AX_SNAPSHOTS_BUCKET}`, and the built `${AX_IMAGE}` and `${ATEOM_IMAGE}` references during deployment.

The installation script builds the required images and applies the resolved
manifests to your cluster:

- the comprehensive **ax** image, built from `cmd/ax/Dockerfile`,
- the **ateom-gvisor** worker image, built with `ko` from the `go.mod` pinned
  substrate module.

## 📋 Prerequisites

Install and configure:

- [`gcloud`](https://cloud.google.com/sdk/docs/install) (authenticated — see below)
- [`kubectl`](https://kubernetes.io/docs/tasks/tools/)
- [Go](https://go.dev/doc/install)
- `git` and `openssl`
- [`ko`](https://ko.build/install/) on your `PATH` (builds the worker image)
- A container engine to build the AX image: **Docker** or **Podman**

## 🚀 Deploying to Agent Substrate

> [!NOTE]
> If you already have a running GKE cluster with substrate already deployed,
> you can skip the step 1 to 4, and directly jump to [Step 5. Deploy AX](#5-deploy-ax).

### 1. Configure your environment

Copy the example env file and edit it for your project:

```bash
cp hack/ax-dev-env.sh.example .ax-dev-env.sh
# edit .ax-dev-env.sh ...
source .ax-dev-env.sh
```

Key variables:

| Variable | Purpose |
| --- | --- |
| `PROJECT_ID` | Your GCP project ID (**required**) |
| `CLUSTER_NAME` | Your GKE cluster name (**required**) |
| `PROJECT_NUMBER` | Derived from `PROJECT_ID` (used by `--create-cluster`) |
| `CLUSTER_LOCATION` / `CLUSTER_VERSION` | GKE cluster location / version |
| `NODE_POOL_NAME` / `NODE_POOL_VERSION` / `GVISOR_NODE_MACHINE_TYPE` | gVisor node pool |
| `NETWORK` / `SUBNETWORK` / `GCE_REGION` | Networking / region |
| `AX_SNAPSHOTS_BUCKET` | GCS bucket for snapshots |
| `KO_DOCKER_REPO` | Image registry (defaults to `gcr.io/${PROJECT_ID}`) |
| `KUBECTL_CONTEXT` | Optional: target an existing cluster by context name |

### 2. Authenticate

```bash
gcloud auth login                                    # user credentials
gcloud auth configure-docker                         # gcr.io image push
gcloud auth application-default login --project=${PROJECT_ID}
```

> [!NOTE]
> This is one-time setup. `gcloud auth configure-docker` just installs the
> `gcloud` credential helper into `~/.docker/config.json` (it does not need to
> be re-run per deploy; image pushes mint fresh tokens automatically). Re-run
> `gcloud auth login` / `application-default login` only when your gcloud
> session expires.

### 3. Create the cluster

Provision the GKE cluster, the GCS snapshot bucket, and IAM bindings:

```bash
./hack/install-ax.sh --create-cluster
```

> [!NOTE]
> This is a one-time step per cluster. Skip it if you already have a
> substrate-ready cluster; just set `KUBECTL_CONTEXT` to target it.

### 4. Deploy substrate

Deploy the substrate control plane:

```bash
./hack/install-ax.sh --deploy-ate-system    # substrate control plane (at AX's pinned version)
```

> [!NOTE]
> AX pins Agent Substrate in `go.mod`, and both the **ateom** worker image and the
substrate control plane are built from that pinned version. The script reads the
pin and materializes the matching substrate source for you. By default the source is a managed clone under
`${XDG_CACHE_HOME:-~/.cache}/ax/substrate`. To use your own substrate checkout
instead, set `AX_SUBSTRATE_DIR=/path/to/substrate`; the script checks out the
pinned commit there and refuses if the tree has uncommitted changes.

### 5. Deploy AX

Deploy AX server + harness:

```bash
./hack/install-ax.sh --deploy-ax            # AX server + harness
```

### 6. Port-Forward Services

```bash
kubectl port-forward -n ax rs/ax-server 8494:8494
```

## Test End-to-End

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
./hack/install-ax.sh --delete-all          # AX + substrate control plane
# or just one layer:
./hack/install-ax.sh --delete-ax           # AX only; preserves the event-log DB
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
