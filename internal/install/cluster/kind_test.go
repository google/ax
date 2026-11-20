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

package cluster

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/google/ax/internal/install/executil"
)

type recordedRunner struct {
	calls []executil.Spec
	runFn func(executil.Spec) (executil.Result, error)
}

func (r *recordedRunner) LookPath(name string) (string, error) {
	switch name {
	case "docker", "kind", "kubectl":
		return "/usr/bin/" + name, nil
	default:
		return "", fmt.Errorf("not found: %s", name)
	}
}

func (r *recordedRunner) Run(_ context.Context, spec executil.Spec) (executil.Result, error) {
	r.calls = append(r.calls, spec)
	if r.runFn != nil {
		return r.runFn(spec)
	}
	return executil.Result{}, nil
}

func TestBuildKindConfig(t *testing.T) {
	cfg := BuildKindConfig()
	for _, want := range []string{
		"ClusterTrustBundle: true",
		"ClusterTrustBundleProjection: true",
		"PodCertificateRequest: true",
		`"certificates.k8s.io/v1beta1": "true"`,
		"serializeImagePulls: false",
	} {
		if !strings.Contains(cfg, want) {
			t.Fatalf("missing %q in BuildKindConfig():\n%s", want, cfg)
		}
	}
	if strings.Contains(cfg, "/dev/kvm") {
		t.Fatalf("unexpected /dev/kvm in BuildKindConfig():\n%s", cfg)
	}
}

func TestKindProviderUpCreatesCluster(t *testing.T) {
	t.Setenv("AX_HOME", t.TempDir())
	t.Setenv("CONTAINER_CLI", "")

	rr := &recordedRunner{
		runFn: func(spec executil.Spec) (executil.Result, error) {
			cmd := spec.Name + " " + strings.Join(spec.Args, " ")
			switch {
			case strings.HasSuffix(cmd, "kind version"):
				return executil.Result{Stdout: "kind v0.32.0\n"}, nil
			case strings.HasSuffix(cmd, "kind get clusters"):
				return executil.Result{Stdout: ""}, nil
			case strings.Contains(cmd, "kind get nodes"):
				return executil.Result{Stdout: "ax-local-control-plane\n"}, nil
			default:
				return executil.Result{}, nil
			}
		},
	}

	p := NewKindProvider(rr, &bytes.Buffer{})
	info, err := p.Up(context.Background(), UpOptions{})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if info.KubeContext != "ax-local" {
		t.Fatalf("KubeContext = %q, want %q", info.KubeContext, "ax-local")
	}

	var sawProxyARP, sawSetContext bool
	for _, c := range rr.calls {
		joined := c.Name + " " + strings.Join(c.Args, " ")
		if strings.Contains(joined, "/dev/kvm") {
			t.Fatalf("unexpected /dev/kvm command: %q", joined)
		}
		if strings.Contains(joined, "net.ipv4.conf.all.proxy_arp=1") {
			sawProxyARP = true
		}
		if strings.Contains(joined, "config set-context ax-local --cluster=kind-ax-local --user=kind-ax-local") {
			sawSetContext = true
		}
	}
	if !sawProxyARP || !sawSetContext {
		t.Fatalf("missing expected calls (proxyARP=%v, setContext=%v)", sawProxyARP, sawSetContext)
	}
}

func TestKindProviderProtectsForeignKubeContext(t *testing.T) {
	t.Setenv("AX_HOME", t.TempDir())
	t.Setenv("CONTAINER_CLI", "")

	foreignKubeconfig := `apiVersion: v1
kind: Config
contexts:
- name: ax-local
  context:
    cluster: minikube
    user: minikube
`
	rr := &recordedRunner{
		runFn: func(spec executil.Spec) (executil.Result, error) {
			cmd := spec.Name + " " + strings.Join(spec.Args, " ")
			switch {
			case strings.HasSuffix(cmd, "kind version"):
				return executil.Result{Stdout: "kind v0.32.0\n"}, nil
			case strings.Contains(cmd, "config view -o yaml"):
				return executil.Result{Stdout: foreignKubeconfig}, nil
			default:
				return executil.Result{}, nil
			}
		},
	}

	p := NewKindProvider(rr, &bytes.Buffer{})
	if _, err := p.Up(context.Background(), UpOptions{}); err == nil || !strings.Contains(err.Error(), "minikube") {
		t.Fatalf("expected Up to refuse overwriting foreign ax-local context, got: %v", err)
	}
	for _, c := range rr.calls {
		joined := c.Name + " " + strings.Join(c.Args, " ")
		if strings.Contains(joined, "create cluster") || strings.Contains(joined, "set-context") {
			t.Fatalf("unexpected mutating call when foreign context collided: %q", joined)
		}
	}

	rr.calls = nil
	if err := p.Delete(context.Background(), ""); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	for _, c := range rr.calls {
		joined := c.Name + " " + strings.Join(c.Args, " ")
		if strings.Contains(joined, "delete-context ax-local") {
			t.Fatalf("expected Delete not to remove foreign ax-local context, saw: %q", joined)
		}
	}
}

