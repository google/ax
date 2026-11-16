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
