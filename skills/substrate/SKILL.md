---
name: substrate
description: >
  How AX depends on Agent Substrate (modules, address contract, mocks, version bumps, E2E pin).
  Load when touching internal/substrate, bumping Substrate, or triaging make test-e2e.
---

# Agent Substrate in AX

AX schedules every `Task` as a sandboxed **actor** on
[Agent Substrate](https://github.com/agent-substrate/substrate). AX owns the
declarative layer (Task/Workspace/Model, reconciler, CLI); Substrate owns
isolation, checkpoint/restore, worker placement and in-cluster routing.
Facts below were verified against `main` on 2026-11-06 — re-check `go.mod`
and `hack/setup-e2e-kind.sh` if they look stale.

## 1. Module map

| Module | Pinned | Provides | Imported by |
|---|---|---|---|
| `go.ate.dev/substrate` | `v1.0.0` | Control API protos `api/ateapi/v1` (atespaces, actor templates, actors, suspend/resume, egress rules) | `internal/substrate`, `internal/server`, `internal/controller`, `internal/apivalidation`, `test/e2e`, `pkg/apis/v1alpha1` (its `.proto` files import `ateapi.proto`) |
| `github.com/agent-substrate/env` | `v0.0.11-0.20260912052224-4468a200b170` (pseudo-version) | Guest services served inside the sandbox (process exec + file system gRPC) and `proto/ateenv/v1alpha` | `internal/guest`, `internal/metadata` |

There is no `replace` directive for either module. Local hacking against a
Substrate checkout goes through `SUBSTRATE_DIR` in E2E (section 5).

## 2. The Control API wrapper: `internal/substrate.Client`

All Control API traffic goes through this package. It encapsulates:

- **Endpoint defaults** — `api.ate-system.svc.cluster.local:443`; authority
  and TLS `ServerName` default to `api.ate-system.svc` when the target
  contains that string.
- **Auth** — bearer token from `SUBSTRATE_TOKEN_FILE` (in-cluster: projected SA
  token with audience `api.ate-system.svc`, see `deploy/ax-server.yaml`), CA
  from `SUBSTRATE_CA_FILE`.
- **Retry** — `IsTransientError` + `retryOnCodes` for `Unavailable`-class errors.
- **Helpers** the reconciler relies on — `EnsureAtespace`, `EnsureActorTemplateWithImage`,
  `EnsureActor`, `SuspendActor`, `ResumeActor`, `DeleteActor`, `WorkerIP`,
  `ResourceLimits`, `BuildActorTemplate`.

### Where a new Control API call lives today

- `internal/substrate/client.go` is the only non-test file that calls
  `ateapipb.NewControlClient`; every existing RPC is a method on `*Client`,
  wrapped in `retryOnCodes(..., codes.Unavailable)` where idempotent.
- Each method has a table-driven case in `client_test.go` (`mockControlClient`).
- Callers in `internal/controller` exercise it through `mockControlServer`;
  `internal/server` and `cmd/ax` tests use `internal/substrate/substratetest`
  (section 4).

## 3. Address contract

| Value | Occurrences |
|---|---|
| `api.ate-system.svc.cluster.local:443` + authority `api.ate-system.svc` | `cmd/ax-server/main.go` (`--substrate-endpoint` / `--substrate-authority` flag defaults), `internal/substrate/client.go` (3×), `deploy/ax-server.yaml`, `test/e2e/manifests/ax-server.yaml`, `test/e2e/harness_test.go` |
| `atenet-router.ate-system.svc[.cluster.local]` | `internal/controller/reconciler.go` (`defaultRouterServerName`), `internal/tunnel/router_tls.go` (`routerServerName`), `cmd/ax/main.go` (ssh port-forward path), `internal/guest/client.go` (doc comment), `deploy/ax-server.yaml`, `test/e2e/manifests/ax-server.yaml`, `docs/networking.md` |
| namespace `ate-system` | all of the above + `test/e2e/harness_test.go` (`ateSystemNamespace`) |
| `opentelemetry-collector.otel-system.svc:4317` (the collector Substrate's kind overlay deploys, `manifests/ate-install/kind/ate-otel-config.yaml` in Substrate) | `test/e2e/manifests/ax-server.yaml` (`ax-otel-config` ConfigMap). An unreachable address does not fail E2E; ax-server only logs OTLP export errors |

Why it matters: tasks never get a Service/Ingress. Every request to a task is
proxied by the **atenet router**, which picks the target from the
`ate-target-actor` header and resumes a suspended actor on demand
(`docs/networking.md`). `ax ssh` port-forwards to `svc/atenet-router` on 443
(mTLS) and falls back to 80.

## 4. Testing against Substrate

### Unit tests: in-process mock, no cluster

```go
type mockControlServer struct {
    ateapipb.UnimplementedControlServer
    // record calls / inject failures here
}
grpcServer := grpc.NewServer()
ateapipb.RegisterControlServer(grpcServer, mockSrv)
// serve on a listener, point substrate.NewClient at it
```

Canonical examples: `internal/controller/reconciler_test.go` (reconciler state
transitions; the largest test file in the repo). `internal/server/server_test.go`
and `cmd/ax/main_test.go` run a real `TaskReconciler` against
`internal/substrate/substratetest`, an in-memory fake of the same API.
`internal/substrate/client_test.go` uses a client-side `mockControlClient`
instead. No mocking library is in `go.mod`.

### Substrate behaviour the code already depends on

- Actor template images must be digest-pinned (`@sha256:…`); a tag-only
  `spec.image` fails at ActorTemplate creation with reason
  `TemplateCreationFailed` (`docs/manifests.md`). `make images` writes
  `bin/ax-server.digest` / `bin/ax-task-runner.digest` for this reason.
- After `ResumeActor` the actor may be on a different worker, so AX never
  stores a worker IP: the server reads it from the actor on every get and list
  (`substrate.WorkerIP` in `internal/server/server.go`), and the reconciler
  uses the IP `ResumeActor` returns only for its readiness probe
  (`internal/controller/reconciler.go`).
- Guest services (process + fs) are mounted by `internal/metadata/server.go`
  only when `task.Spec.Debug` is true; `ax ssh` (`cmd/ax/main.go`) refuses to
  connect otherwise.
- Readiness: Substrate and the reconciler probe the runner on port 80
  (`/healthz`, `/readyz?check=workspace`, served by `internal/metadata`);
  `/readyz` is 503 until all workspaces are materialized (`docs/sandbox.md`).
- Snapshot storage: Actor templates created by AX leave `SnapshotConfig.StorageLocation`
  empty by default so Substrate uses its cluster-wide `--snapshot-location`. Specifying a custom
  snapshot bucket (`snapshotsBucket` in `BuildActorTemplate`) can only be used when the
  Substrate cluster has the `PerTemplateSnapshotLocation` preview feature gate enabled;
  otherwise Substrate rejects template creation with
  `actor_template.snapshot_config.storage_location: Forbidden`. Since v1.0.0 each path
  segment may contain only `[A-Za-z0-9_-]` and no segment may be `atespaces`; AX passes
  the value through unvalidated, so Substrate is the one that rejects it.

## 5. E2E and the Substrate version pin

`test/e2e` is compiled by `go test ./...` but **self-skips** unless
`AX_E2E=true` or `-args --e2e`. The real runs go through `make`:

| Target | Substrate version | When |
|---|---|---|
| `make test-e2e` | exactly `go list -m -f '{{.Version}}' go.ate.dev/substrate` | PR CI and the nightly schedule (06:00 UTC) in `e2e.yml` |
| `make test-e2e SUBSTRATE_REF=<release tag>` | override | local, or `workflow_dispatch` input `substrate_ref` in `e2e.yml` |
| `make e2e-setup` / `e2e-run` / `e2e-cleanup` | same as above | iterate without re-provisioning |

E2E never builds Substrate. `hack/setup-e2e-kind.sh` installs the release's
published images from `ghcr.io/agent-substrate/substrate/<image>:<tag>`
(`SUBSTRATE_IMAGE_REPO` to use a mirror) via `ATE_IMAGE_REPO`/`ATE_IMAGE_TAG`,
and pins the WorkerPool's worker image by digest. It exits early unless:

- `SUBSTRATE_REF` is a release tag, `vX.Y.Z` or `vX.Y.Z-rc.N`. Branches,
  commits, and Go pseudo-versions are rejected, and there is no run against
  Substrate `main`.
- The Substrate checkout is clean and at that tag. It still supplies the
  manifests and `ate-setup`, so they must match the images.
- That release's `ate-setup` supports `--image-repo`, and every image the
  install uses exists under the tag.

Useful env (full list in `test/e2e/README.md`): `SUBSTRATE_DIR` (local
checkout; a detached worktree is created under `.tmp/` if HEAD ≠ ref),
`SUBSTRATE_REPO`, `E2E_ARGS='-test.run TestE2E_ModelCRUD'`.

## 6. Bumping Substrate

What the repo mechanically requires when `go.ate.dev/substrate` or
`github.com/agent-substrate/env` changes in `go.mod`:

```bash
go get go.ate.dev/substrate@vX.Y.Z            # and/or github.com/agent-substrate/env@<sha>
go mod tidy                                    # CI diffs go.mod/go.sum
make generate                                  # buf reads ateapi.proto from the new version
go build ./... && go test ./...                # unit tests compile against the new ateapipb
make test-e2e                                  # hack/setup-e2e-kind.sh installs the go.mod version
```

`go.ate.dev/substrate` must stay on a release tag: E2E installs that
release's published images, and a pseudo-version fails E2E setup.

`Gateway.spec.egress` is Substrate's `EgressRule`, so a bump can change what
users may write in a Gateway. Diff the egress messages in
`api/ateapi/v1/ateapi.proto` between the two versions. If the new version adds
a rule kind besides `http`, `https`, and `tlsPassthrough`, `ValidateGateway`
(`internal/apivalidation/gateway.go`) rejects rules of that kind until its
checks cover it. `docs/manifests.md` links to Substrate's egress credential
injection guide at the pinned version.
