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

package tunnel

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestListAndSetCurrentContext(t *testing.T) {
	tmp := t.TempDir()
	kc := filepath.Join(tmp, "kubeconfig")
	initial := `apiVersion: v1
kind: Config
current-context: local
contexts:
  - name: local
    context:
      cluster: kind-local
      user: kind-local
  - name: gke-prod
    context:
      cluster: gke-prod-cluster
      user: gke-prod-user
`
	if err := os.WriteFile(kc, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", kc)
	t.Setenv("KUBECONTEXT", "")

	entries, err := ListContexts("")
	if err != nil {
		t.Fatalf("ListContexts: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}

	if err := SetCurrentContext("gke-prod"); err != nil {
		t.Fatalf("SetCurrentContext: %v", err)
	}
	cur, err := CurrentContext("")
	if err != nil || cur != "gke-prod" {
		t.Fatalf("CurrentContext = %q, %v; want gke-prod", cur, err)
	}

	if err := SetCurrentContext("nonexistent"); err == nil {
		t.Fatal("expected error switching to nonexistent context")
	}
}

func TestRouterTLSCacheUsesClusterFingerprintAndIsolatesListTunnels(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("AX_HOME", filepath.Join(tmp, "ax"))
	kc := filepath.Join(tmp, "kubeconfig")
	t.Setenv("KUBECONFIG", kc)
	t.Setenv("KUBECONTEXT", "")

	writeKC := func(server, caData string) {
		t.Helper()
		content := `apiVersion: v1
kind: Config
current-context: local
contexts:
  - name: local
    context:
      cluster: kind-local
      user: kind-local
clusters:
  - name: kind-local
    cluster:
      server: ` + server + `
      certificate-authority-data: ` + caData + `
`
		if err := os.WriteFile(kc, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	writeKC("https://127.0.0.1:41001", "Y2EtY2x1c3Rlci0x")
	if err := saveCachedRouterTLS("local", "ax-system", "ax-server", &cachedRouterTLS{
		ClusterFingerprint: clusterFingerprint("local"),
		ExpiresAt:          time.Now().Add(10 * time.Minute),
	}); err != nil {
		t.Fatalf("saveCachedRouterTLS cluster 1: %v", err)
	}

	tunnels, err := ListTunnels()
	if err != nil {
		t.Fatalf("ListTunnels: %v", err)
	}
	if len(tunnels) != 0 {
		t.Fatalf("ListTunnels() returned %d entries, want 0 (router TLS cache must not pollute tunnel list)", len(tunnels))
	}

	// Simulate `ax local delete` + `ax local up` recreating context "local" with
	// a new API server port and CA certificate: loadCachedRouterTLS must reject
	// the stale cluster fingerprint, and saving overwrites the single cache file.
	writeKC("https://127.0.0.1:42002", "Y2EtY2x1c3Rlci0y")
	if _, err := loadCachedRouterTLS("local", "ax-system", "ax-server"); err == nil || !strings.Contains(err.Error(), "cluster changed") {
		t.Fatalf("loadCachedRouterTLS after cluster recreate = %v, want cluster changed error", err)
	}
}
