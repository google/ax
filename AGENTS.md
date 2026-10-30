# AX — Agent contract

The one contract for every coding agent in this repository. `CLAUDE.md` and
`GEMINI.md` are symlinks to this file.

This file holds only what every task needs: layout, the CI gate, hard rules,
and an index of skills. Procedures, background, and troubleshooting live in
`skills/<name>/SKILL.md` — load the relevant one **before** editing.

## What this is

Declarative, Kubernetes-shaped orchestrator for sandboxed agent tasks on
[Agent Substrate](https://github.com/agent-substrate/substrate). Three
primitives — `Task`, `Workspace`, `Model` (`ax.io/v1alpha1`) — and a
`kubectl`-like CLI. `DESIGN.md` explains why; read it before touching the
reconciler or server.

## Layout

```
cmd/ax                 CLI (apply/get/describe/ssh/suspend/resume/delete/ctx/tunnel)
cmd/ax-server          Control plane: gRPC API; provisions, suspends, resumes and deletes task actors directly
cmd/ax-task-runner     PID 1 inside the sandbox; materializes the Workspace, runs the agent
internal/controller    TaskReconciler: Substrate side of task suspend/resume/delete; reconciler_test.go is the test template
internal/server        gRPC handlers
internal/store/        memory + postgres backends (schema.sql and schema.go change together)
internal/substrate     The only place that talks to the Substrate Control API
internal/workspace     git clone / inline files / skills dir (MCP is spec-only; agents read it via the metadata server)
internal/{tunnel,guest,metadata}  k8s port-forward, in-sandbox guest client, metadata server
pkg/apis/v1alpha1      ax.proto (source of truth) -> *.pb.go (generated), types.go, resources.go
test/e2e               Kind-based; compiled by `go test ./...` but self-skips without AX_E2E=true
hack/                  E2E provisioning scripts
docs/                  manifests.md is the user-facing API reference
skills/                Developer skills (SKILL.md format). .agents/skills, .claude/skills, and .gemini/skills symlink here.
```

## Build / test (verified; this is what CI runs)

```bash
export GOTOOLCHAIN=auto          # go.mod wants 1.27.1; older local Go auto-downloads it
go mod tidy && git diff --exit-code go.mod go.sum
go build ./cmd/ax ./cmd/ax-server ./cmd/ax-task-runner
go test ./...                    # ~20 s, 16 packages with tests; Postgres + E2E tiers self-skip
go vet ./... && gofmt -l .       # not run by CI; both are clean at HEAD
```

Opt-in tiers:
- **Postgres store**: set `AX_TEST_POSTGRES_URL='postgres://ax:ax@localhost:5432/ax?sslmode=disable'` (CI runs `postgres:17-alpine`) and run `go test -count=1 ./internal/store/postgres/...`.
- **E2E**: `make test-e2e` (or `make test-e2e E2E_ARGS='-test.run TestE2E_ModelCRUD'`; see `test/e2e/README.md`).

## Rules

Each rule below is enforced by code, CI, or `CONTRIBUTING.md`; the cited
location is where to look. How-to lives in the skills.

**A. Correctness**
- `ax.proto` is the source of truth; `*.pb.go` are generated (`DO NOT EDIT`
  headers). There is no `make generate` target.
- `internal/store/postgres/schema.go` loads `schema.sql`; change them together.

**B. Agent Substrate** (details: `/substrate`)
- `internal/substrate/client.go` is the only non-test file that constructs
  `ateapipb.NewControlClient`; new Control API calls go there.
- `api.ate-system.svc`, `atenet-router.ate-system.svc`, and namespace
  `ate-system` are hard-coded across 10 files (list in `/substrate`).
- Substrate rejects tag-only images; `spec.image` must be digest-pinned
  (`docs/manifests.md`, `TemplateCreationFailed`).
- Guest services (`ax ssh`) are served only when `spec.debug` is true
  (`internal/metadata/server.go`, `cmd/ax/main.go`).
- Unit tests run against in-process fakes: `mockControlServer`
  (`internal/controller/reconciler_test.go`) and `internal/substrate/substratetest`
  (`internal/server/server_test.go`, `cmd/ax/main_test.go`).
- `go.mod` has no `replace` directive; E2E installs Substrate at the `go.mod`
  version (`hack/setup-e2e-kind.sh`).

**C. API surface**
- A `Task`/`Workspace`/`Model` field is defined in `ax.proto`, mapped in
  `types.go`, validated in `resources.go`, and documented in `docs/manifests.md`.

**D. Hygiene**
- CI fails on an untidy `go.mod`/`go.sum` (`.github/workflows/go.yml`).
- Every checked-in `.go`, `.sh`, `Makefile`, and `Dockerfile` carries the
  Apache-2.0 header (copy from `Makefile`).

**E. Process** (`CONTRIBUTING.md`)
- One approving review + green `build-and-test` to merge; a push after
  approval needs a fresh approval. Assign yourself to the issue or PR you pick up.

## Skills — load before coding

| Area | Skill | When to load |
|---|---|---|
| Substrate | `/substrate` | Touching `internal/substrate`, `internal/guest`, `internal/metadata`; bumping Substrate; any `ate-system` address; triaging E2E |
| Contract sync | `/sync-contract` | After any code change, before declaring done: verify `AGENTS.md` and `skills/` still match the code |

Conventions: one directory per skill, `skills/<name>/SKILL.md`, frontmatter
`name` + `description` (one sentence what, one sentence when — it is routing
text, not documentation; long bodies split into `references/`). Agent-local
roots (`.agents/`, `.claude/`, `.gemini/`) only symlink to `skills/`; never put
skill source there. Adding a skill means adding a row above.

## Definition of done

1. The four CI commands above pass locally.
2. Postgres or E2E tier passes if you touched `internal/store/postgres` or
   the E2E paths.
3. `/sync-contract` has been run against your diff so `AGENTS.md` and `skills/`
   stay in sync with the code.
