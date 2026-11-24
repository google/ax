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
# context: a minimal Task, a Gateway, a Workspace with setup, a stand-in agent
# that reads its goal, config and MCP servers through the launcher contract,
# suspend and resume with progress kept in the workspace, immutability,
# fan-out from one workspace snapshot, an agent built on the Gemini API SDK
# whose API key the Gateway injects, atespaces, and Models. Every command is
# printed before it runs, and everything the tour creates is deleted when it
# exits.
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
#   GEMINI_API_KEY  Gemini API key for the Gemini step, which is skipped when
#                   unset. The tour stores it in Secret default/tour-gemini-key
#                   of the current kube context; it never enters a sandbox.

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
AGENT="tour-agent"
FANOUT_A="tour-fanout-js"
FANOUT_B="tour-fanout-docs"
TENANT_TASK="tour-report"
TENANTS=(tour-team-a tour-team-b)
GEMINI_SECRET="tour-gemini-key"
GEMINI_GATEWAY="tour-gemini-gw"
GEMINI_WORKSPACE="tour-gemini-ws"
GEMINI_TASK="tour-gemini"
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

# ssh_out prints the output of a command run in task $1's sandbox without
# echoing it, for values the tour compares.
ssh_out() {
  local task="$1"
  shift
  ax ssh "${task}" -- "$@"
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
  local kind name atespace
  for r in "task ${SIMPLE_TASK}" "task ${AGENT}" "task ${FANOUT_A}" "task ${FANOUT_B}" \
    "task ${TENANT_TASK} ${TENANTS[0]}" "task ${TENANT_TASK} ${TENANTS[1]}" \
    "task ${GEMINI_TASK}" "workspace ${WORKSPACE}" "workspace ${GEMINI_WORKSPACE}" \
    "gateway ${GATEWAY}" "gateway ${GEMINI_GATEWAY}" "model ${MODEL}"; do
    read -r kind name atespace <<< "${r}"
    if ax get "${kind}" "${name}" -a "${atespace:-default}" > /dev/null 2>&1; then
      run ax delete "${kind}" "${name}" -a "${atespace:-default}" || true
    fi
  done
  if kubectl get secret "${GEMINI_SECRET}" -n default > /dev/null 2>&1; then
    run kubectl delete secret "${GEMINI_SECRET}" -n default
  fi
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

step "2. A Gateway: the only way out of a sandbox"
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

step "3. A Workspace: prepared once, snapshotted, reused by every task"
say "Setup clones a repository, writes files (here, a small agent), and runs"
say "commands in a setup sandbox that setup.gateway lets reach GitHub. The"
say "MCP servers are handed to every task's agent."
apply << EOF
apiVersion: ax.io/v1alpha1
kind: Workspace
metadata:
  name: ${WORKSPACE}
spec:
  profile:
    image: "${AX_TASK_IMAGE}"
  mcp:
    servers:
      - name: docs
        endpoint: "https://mcp.example.com/mcp"
      - name: git-tools
        command: git-mcp
        args: ["--repo", "/workspace/chalk"]
  setup:
    gateway:
      name: ${GATEWAY}
    git:
      - repo: "https://github.com/chalk/chalk.git"
        depth: 1
    files:
      - path: agent.py
        content: |
          # A stand-in for a coding agent. It reads its task through the
          # launcher contract (AX_*_FILE) and the metadata server, writes a
          # report to AX_OUTPUT_FILE, and then keeps working, saving its
          # progress in the workspace so it continues after a resume.
          import json, os, pathlib, time, urllib.request

          def read(env):
              return pathlib.Path(os.environ[env]).read_text()

          meta = os.environ["AX_METADATA_URL"] + "/metadata/v1alpha1/ax/"
          task = urllib.request.urlopen(meta + "task").read().decode()
          name = next(l.split(":", 1)[1].strip() for l in task.splitlines() if l.strip().startswith("name:"))
          config = json.loads(read("AX_CONFIG_FILE"))
          mcp = json.loads(read("AX_MCP_CONFIG_FILE"))["mcpServers"]
          files = sorted(pathlib.Path(config["dir"]).rglob(config["pattern"]))
          report = [
              f"task:    {name}",
              f"session: {read('AX_SESSION_FILE')}",
              f"goal:    {read('AX_GOAL_FILE')}",
              "mcp:     " + ", ".join(k + " (" + v["type"] + ")" for k, v in sorted(mcp.items())),
              f"found {len(files)} {config['pattern']} files under {config['dir']}:",
          ] + [f"  {f}" for f in files[: config.get("limit", 5)]]
          pathlib.Path(os.environ["AX_OUTPUT_FILE"]).write_text("\n".join(report) + "\n")

          heartbeat = pathlib.Path(".ax/heartbeat")
          beat = int(heartbeat.read_text()) if heartbeat.exists() else 0
          while True:
              beat += 1
              heartbeat.write_text(str(beat))
              time.sleep(1)
    commands:
      - "git -C chalk log -1 --format='chalk at %h: %s' > setup.txt"
EOF
run ax get workspaces
run ax describe workspace "${WORKSPACE}"

step "4. Bring your own agent"
say "The task names a goal and spec.config; the runner hands them to the agent"
say "as files, along with the session ID and the Workspace's MCP servers."
apply << EOF
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: ${AGENT}
spec:
  workspace:
    name: ${WORKSPACE}
  gateway:
    name: ${GATEWAY}
  goal:
    text: "Survey chalk's JavaScript sources."
  config:
    dir: chalk/source
    pattern: "*.js"
    limit: 3
  sessionId: tour-session-1
  commands: ["python3", "agent.py"]
  debug: true
EOF
wait_state "${AGENT}" Suspended
run ax resume task "${AGENT}"
wait_state "${AGENT}" Running
say "The task starts warm from the workspace snapshot."
run ax describe task "${AGENT}"
run ax ssh "${AGENT}" -- sh -c 'cd /workspace && ls -A && cat setup.txt'
sleep 1
say "The agent's report, from \$AX_OUTPUT_FILE:"
run ax ssh "${AGENT}" -- cat /workspace/.ax/output
say "Its Gateway lets it reach GitHub, and nothing else."
run ax ssh "${AGENT}" -- git ls-remote https://github.com/chalk/chalk.git HEAD
expect_fail ax ssh "${AGENT}" -- curl -sS -m 5 https://example.com

step "5. Suspend and resume"
say "Suspending checkpoints the workspace volume and frees the worker. Resuming"
say "starts the command again on the restored workspace."
before="$(ssh_out "${AGENT}" cat /workspace/.ax/heartbeat)"
say "heartbeat before suspend: ${before}"
run ax suspend task "${AGENT}"
wait_state "${AGENT}" Suspended
run ax resume task "${AGENT}"
wait_state "${AGENT}" Running
sleep 2
after="$(ssh_out "${AGENT}" cat /workspace/.ax/heartbeat)"
say "heartbeat after resume: ${after}"
if ((after <= before)); then
  printf '%sthe agent lost its progress%s\n' "${red}" "${reset}" >&2
  exit 1
fi
say "The agent picked up its saved progress and kept counting."

step "6. Tasks are immutable"
expect_fail apply << EOF
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: ${AGENT}
spec:
  workspace:
    name: ${WORKSPACE}
  goal:
    text: "Do something else."
EOF
run ax delete task "${AGENT}"
expect_fail ax delete task "${AGENT}"

step "7. Fan out: many tasks, one snapshot, isolated copies"
say "Two tasks bind the same Workspace with different goals and config."
for spec in "${FANOUT_A}|Find the JavaScript.|chalk|*.js" "${FANOUT_B}|Find the docs.|chalk|*.md"; do
  IFS='|' read -r name goal dir pattern <<< "${spec}"
  apply << EOF
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: ${name}
spec:
  workspace:
    name: ${WORKSPACE}
  goal:
    text: "${goal}"
  config:
    dir: ${dir}
    pattern: "${pattern}"
  commands: ["python3", "agent.py"]
  debug: true
EOF
done
run ax get tasks
say "Each runs in its own sandbox; suspended tasks hold no worker."
run ax resume task "${FANOUT_A}"
wait_state "${FANOUT_A}" Running
sleep 1
run ax ssh "${FANOUT_A}" -- cat /workspace/.ax/output
say "This task writes a file into its copy of the workspace."
run ax ssh "${FANOUT_A}" -- sh -c "echo 'written by ${FANOUT_A}' > /workspace/claim.txt && cat /workspace/claim.txt"
run ax suspend task "${FANOUT_A}"
wait_state "${FANOUT_A}" Suspended
run ax resume task "${FANOUT_B}"
wait_state "${FANOUT_B}" Running
sleep 1
run ax ssh "${FANOUT_B}" -- cat /workspace/.ax/output
say "The other task's copy does not have it."
expect_fail ax ssh "${FANOUT_B}" -- cat /workspace/claim.txt
run ax delete task "${FANOUT_A}"
run ax delete task "${FANOUT_B}"

step "8. An agent on the Gemini API SDK"
if [[ -z "${GEMINI_API_KEY:-}" ]]; then
  say "Skipped: set GEMINI_API_KEY to run it."
else
  say "The API key goes into a Kubernetes Secret, labeled so that the default"
  say "atespace's tasks may use it."
  show kubectl create secret generic "${GEMINI_SECRET}" -n default '--from-literal=api-key=$GEMINI_API_KEY'
  kubectl create secret generic "${GEMINI_SECRET}" -n default --from-literal=api-key="${GEMINI_API_KEY}"
  run kubectl label secret "${GEMINI_SECRET}" -n default ax.io/credential=true
  say "The Gateway sets the x-goog-api-key header on requests to the Gemini API"
  say "outside the sandbox, so the task holds only a placeholder. It also lets"
  say "setup reach PyPI and GitHub."
  apply << EOF
apiVersion: ax.io/v1alpha1
kind: Gateway
metadata:
  name: ${GEMINI_GATEWAY}
spec:
  egress:
    - https:
        hostnames: ["generativelanguage.googleapis.com"]
        effects:
          replaceHeaders:
            - header: x-goog-api-key
              credentialUri: "ate-secret://k8s.io/default/default/${GEMINI_SECRET}/api-key"
    - https:
        hostnames: ["pypi.org", "files.pythonhosted.org"]
    - https:
        hostnames: ["github.com", "*.github.com", "*.githubusercontent.com"]
EOF
  say "Setup installs the SDK into the workspace, so every task starts with it."
  apply << EOF
apiVersion: ax.io/v1alpha1
kind: Workspace
metadata:
  name: ${GEMINI_WORKSPACE}
spec:
  profile:
    image: "${AX_TASK_IMAGE}"
  setup:
    gateway:
      name: ${GEMINI_GATEWAY}
    git:
      - repo: "https://github.com/chalk/chalk.git"
        depth: 1
    files:
      - path: gemini_agent.py
        content: |
          # Sends the task's goal, with the workspace files that spec.config
          # names, to Gemini and writes the answer to AX_OUTPUT_FILE.
          import json, os, pathlib
          from google import genai

          goal = pathlib.Path(os.environ["AX_GOAL_FILE"]).read_text()
          config = json.loads(pathlib.Path(os.environ["AX_CONFIG_FILE"]).read_text())
          prompt = goal + "".join(
              f"\n\n--- {path} ---\n{pathlib.Path(path).read_text()}" for path in config.get("files", [])
          )
          response = genai.Client().models.generate_content(model=config["model"], contents=prompt)
          pathlib.Path(os.environ["AX_OUTPUT_FILE"]).write_text(response.text)
          print(response.text)
    commands:
      - "python3 -m venv .venv && .venv/bin/pip install --quiet --disable-pip-version-check google-genai"
EOF
  apply << EOF
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: ${GEMINI_TASK}
spec:
  workspace:
    name: ${GEMINI_WORKSPACE}
  gateway:
    name: ${GEMINI_GATEWAY}
  goal:
    text: "In three short bullet points, what does this library do and why would I use it?"
  config:
    model: gemini-3.8-flash
    files: ["chalk/readme.md"]
  env:
    - name: GEMINI_API_KEY
      value: placeholder # the Gateway replaces it with the real key
  commands: [".venv/bin/python", "gemini_agent.py"]
  debug: true
EOF
  wait_state "${GEMINI_TASK}" Suspended
  run ax resume task "${GEMINI_TASK}"
  wait_state "${GEMINI_TASK}" Running
  say "Waiting for Gemini's answer..."
  deadline=$((SECONDS + TOUR_TIMEOUT))
  until ax ssh "${GEMINI_TASK}" -- test -s /workspace/.ax/output 2> /dev/null; do
    if ((SECONDS > deadline)); then
      printf '%sno answer from Gemini after %ss%s\n' "${red}" "${TOUR_TIMEOUT}" "${reset}" >&2
      exit 1
    fi
    sleep 1
  done
  run ax ssh "${GEMINI_TASK}" -- cat /workspace/.ax/output
  run ax delete task "${GEMINI_TASK}"
  run ax delete workspace "${GEMINI_WORKSPACE}"
  run ax delete gateway "${GEMINI_GATEWAY}"
  run kubectl delete secret "${GEMINI_SECRET}" -n default
fi

step "9. Atespaces: one name, separate tenants"
for atespace in "${TENANTS[@]}"; do
  apply << EOF
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: ${TENANT_TASK}
  atespace: ${atespace}
spec:
  profile:
    image: "${AX_TASK_IMAGE}"
  commands: ["sh", "-c", "echo report for ${atespace}"]
EOF
done
for atespace in "${TENANTS[@]}"; do
  run ax get tasks -a "${atespace}"
done
say "The default atespace does not see them."
run ax get tasks
for atespace in "${TENANTS[@]}"; do
  run ax delete task "${TENANT_TASK}" -a "${atespace}"
done

step "10. Models"
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
