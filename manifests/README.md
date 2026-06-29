# Deploying Agent Substrate + AX on Kubernetes

> [!WARNING]
> 🚧 **This deployment path is under active development.**
>
> It is experimental and incomplete: the manifests, scripts, and runtime
> behavior will change and may break without notice.

This guide deploys the full stack from the **AX** repository using a single
script, [`hack/install-ax.sh`](../hack/install-ax.sh):

1. the [Agent Substrate](https://github.com/agent-substrate/substrate) control
   plane (the compute layer AX runs on), and
2. the AX server and the Antigravity harness.

You do not need to clone substrate or pick a substrate version. The script
deploys the exact substrate version AX pins in `go.mod` and fetches that source
automatically — see [Substrate version is automatic](#substrate-version-is-automatic).

## Workflow at a glance

Deployment has two phases:

- **One-time setup** — provision the GKE cluster and GCP resources
  (`--create-cluster`). Run this once per cluster.
- **Deploy** — build and roll out substrate + AX (`--deploy-all`). Re-run this
  every time you change your code. For the common inner loop where only AX
  changed, use `--deploy-ax-server`.

## Prerequisites

Install and configure:

- [`gcloud`](https://cloud.google.com/sdk/docs/install) (authenticated — see below)
- [`kubectl`](https://kubernetes.io/docs/tasks/tools/)
- [Go](https://go.dev/doc/install)
- `git` and `openssl`
- [`ko`](https://ko.build/install/) on your `PATH` (builds the worker image)
- A container engine to build the AX image: **Docker** or **Podman**. The script
  auto-detects one (preferring a *running* docker, then podman); force a choice
  with `CONTAINER_ENGINE=docker` or `CONTAINER_ENGINE=podman`. On macOS both
  cross-build linux/amd64 via emulation; with Podman, first run
  `podman machine init && podman machine start`.

## 1. Configure your environment

Copy the example env file, edit it for your project, and source it. The script
also sources `.ax-dev-env.sh` automatically (set `NO_DEV_ENV=1` to skip):

```bash
cp hack/ax-dev-env.sh.example .ax-dev-env.sh
# edit .ax-dev-env.sh ...
source .ax-dev-env.sh
```

`PROJECT_ID` and `CLUSTER_NAME` are **required** (no defaults); the rest have
sensible defaults you can keep. Key variables (see the file for the full list):

| Variable | Purpose |
| --- | --- |
| `PROJECT_ID` | Your GCP project ID (**required**) |
| `CLUSTER_NAME` | Your GKE cluster name (**required**) |
| `PROJECT_NUMBER` | Derived from `PROJECT_ID` (used by `--create-cluster`) |
| `CLUSTER_LOCATION` / `CLUSTER_VERSION` | GKE cluster location / version |
| `NODE_POOL_NAME` / `NODE_POOL_VERSION` / `GVISOR_NODE_MACHINE_TYPE` | gVisor node pool |
| `NETWORK` / `SUBNETWORK` / `GCE_REGION` | Networking / region |
| `BUCKET_NAME` | GCS bucket for snapshots |
| `KO_DOCKER_REPO` | Image registry (defaults to `gcr.io/${PROJECT_ID}`) |
| `GEMINI_API_KEY` | Gemini AI Studio key for the AX server |
| `KUBECTL_CONTEXT` | Optional: target an existing cluster by context name |

## 2. Authenticate (one-time)

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

## 3. Create the cluster (one-time)

Provision the GKE cluster (with Workload Identity and the beta APIs substrate
requires), the GCS snapshot bucket, and IAM bindings:

```bash
./hack/install-ax.sh --create-cluster
```

> [!NOTE]
> This is a one-time step per cluster. Skip it if you already have a
> substrate-ready cluster; just set `KUBECTL_CONTEXT` to target it.

## 4. Deploy substrate + AX

```bash
./hack/install-ax.sh --deploy-all
```

This builds the images and deploys the substrate control plane followed by the
AX server. It is **idempotent and re-runnable**: re-run it after every code
change. It only rolls pods when an image digest or manifest actually changes, so
re-deploying an unchanged version is a no-op.

Re-deploy a single layer:

```bash
./hack/install-ax.sh --deploy-ax-server     # only AX changed (fast inner loop)
./hack/install-ax.sh --deploy-ate-system    # only the substrate control plane
```

> [!NOTE]
> Do not manually edit `manifests/ax-deployment.yaml`. The script injects your
> `${GEMINI_API_KEY}`, `${BUCKET_NAME}`, the built image references, and the
> event-log Postgres password during deployment.

## Substrate version is automatic

AX pins Agent Substrate in `go.mod`, and both the **ateom** worker image and the
substrate control plane are built from that pinned version. The script reads the
pin and materializes the matching substrate source for you, so the go.mod pin,
the deployed control plane, and the worker image always agree — no manual
alignment.

By default the source is a managed clone under
`${XDG_CACHE_HOME:-~/.cache}/ax/substrate`. To use your own substrate checkout
instead, set `AX_SUBSTRATE_DIR=/path/to/substrate`; the script checks out the
pinned commit there and refuses if the tree has uncommitted changes.

## Connect and test

The `harness` path has no Envoy router or `Service`; connect directly to the
`ax-server` `ReplicaSet`:

```bash
kubectl port-forward -n ax rs/ax-server 8494:8494
```

Run an execution against the port-forwarded server. The default `antigravity`
harness serves the example `examples/antigravity_agent/agent.py`, which exposes
a `get_weather` tool:

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
