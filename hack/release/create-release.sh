#!/usr/bin/env bash

# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Publishes the GitHub release for an existing tag with its assets.
#
#   hack/release/create-release.sh <tag> [asset-dir]
#
# Uploads every file in asset-dir (default bin/release) except images.env.
#
# If there is no release for <tag>, creates and publishes one with GitHub's
# generated notes. A -PRERELEASE tag is published as a pre-release, and a
# release is marked Latest only if <tag> is the highest stable tag, so a patch
# to an older line does not take Latest from a newer one.
#
# If a release for <tag> already exists, its notes and state are left alone and
# only the assets it lacks are uploaded; an existing asset is never replaced.
#
# Needs git (with tags fetched), jq, and gh authenticated with contents: write.
# GITHUB_REPOSITORY selects the repository; it defaults to google/ax.

set -o errexit -o nounset -o pipefail

ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=hack/release/lib.sh
source "${ROOT}/hack/release/lib.sh"

TAG="${1:-}"
ASSETS_DIR="${2:-${ROOT}/bin/release}"
REPO="${GITHUB_REPOSITORY:-google/ax}"
require_release_version "${TAG}"

if ! git rev-parse -q --verify "refs/tags/${TAG}" >/dev/null; then
  echo "error: tag ${TAG} does not exist in this clone" >&2
  exit 1
fi

assets=()
while IFS= read -r -d '' f; do
  assets+=("${f}")
done < <(find "${ASSETS_DIR}" -maxdepth 1 -type f ! -name images.env -print0 | sort -z)
if [[ ${#assets[@]} -eq 0 ]]; then
  echo "error: no assets in ${ASSETS_DIR}" >&2
  exit 1
fi

if existing="$(gh release view "${TAG}" --repo "${REPO}" --json assets 2>/dev/null)"; then
  echo "==> Release ${TAG} exists; uploading only missing assets."
  missing=()
  for f in "${assets[@]}"; do
    name="$(basename "${f}")"
    if jq -e --arg n "${name}" '.assets | any(.name == $n)' <<<"${existing}" >/dev/null; then
      echo "    keep ${name} (already attached)"
    else
      missing+=("${f}")
    fi
  done
  if [[ ${#missing[@]} -gt 0 ]]; then
    gh release upload "${TAG}" --repo "${REPO}" "${missing[@]}"
  fi
  exit 0
fi

flags=(--repo "${REPO}" --verify-tag --title "${TAG}" --generate-notes)
if is_prerelease "${TAG}"; then
  flags+=(--prerelease --latest=false)
elif is_latest_stable "${TAG}"; then
  flags+=(--latest)
else
  flags+=(--latest=false)
fi

# Start notes at the previous stable tag, so a release's notes cover every
# change since the last release rather than since the last pre-release.
prev="$(git tag --list 'v*' --sort=-v:refname --merged "${TAG}^{commit}" \
  | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | grep -vx "${TAG}" | head -n1 || true)"
if [[ -n "${prev}" ]]; then
  flags+=(--notes-start-tag "${prev}")
fi

# Install notes go above GitHub's generated notes (gh appends those after
# --notes-file).
# shellcheck disable=SC1091
source "${ASSETS_DIR}/images.env"
substrate="$(awk '$1 == "go.ate.dev/substrate" { print $2 }' "${ROOT}/go.mod")"
download="https://github.com/${REPO}/releases/download/${TAG}"
notes="$(mktemp)"
trap 'rm -f "${notes}"' EXIT
if [[ -f "${ASSETS_DIR}/install.sh" ]]; then
  cli_install="\`\`\`sh
curl -fsSL ${download}/install.sh | AX_VERSION=${TAG} sh
\`\`\`

or download \`ax_<os>_<arch>.tar.gz\` and check it against \`checksums.txt\`."
else
  cli_install="Download \`ax_<os>_<arch>.tar.gz\`, check it against \`checksums.txt\`, and put \`ax\` on your PATH."
fi
cat > "${notes}" <<EOF
## Install

Requires [Agent Substrate](https://github.com/agent-substrate/substrate) \`${substrate}\`; see [docs/installation.md](https://github.com/${REPO}/blob/${TAG}/docs/installation.md).

\`\`\`sh
kubectl create namespace ax-system
kubectl -n ax-system create secret generic ax-postgres --from-literal=password="\$(openssl rand -hex 24)"
kubectl apply -f ${download}/postgres.yaml
kubectl apply -f ${download}/ax-server.yaml
\`\`\`

The \`ax\` CLI is attached for linux and darwin (amd64, arm64).

${cli_install}

| Image | Reference |
|---|---|
| ax-server | \`${AX_SERVER_IMAGE}\` |
| ax-task-runner (default task image) | \`${AX_DEFAULT_TASK_IMAGE}\` |

EOF

echo "==> Publishing release ${TAG} (${flags[*]})"
gh release create "${TAG}" "${flags[@]}" --notes-file "${notes}" "${assets[@]}"
