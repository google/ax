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

# Comprehensive AX image: the Go `ax` binary plus the Antigravity Python sidecar
# (SDK, localharness, agent), on a Debian-based image with shell tooling for
# skills/code execution. Used by both the ax-server (`ax serve`) and the harness
# actor (`ax harness`, which forks the Python sidecar).
#
# Build context: repository root. The Antigravity runtime deps -- including the
# linux/amd64 google-antigravity wheel that bundles the linux `localharness`
# binary -- are installed offline from a "wheels" build context, so the build
# needs no access to the private package registry. Populate the wheel cache with
# `internal/hack/install-ax.sh --fetch-wheels` (which also drives build/deploy).
#
# Requires an OCI builder that supports additional build contexts and
# RUN --mount: docker with BuildKit (default since Docker 23; older Docker: use
# `docker buildx build`), or podman/buildah >= 4.0. For example:
#   docker build --platform linux/amd64 \
#     --build-context wheels=$HOME/.cache/ax-antigravity-wheels \
#     -f cmd/ax/Dockerfile -t <ref> . && docker push <ref>

# --- Stage 1: build the ax Go binary (with the harness build tag) -------------
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -tags=harness -o /out/ax ./cmd/ax

# --- Stage 2: comprehensive runtime (Debian + Python + Antigravity) -----------
# Python image: a full Debian base (buildpack-deps) that
# already ships bash, git, curl, wget, ca-certificates, build-essential, and the
# common CLIs (find/ps/less/grep/sed/...).
FROM python:3.13
WORKDIR /app

# Antigravity runtime deps, installed offline from the pre-downloaded wheels.
COPY python/antigravity/requirements.txt /tmp/requirements.txt
RUN --mount=type=bind,from=wheels,target=/tmp/wheels \
    pip install --no-cache-dir --no-index --find-links=/tmp/wheels -r /tmp/requirements.txt

# Python sidecar (with generated proto stubs), the agent it serves, and the ax
# binary built in stage 1.
COPY python/ /app/python
COPY examples/antigravity_agent/ /app/examples/antigravity_agent
COPY --from=build /out/ax /ax

# `from python.proto import ...` needs /app on the path; the generated stubs do
# `from proto import ...`, which needs /app/python.
ENV PYTHONPATH=/app:/app/python

# Flush stdout/stderr immediately so server logs surface in container logs.
ENV PYTHONUNBUFFERED=1

EXPOSE 80

# Default command for local docker runs. Substrate ignores the image CMD and runs
# the ActorTemplate `command` instead. `ax harness` forks the Python sidecar,
# which serves the HarnessService on :80.
CMD ["/ax", "harness", \
     "--antigravity-agent-file", "/app/examples/antigravity_agent/agent.py", \
     "--host", "0.0.0.0", "--port", "80"]
