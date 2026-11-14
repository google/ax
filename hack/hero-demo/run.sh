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
STUDIO_URL="${STUDIO_URL:-http://127.0.0.1:8080}"
REMOTE_CONTEXT="${REMOTE_CONTEXT:-}"
AX_TIMEOUT="${AX_TIMEOUT:-120}"
ONLY_STEP=""

WORKSPACE_MANIFEST="${HERO_DIR}/workspace.yaml"
STARTER_MANIFEST="${HERO_DIR}/task-starter.yaml"
CUSTOM_MANIFEST="${HERO_DIR}/task-custom.yaml"
WORKSPACE_NAME="hero-demo-ws"
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
    # TODO(@YoyinZyc): Replace with 'ax local start' once local mode lands on main (#526).
    mark_stub "${step}" "ax CLI found; wire 'ax local start' + Studio health check"
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
  # Workspaces are immutable, and applying an identical spec again is a no-op.
  if ! ax_quiet apply -f "${WORKSPACE_MANIFEST}"; then
    mark_fail "${step}" "ax apply -f workspace.yaml failed"
    return
  fi
  # TODO(@YoyinZyc): Check that the workspace reaches Ready from a warm snapshot
  # once pre-warmed workspace snapshots land on main (#584).
  mark_stub "${step}" "Workspace ${WORKSPACE_NAME} applied; wire the warm snapshot check"
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
  # Tasks are immutable, so start from a fresh one on every run.
  run_ax delete task "${STARTER_TASK}" >/dev/null 2>&1 || true
  if ! ax_quiet apply -f "${STARTER_MANIFEST}"; then
    mark_fail "${step}" "ax apply -f task-starter.yaml failed"
    return
  fi
  local verb
  for verb in resume suspend resume; do
    if ! ax_quiet "${verb}" task "${STARTER_TASK}"; then
      mark_fail "${step}" "ax ${verb} task ${STARTER_TASK} failed"
      return
    fi
  done
  # TODO(@aprajitaarora13, @itssimrank): Send a second Session turn and check the
  # task keeps its state across suspend/resume, once sessions land on main.
  mark_stub "${step}" "Apply + resume/suspend/resume OK; wire the second Session turn"
}

# ------------------------------------------------------------------------------
# Step 4: Custom Image Task Without Starter Template
# ------------------------------------------------------------------------------
step4_custom_image_task() {
  local step="${STEPS[3]}"
  echo "==> [Step 4] Custom Image Task - No Starter Template"
  # A plain image has no ax-task-runner, so task-custom.yaml cannot run on main
  # yet; nothing is called here even in live mode.
  # TODO(@itssimrank, @siyuanfoundation): Once automatic runner injection lands, run
  #   ax apply -f task-custom.yaml && ax resume task hero-demo-custom-task
  # and check the unit tests pass, e.g. with
  #   ax ssh hero-demo-custom-task -- python3 -m unittest discover -s /workspace/app
  : "${CUSTOM_MANIFEST}" "${CUSTOM_TASK}"
  mark_stub "${step}" "Wire custom image + command Task (needs runner injection)"
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
  # Promotion applies the same manifests, unchanged, to the remote cluster.
  run_ax --context "${REMOTE_CONTEXT}" delete task "${STARTER_TASK}" >/dev/null 2>&1 || true
  if ax_quiet --context "${REMOTE_CONTEXT}" apply -f "${WORKSPACE_MANIFEST}" &&
    ax_quiet --context "${REMOTE_CONTEXT}" apply -f "${STARTER_MANIFEST}"; then
    # TODO(@siyuanfoundation): Promote in one command instead of one apply per manifest.
    mark_pass "${step}" "Manifests applied unchanged to ${REMOTE_CONTEXT}"
  else
    mark_fail "${step}" "Applying the manifests to ${REMOTE_CONTEXT} failed"
  fi
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
