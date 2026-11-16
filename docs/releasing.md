# Releasing AX

Pushing a `vX.Y.Z` or `vX.Y.Z-PRERELEASE` tag publishes a release.
[`.github/workflows/release.yml`](../.github/workflows/release.yml) runs two
jobs:

1. **images** builds `ax-server` and `ax-task-runner` for `linux/amd64` and
   `linux/arm64` and pushes them as `ghcr.io/google/ax/<image>:<tag>`.
2. **release** builds the `ax` CLI, renders the install manifests with both
   images pinned by digest, and publishes the GitHub release with them
   attached.

If the images job fails, no release is created. Every run's image digests are
in its job summary.

This workflow is the release-on-tag stage of the build and release epic
(#609). Record validation runs in #610.

## Cutting a release

1. Check that `main` is green, including the nightly E2E run, and that
   `go.mod` pins the Substrate release you want to ship against.
2. Tag the commit and push the tag:

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
| `ax-server.yaml` | [`deploy/ax-server.yaml`](../deploy/ax-server.yaml) with `ax-server` and the default task image pinned by digest. |
| `postgres.yaml` | [`deploy/postgres.yaml`](../deploy/postgres.yaml), unchanged. |

The release notes start with install commands, the Substrate version from
`go.mod`, and the image digests, followed by GitHub's generated notes since the
previous stable tag.

## Images

Images go to `ghcr.io/google/ax/ax-server` and
`ghcr.io/google/ax/ax-task-runner` (`version.ImageRepo` in
[`internal/version`](../internal/version/version.go)), tagged with the release
tag. The highest stable release also moves `:latest`.

Before building, the images job refuses Dockerfiles that pin
`ARG TARGETARCH=amd64` (AX before v0.4), which would put amd64 binaries in the
arm64 images. After pushing, it checks that each platform's image holds that
platform's binary.

The binaries are stamped with the tag through
`github.com/google/ax/internal/version.Version`. `version.Image("ax-server")`
returns the image of the binary's own release (`:latest` for a dev build), for
`ax install` defaults once a release is published (see the `TODO(release)` in
`internal/install/axdeploy`).

GitHub creates a new package as private:
after the first release, an owner of the `google` organization must make both
packages public and link them to this repository, otherwise clusters cannot
pull them without credentials.

Agent task images (Claude Code, Antigravity, ...) are not built here. Their
builds and releases live in
[agentexecutor/images](https://github.com/agentexecutor/images).

`hack/publish-multiarch-images.sh` pushes images to a personal registry for
development. It is not part of releases.

## Reruns and existing releases

Run the workflow manually from the Actions tab with the tag as input to
retry a failed release or to attach assets to a release created by hand.

- If every image already exists for the tag on both platforms, nothing is
  rebuilt and the existing digests are used, so a rerun never changes the
  digests a release points at. An incomplete set is rebuilt in full.
- `overwrite` rebuilds and replaces a complete set, but is refused once the
  release is published.
- If a release already exists for the tag, its notes are left alone and only
  the assets it lacks are uploaded. An attached asset is never replaced.

Only `google/ax` publishes; in a fork, only dry runs do anything.

## Dry runs

Pull requests that change the workflow, `hack/release`, a `Dockerfile.*`,
`.dockerignore`, or `deploy/` do a dry run, and so does a manual run with
`dry_run`. A dry run builds the images into a registry that exists only inside
the job, builds the CLI, and renders the manifests, but publishes nothing.

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
multi-platform images (`docker buildx create --driver docker-container --use`)
QEMU for the non-native platform, and `file`.