func TestKindProviderDeletesOwnedKubeContext(t *testing.T) {
	t.Setenv("AX_HOME", t.TempDir())
	t.Setenv("CONTAINER_CLI", "")

	ownedKubeconfig := `apiVersion: v1
kind: Config
current-context: ax-local
contexts:
- name: ax-local
  context:
    cluster: kind-ax-local
    user: kind-ax-local
`
	rr := &recordedRunner{
		runFn: func(spec executil.Spec) (executil.Result, error) {
			cmd := spec.Name + " " + strings.Join(spec.Args, " ")
			switch {
			case strings.HasSuffix(cmd, "kind version"):
				return executil.Result{Stdout: "kind v0.32.0\n"}, nil
			case strings.Contains(cmd, "config view -o yaml"):
				return executil.Result{Stdout: ownedKubeconfig}, nil
			default:
				return executil.Result{}, nil
			}
		},
	}

	p := NewKindProvider(rr, &bytes.Buffer{})
	if err := p.Delete(context.Background(), ""); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	sawDeleteCtx := false
	sawUnsetCurrent := false
	for _, c := range rr.calls {
		joined := c.Name + " " + strings.Join(c.Args, " ")
		if strings.Contains(joined, "config delete-context ax-local") {
			sawDeleteCtx = true
		}
		if strings.Contains(joined, "config unset current-context") {
			sawUnsetCurrent = true
		}
	}
	if !sawDeleteCtx {
		t.Fatal("expected Delete to remove owned ax-local context")
	}
	if !sawUnsetCurrent {
		t.Fatal("expected Delete to unset current-context when active context was deleted")
	}
}

func TestKindProviderStatusSysctlWarning(t *testing.T) {
	t.Setenv("AX_HOME", t.TempDir())
	t.Setenv("CONTAINER_CLI", "")

	rr := &recordedRunner{
		runFn: func(spec executil.Spec) (executil.Result, error) {
			cmd := spec.Name + " " + strings.Join(spec.Args, " ")
			switch {
			case strings.HasSuffix(cmd, "kind version"):
				return executil.Result{Stdout: "kind v0.32.0\n"}, nil
			case strings.Contains(cmd, "kind get nodes"):
				return executil.Result{Stdout: "ax-local-control-plane\n"}, nil
			case strings.Contains(cmd, "inspect -f {{.State.Running}} ax-local-control-plane"):
				return executil.Result{Stdout: "true\n"}, nil
			case strings.Contains(cmd, "get deployment ate-api-server"):
				return executil.Result{Stdout: "/1"}, nil
			case strings.Contains(cmd, "sysctl -n net.ipv4.conf.all.proxy_arp"):
				return executil.Result{Stdout: "0\n"}, nil
			default:
				return executil.Result{}, nil
			}
		},
	}

	p := NewKindProvider(rr, &bytes.Buffer{})
	st, err := p.Status(context.Background(), "ax-local")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Name != "ax-local" || st.KubeContext != "ax-local" {
		t.Fatalf("expected Name=ax-local and KubeContext=ax-local when running, got %+v", st)
	}
	if !strings.Contains(st.SubstrateStatus, "0/1 available") || !strings.Contains(st.SubstrateStatus, "degraded: node sysctls reset") {
		t.Fatalf("expected 0/1 replica ratio and sysctl reset warning in SubstrateStatus, got %q", st.SubstrateStatus)
	}
}

