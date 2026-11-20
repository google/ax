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

# AX v0.1 hero demo harness.
#
# Usage: ./hack/hero-demo/run.sh [--step N] [--remote-context <kube-context>]
#
# Runs the five hero demo steps in order and prints a scoreboard
# (🟢 PASS / 🟡 STUB / 🔴 FAIL). By default it makes no calls to a cluster, so it
# runs cleanly anywhere. Set RUN_LIVE_STEPS=1 to run the steps that already work
# on main against the AX server of your current kube context.
#
# Each step owner replaces their 🟡 STUB with the real call in their step function below.
#
# Environment:
#   AX_BIN          ax CLI to use (default: ax on PATH, else ./bin/ax)
#   RUN_LIVE_STEPS  1 to call the AX server (default: 0)
#   STARTER_IMAGE   Starter image digest override for Step 3 / Step 5
#   STUDIO_URL      AX Studio base URL (default: http://127.0.0.1:8080)
#   REMOTE_CONTEXT  kube context of a remote cluster for Step 5 (same as --remote-context)
#   AX_TIMEOUT      seconds allowed per ax call in live mode (default: 120)

set -uo pipefail

HERO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${HERO_DIR}/../.." && pwd)"

if [[ -z "${AX_BIN:-}" ]]; then
  if command -v ax >/dev/null 2>&1; then
    AX_BIN="ax"
  else
    AX_BIN="${REPO_ROOT}/bin/ax"
  fi
fi
RUN_LIVE_STEPS="${RUN_LIVE_STEPS:-0}"
STARTER_IMAGE="${STARTER_IMAGE:-}"
STUDIO_URL="${STUDIO_URL:-http://127.0.0.1:8080}"
REMOTE_CONTEXT="${REMOTE_CONTEXT:-}"
AX_TIMEOUT="${AX_TIMEOUT:-120}"
ONLY_STEP=""

WORKSPACE_MANIFEST="${HERO_DIR}/workspace.yaml"
GATEWAY_MANIFEST="${HERO_DIR}/gateway.yaml"
STARTER_MANIFEST="${HERO_DIR}/task-starter.yaml"
CUSTOM_MANIFEST="${HERO_DIR}/task-custom.yaml"
WORKSPACE_NAME="hero-demo-ws"
GATEWAY_NAME="hero-demo-gw"
STARTER_TASK="hero-demo-starter-task"
CUSTOM_TASK="hero-demo-custom-task"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --step)
      ONLY_STEP="${2:-}"
      shift 2
      ;;
    --remote-context)
      REMOTE_CONTEXT="${2:-}"
      shift 2
      ;;
    -h | --help)
      # Print the comment block that starts at the "hero demo harness" line.
      awk '/^# AX v0.1 hero demo harness/{p=1} p&&!/^#/{exit} p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      echo "unknown argument: $1 (see --help)" >&2
      exit 2
      ;;
  esac
done
if [[ -n "${ONLY_STEP}" && ! "${ONLY_STEP}" =~ ^[1-5]$ ]]; then
  echo "--step takes a number from 1 to 5" >&2
  exit 2
fi

STEPS=(
  "1. Local Up & Studio"
  "2. Warm Workspace Snapshot"
  "3. Starter Task, Session & Resume"
  "4. Custom Image Task"
  "5. Remote Cluster Promotion"
)
declare -A STATUS
declare -A NOTE

mark_pass() { STATUS["$1"]="🟢 PASS"; NOTE["$1"]="$2"; }
mark_stub() { STATUS["$1"]="🟡 STUB"; NOTE["$1"]="$2"; }
mark_fail() { STATUS["$1"]="🔴 FAIL"; NOTE["$1"]="$2"; }

live() { [[ "${RUN_LIVE_STEPS}" == "1" ]]; }

have_ax() { [[ -x "${AX_BIN}" ]] || command -v "${AX_BIN}" >/dev/null 2>&1; }

# run_ax runs the ax CLI, bounded by AX_TIMEOUT where `timeout` is available.
run_ax() {
  if command -v timeout >/dev/null 2>&1; then
    timeout "${AX_TIMEOUT}" "${AX_BIN}" "$@"
  else
    command "${AX_BIN}" "$@"
  fi
}

