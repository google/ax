#!/bin/bash
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

set -e

# Unified script to deploy the Open-Source SubstrATE controller infra on GKE
# under the isolated namespace 'oss-ate-system'.

echo "[step]: Creating namespace oss-ate-system..."
kubectl create namespace oss-ate-system --dry-run=client -o yaml | kubectl apply -f -

echo "[step]: Deploying OSS SubstrATE controller CRDs and resources..."
kubectl apply -k manifests/oss-substrate/

echo "[step]: Waiting for OSS SubstrATE controller pods to be ready..."
kubectl wait --for=condition=Ready pods -n oss-ate-system -l app=ate-controller --timeout=3m || true

echo "[step]: Deploy complete!"
