# AX End-to-End (E2E) Tests

This directory contains the end-to-end test suite for **AX** running on
**Substrate** in a local [Kind](https://kind.sigs.k8s.io/) cluster, along with
the manifests and Dockerfiles used to deploy AX for the tests.

The suite exercises real gVisor sandboxes, Substrate's `ate-api-server`,
in-cluster RustFS snapshot storage, Redis, `ax-server`, `ax-task-runner`, and
the `ax` CLI, against a configurable Substrate branch, tag, or commit.

| Path | Purpose |
|---|---|
| `harness_test.go` | `TestMain`, cluster connection setup, and shared helpers |
| `e2e_test.go` | The test cases |
| `manifests/` | Namespace, WorkerPool, Redis, and ax-server manifests (rendered with `envsubst`) |
| `Dockerfile.task-runner.e2e` | Extends `Dockerfile.task-runner` with test fixtures (a sample Git repo) |

## Quick Start

Provision a cluster and run the suite against the Substrate version pinned in `go.mod`:

```bash
make test-e2e
```

Against Substrate `main`, or any branch, tag, or commit:

```bash
make test-e2e-head
make test-e2e SUBSTRATE_REF=release-0.4
```

### Iterating

Provisioning takes several minutes; reuse the cluster between runs:

```bash
make e2e-setup                     # provision Kind + Substrate + AX (re-run after changing server code)
make e2e-run                       # run all tests
make e2e-run E2E_ARGS='-test.run TestE2E_ModelCRUD'
make e2e-cleanup                   # delete the Kind cluster
```

`make e2e-run` rebuilds the `ax` CLI, but not the in-cluster `ax-server` or
`ax-task-runner` images; re-run `make e2e-setup` to redeploy those.

`go test ./...` skips this package unless `-args --e2e` is passed or `AX_E2E=true` is set.

## Configuration

`hack/setup-e2e-kind.sh` reads these environment variables and writes the
resolved values the tests need to `.tmp/e2e.env`.

| Variable | Default | Description |
|---|---|---|
| `SUBSTRATE_REF` | version in `go.mod` | Substrate branch, tag, commit, or Go module version |
| `SUBSTRATE_DIR` | `../substrate` if present | Local Substrate checkout. If its `HEAD` is not `SUBSTRATE_REF`, a detached worktree is created under `.tmp/` |
| `SUBSTRATE_REPO` | `https://github.com/agent-substrate/substrate.git` | Cloned into `.tmp/substrate` when no local checkout exists |
| `KIND_CLUSTER_NAME` | `ax-e2e` | Kind cluster name |
| `KUBECONFIG` | `.tmp/kubeconfig-${KIND_CLUSTER_NAME}` | Kubeconfig for the Kind cluster |
| `AX_SYSTEM_NAMESPACE` | `ax-system` | Namespace for `ax-redis`, `ax-server`, and the `ax-workers` WorkerPool |
| `AX_WORKER_REPLICAS` | `4` | gVisor worker replicas |
| `AX_SNAPSHOTS_BUCKET` | `gs://ate-snapshots/ax/` | Snapshot location in Substrate's in-cluster RustFS |
| `RECREATE_KIND` | `false` | Delete and recreate the Kind cluster |
| `FORCE_INSTALL_SUBSTRATE` | `false` | Reinstall Substrate even if the requested commit is already installed |
| `CLEANUP_KIND` | `false` | `hack/run-e2e-kind.sh` only: delete the cluster on exit |
| `SKIP_SETUP` | `false` | `hack/run-e2e-kind.sh` only: skip provisioning |
| `E2E_TIMEOUT` | `30m` | `go test` timeout |
