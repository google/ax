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

package substrate

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestEmbeddedManifestsMatchGoModVersion(t *testing.T) {
	goModBytes, err := os.ReadFile(filepath.Join("..", "..", "..", "go.mod"))
	if err != nil {
		t.Fatalf("reading go.mod: %v", err)
	}
	re := regexp.MustCompile(`(?m)^\s*go\.ate\.dev/substrate\s+(\S+)`)
	m := re.FindSubmatch(goModBytes)
	if len(m) < 2 {
		t.Fatalf("could not find go.ate.dev/substrate in go.mod")
	}
	want := string(m[1])
	got := EmbeddedVersion()
	if got != want {
		t.Fatalf("embedded Substrate manifests version = %q, go.mod wants %q; run ./hack/sync-substrate-manifests.sh", got, want)
	}
}

func TestEmbeddedManifestsContainNoUnresolvedPlaceholders(t *testing.T) {
	entries, err := embeddedManifests.ReadDir("manifests")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	yamlCount := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		yamlCount++
		raw, err := embeddedManifests.ReadFile("manifests/" + e.Name())
		if err != nil {
			t.Fatalf("ReadFile %s: %v", e.Name(), err)
		}
		s := string(raw)
		for _, bad := range []string{"ko://go.ate.dev/substrate", "${ENVOY_DATAPLANE_IMAGE}", "#ATE_EGRESS_INJECT_FLAGS"} {
			if strings.Contains(s, bad) {
				t.Fatalf("manifest %s still contains unresolved %q", e.Name(), bad)
			}
		}
	}
	if yamlCount != 8 {
		t.Fatalf("expected 8 embedded YAML bundles, got %d", yamlCount)
	}
}

func TestLocalRegistryEndpoint(t *testing.T) {
	// The embedded manifests name the registry as <host>:<port>.
	endpoint, err := LocalRegistryEndpoint()
	if err != nil {
		t.Fatalf("LocalRegistryEndpoint: %v", err)
	}
	if host, port, err := net.SplitHostPort(endpoint); err != nil || host == "" || port == "" {
		t.Fatalf("LocalRegistryEndpoint() = %q, want <host>:<port> (error: %v)", endpoint, err)
	}

	manifest := func(daemonSet string, args ...string) []byte {
		var b strings.Builder
		fmt.Fprintf(&b, "kind: Deployment\nmetadata:\n  name: atelet\n---\nkind: DaemonSet\nmetadata:\n  name: %s\n", daemonSet)
		b.WriteString("spec:\n  template:\n    spec:\n      containers:\n      - name: atelet\n        args:\n")
		for _, arg := range args {
			fmt.Fprintf(&b, "        - %s\n", arg)
		}
		return []byte(b.String())
	}
	for _, tc := range []struct {
		name     string
		manifest []byte
		want     string
	}{
		{name: "flag set", manifest: manifest("atelet", "--log-level=debug", "--localhost-registry-replacement=my-registry:5000"), want: "my-registry:5000"},
		{name: "flag not set", manifest: manifest("atelet", "--log-level=debug")},
		{name: "flag on another DaemonSet", manifest: manifest("other", "--localhost-registry-replacement=my-registry:5000")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := localRegistryEndpoint(tc.manifest)
			if err != nil || got != tc.want {
				t.Errorf("localRegistryEndpoint() = %q, %v, want %q", got, err, tc.want)
			}
		})
	}
}

func TestRenderSystemBundleStampsAuthzAndEnvHash(t *testing.T) {
	raw, err := embeddedManifests.ReadFile("manifests/05-ate-system.yaml")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	rendered, err := RenderSystemBundle(raw, "deadbeef1234", "ax-custom")
	if err != nil {
		t.Fatalf("RenderSystemBundle: %v", err)
	}
	s := string(rendered)
	for _, want := range []string{
		"ate.dev/env-hash: deadbeef1234",
		"--authz-bootstrap-owners=system:serviceaccount:ate-system:ate-client,system:serviceaccount:ax-custom:ax-server",
		"--ingress-allowed-spiffe-ids=spiffe://cluster.local/ns/ax-custom/sa/ax-server",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("expected %q in rendered system bundle", want)
		}
	}
	if strings.Contains(s, "replicas: 2") {
		t.Fatalf("expected local system bundle to scale multi-replica control-plane Deployments down to 1 replica")
	}
}
