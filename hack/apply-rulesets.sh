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

# Keeps the repository rulesets on GitHub in sync with .github/rulesets/*.json.
# See docs/rulesets.md.
#
# Usage:
#   hack/apply-rulesets.sh             Create or update the live rulesets from the files (repo admin).
#   hack/apply-rulesets.sh --dry-run   Show what would change, without changing anything.
#   hack/apply-rulesets.sh --check     Like --dry-run, but exit 1 if anything differs (CI drift check).
#   hack/apply-rulesets.sh --export    Write the live rulesets to the files.
#
# Rulesets are matched by name. Live rulesets that have no file are listed,
# never deleted. Only repository-level rulesets are managed; rulesets
# inherited from the organization are ignored.
#
# Environment:
#   REPO           Repository to manage (default: google/ax).
#   RULESETS_DIR   Directory with the ruleset files (default: .github/rulesets).
#
# Requires an authenticated gh and jq. Only admins can see bypass_actors; for
# anyone else, GitHub returns null and bypass_actors are left out of the
# comparison.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO="${REPO:-google/ax}"
RULESETS_DIR="${RULESETS_DIR:-${ROOT}/.github/rulesets}"

usage() {
  sed -n '/^# Usage:/,/^# Requires/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

mode=apply
case "${1:-}" in
  "") ;;
  --dry-run) mode=dry-run ;;
  --check) mode=check ;;
  --export) mode=export ;;
  -h | --help)
    usage
    exit 0
    ;;
  *)
    usage >&2
    exit 2
    ;;
esac

for tool in gh jq; do
  command -v "${tool}" >/dev/null || {
    echo "error: ${tool} is required" >&2
    exit 1
  }
done

# normalize prints only the policy fields of a ruleset, with rules sorted by
# type and keys sorted, so files and live rulesets compare byte for byte.
# Pass "true" to drop bypass_actors from the output.
normalize() {
  jq -S --argjson drop_bypass "$1" '
    {
      name,
      target,
      enforcement,
      conditions,
      rules: ((.rules // []) | sort_by(.type)),
      bypass_actors: (.bypass_actors // [])
    }
    | if $drop_bypass then del(.bypass_actors) else . end'
}

# slug turns a ruleset name into a file name: "protect-main" -> protect-main.json.
slug() {
  tr '[:upper:]' '[:lower:]' <<<"$1" | sed -E 's/[^a-z0-9]+/-/g; s/^-+//; s/-+$//'
}

# live_ids prints "<id>\t<name>" for every repository-level ruleset.
live_ids() {
  gh api --paginate "repos/${REPO}/rulesets?includes_parents=false" \
    --jq '.[] | select(.source_type == "Repository") | [.id, .name] | @tsv'
}

declare -A live_by_name=()
while IFS=$'\t' read -r id name; do
  [[ -n "${id}" ]] && live_by_name["${name}"]="${id}"
done < <(live_ids)

if [[ "${mode}" == export ]]; then
  mkdir -p "${RULESETS_DIR}"
  for name in "${!live_by_name[@]}"; do
    file="${RULESETS_DIR}/$(slug "${name}").json"
    live="$(gh api "repos/${REPO}/rulesets/${live_by_name[${name}]}")"
    if [[ "$(jq '.bypass_actors == null' <<<"${live}")" == true ]]; then
      echo "warning: bypass_actors of '${name}' aren't visible without admin;" \
        "keeping the ones in ${file#"${ROOT}"/}, if any" >&2
      existing='[]'
      [[ -f "${file}" ]] && existing="$(jq '.bypass_actors // []' "${file}")"
      live="$(jq --argjson b "${existing}" '.bypass_actors = $b' <<<"${live}")"
    fi
    normalize false <<<"${live}" >"${file}"
    echo "exported '${name}' -> ${file#"${ROOT}"/}"
  done
  exit 0
fi

shopt -s nullglob
files=("${RULESETS_DIR}"/*.json)
if ((${#files[@]} == 0)); then
  echo "error: no ruleset files in ${RULESETS_DIR}" >&2
  exit 1
fi

drift=0
declare -A managed=()

for file in "${files[@]}"; do
  rel="${file#"${ROOT}"/}"
  name="$(jq -r '.name' "${file}")"
  if [[ -z "${name}" || "${name}" == null ]]; then
    echo "error: ${rel} has no name" >&2
    exit 1
  fi
  if [[ "$(basename "${file}")" != "$(slug "${name}").json" ]]; then
    echo "warning: ${rel} should be named $(slug "${name}").json" >&2
  fi
  managed["${name}"]=1
  id="${live_by_name[${name}]:-}"

  if [[ -z "${id}" ]]; then
    drift=1
    echo "=== ${name}: not on GitHub; $([[ "${mode}" == apply ]] && echo creating || echo "would create") it from ${rel}"
    if [[ "${mode}" == apply ]]; then
      normalize false <"${file}" | gh api -X POST "repos/${REPO}/rulesets" --input - --jq '"created \(.name) (id \(.id))"'
    fi
    continue
  fi

  live="$(gh api "repos/${REPO}/rulesets/${id}")"
  drop_bypass="$(jq '.bypass_actors == null' <<<"${live}")"
  if diff -u \
    --label "live: ${name} (id ${id})" <(normalize "${drop_bypass}" <<<"${live}") \
    --label "${rel}" <(normalize "${drop_bypass}" <"${file}"); then
    echo "=== ${name}: up to date"
  else
    drift=1
    echo "=== ${name}: differs from ${rel}"
    if [[ "${mode}" == apply ]]; then
      normalize false <"${file}" | gh api -X PUT "repos/${REPO}/rulesets/${id}" --input - --jq '"updated \(.name) (id \(.id))"'
    fi
  fi
  if [[ "${drop_bypass}" == true ]]; then
    echo "    (bypass_actors not compared: needs admin)"
  fi
done

for name in "${!live_by_name[@]}"; do
  if [[ -z "${managed[${name}]:-}" ]]; then
    drift=1
    echo "=== ${name}: on GitHub (id ${live_by_name[${name}]}) but has no file; not deleting it"
  fi
done

if [[ "${mode}" == check && "${drift}" -ne 0 ]]; then
  echo >&2
  echo "The live rulesets differ from ${RULESETS_DIR#"${ROOT}"/}." >&2
  echo "A repo admin should run hack/apply-rulesets.sh (see docs/rulesets.md)." >&2
  exit 1
fi