func TestKindProviderStatusNotFound(t *testing.T) {
	t.Setenv("AX_HOME", t.TempDir())
	t.Setenv("CONTAINER_CLI", "")

	rr := &recordedRunner{
		runFn: func(spec executil.Spec) (executil.Result, error) {
			cmd := spec.Name + " " + strings.Join(spec.Args, " ")
			if strings.HasSuffix(cmd, "kind version") {
				return executil.Result{Stdout: "kind v0.32.0\n"}, nil
			}
			return executil.Result{}, nil
		},
	}

	p := NewKindProvider(rr, &bytes.Buffer{})
	st, err := p.Status(context.Background(), "")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.State != StateAbsent || st.Name != "N/A" || st.KubeContext != "N/A" {
		t.Fatalf("expected State=NotFound, Name=N/A, KubeContext=N/A when absent, got %+v", st)
	}
}

// A host whose containers have no outbound network (the friction log's
// workstation): Up still returns the cluster, and says why pulls will fail.
func TestKindProviderUpWarnsWithoutEgress(t *testing.T) {
	t.Setenv("AX_HOME", t.TempDir())
	t.Setenv("CONTAINER_CLI", "")

	for _, tc := range []struct {
		name         string
		curl         executil.Result
		curlFails    bool
		forwarding   string
		wantWarnings []string
	}{
		{
			name:       "forwarding off",
			curl:       executil.Result{ExitCode: 28, Stderr: "curl: (28) Connection timed out after 5001 milliseconds\n"},
			curlFails:  true,
			forwarding: "false\n",
			wantWarnings: []string{
				"Warning: Kind node ax-local-control-plane cannot reach https://ghcr.io/v2/",
				"curl: (28) Connection timed out",
				"IPv4 forwarding off",
			},
		},
		{
			name:         "other cause",
			curl:         executil.Result{ExitCode: 6, Stderr: "curl: (6) Could not resolve host: ghcr.io\n"},
			curlFails:    true,
			forwarding:   "true\n",
			wantWarnings: []string{"Could not resolve host", "proxy that containers do not use"},
		},
		{name: "reachable", forwarding: "true\n"},
		{name: "no curl in the node image", curl: executil.Result{ExitCode: 127}, curlFails: true, forwarding: "false\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := &recordedRunner{
				runFn: func(spec executil.Spec) (executil.Result, error) {
					cmd := spec.Name + " " + strings.Join(spec.Args, " ")
					switch {
					case strings.HasSuffix(cmd, "kind version"):
						return executil.Result{Stdout: "kind v0.32.0\n"}, nil
					case strings.Contains(cmd, "kind get nodes"):
						return executil.Result{Stdout: "ax-local-control-plane\n"}, nil
					case strings.Contains(cmd, "exec ax-local-control-plane curl"):
						if tc.curlFails {
							return tc.curl, fmt.Errorf("exit status %d", tc.curl.ExitCode)
						}
						return executil.Result{}, nil
					case strings.Contains(cmd, "{{.IPv4Forwarding}}"):
						return executil.Result{Stdout: tc.forwarding}, nil
					}
					return executil.Result{}, nil
				},
			}
			var out bytes.Buffer
			p := NewKindProvider(rr, &out)
			if _, err := p.Up(context.Background(), UpOptions{}); err != nil {
				t.Fatalf("Up: %v", err)
			}
			if len(tc.wantWarnings) == 0 && strings.Contains(out.String(), "Warning") {
				t.Fatalf("unexpected warning:\n%s", out.String())
			}
			for _, want := range tc.wantWarnings {
				if !strings.Contains(out.String(), want) {
					t.Errorf("output does not contain %q:\n%s", want, out.String())
				}
			}
		})
	}
}

func TestLoopbackRegistries(t *testing.T) {
	got := loopbackRegistries([]string{
		"localhost:5001/ax-server@sha256:1111111111111111111111111111111111111111111111111111111111111111",
		"localhost:5001/ax-task-runner:latest",
		"127.0.0.1:5002/ax-runner-volume",
		"[::1]:5003/ax-server",
		"localhost/ax-server",
		"ghcr.io/google/ax/ax-server:v1.0.0",
		"registry.example.com:5000/ax-server",
		"busybox",
		"",
	})
	want := []string{"127.0.0.1:5002", "[::1]:5003", "localhost", "localhost:5001"}
	if !slices.Equal(got, want) {
		t.Fatalf("loopbackRegistries() = %q, want %q", got, want)
	}
}

