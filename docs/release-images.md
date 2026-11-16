# Release images

`.github/workflows/release-images.yml` runs when a `vX.Y.Z[-PRERELEASE]` tag
is pushed. It builds the AX images from that tag and pushes them to
`ghcr.io/google/ax/<image>:<tag>`.

Substrate accepts only digest-pinned images, so copy the digest from the run
summary (or `docker buildx imagetools inspect`) into `spec.image`. Don't use the
tag directly.

| Image | Dockerfile | Platforms | Built |
|---|---|---|---|
| `ax-server` | `Dockerfile.server` | linux/amd64, linux/arm64 | always |
| `ax-task-runner` | `Dockerfile.task-runner` | linux/amd64, linux/arm64 | always |
| `claude-code` | `Dockerfile.claude-code` | linux/amd64 | only when enabled |

If an image already exists for the tag, it isn't rebuilt, so rerunning the
workflow never changes the digests of a published tag. If the registry can't be
checked (an auth, rate-limit, or network error), the run fails rather than
rebuilding. To rebuild an existing tag and replace its images, run the workflow
manually with `overwrite`. The workflow never moves `:latest`.

Only tags that contain this workflow can be built. Earlier tags (v0.3.1 and
before) have Dockerfiles that always copy the amd64 binary, so the workflow
refuses them instead of publishing arm64 images with amd64 code in them. Every
run also checks that each platform's image holds a binary for that platform.

Scheduled (nightly) builds of `main` are deferred until the tag flow has been
validated. See #609 and #610.

## Agent images are opt-in

An agent image (a task image with an agent CLI preinstalled) redistributes that
CLI, so a release publishes one only when it is enabled. To enable agent images,
set the repository variable `AX_RELEASE_AGENT_IMAGES` (Settings → Secrets and
variables → Actions → Variables) to a comma-separated list of names from
`AGENT_CATALOG` in the workflow:

```bash
gh variable set AX_RELEASE_AGENT_IMAGES --repo google/ax --body claude-code
```

If the variable is unset or empty, only the base images are built. A manual run
can override the variable with the `agent_images` input, where `none` builds no
agent images:

```bash
gh workflow run release-images.yml --repo google/ax -f tag=v0.4.0 -f agent_images=claude-code -f dry_run=true
```

Each agent image is built on the `ax-task-runner` image of the same tag
(`--build-arg BASE_IMAGE=<repo>/ax-task-runner@<digest>`), so its runner and
base always come from the same commit.

## Adding an agent image

1. Add a runner under `cmd/runners/<runner>`, as `cmd/runners/claude-code-runner`
   does.
2. Add `Dockerfile.<agent>`. It must accept `ARG BASE_IMAGE` and
   `ARG TARGETARCH`, and copy `bin/linux_${TARGETARCH}/<runner>` to
   `/usr/local/bin/ax-task-runner` (see `Dockerfile.claude-code`).
3. Allow the runner binary in `.dockerignore` (`!bin/linux_amd64/<runner>`), or
   it won't be in the build context.
4. Add `{"name": "<agent>", "dockerfile": "Dockerfile.<agent>", "runner": "<runner>"}`
   to `AGENT_CATALOG` in `.github/workflows/release-images.yml`.
5. Run the workflow manually with `agent_images=<agent>` and `dry_run=true`.
   When that passes, add the name to `AX_RELEASE_AGENT_IMAGES`.

## Dry runs

Pull requests that change the workflow, a `Dockerfile.*`, or `.dockerignore`
do a dry run, and so does a manual run with `dry_run`. A dry run builds every
image it would publish, but pushes them to a registry that exists only inside
the job. Forks can only do dry runs, because only `google/ax` publishes.

A pull request's dry run builds agent images only if they are enabled in
`AX_RELEASE_AGENT_IMAGES`. To check a change to an agent Dockerfile while it is
disabled, run the workflow manually with `agent_images=<agent>` and
`dry_run=true`.

## First publish

GitHub creates a package the first time a workflow pushes to it, and an org
package starts out private. The images' `org.opencontainers.image.source`
label links each new package to `google/ax`. After the first tagged run, an org
owner has to set each new package (`ax-server`, `ax-task-runner`, and any agent
image) to public.

If a package with the same name already exists but isn't linked to `google/ax`
(or doesn't grant this repository's Actions write access), the push fails with
403. Fix that in the package's settings.