# ax_quiet runs run_ax and shows its output only when it fails.
ax_quiet() {
  local out
  if out="$(run_ax "$@" 2>&1)"; then
    return 0
  fi
  printf '    %s\n' "${out//$'\n'/$'\n'    }" >&2
  return 1
}

# render_starter_manifest writes a manifest (task-starter.yaml by default) to a
# temp file, substituting STARTER_IMAGE when set.
render_starter_manifest() {
  local out_file="$1"
  python3 - "${2:-${STARTER_MANIFEST}}" "${out_file}" "${STARTER_IMAGE}" <<'PY'
import re
import sys

src, dst, image = sys.argv[1:4]
with open(src, "r", encoding="utf-8") as f:
    text = f.read()
if image:
    text, count = re.subn(
        r'^(\s*image:\s*)"[^"]*"',
        lambda m: f'{m.group(1)}"{image}"',
        text,
        count=1,
        flags=re.MULTILINE,
    )
    if count != 1:
        raise SystemExit(f"failed to substitute spec.profile.image in {src}")
with open(dst, "w", encoding="utf-8") as f:
    f.write(text)
PY
}

# task_state prints the State line of `ax describe task`, such as Working.
# Arguments after the task name are global ax flags, such as --context.
task_state() {
  local task_name="$1"
  shift
  run_ax "$@" describe task "${task_name}" 2>/dev/null | awk '/^State:/ { print $2 }'
}

# wait_for_file polls over ax ssh until the task's sandbox has the file.
# Redirect stdin from /dev/null because timeout puts ax ssh in a background
# process group, where reading the controlling tty triggers SIGTTIN.
wait_for_file() {
  local task_name="$1" file="$2"
  local deadline=$((SECONDS + AX_TIMEOUT))
  while ((SECONDS < deadline)); do
    if AX_TIMEOUT=10 run_ax ssh "${task_name}" -- test -f "${file}" </dev/null >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
  done
  return 1
}

# start_task applies a task from a fresh start and resumes it. On failure it
# marks the step failed and returns 1.
start_task() {
  local step="$1" task_name="$2" manifest="$3" manifest_name="$4"
  # Tasks are immutable, so start from a fresh one on every run.
  run_ax delete task "${task_name}" >/dev/null 2>&1 || true
  if ! ax_quiet apply -f "${manifest}"; then
    mark_fail "${step}" "ax apply -f ${manifest_name} failed"
    return 1
  fi
  if ! ax_quiet resume task "${task_name}"; then
    mark_fail "${step}" "ax resume task ${task_name} failed"
    return 1
  fi
}

# finish_task waits until the task has completed with output. A finished task
# is terminal, so ax ssh, ax suspend and ax resume refuse it. Arguments after
# the task name are global ax flags, such as --context. On failure finish_task
# marks the step failed and returns 1.
finish_task() {
  local step="$1" task_name="$2"
  shift 2
  local state="" deadline=$((SECONDS + AX_TIMEOUT))
  while ((SECONDS < deadline)); do
    state="$(task_state "${task_name}" "$@")"
    if [[ "${state}" == Completed || "${state}" == Failed ]]; then
      break
    fi
    sleep 2
  done
  case "${state}" in
    Completed) ;;
    Failed)
      # Show the end of its output, as ax_quiet does for a command that fails.
      run_ax "$@" describe task "${task_name}" 2>/dev/null | sed -n '/^Output:/,/^$/p' | grep -v '^$' | tail -n 12 | sed 's/^/    /' >&2
      mark_fail "${step}" "${task_name} is Failed; see 'ax${*:+ $*} describe task ${task_name}'"
      return 1
      ;;
    *)
      mark_fail "${step}" "${task_name} did not complete within ${AX_TIMEOUT}s (state: ${state:-unknown})"
      return 1
      ;;
  esac
  # The output is the task's $AX_OUTPUT_FILE (/workspace/.ax/output), or else
  # the end of what its command printed.
  if [[ "$(run_ax "$@" describe task "${task_name}" 2>/dev/null)" != *$'\n'"Output:"* ]]; then
    mark_fail "${step}" "${task_name} completed without output"
    return 1
  fi
}

