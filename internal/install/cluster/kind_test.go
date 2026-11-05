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
	withKVM := BuildKindConfig(true)
	for _, want := range []string{
		"ClusterTrustBundle: true",
		"ClusterTrustBundleProjection: true",
		"PodCertificateRequest: true",
		`"certificates.k8s.io/v1beta1": "true"`,
		"serializeImagePulls: false",
		"hostPath: /dev/kvm",
	} {
		if !strings.Contains(withKVM, want) {
			t.Fatalf("missing %q in BuildKindConfig(true):\n%s", want, withKVM)
		}
	}

	withoutKVM := BuildKindConfig(false)
	if strings.Contains(withoutKVM, "/dev/kvm") {
		t.Fatalf("unexpected /dev/kvm in BuildKindConfig(false):\n%s", withoutKVM)
	}
}

func TestKindProviderUpCreatesClusterAndLoopbackRegistry(t *testing.T) {
	t.Setenv("AX_HOME", t.TempDir())
	t.Setenv("CONTAINER_CLI", "")

	rr := &recordedRunner{
		runFn: func(spec executil.Spec) (executil.Result, error) {
			cmd := spec.Name + " " + strings.Join(spec.Args, " ")
			switch {
			case strings.HasSuffix(cmd, "kind version"):
				return executil.Result{Stdout: "kind v0.32.0\n"}, nil
			case strings.Contains(cmd, "inspect -f {{.State.Running}} kind-registry"):
				return executil.Result{}, fmt.Errorf("no such container")
			case strings.HasSuffix(cmd, "kind get clusters"):
				return executil.Result{Stdout: ""}, nil
			case strings.Contains(cmd, "kind get nodes"):
				return executil.Result{Stdout: "local-control-plane\n"}, nil
			default:
				return executil.Result{}, nil
			}
		},
	}

	p := NewKindProvider(rr, &bytes.Buffer{})
	p.ProbeKVM = func(context.Context, executil.ContainerRuntime) bool { return true }

	info, err := p.Up(context.Background(), UpOptions{Name: "local", WithRegistry: true})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if info.KubeContext != "local" {
		t.Fatalf("KubeContext = %q, want %q", info.KubeContext, "local")
	}
	if info.RegistryAddr != "localhost:5001" {
		t.Fatalf("RegistryAddr = %q, want localhost:5001", info.RegistryAddr)
	}

	var sawLoopbackRegistry, sawProxyARP, sawSetContext bool
	for _, c := range rr.calls {
		joined := c.Name + " " + strings.Join(c.Args, " ")
		if strings.Contains(joined, "127.0.0.1:5001:5000") && strings.Contains(joined, "[::1]:5001:5000") {
			if strings.Contains(joined, "0.0.0.0") {
				t.Fatalf("registry bound to 0.0.0.0: %s", joined)
			}
			sawLoopbackRegistry = true
		}
		if strings.Contains(joined, "net.ipv4.conf.all.proxy_arp=1") {
			sawProxyARP = true
		}
		if strings.Contains(joined, "config set-context local --cluster=kind-local --user=kind-local") {
			sawSetContext = true
		}
	}
	if !sawLoopbackRegistry || !sawProxyARP || !sawSetContext {
		t.Fatalf("missing expected calls (registry=%v, proxyARP=%v, setContext=%v)", sawLoopbackRegistry, sawProxyARP, sawSetContext)
	}
}
