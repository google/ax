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

package axclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/google/ax/internal/tunnel"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// isolate points the package at an empty ~/.ax and an environment that names
// no server, namespace or context.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("AX_HOME", t.TempDir())
	t.Setenv("AX_SERVER", "")
	t.Setenv("AX_SYSTEM_NAMESPACE", "")
	t.Setenv("KUBECONTEXT", "")
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "kubeconfig"))
}

func TestServerURL(t *testing.T) {
	isolate(t)
	// A healthy tunnel is recorded for the context, so no kubectl runs.
	healthz := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(healthz.Close)
	port := healthz.Listener.Addr().(*net.TCPAddr).Port
	if err := tunnel.SaveTunnel(&tunnel.TunnelInfo{Context: "kind-local", Namespace: "ax-system", Service: "ax-server", Port: port}); err != nil {
		t.Fatal(err)
	}
	tunnelURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	t.Setenv("AX_SERVER", "http://from-env:8080")

	for _, tc := range []struct {
		name string
		opts Options
		want string
	}{
		{"explicit URL", Options{ServerURL: "http://explicit:8080", Context: "kind-local"}, "http://explicit:8080"},
		{"AX_SERVER", Options{Context: "kind-local"}, "http://from-env:8080"},
		{"recorded tunnel", Options{Context: "kind-local", IgnoreEnv: true}, tunnelURL},
	} {
		got, err := ServerURL(context.Background(), tc.opts)
		if err != nil || got != tc.want {
			t.Errorf("%s: ServerURL() = %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
}

func TestServerURLDoesNotDownloadKubectl(t *testing.T) {
	isolate(t)
	t.Setenv("PATH", t.TempDir())
	_, err := ServerURL(context.Background(), Options{Context: "kind-local"})
	if !errors.Is(err, ErrKubectlNotFound) {
		t.Fatalf("ServerURL() error = %v, want ErrKubectlNotFound", err)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("AX_HOME"), "bin", "kubectl")); !os.IsNotExist(err) {
		t.Errorf("kubectl was installed into AX_HOME: stat error %v", err)
	}
}

func TestDial(t *testing.T) {
	isolate(t)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	v1alpha1.RegisterAXServer(srv, v1alpha1.UnimplementedAXServer{})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	client, conn, err := Dial(context.Background(), Options{ServerURL: "http://" + lis.Addr().String()})
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	// The stub server answers Unimplemented, which shows the call reached it.
	_, err = client.GetModel(context.Background(), &v1alpha1.GetModelRequest{})
	if got := status.Code(err); got != codes.Unimplemented {
		t.Errorf("GetModel() code = %v (%v), want Unimplemented", got, err)
	}
}

func TestContexts(t *testing.T) {
	isolate(t)
	const kubeconfig = `apiVersion: v1
kind: Config
current-context: remote
contexts:
  - name: remote
    context:
      cluster: gke
      user: me
  - name: ax-local
    context:
      cluster: kind-ax-local
      user: kind-ax-local
`
	if err := os.WriteFile(os.Getenv("KUBECONFIG"), []byte(kubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}

	if got, err := CurrentContext(); err != nil || got != "remote" {
		t.Errorf("CurrentContext() = %q, %v; want %q", got, err, "remote")
	}
	got, err := ListContexts()
	if err != nil {
		t.Fatal(err)
	}
	want := []Context{
		{Name: "ax-local", Cluster: "kind-ax-local", User: "kind-ax-local"},
		{Name: "remote", Cluster: "gke", User: "me", Current: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListContexts() = %+v, want %+v", got, want)
	}
}