# ------------------------------------------------------------------------------
# Step 1: Local Up & AX Studio
# ------------------------------------------------------------------------------
step1_local_up() {
  local step="${STEPS[0]}"
  echo "==> [Step 1] Local Up & AX Studio"
  if ! have_ax; then
    mark_stub "${step}" "ax CLI not found; run 'make install' or set AX_BIN"
    return
  fi
  if ! live; then
    if "${AX_BIN}" local --help >/dev/null 2>&1; then
      mark_stub "${step}" "ax local start ready; wire Studio health check (#543)"
    else
      mark_stub "${step}" "ax CLI found; wire 'ax local start' + Studio health check"
    fi
    return
  fi
  if ! ax_quiet get workspaces; then
    mark_fail "${step}" "ax-server not reachable from the current kube context"
    return
  fi
  if curl -fsS --max-time 5 "${STUDIO_URL}/healthz" >/dev/null 2>&1; then
    mark_pass "${step}" "ax-server reachable and AX Studio healthy at ${STUDIO_URL}"
  else
    # TODO(@YoyinZyc, @Raji14): Wire the AX Studio health check once Studio lands (#543).
    mark_stub "${step}" "ax-server reachable; AX Studio not running at ${STUDIO_URL}"
  fi
}

# ------------------------------------------------------------------------------
# Step 2: Warm Workspace Snapshot
# ------------------------------------------------------------------------------
step2_warm_workspace() {
  local step="${STEPS[1]}"
  echo "==> [Step 2] Warm Workspace Snapshot"
  if ! live; then
    mark_stub "${step}" "Set RUN_LIVE_STEPS=1 to apply workspace.yaml"
    return
  fi
  local tmp_manifest
  tmp_manifest="$(mktemp)"
  render_starter_manifest "${tmp_manifest}" "${WORKSPACE_MANIFEST}"
  # Workspaces are immutable, and applying an identical spec again is a no-op.
  # The first apply waits for the pre-warm: image pull, spec.setup, snapshot.
  if ! AX_TIMEOUT=600 ax_quiet apply -f "${tmp_manifest}"; then
    rm -f "${tmp_manifest}"
    mark_fail "${step}" "ax apply -f workspace.yaml failed"
    return
  fi
  rm -f "${tmp_manifest}"
  if ! run_ax describe workspace "${WORKSPACE_NAME}" 2>/dev/null | grep -q 'SnapshotReady  *True'; then
    mark_fail "${step}" "Workspace ${WORKSPACE_NAME} has no warm snapshot; see 'ax describe workspace ${WORKSPACE_NAME}'"
    return
  fi
  mark_pass "${step}" "Workspace ${WORKSPACE_NAME} is Ready with a warm snapshot"
}

# ------------------------------------------------------------------------------
# Step 3: Starter Task, Session & Resume
# ------------------------------------------------------------------------------
step3_starter_task_session() {
  local step="${STEPS[2]}"
  echo "==> [Step 3] Starter Task, Session & Resume"
  if ! live; then
    mark_stub "${step}" "Set RUN_LIVE_STEPS=1 to run the starter task lifecycle"
    return
  fi
  # A task can only bind a Gateway that already exists.
  if ! ax_quiet apply -f "${GATEWAY_MANIFEST}"; then
    mark_fail "${step}" "ax apply -f gateway.yaml failed"
    return
  fi
  local tmp_manifest
  tmp_manifest="$(mktemp)"
  render_starter_manifest "${tmp_manifest}"
  start_task "${step}" "${STARTER_TASK}" "${tmp_manifest}" task-starter.yaml
  local rc=$?
  rm -f "${tmp_manifest}"
  ((rc == 0)) || return
  finish_task "${step}" "${STARTER_TASK}" || return

  # TODO(@aprajitaarora13, @itssimrank): Send a second Session turn on the
  # same task's workspace once multi-turn input on a single task lands. A
  # completed task refuses suspend/resume, so Step 4 shows those.
  mark_stub "${step}" "Turn 1 OK (task completed with output); wire the second Session turn"
}

