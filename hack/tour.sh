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

# Walks through AX end to end against the AX server of the current kube
# context: a minimal Task, a Gateway, a Workspace with setup, a Task bound to
# both, suspend and resume, immutability, and Models. Every command is printed
# before it runs, and everything the tour creates is deleted when it exits.
#
# Usage: ./hack/tour.sh
#
# Run `make build-local-cluster` first for a local cluster that runs this
# checkout.
#
# Environment:
#   AX_BIN          ax CLI to use (default: ./bin/ax, else ax on PATH)
#   AX_TASK_IMAGE   Digest-pinned runner image for spec.profile.image
#                   (default: the ax-runner that `make build-local-cluster`
#                   pushed to localhost:5001, read from bin/ax-runner.digest)
#   TOUR_TIMEOUT    Seconds to wait for a task to change state (default: 180)

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [[ -z "${AX_BIN:-}" ]]; then
  if [[ -x "${REPO_ROOT}/bin/ax" ]]; then
    AX_BIN="${REPO_ROOT}/bin/ax"
  else
    AX_BIN="ax"
  fi
fi
if [[ -z "${AX_TASK_IMAGE:-}" && -f "${REPO_ROOT}/bin/ax-runner.digest" ]]; then
  AX_TASK_IMAGE="localhost:5001/ax-runner@$(cat "${REPO_ROOT}/bin/ax-runner.digest")"
fi
if [[ "${AX_TASK_IMAGE:-}" != *@sha256:* ]]; then
  echo "set AX_TASK_IMAGE to a digest-pinned runner image, or run make build-local-cluster first" >&2
  exit 1
fi
TOUR_TIMEOUT="${TOUR_TIMEOUT:-180}"

SIMPLE_TASK="tour-simple"
GATEWAY="tour-github-gw"
WORKSPACE="tour-ws"
TASK="tour-task"
MODEL="tour-model"

bold=$'\033[1m' cyan=$'\033[36m' green=$'\033[32m' red=$'\033[31m' reset=$'\033[0m'

step() {
  printf '\n%s━━ %s%s\n' "${bold}" "$*" "${reset}"
}

say() {
  printf '%s# %s%s\n' "${green}" "$*" "${reset}"
}

# show prints a command, quoting the arguments that need it.
show() {
  local out="" arg
  for arg in "$@"; do
    if [[ "${arg}" =~ ^[A-Za-z0-9_./:=@-]+$ ]]; then
      out+=" ${arg}"
    else
      out+=" '${arg//\'/\'\\\'\'}'"
    fi
  done
  printf '%s$%s%s\n' "${cyan}" "${out}" "${reset}"
}

# run prints a command and runs it.
run() {
  show "$@"
  "$@"
}

# expect_fail runs a command that must fail and shows its error. apply prints
# itself.
expect_fail() {
  if [[ "$1" != "apply" ]]; then
    show "$@"
  fi
  if "$@"; then
    printf '%sexpected the command to fail%s\n' "${red}" "${reset}" >&2
    exit 1
  fi
}

# apply prints the manifest on stdin and applies it.
apply() {
  local manifest
  manifest="$(cat)"
  printf '%s$ ax apply -f - <<EOF%s\n%s\n%sEOF%s\n' "${cyan}" "${reset}" "${manifest}" "${cyan}" "${reset}"
  "${AX_BIN}" apply -f - <<< "${manifest}"
}

ax() {
  "${AX_BIN}" "$@"
}

# wait_state waits until task $1 is in state $2.
wait_state() {
  local task="$1" want="$2" state=""
  local deadline=$((SECONDS + TOUR_TIMEOUT))
  while ((SECONDS < deadline)); do
    state="$(ax get tasks 2> /dev/null | awk -v t="${task}" '$1 == t { print $3 }')"
    if [[ "${state}" == "${want}" ]]; then
      say "task ${task} is ${want}"
      return 0
    fi
    sleep 0.5
  done
  printf '%stask %s is %s, not %s, after %ss%s\n' "${red}" "${task}" "${state:-unknown}" "${want}" "${TOUR_TIMEOUT}" "${reset}" >&2
  exit 1
}

cleanup() {
  printf '\n%s━━ Cleaning up%s\n' "${bold}" "${reset}"
  local kind name
  for r in "task ${SIMPLE_TASK}" "task ${TASK}" "workspace ${WORKSPACE}" "gateway ${GATEWAY}" "model ${MODEL}"; do
    read -r kind name <<< "${r}"
    if ax get "${kind}" "${name}" > /dev/null 2>&1; then
      run ax delete "${kind}" "${name}" || true
    fi
  done
}
trap cleanup EXIT

step "0. Where AX runs"
run ax version
run ax ctx
say "runner image: ${AX_TASK_IMAGE}"

