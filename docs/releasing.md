# Releasing AX

Pushing a `vX.Y.Z` or `vX.Y.Z-PRERELEASE` tag publishes a release.
[`.github/workflows/release.yml`](../.github/workflows/release.yml) runs two
jobs:

1. **images** builds `ax-server`, `ax-runner`, and `ax-runner-volume` for
   `linux/amd64` and `linux/arm64` and pushes them as
   `ghcr.io/google/ax/<image>:<tag>`.
2. **release** builds the `ax` CLI, renders the install manifests with
   `ax-server` pinned by digest (and the runner volume, when it is switched
   on), and publishes the GitHub release with them attached. The release notes
   list the digest-pinned `ax-runner` image for `spec.profile.image`.

If the images job fails, no release is created. Every run's image digests are
in its job summary.

This workflow is the release-on-tag stage of the build and release epic
(#609). Record validation runs in #610.

## Cutting a release

1. Check that `main` is green, including the nightly E2E run, that `go.mod`
   pins the Substrate release you want to ship against, and (once
   `agentexecutor/studio` has published releases) that `PINNED_STUDIO_VERSION`
   and `pinned_studio_sha256` in [`install.sh`](../install.sh) match an AX
   Studio release built against this `v1alpha1` API.
2. Tag a commit on `main` (or a `release-*` branch; the workflow refuses any
   other commit) and push the tag:

   ```sh
   git tag -a v0.4.0 -m v0.4.0 <commit>
   git push origin v0.4.0
   ```

3. Watch the **Release** workflow in the Actions tab. When it finishes, the
   release is published.
4. Optionally, edit the release to add a summary above the generated notes and
   to describe breaking changes with their upgrade steps.

Tags with a `-PRERELEASE` suffix (`v0.4.0-rc.1`) are published as
pre-releases. A stable release is marked Latest only if its tag is the highest
stable tag, so a patch to an older line does not take Latest from a newer one.

## What a release contains

| Asset | Contents |
|---|---|
| `ax_<os>_<arch>.tar.gz` | The `ax` CLI for linux and darwin, amd64 and arm64. Static, and `ax version` prints the tag. |
| `checksums.txt` | SHA-256 of every archive, in `sha256sum` format. |
| `install.sh` | The CLI installer from the repository root, when the tag has one, so `releases/latest/download/install.sh` always matches the latest archives. |
| `ax-server.yaml` | [`deploy/ax-server.yaml`](../deploy/ax-server.yaml) with `ax-server` pinned by digest. `AX_RUNNER_VOLUME_IMAGE` is empty (tasks run the runner baked into their image) unless `AX_RELEASE_RUNNER_VOLUME` is `true`. |
| `postgres.yaml` | [`deploy/postgres.yaml`](../deploy/postgres.yaml), unchanged. |

The release notes start with install commands, the Substrate version from
`go.mod`, and the image digests, followed by GitHub's generated notes since the
previous stable tag.

## Images

Images go to `ghcr.io/google/ax/ax-server`, `ghcr.io/google/ax/ax-runner`,
and `ghcr.io/google/ax/ax-runner-volume` (`version.ImageRepo` in
[`internal/version`](../internal/version/version.go)), tagged with the release
tag. The highest stable release also moves `:latest`. The list is
`RELEASE_IMAGES` in [`hack/release/lib.sh`](../hack/release/lib.sh); a new
image is published only once it is added there.

`ax-runner-volume` (`Dockerfile.runner-volume`) holds only the static
`ax-runner` binary. When ax-server's `AX_RUNNER_VOLUME_IMAGE` is set to
it, every task mounts the runner read-only at `/opt/ax` instead of needing it
built into the task image (#620). Release manifests leave it empty unless the
repository variable `AX_RELEASE_RUNNER_VOLUME` is `true` (case-insensitive;
#625). The feature is experimental until #625 is done, and `ax install` does
not enable it by default yet (opt in with `--runner-volume-image` or
`$AX_RUNNER_VOLUME_IMAGE`); only the `ax-server.yaml` release manifest does.
Dry runs render the manifest both ways and check each result.

Before building, the images job refuses Dockerfiles that pin
`ARG TARGETARCH=amd64` (AX before v0.4), which would put amd64 binaries in the
arm64 images. After pushing, it checks that each platform's image holds that
platform's binary.

The binaries are stamped with the tag through
`github.com/google/ax/internal/version.Version`, and the release workflow also
stamps the published `ax-server` digest through
`github.com/google/ax/internal/version.ServerImageDigest` (via `AX_SERVER_IMAGE`
in `hack/release/build-binaries.sh`). Release builds of `ax local` and
`ax install` default `--server-image` to
`version.Image("ax-server") + "@" + version.ServerImageDigest` (falling back to
the tag-only reference resolved at install time when `ServerImageDigest` is
unset); unversioned dev builds still require `--server-image` or
`$AX_SERVER_IMAGE`. `TODO(release)` in `internal/install/axdeploy` remains only
for `DefaultRunnerVolumeImage`.

GitHub creates a new package as private: after the first release that
publishes a new package, an owner of the `google` organization must make it
public and link it to this repository, otherwise clusters cannot pull it
without credentials.

Agent task images (Claude Code, Antigravity, ...) are not built here. Their
builds and releases live in
[agentexecutor/images](https://github.com/agentexecutor/images).

`hack/publish-multiarch-images.sh` pushes images to a personal registry for
development. It is not part of releases.

## Reruns and existing releases

Run the workflow manually from the Actions tab with the tag as input to retry
a failed release. A manual run uses the workflow and scripts of the tag
itself, so it works only for tags that contain this workflow, not for v0.3.1
and earlier.

- An image that already exists for the tag on both platforms is never rebuilt;
  its digest is reused. Only missing or incomplete images are built.
- A registry error while checking (auth, rate limit, network) fails the run
  rather than rebuilding. ghcr.io answers "denied" for a package that was
  never pushed, so that case is confirmed through the packages API.
- Once the release is published, nothing is built: if an image is missing, the
  run fails. `overwrite` (rebuild every image) is refused too.
- A **draft** release (for example, left by a failed run) gets this run's
  assets and notes, replacing its own, and is published.
- A **published** release keeps its notes and assets; only assets it lacks are
  uploaded.
- `:latest` is moved only after refetching tags, so an older release that
  finishes late does not take it back from a newer one.

Only `google/ax` publishes; in a fork, only dry runs do anything.

## Dry runs

Pull requests that change the workflow, `hack/release`, a `Dockerfile.*`,
`.dockerignore`, `deploy/`, `install.sh`, `go.mod`/`go.sum`, the three
binaries under `cmd/`, or `internal/version` do a dry run, and so does a manual
run with `dry_run`. A dry run builds the images into a registry that exists
only inside the job, then runs the image step again to check that it reuses
them with the same digests. It also builds the CLI and renders the manifests,
but publishes nothing.

A dry run never moves `:latest` (pull requests use a prerelease version), and
it cannot exercise the ghcr.io "denied" case.

## Before the first release

- A tag ruleset (#546) and, ideally, a `release` environment with required
  reviewers, so only release managers can trigger a publish. Until then, any
  writer can push a `v*` tag on `main`.
- Cut `vX.Y.Z-rc.1` first and validate it against #610.

## Building release artifacts by hand

Each workflow step is a script in [`hack/release`](../hack/release) that also
runs locally from a clean checkout of the tag:

```sh
hack/release/build-binaries.sh v0.4.0                 # ax CLI archives -> bin/release
AX_IMAGE_REPO=registry.example.com/ax \
  hack/release/publish-images.sh v0.4.0               # push images, write bin/release/images.env
hack/release/render-manifests.sh                      # bin/release/{ax-server,postgres}.yaml
hack/release/create-release.sh v0.4.0                 # publish the GitHub release (needs gh)
```

`publish-images.sh` needs `docker buildx` with a builder that can push
multi-platform images (`docker buildx create --driver docker-container --use`),
QEMU for the non-native platform, `jq`, and `file`. `build-binaries.sh` needs
GNU tar and coreutils (on macOS, `gtar` and `coreutils` from Homebrew).