# ------------------------------------------------------------------------------
# Step 4: Custom Image Task Without Starter Template
# ------------------------------------------------------------------------------
step4_custom_image_task() {
  local step="${STEPS[3]}"
  echo "==> [Step 4] Custom Image Task - No Starter Template"
  if ! live; then
    mark_stub "${step}" "Set RUN_LIVE_STEPS=1 to run the custom image task"
    return
  fi
  # The image is a plain one: ax-server mounts ax-task-runner into it from its
  # runner volume image.
  start_task "${step}" "${CUSTOM_TASK}" "${CUSTOM_MANIFEST}" task-custom.yaml || return
  # The command waits on its first run, so the task can be suspended while it
  # is Working. After the resume it starts again and runs the workspace's unit
  # tests, and the task fails unless they pass.
  if ! wait_for_file "${CUSTOM_TASK}" /workspace/first-run; then
    mark_fail "${step}" "${CUSTOM_TASK} did not start its command within ${AX_TIMEOUT}s"
    return
  fi
  local verb
  for verb in suspend resume; do
    if ! ax_quiet "${verb}" task "${CUSTOM_TASK}"; then
      mark_fail "${step}" "ax ${verb} task ${CUSTOM_TASK} failed"
      return
    fi
  done
  finish_task "${step}" "${CUSTOM_TASK}" || return
  mark_pass "${step}" "${CUSTOM_TASK} survived suspend/resume and ran the unit tests of ${WORKSPACE_NAME} in a plain image"
}

# ------------------------------------------------------------------------------
# Step 5: Remote Cluster Promotion
# ------------------------------------------------------------------------------
step5_remote_promote() {
  local step="${STEPS[4]}"
  echo "==> [Step 5] Remote Cluster Promotion"
  if [[ -z "${REMOTE_CONTEXT}" ]]; then
    # TODO(@YoyinZyc, @MushuEE): Wire a Kind remote context into the E2E run.
    mark_stub "${step}" "Set REMOTE_CONTEXT or --remote-context to promote"
    return
  fi
  if ! have_ax; then
    mark_fail "${step}" "ax CLI not found; run 'make install' or set AX_BIN"
    return
  fi
  # Copies the starter task that Step 3 runs, with the Gateway and Workspace it
  # binds, to the remote cluster. --suspend-source suspends it here if it is
  # still running; after Step 3 it has finished. --replace lets a rerun start
  # from a fresh task there.
  if ! ax_quiet promote task "${STARTER_TASK}" --to "${REMOTE_CONTEXT}" --replace --suspend-source; then
    mark_fail "${step}" "ax promote task ${STARTER_TASK} --to ${REMOTE_CONTEXT} failed"
    return
  fi
  # The promoted task runs from the start on the remote cluster.
  finish_task "${step}" "${STARTER_TASK}" --context "${REMOTE_CONTEXT}" || return
  mark_pass "${step}" "${STARTER_TASK} promoted to ${REMOTE_CONTEXT} and completed there"
}

FUNCS=(step1_local_up step2_warm_workspace step3_starter_task_session step4_custom_image_task step5_remote_promote)
for i in "${!FUNCS[@]}"; do
  if [[ -n "${ONLY_STEP}" && "${ONLY_STEP}" != "$((i + 1))" ]]; then
    continue
  fi
  "${FUNCS[$i]}"
done

echo ""
echo "================================================================================"
echo "  AX v0.1 HERO DEMO SCOREBOARD$([[ "${RUN_LIVE_STEPS}" == "1" ]] && echo " (live)")"
echo "================================================================================"
for i in "${!STEPS[@]}"; do
  if [[ -n "${ONLY_STEP}" && "${ONLY_STEP}" != "$((i + 1))" ]]; then
    continue
  fi
  step="${STEPS[$i]}"
  printf "  %-34s | %-7s | %s\n" "${step}" "${STATUS[$step]}" "${NOTE[$step]}"
done
echo "================================================================================"
