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

func TestEnsureServerURLIgnoreEnv(t *testing.T) {
	// A healthy tunnel is already recorded for the context, so no kubectl runs.
	healthz := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer healthz.Close()
	port := healthz.Listener.Addr().(*net.TCPAddr).Port
	t.Setenv("AX_HOME", t.TempDir())
	if err := SaveTunnel(&TunnelInfo{Context: "remote", Namespace: "ax-system", Port: port}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AX_SERVER", "http://explicit:8080")
	t.Setenv("AX_SYSTEM_NAMESPACE", "other-namespace")

	want := fmt.Sprintf("http://127.0.0.1:%d", port)
	if got, err := EnsureServerURL(Options{Context: "remote", IgnoreEnv: true}); err != nil || got != want {
		t.Errorf("EnsureServerURL() = %q, %v; want %q", got, err, want)
	}
}