step "1. The smallest Task"
say "A Task is immutable. Creating it provisions a sandbox, which starts suspended."
apply << EOF
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: ${SIMPLE_TASK}
spec:
  profile:
    image: "${AX_TASK_IMAGE}"
  commands: ["sh", "-c", "echo hello from \$GREETING; sleep 3600"]
  env:
    - name: GREETING
      value: AX
  debug: true # serve guest services so ax ssh works
EOF
wait_state "${SIMPLE_TASK}" Suspended
run ax get tasks
say "Resuming the task starts its command."
run ax resume task "${SIMPLE_TASK}"
wait_state "${SIMPLE_TASK}" Running
run ax describe task "${SIMPLE_TASK}"
say "spec.debug lets ax ssh run commands in the sandbox."
run ax ssh "${SIMPLE_TASK}" -- sh -c 'echo "GREETING=$GREETING"; ps -eo pid,user,args'
say "A task without a Gateway has no external network access."
expect_fail ax ssh "${SIMPLE_TASK}" -- git ls-remote https://github.com/chalk/chalk.git HEAD
run ax delete task "${SIMPLE_TASK}"

step "2. A Gateway: what a sandbox may reach"
apply << EOF
apiVersion: ax.io/v1alpha1
kind: Gateway
metadata:
  name: ${GATEWAY}
spec:
  egress:
    - https:
        hostnames: ["github.com", "*.github.com", "*.githubusercontent.com"]
EOF
run ax describe gateway "${GATEWAY}"

step "3. A Workspace: prepared once, snapshotted, reused by tasks"
say "Setup clones the repository, writes files, and runs commands in a setup sandbox."
say "setup.gateway lets that sandbox reach GitHub."
apply << EOF
apiVersion: ax.io/v1alpha1
kind: Workspace
metadata:
  name: ${WORKSPACE}
spec:
  profile:
    image: "${AX_TASK_IMAGE}"
  setup:
    gateway:
      name: ${GATEWAY}
    git:
      - repo: "https://github.com/chalk/chalk.git"
        depth: 1
    files:
      - path: NOTES.md
        content: |
          Written by Workspace spec.setup.files.
    commands:
      - "echo 'Written by Workspace spec.setup.commands.' > setup.txt"
EOF
run ax get workspaces
run ax describe workspace "${WORKSPACE}"

step "4. A Task bound to the Workspace and the Gateway"
apply << EOF
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: ${TASK}
spec:
  workspace:
    name: ${WORKSPACE}
  gateway:
    name: ${GATEWAY}
  commands: ["sh", "-c", "ls > listing.txt; sleep 3600"]
  debug: true
EOF
wait_state "${TASK}" Suspended
run ax resume task "${TASK}"
wait_state "${TASK}" Running
say "The task starts from the workspace snapshot."
run ax describe task "${TASK}"
run ax ssh "${TASK}" -- sh -c 'cd /workspace && ls -A && cat NOTES.md setup.txt'
say "Its Gateway lets it reach GitHub."
run ax ssh "${TASK}" -- git ls-remote https://github.com/chalk/chalk.git HEAD

step "5. Suspend and resume"
run ax ssh "${TASK}" -- sh -c 'echo "written before suspend" > /workspace/progress.txt'
run ax suspend task "${TASK}"
wait_state "${TASK}" Suspended
run ax resume task "${TASK}"
wait_state "${TASK}" Running
say "The workspace and the running command survive."
run ax ssh "${TASK}" -- sh -c 'cat /workspace/progress.txt; ps -eo pid,user,args'

step "6. Tasks are immutable"
expect_fail apply << EOF
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: ${TASK}
spec:
  workspace:
    name: ${WORKSPACE}
  commands: ["sh", "-c", "echo a different command"]
EOF
run ax delete task "${TASK}"
expect_fail ax delete task "${TASK}"

step "7. Models"
say "A Model names a provider model and the Secret that holds its API key."
apply << EOF
apiVersion: ax.io/v1alpha1
kind: Model
metadata:
  name: ${MODEL}
spec:
  provider: google
  model: gemini-3.8-flash
  secretKey:
    name: gemini-api-secret
    key: GEMINI_API_KEY
EOF
run ax get models
say "Unlike a Task, a Model can be updated in place."
apply << EOF
apiVersion: ax.io/v1alpha1
kind: Model
metadata:
  name: ${MODEL}
spec:
  provider: anthropic
  model: claude-opus-5
  secretKey:
    name: anthropic-api-secret
    key: ANTHROPIC_API_KEY
EOF
run ax describe model "${MODEL}"
run ax delete model "${MODEL}"
expect_fail ax delete model "${MODEL}"

printf '\n%sTour complete.%s\n' "${green}${bold}" "${reset}"
