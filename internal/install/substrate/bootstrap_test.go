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
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/google/ax/internal/install/executil"
	"gopkg.in/yaml.v3"
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

// funcRunner fakes kubectl with a function, and records the commands.
type funcRunner struct {
	calls []string
	run   func(cmd string) (executil.Result, error)
}

func (f *funcRunner) LookPath(file string) (string, error) { return "/usr/bin/" + file, nil }

func (f *funcRunner) Run(_ context.Context, spec executil.Spec) (executil.Result, error) {
	cmd := strings.Join(spec.Args, " ")
	f.calls = append(f.calls, cmd)
	return f.run(cmd)
}

// envoyLivenessProbe decodes manifest and returns the livenessProbe of the
// envoy container on Deployment name.
func envoyLivenessProbe(t *testing.T, manifest []byte, name string) map[string]any {
	t.Helper()
	dec := yaml.NewDecoder(bytes.NewReader(manifest))
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("decoding manifest: %v", err)
		}
		meta, _ := doc["metadata"].(map[string]any)
		if doc["kind"] != "Deployment" || meta["name"] != name {
			continue
		}
		spec, _ := doc["spec"].(map[string]any)
		tmpl, _ := spec["template"].(map[string]any)
		podSpec, _ := tmpl["spec"].(map[string]any)
		containers, _ := podSpec["containers"].([]any)
		for _, c := range containers {
			cm, _ := c.(map[string]any)
			if cm["name"] == "envoy" {
				probe, _ := cm["livenessProbe"].(map[string]any)
				return probe
			}
		}
	}
	t.Fatalf("Deployment %q or its envoy container not found", name)
	return nil
}

// https://github.com/google/ax/issues/697: RenderSystemBundle and
// RenderEgressBundle stamp a livenessProbe on /ready (port envoy-metrics) onto
// the envoy sidecar of atenet-router and atenet-egress, without clobbering one
// if upstream already defines it.
func TestRenderBundlesAddEnvoyLivenessProbe(t *testing.T) {
	rawSystem, err := embeddedManifests.ReadFile("manifests/05-ate-system.yaml")
	if err != nil {
		t.Fatalf("ReadFile 05-ate-system.yaml: %v", err)
	}
	renderedSystem, err := RenderSystemBundle(rawSystem, "deadbeef1234", "ax-system")
	if err != nil {
		t.Fatalf("RenderSystemBundle: %v", err)
	}

	rawEgress, err := embeddedManifests.ReadFile("manifests/07-atenet-egress.yaml")
	if err != nil {
		t.Fatalf("ReadFile 07-atenet-egress.yaml: %v", err)
	}
	renderedEgress, err := RenderEgressBundle(rawEgress)
	if err != nil {
		t.Fatalf("RenderEgressBundle: %v", err)
	}

	for _, tc := range []struct {
		name     string
		manifest []byte
	}{
		{name: "atenet-router", manifest: renderedSystem},
		{name: "atenet-egress", manifest: renderedEgress},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := envoyLivenessProbe(t, tc.manifest, tc.name)
			if probe == nil {
				t.Fatalf("envoy container on %s has no livenessProbe", tc.name)
			}
			httpGet, _ := probe["httpGet"].(map[string]any)
			if httpGet["path"] != "/ready" || httpGet["port"] != "envoy-metrics" {
				t.Errorf("httpGet = %v, want path=/ready port=envoy-metrics", httpGet)
			}
			if probe["initialDelaySeconds"] != 30 || probe["periodSeconds"] != 10 || probe["failureThreshold"] != 6 {
				t.Errorf("probe thresholds = %v, want initialDelaySeconds=30 periodSeconds=10 failureThreshold=6", probe)
			}
		})
	}

	// An upstream manifest that already defines a livenessProbe keeps its own.
	const customEgress = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: atenet-egress
spec:
  template:
    spec:
      containers:
      - name: envoy
        livenessProbe:
          httpGet:
            path: /custom
            port: envoy-metrics
          periodSeconds: 20
`
	kept, err := RenderEgressBundle([]byte(customEgress))
	if err != nil {
		t.Fatalf("RenderEgressBundle(custom): %v", err)
	}
	probe := envoyLivenessProbe(t, kept, "atenet-egress")
	httpGet, _ := probe["httpGet"].(map[string]any)
	if httpGet["path"] != "/custom" || probe["periodSeconds"] != 20 {
		t.Errorf("existing livenessProbe was overwritten: got %v", probe)
	}
}

// https://github.com/google/ax/issues/697: alreadyHealthy looked at
// atenet-router but not at atenet-egress, whose envoy container gets stuck
// the same way, and reported the cluster healthy.
func TestAlreadyHealthyRequiresEveryWorkload(t *testing.T) {
	healthy := func(unready string) bool {
		r := &funcRunner{run: func(cmd string) (executil.Result, error) {
			switch {
			case strings.Contains(cmd, "get namespace"):
				return executil.Result{Stdout: "v1.2.3"}, nil
			case strings.Contains(cmd, "get deployment ax-workers"):
				return executil.Result{Stdout: "1"}, nil
			case unready != "" && strings.Contains(cmd, "wait --for=condition=Ready") && strings.Contains(cmd, unready):
				return executil.Result{}, fmt.Errorf("timed out waiting for the condition on %s", unready)
			}
			return executil.Result{}, nil
		}}
		k := &kubeHelper{runner: r, kubectl: "kubectl", context: "ax-local"}
		return k.alreadyHealthy(context.Background(), "v1.2.3", 1)
	}
	if !healthy("") {
		t.Fatal("alreadyHealthy = false with every pod Ready")
	}
	for _, unready := range []string{
		"pod/postgres-0",
		"app=podcertificate-controller",
		"app=ate-api-server",
		"app=ate-idp-server",
		"app=ate-controller",
		"app=atenet-router",
		"app=atenet-egress",
		"app=k8s-credential-provider",
		"app=atelet",
		"ate.dev/worker-pool=ax-workers",
	} {
		if healthy(unready) {
			t.Errorf("alreadyHealthy = true while %s is not Ready", unready)
		}
	}
}
