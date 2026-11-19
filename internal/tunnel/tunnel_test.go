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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
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
	// Without FromEnv, neither variable decides which installation is reached.
	t.Setenv("AX_SERVER", "http://explicit:8080")
	t.Setenv("AX_SYSTEM_NAMESPACE", "ax-remote")

	// Each namespace has its own tunnel.
	for _, namespace := range []string{"", "ax-remote"} {
		if got, err := EnsureServerURL(context.Background(), Options{Context: "kind-local", Namespace: namespace}); err != nil || got != urls[namespace] {
			t.Errorf("EnsureServerURL(namespace %q) = %q, %v; want %q", namespace, got, err, urls[namespace])
		}
	}

	// Even with FromEnv, an explicit Context beats AX_SERVER, while an empty
	// Namespace falls back to AX_SYSTEM_NAMESPACE.
	if got, err := EnsureServerURL(context.Background(), Options{Context: "kind-local", FromEnv: true}); err != nil || got != urls["ax-remote"] {
		t.Errorf("EnsureServerURL(explicit context, FromEnv) = %q, %v; want %q", got, err, urls["ax-remote"])
	}

	// Stopping the context's tunnels stops both.
	if err := StopTunnelByContext("kind-local"); err != nil {
		t.Fatal(err)
	}
	if tunnels, err := ListTunnels(); err != nil || len(tunnels) != 0 {
		t.Errorf("expected no tunnels after stopping the context's, got %v, error %v", tunnels, err)
	}
}

func TestEnsureServerURLNoContextAndFallback(t *testing.T) {
	t.Setenv("AX_HOME", t.TempDir())
	t.Setenv("AX_SERVER", "")
	t.Setenv("KUBECONTEXT", "")
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("PATH", t.TempDir())

	if got, err := EnsureServerURL(context.Background(), Options{}); !errors.Is(err, ErrNoContext) {
		t.Fatalf("EnsureServerURL() without FallbackURL = %q, %v; want ErrNoContext", got, err)
	}
	if got, err := EnsureServerURL(context.Background(), Options{FallbackURL: "http://localhost:8080"}); err != nil || got != "http://localhost:8080" {
		t.Fatalf("EnsureServerURL() with FallbackURL = %q, %v; want http://localhost:8080", got, err)
	}
}

// The files in ~/.ax/tunnels are read and written by every binary built from
// this package, at whatever version each was built, so their names and JSON
// keys cannot change without breaking the others.
func TestTunnelStateFileFormat(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AX_HOME", home)
	for _, tc := range []struct{ namespace, file string }{
		{"", "kind-local.json"},
		{"ax-system", "kind-local.json"},
		{"ax-remote", "kind-local.ax-remote.json"},
	} {
		if err := SaveTunnel(&TunnelInfo{Context: "kind-local", Namespace: tc.namespace, Service: "ax-server", Port: 1, PID: 2}); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(home, "tunnels", tc.file))
		if err != nil {
			t.Errorf("namespace %q: %v", tc.namespace, err)
			continue
		}
		var fields map[string]any
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		var keys []string
		for k := range fields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if got, want := fmt.Sprint(keys), "[context created_at namespace pid port service]"; got != want {
			t.Errorf("namespace %q: keys of %s = %s, want %s", tc.namespace, tc.file, got, want)
		}
	}
}

func TestEnsureServerURLWithoutKubectl(t *testing.T) {
	// No tunnel is recorded, so one has to be started, and no kubectl is
	// installed. With no resolver given, nothing is downloaded.
	home := t.TempDir()
	t.Setenv("AX_HOME", home)
	t.Setenv("PATH", t.TempDir())
	_, err := EnsureServerURL(context.Background(), Options{Context: "kind-local"})
	if !errors.Is(err, ErrKubectlNotFound) {
		t.Fatalf("EnsureServerURL() error = %v, want ErrKubectlNotFound", err)
	}
	if _, err := os.Stat(filepath.Join(home, "bin", "kubectl")); !os.IsNotExist(err) {
		t.Errorf("kubectl was installed into AX_HOME: stat error %v", err)
	}
}

func TestEnsureServerURLCancel(t *testing.T) {
	// kubectl never reports a forwarded port. Cancelling the context returns
	// before the 5 s wait for the port ends, and records no tunnel.
	t.Setenv("AX_HOME", t.TempDir())
	bin := t.TempDir()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := EnsureServerURL(ctx, Options{Context: "kind-local"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("EnsureServerURL() error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("EnsureServerURL() returned after %v, want soon after the 200ms deadline", elapsed)
	}
	if tunnels, err := ListTunnels(); err != nil || len(tunnels) != 0 {
		t.Errorf("ListTunnels() = %v, %v; want no tunnel recorded", tunnels, err)
	}
}

func TestEnsureServerURLUsesKubectlResolver(t *testing.T) {
	// The caller's resolver decides which kubectl runs, and its error is
	// returned as is.
	t.Setenv("AX_HOME", t.TempDir())
	want := errors.New("download failed")
	called := false
	_, err := EnsureServerURL(context.Background(), Options{
		Context: "kind-local",
		Kubectl: func(context.Context) (string, error) {
			called = true
			return "", want
		},
	})
	if !called || !errors.Is(err, want) {
		t.Fatalf("EnsureServerURL() called resolver = %v, error = %v; want the resolver's error", called, err)
	}
}