// localRegistryRunner answers as a host with a container my-registry that is
// not yet on the network of the cluster.
func localRegistryRunner(registryRunning bool) *recordedRunner {
	return &recordedRunner{
		runFn: func(spec executil.Spec) (executil.Result, error) {
			cmd := spec.Name + " " + strings.Join(spec.Args, " ")
			switch {
			case strings.HasSuffix(cmd, "kind version"):
				return executil.Result{Stdout: "kind v0.32.0\n"}, nil
			case strings.Contains(cmd, "kind get nodes"):
				return executil.Result{Stdout: "ax-local-control-plane\n"}, nil
			case strings.HasSuffix(cmd, "{{.State.Running}} my-registry"):
				if !registryRunning {
					return executil.Result{}, fmt.Errorf("no such container")
				}
				return executil.Result{Stdout: "true\n"}, nil
			case strings.Contains(cmd, ".NetworkSettings.Networks") && strings.HasSuffix(cmd, "ax-local-control-plane"):
				return executil.Result{Stdout: "kind\n"}, nil
			case strings.Contains(cmd, ".NetworkSettings.Networks") && strings.HasSuffix(cmd, "my-registry"):
				return executil.Result{Stdout: "bridge\n"}, nil
			default:
				return executil.Result{}, nil
			}
		},
	}
}

func TestKindProviderUpServesLoopbackRegistry(t *testing.T) {
	t.Setenv("AX_HOME", t.TempDir())
	t.Setenv("CONTAINER_CLI", "")

	rr := localRegistryRunner(true)
	p := NewKindProvider(rr, &bytes.Buffer{})
	if _, err := p.Up(context.Background(), UpOptions{
		Images: []string{
			"localhost:5001/ax-server@sha256:1111111111111111111111111111111111111111111111111111111111111111",
			"ghcr.io/google/ax/ax-task-runner:v1.0.0",
		},
		LocalRegistry: "my-registry:5050",
	}); err != nil {
		t.Fatalf("Up: %v", err)
	}

	var connected bool
	var hosts string
	for _, c := range rr.calls {
		joined := c.Name + " " + strings.Join(c.Args, " ")
		if strings.HasSuffix(joined, "network connect kind my-registry") {
			connected = true
		}
		if strings.Contains(joined, "exec -i ax-local-control-plane") && strings.HasSuffix(joined, "/etc/containerd/certs.d/localhost:5001") {
			raw, err := io.ReadAll(c.Stdin)
			if err != nil {
				t.Fatalf("reading hosts.toml: %v", err)
			}
			hosts = string(raw)
		}
		if strings.Contains(joined, "certs.d/ghcr.io") {
			t.Fatalf("unexpected registry configuration for a remote registry: %q", joined)
		}
	}
	if !connected {
		t.Errorf("expected my-registry to be connected to the network of the nodes, calls: %v", rr.calls)
	}
	if want := "[host.\"http://my-registry:5050\"]\n"; hosts != want {
		t.Errorf("hosts.toml for localhost:5001 = %q, want %q", hosts, want)
	}
}

func TestKindProviderUpNeedsLocalRegistry(t *testing.T) {
	t.Setenv("AX_HOME", t.TempDir())
	t.Setenv("CONTAINER_CLI", "")

	for _, tc := range []struct {
		name          string
		localRegistry string
		wantErr       []string
	}{
		{
			name:          "registry container not running",
			localRegistry: "my-registry:5050",
			wantErr:       []string{`"my-registry"`, "-p 127.0.0.1:5001:5050 --name my-registry"},
		},
		{name: "no registry for localhost images", wantErr: []string{"localhost:5001", "no registry serves"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := localRegistryRunner(false)
			p := NewKindProvider(rr, &bytes.Buffer{})
			_, err := p.Up(context.Background(), UpOptions{
				Images:        []string{"localhost:5001/ax-server:latest"},
				LocalRegistry: tc.localRegistry,
			})
			for _, want := range tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("Up error = %v, want it to contain %q", err, want)
				}
			}
			for _, c := range rr.calls {
				if joined := c.Name + " " + strings.Join(c.Args, " "); strings.Contains(joined, "create cluster") {
					t.Fatalf("expected no cluster to be created, saw: %q", joined)
				}
			}
		})
	}
}
