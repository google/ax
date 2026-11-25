# AX — Agent contract

The one contract for every coding agent in this repository. There are no
per-agent copies such as `CLAUDE.md` or `GEMINI.md`.

This file holds only what every task needs: layout, the CI gate, hard rules,
and an index of skills. Procedures, background, and troubleshooting live in
`skills/<name>/SKILL.md` — load the relevant one **before** editing.

## What this is

Declarative, Kubernetes-shaped orchestrator for sandboxed agent tasks on
[Agent Substrate](https://github.com/agent-substrate/substrate). Four
primitives — `Task`, `Workspace`, `Model`, `Gateway` (`ax.io/v1alpha1`) — and a
`kubectl`-like CLI. `DESIGN.md` explains why; read it before touching the
task manager or server.

## Layout

```
client                 Public Go client, alpha: ServerURL, Dial, CurrentContext, ListContexts over internal/tunnel. It never downloads kubectl (ErrKubectlNotFound): internal/tunnel does not import internal/install, and only cmd/ax passes a tunnel.KubectlResolver that downloads
cmd/ax                 CLI (apply/get/describe/ssh/suspend/resume/promote/delete/local/install/uninstall/studio/ctx/tunnel)
cmd/ax-server          Control plane: gRPC API; provisions, suspends, resumes and deletes task actors directly
cmd/ax-runner          PID 1 inside the sandbox; materializes the Workspace, runs the agent
deploy/                Canonical postgres.yaml + ax-server.yaml manifests and //go:embed renderers
internal/apivalidation Manifest admission validation (common, task, workspace, model, gateway); zz_generated.validation.go holds the rules declared as +k8s: tags in the .proto files (generator settings in doc.go); server-side only
internal/controller    TaskManager + WorkspaceManager: Substrate side of task lifecycle and workspace snapshot tags; task_test.go is the test template
internal/install/      Self-contained local cluster (Kind), Substrate bootstrap, and AX installer/uninstaller
internal/resources     Kubernetes CPU/memory resource quantity parsing
internal/server        gRPC handlers
internal/store/        memory + postgres backends (schema.sql and schema.go change together)
internal/substrate     The only place that talks to the Substrate Control API
internal/workspace     spec.setup execution (git clone / inline files / skills dir / commands; MCP is read via metadata server)
internal/{tunnel,guest,metadata}  k8s port-forward, in-sandbox guest client, metadata server
internal/telemetry     ax-server's OpenTelemetry setup: OTLP traces, metrics, and events (Emit, stdout fallback), Prometheus /metrics (:9090), JSON slog with trace IDs
internal/telemetry/otelenv  OpenTelemetry rules ax-server and the CLI share: OTEL_*_EXPORTER parsing and the resource
internal/version       Release version stamped by hack/release (`ax version` of any other build prints `dev+<commit>`), and the matching ghcr.io/google/ax image refs
internal/axattr        OpenTelemetry attribute names (ax.task.*, and Substrate's ate.* for actor identity)
pkg/apis/v1alpha1      *.proto (source of truth) -> *.pb.go (generated), types.go (constants + YAML codec)
test/e2e               Kind-based; compiled by `go test ./...` but self-skips without AX_E2E=true
install.sh             One-line CLI installer (`releases/latest/download/install.sh`)
.github/rulesets/      GitHub rulesets as code (source of truth); applied by an admin with hack/apply-rulesets.sh (docs/rulesets.md)
hack/                  E2E provisioning, the local install smoke test (test-local-install.sh), `make build-local-cluster` (build-local-cluster.sh: images from the checkout into the localhost registry, then `ax local start`, keeping the cluster), tour.sh (a basic tour of features against the current context), Substrate manifest sync, and multi-arch image scripts; apply-rulesets.sh; tools/<name>/go.mod pins buf and code-generator's validation-gen (`make generate`; validation-gen at the Kubernetes release of k8s.io/apimachinery in go.mod) and golangci-lint (`make verify`); boilerplate/go.txt is the license header of generated Go code
hack/release/          Release scripts run by .github/workflows/release.yml on a vX.Y.Z tag (docs/releasing.md)
docs/                  manifests.md is the user-facing API reference; local_installer.md walks through ax local / ax install / ax promote / ax studio
skills/                Developer skills (SKILL.md format). .agents/skills and .claude/skills symlink here.
```

## Build / test (verified; this is what CI runs)

```bash
export GOTOOLCHAIN=auto          # go.mod wants 1.27.1; older local Go auto-downloads it
make verify                      # make generate, go mod tidy, go fmt, golangci-lint (CI asserts no diff)
go build ./cmd/ax ./cmd/ax-server ./cmd/ax-runner
go test ./...                    # ~25 s, 27 packages with tests; Postgres + E2E tiers self-skip
```

Opt-in tiers:
- **Postgres store**: set `AX_TEST_POSTGRES_URL='postgres://ax:ax@localhost:5432/ax?sslmode=disable'` (CI runs `postgres:17-alpine`) and run `go test -count=1 ./internal/store/postgres/...`.
- **E2E**: `make test-e2e` (or `make test-e2e E2E_ARGS='-test.run TestE2E_ModelCRUD'`; see `test/e2e/README.md`).
- **Local install**: `make test-local-install` (`hack/test-local-install.sh`) is the only test that runs `ax local start` on a real cluster; E2E installs AX with Substrate's own tools. It builds the images into a registry on `localhost`, starts a cluster `ax-smoke` with its own kubeconfig and AX home below `.tmp`, runs a task, and deletes the cluster. CI runs it as job `local-install` in `e2e.yml`.

## Rules

Each rule below is enforced by code, CI, or `CONTRIBUTING.md`; the cited
location is where to look. How-to lives in the skills.

**A. Correctness**
- `pkg/apis/v1alpha1/*.proto` are the source of truth: `ax.proto` is the `AX`
  service, and the messages are in `common.proto`, `task.proto`,
  `workspace.proto`, `model.proto`, and `gateway.proto`. `*.pb.go` and
  `internal/apivalidation/zz_generated.validation.go` are
  generated (`DO NOT EDIT` headers); run `make generate` after editing a
  `.proto`. Imports are repo-root-relative, except Agent Substrate's
  `api/ateapi/v1/ateapi.proto` (`make generate` links that module from the Go
  module cache into `.tmp/buf/substrate` for `buf.yaml`).
  `.tmp/substrate` is the E2E scripts' Substrate clone.
- `internal/store/postgres/schema.go` loads `schema.sql`; change them together.

**B. Agent Substrate** (details: `/substrate`)
- `internal/substrate/client.go` is the only non-test file that constructs
  `ateapipb.NewControlClient`; new Control API calls go there.
- `api.ate-system.svc`, `atenet-router.ate-system.svc`, and namespace
  `ate-system` are hard-coded across 16 files (list in `/substrate`).
- Substrate rejects tag-only images; `profile.image` must be
  digest-pinned (`docs/manifests.md`, `TemplateCreationFailed`).
- Guest services (`ax ssh`) are served only when `spec.debug` is true
  (`internal/metadata/server.go`, `cmd/ax/main.go`).
- Unit tests run against in-process fakes: `mockControlServer`
  (`internal/controller/task_test.go`) and `internal/substrate/substratetest`
  (`internal/server/server_test.go`, `cmd/ax/main_test.go`).
- `go.mod` has no `replace` directive; E2E installs Substrate at the `go.mod`
  version (`hack/setup-e2e-kind.sh`).

**C. API surface**
- A `Task`/`Workspace`/`Model`/`Gateway` field is defined in that kind's
  `.proto` file (shared messages in `common.proto`), mapped in `types.go`,
  validated by `+k8s:` tags in its `.proto` comment or in
  `internal/apivalidation`, and documented in `docs/manifests.md`.

**D. Hygiene**
- CI runs `make verify` and fails if it changes a file (stale generated code,
  untidy `go.mod`/`go.sum`, unformatted Go) or golangci-lint reports a finding
  (`.github/workflows/go.yml`, `.golangci.yaml`).
- Every checked-in `.go`, `.sh`, `Makefile`, and `Dockerfile` carries the
  Apache-2.0 header (copy from `Makefile`).

**E. Process** (`CONTRIBUTING.md`, `.github/rulesets/`)
- One approving review + green `build-and-test` to merge; a push after
  approval needs a fresh approval. Assign yourself to the issue or PR you pick up.
- Rulesets are edited only through `.github/rulesets/*.json`, never in the
  GitHub UI alone; `.github/workflows/rulesets.yml` fails daily on drift.

## Skills — load before coding

| Area | Skill | When to load |
|---|---|---|
| Substrate | `/substrate` | Touching `internal/substrate`, `internal/install`, `internal/guest`, `internal/metadata`; bumping Substrate; any `ate-system` address; triaging E2E |
| Contract sync | `/sync-contract` | After any code change, before declaring done: verify `AGENTS.md` and `skills/` still match the code |

Conventions: one directory per skill, `skills/<name>/SKILL.md`, frontmatter
`name` + `description` (one sentence what, one sentence when — it is routing
text, not documentation; long bodies split into `references/`). Agent-local
roots (`.agents/`, `.claude/`) only symlink to `skills/`; never put
skill source there. Adding a skill means adding a row above.

## Definition of done

1. The three CI commands above pass locally.
2. Postgres, E2E or local install tier passes if you touched
   `internal/store/postgres`, the E2E paths, or `internal/install`.
3. `/sync-contract` has been run against your diff so `AGENTS.md` and `skills/`
   stay in sync with the code.
