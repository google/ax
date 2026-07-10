# AX Harness Deployment on Kubernetes

> [!WARNING]
>
> This path is experimental and incomplete: the manifests, scripts, and
> runtime behavior will change and may break without notice.

This directory contains Kubernetes manifests and configurations to deploy
and verify the AX on Kubernetes using Agent Substrate.

The target Kubernetes cluster is assumed to have
[Agent Substrate](https://github.com/agent-substrate/substrate) installed.

---

## 🚀 Deploying to Agent Substrate

### 1. Build and Deploy

> [!NOTE]
> Do not manually edit `manifests/ax-deployment.yaml`. The installation script automatically injects your `${GEMINI_API_KEY}`, `${AX_SNAPSHOTS_BUCKET}`, and the built `${AX_IMAGE}` and `${ATEOM_IMAGE}` references during deployment.

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

`GOOGLE_CLOUD_PROJECT` sets `AX_IMAGE_REPO=gcr.io/$GOOGLE_CLOUD_PROJECT`. The deploy pushes two
images — the **ax** image (via your container engine) and the **ateom** image
(via `ko`) — and both authenticate through the gcloud credential helper:

```bash
gcloud auth login              # authenticate gcloud
gcloud auth configure-docker   # set up the gcr.io credential helper
```

#### Deploy

The event log is stored in Postgres. By default ax-server connects to an
**existing** Postgres that you provide via the `AX_EVENTLOG_DSN` env var (bring your own database). Pass `--deploy-postgres` to also
create a **bundled** Postgres in-cluster instead (for testing).

```bash
export GOOGLE_CLOUD_PROJECT="ax-substrate" # Your GCP project ID
export GEMINI_API_KEY="your-api-key"
export AX_SNAPSHOTS_BUCKET="snapshot-substrate-test-$GOOGLE_CLOUD_PROJECT"

# Connect to your existing Postgres:
export AX_EVENTLOG_DSN="postgres://user:pass@host:5432/db?sslmode=require"
./hack/install-ax.sh --deploy-ax-server

# Or deploy a bundled Postgres for testing:
./hack/install-ax.sh --deploy-ax-server --deploy-postgres
```

The bundled Postgres uses an auto-generated password. To get its DSN:

```bash
kubectl get secret ax-eventlog-postgres -n ax -o go-template='{{.data.dsn | base64decode}}'
```

#### Vertex AI access for the Antigravity Interactions harness

> [!NOTE]
> You may skip this if you only use the default harness.

The Antigravity **Interactions** harness (`ax harness antigravity-interactions`,
ActorTemplate `ax-harness-interactions-template`) calls the Vertex AI GenAI API
and authenticates with the actor's Google Cloud credentials — unlike the default
Antigravity harness, which uses `GEMINI_API_KEY`.

The worker pods (WorkerPool `ax-harness-workerpool`, namespace `ax`) have no GSA
annotation, so with Workload Identity the actor authenticates **directly as the
Kubernetes ServiceAccount principal** `ax/default`. Grant that principal
`roles/aiplatform.user` on the project the harness reads from `GOOGLE_CLOUD_PROJECT`, or
Vertex calls fail with `PermissionDenied (403)`. IAM changes can take a minute or two to propagate.

```bash
PROJECT_NUMBER="$(gcloud projects describe "${GOOGLE_CLOUD_PROJECT}" --format='value(projectNumber)')"

gcloud projects add-iam-policy-binding "${GOOGLE_CLOUD_PROJECT}" \
  --role=roles/aiplatform.user \
  --member="principal://iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/${GOOGLE_CLOUD_PROJECT}.svc.id.goog/subject/ns/ax/sa/default" \
  --condition=None
```

### 2. Port-Forward Services

```bash
# Port-forward the ax-server ReplicaSet
kubectl port-forward -n ax rs/ax-server 8494:8494
```

### 3. Test End-to-End

Run an execution targeting the port-forwarded server.

```bash
ax exec --server=localhost:8494 --input="hello, who are you?"
```

The server should respond with something like:
```text
Conversation: fb344a18-3720-4c4f-8a6e-2ce34db975b3

⏺ hello, who are you?

I am a helpful assistant. How can I help you today?
```
*The request is served by the antigravity harness actor running on Substrate.*

## 🧹 How to Uninstall

To remove the AX server and its components, run:

```bash
./hack/install-ax.sh --delete-ax-server
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
kubectl ate get actors -a ax

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
