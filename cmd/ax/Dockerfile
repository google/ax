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

# Multi-target AX image build:
#   --target antigravity : python:3.13-slim + the Antigravity Python sidecar
#                          (SDK, localharness, agent). Used by the antigravity
#                          harness actor (`ax harness antigravity`).
#   --target ax          : debian:stable-slim + the Go `ax` binary. Used by 
#                          the ax-server (`ax serve`) and the Go interactions 
#                          harness actor (`ax harness antigravity-interactions`).
#
# Build context: repository root. Images target the cluster's linux/amd64 nodes,
# so build with `--platform linux/amd64`. 
# For a standalone build:
#   docker build --platform linux/amd64 --target antigravity -f cmd/ax/Dockerfile -t <ref> . && docker push <ref>
#   docker build --platform linux/amd64 --target ax -f cmd/ax/Dockerfile -t <ref> . && docker push <ref>

# --- Stage 1: build the ax Go binary (with the harness build tag) -------------
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -o /out/ax ./cmd/ax

# --- antigravity image -------
FROM python:3.13-slim AS antigravity
WORKDIR /workspace

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        less \
        procps \
        wget \
    && rm -rf /var/lib/apt/lists/*

# Antigravity runtime deps, installed from PyPI at build time.
COPY python/antigravity/requirements.txt /tmp/requirements.txt
RUN pip install --no-cache-dir -r /tmp/requirements.txt

# Python sidecar (with generated proto stubs), the agent it serves, and the ax
# binary built in stage 1.
COPY python/ /ax-app/python

# You can copy skills or use skills managers (e.g. npx skills)
# to download them into the image.
# Example: COPY examples/skills/ /ax/skills/

ENV SKILLS_DIR=/ax/skills
COPY --from=build /out/ax /ax-app/ax

# `from python.proto import ...` needs /ax-app on the path; the generated stubs do
# `from proto import ...`, which needs /ax-app/python.
ENV PYTHONPATH=/ax-app:/ax-app/python

# Flush stdout/stderr immediately so server logs surface in container logs.
ENV PYTHONUNBUFFERED=1

EXPOSE 80

# Default command for local docker runs. Substrate ignores the image CMD and runs
# the ActorTemplate `command` instead. `ax harness` forks the Python sidecar,
# which serves the HarnessService on :80.
CMD ["/ax-app/ax", "harness", \
     "--host", "0.0.0.0", "--port", "80"]

# --- ax image (Go-only) for the interactions harness and ax-server -----------
FROM debian:stable-slim AS ax
WORKDIR /workspace

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        less \
        procps \
        wget \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/ax /ax-app/ax

EXPOSE 80

CMD ["/ax-app/ax", "harness", "antigravity-interactions", \
     "--host", "0.0.0.0", "--port", "80"]
