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
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSanitizeContext(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"my-cluster", "my-cluster"},
		{"gke_project_us-central1_cluster", "gke_project_us-central1_cluster"},
		{"arn:aws:eks:us-west-2:123456789:cluster/prod", "arn_aws_eks_us-west-2_123456789_cluster_prod"},
		{"", "default"},
	}

	for _, tt := range tests {
		got := SanitizeContext(tt.input)
		if got != tt.want {
			t.Errorf("SanitizeContext(%q) = %q; want %q", tt.input, got, tt.want)
		}
	}
}

func TestPortForwardRegex(t *testing.T) {
	cases := []struct {
		log  string
		port string
	}{
		{"Forwarding from 127.0.0.1:54947 -> 8080\n", "54947"},
		{"Forwarding from [::1]:61234 -> 8080\n", "61234"},
		{"Some prelude\nForwarding from 127.0.0.1:8080 -> 8080\n", "8080"},
	}

	for _, c := range cases {
		match := portForwardRegex.FindStringSubmatch(c.log)
		if len(match) < 2 || match[1] != c.port {
			t.Errorf("expected port %q from %q, got %v", c.port, c.log, match)
		}
	}
}

func TestEnsureServerURL(t *testing.T) {
	// Healthy tunnels are already recorded for two AX installations in one
	// cluster, so no kubectl runs. The first is recorded without a namespace,
	// as tunnels were before the namespace was part of the file name.
	t.Setenv("AX_HOME", t.TempDir())
	urls := map[string]string{}
	for _, namespace := range []string{"", "ax-remote"} {
		healthz := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		t.Cleanup(healthz.Close)
		port := healthz.Listener.Addr().(*net.TCPAddr).Port
		if err := SaveTunnel(&TunnelInfo{Context: "kind-local", Namespace: namespace, Port: port}); err != nil {
			t.Fatal(err)
		}
		urls[namespace] = fmt.Sprintf("http://127.0.0.1:%d", port)
	}
	// With IgnoreEnv, neither variable decides which installation is reached.
	t.Setenv("AX_SERVER", "http://explicit:8080")
	t.Setenv("AX_SYSTEM_NAMESPACE", "ax-remote")

	// Each namespace has its own tunnel.
	for _, namespace := range []string{"", "ax-remote"} {
		if got, err := EnsureServerURL(Options{Context: "kind-local", Namespace: namespace, IgnoreEnv: true}); err != nil || got != urls[namespace] {
			t.Errorf("EnsureServerURL(namespace %q) = %q, %v; want %q", namespace, got, err, urls[namespace])
		}
	}

	// Stopping the context's tunnels stops both.
	if err := StopTunnelByContext("kind-local"); err != nil {
		t.Fatal(err)
	}
	if tunnels, err := ListTunnels(); err != nil || len(tunnels) != 0 {
		t.Errorf("expected no tunnels after stopping the context's, got %v, error %v", tunnels, err)
	}
}
