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

// Package deploy embeds the canonical AX Kubernetes manifests and renders them
// for a target namespace and resolved container image set.
package deploy

import (
	_ "embed"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"github.com/google/go-containerregistry/pkg/name"
)

//go:embed postgres.yaml
var rawPostgresYAML string

//go:embed ax-server.yaml
var rawServerYAML string

var dns1123LabelRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// ServerConfig configures rendering of deploy/ax-server.yaml.
type ServerConfig struct {
	Namespace        string
	ServerImage      string
	DefaultTaskImage string
}

// ValidateNamespace checks that ns is a valid Kubernetes namespace name.
func ValidateNamespace(ns string) error {
	if ns == "" {
		return fmt.Errorf("namespace must not be empty")
	}
	if len(ns) > 63 || !dns1123LabelRe.MatchString(ns) {
		return fmt.Errorf("invalid Kubernetes namespace %q: must be a DNS-1123 label", ns)
	}
	return nil
}

// ClusterRoleName returns the ClusterRole / ClusterRoleBinding name used for
// an AX installation in namespace. Keeping "ax-server" for the default
// namespace preserves compatibility with deploy/ax-server.yaml, while suffixing
// non-default namespaces avoids collisions across multi-namespace installs on
// the same cluster.
func ClusterRoleName(namespace string) string {
	if namespace == "" || namespace == v1alpha1.DefaultSystemNamespace {
		return "ax-server"
	}
	return "ax-server-" + namespace
}

// RenderPostgres renders deploy/postgres.yaml for the target namespace.
func RenderPostgres(namespace string) ([]byte, error) {
	if namespace == "" {
		namespace = v1alpha1.DefaultSystemNamespace
	}
	if err := ValidateNamespace(namespace); err != nil {
		return nil, err
	}
	rendered := rewriteNamespace(rawPostgresYAML, namespace)
	return []byte(rendered), nil
}

// RenderServer renders deploy/ax-server.yaml with resolved image references
// and target namespace wiring.
func RenderServer(cfg ServerConfig) ([]byte, error) {
	ns := cfg.Namespace
	if ns == "" {
		ns = v1alpha1.DefaultSystemNamespace
	}
	if err := ValidateNamespace(ns); err != nil {
		return nil, err
	}
	srvImg := strings.TrimSpace(cfg.ServerImage)
	if srvImg == "" {
		return nil, fmt.Errorf("server image must not be empty")
	}
	if _, err := name.ParseReference(srvImg); err != nil {
		return nil, fmt.Errorf("invalid server image %q: %w", cfg.ServerImage, err)
	}
	taskImg := strings.TrimSpace(cfg.DefaultTaskImage)
	if taskImg == "" {
		return nil, fmt.Errorf("default task image must not be empty")
	}
	if _, err := name.NewDigest(taskImg); err != nil {
		return nil, fmt.Errorf("default task image %q must be digest-pinned (@sha256:...): %w", cfg.DefaultTaskImage, err)
	}

	rendered := rawServerYAML
	rendered = strings.ReplaceAll(rendered, "${AX_SERVER_IMAGE}", srvImg)
	rendered = strings.ReplaceAll(rendered, "${AX_DEFAULT_TASK_IMAGE}", taskImg)
	rendered = rewriteNamespace(rendered, ns)

	if ns != v1alpha1.DefaultSystemNamespace {
		rendered = strings.ReplaceAll(
			rendered,
			"ax-postgres.ax-system.svc.cluster.local:5432",
			fmt.Sprintf("ax-postgres.%s.svc.cluster.local:5432", ns),
		)
		rendered = strings.ReplaceAll(
			rendered,
			"--template-atespace=ax-system",
			"--template-atespace="+ns,
		)
		roleName := ClusterRoleName(ns)
		rendered = strings.Replace(
			rendered,
			"kind: ClusterRole\nmetadata:\n  name: ax-server\n",
			fmt.Sprintf("kind: ClusterRole\nmetadata:\n  name: %s\n", roleName),
			1,
		)
		rendered = strings.Replace(
			rendered,
			"kind: ClusterRoleBinding\nmetadata:\n  name: ax-server\n",
			fmt.Sprintf("kind: ClusterRoleBinding\nmetadata:\n  name: %s\n", roleName),
			1,
		)
		rendered = strings.Replace(
			rendered,
			"  kind: ClusterRole\n  name: ax-server\n",
			fmt.Sprintf("  kind: ClusterRole\n  name: %s\n", roleName),
			1,
		)
	}
	return []byte(rendered), nil
}

func rewriteNamespace(manifest, namespace string) string {
	if namespace == v1alpha1.DefaultSystemNamespace {
		return manifest
	}
	out := strings.Replace(
		manifest,
		"kind: Namespace\nmetadata:\n  name: ax-system\n",
		fmt.Sprintf("kind: Namespace\nmetadata:\n  name: %s\n", namespace),
		1,
	)
	out = strings.ReplaceAll(out, "  namespace: ax-system\n", fmt.Sprintf("  namespace: %s\n", namespace))
	out = strings.ReplaceAll(out, "    namespace: ax-system\n", fmt.Sprintf("    namespace: %s\n", namespace))
	return out
}
