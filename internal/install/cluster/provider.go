// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package cluster defines the local Kubernetes cluster lifecycle interface used
// by `ax local` and provides the MVP Kind implementation.
package cluster

import "context"

const (
	// DefaultName is the default local cluster name (`ax local start`).
	DefaultName = "ax-local"
	// DefaultNodeImage is the Kubernetes 1.36+ Kind node image required for
	// ClusterTrustBundle and PodCertificateRequest projected volumes.
	DefaultNodeImage = "kindest/node:v1.36.1@sha256:3489c7674813ba5d8b1a9977baea8a6e553784dab7b84759d1014dbd78f7ebd5"
)

// State represents the lifecycle state of a local cluster.
type State string

const (
	StateAbsent  State = "NotFound"
	StateStopped State = "Stopped"
	StateRunning State = "Running"
)

// UpOptions configures `Provider.Up`.
type UpOptions struct {
	Name      string
	NodeImage string
}

// Info describes a provisioned local cluster.
type Info struct {
	Name        string
	KubeContext string
}

// Status describes the observed state of a local cluster and its layers.
type Status struct {
	Name            string
	Backend         string
	State           State
	KubeContext     string
	Nodes           []string
	SubstrateStatus string
	AXStatus        string
}

// Provider manages a local single-user Kubernetes cluster (Kind in MVP;
// native macOS VM in Phase 2).
type Provider interface {
	Up(ctx context.Context, opts UpOptions) (Info, error)
	Stop(ctx context.Context, name string) error
	Status(ctx context.Context, name string) (Status, error)
	Delete(ctx context.Context, name string) error
}

// KubeContextName returns the kubeconfig context name registered for a local
// cluster. The default cluster ("ax-local") registers context "ax-local"
// directly; custom cluster names keep `kind-<name>`.
func KubeContextName(name string) string {
	if name == "" || name == DefaultName {
		return DefaultName
	}
	return "kind-" + name
}
