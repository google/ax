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

package axdeploy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/ax/internal/install/executil"
	"github.com/google/ax/internal/tunnel"
)

func TestEnsureDigestPinnedWithBearerChallenge(t *testing.T) {
	const (
		manifestBody = `{"schemaVersion":2}`
		wantDigest   = "sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	)
	var srvURL string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/":
			if r.Header.Get("Authorization") != "Bearer test-jwt" {
				w.Header().Set("Www-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="ghcr.io"`, srvURL))
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusOK)
		case "/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"test-jwt"}`))
		case "/v2/google/ax/ax-task-runner/manifests/v0.1.0":
			if r.Header.Get("Authorization") != "Bearer test-jwt" {
				w.Header().Set("Www-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="ghcr.io",scope="repository:google/ax/ax-task-runner:pull"`, srvURL))
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(manifestBody)))
			w.Header().Set("Docker-Content-Digest", wantDigest)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	srvURL = srv.URL

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	imgRef := u.Host + "/google/ax/ax-task-runner:v0.1.0"
	pinned, err := EnsureDigestPinned(context.Background(), imgRef, srv.Client())
	if err != nil {
		t.Fatalf("EnsureDigestPinned: %v", err)
	}
	if pinned != imgRef+"@"+wantDigest {
		t.Fatalf("got %q, want %q", pinned, imgRef+"@"+wantDigest)
	}
}

func TestEnsureDigestPinnedPlainHTTPLoopback(t *testing.T) {
	const (
		manifestBody = `{"schemaVersion":2}`
		wantDigest   = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/":
			w.WriteHeader(http.StatusOK)
		case "/v2/ax-server/manifests/dev":
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(manifestBody)))
			w.Header().Set("Docker-Content-Digest", wantDigest)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	imgRef := u.Host + "/ax-server:dev"
	pinned, err := EnsureDigestPinned(context.Background(), imgRef, srv.Client())
	if err != nil {
		t.Fatalf("EnsureDigestPinned plain HTTP loopback: %v", err)
	}
	if pinned != imgRef+"@"+wantDigest {
		t.Fatalf("got %q, want %q", pinned, imgRef+"@"+wantDigest)
	}
}

func TestMutateCSVFlagArgsIdempotentAddAndRemove(t *testing.T) {
	base := []string{
		"--port=443",
		"--authz-bootstrap-owners=system:serviceaccount:ate-system:ate-client",
	}
	added, changed := mutateCSVFlagArgs(base, "--authz-bootstrap-owners", "system:serviceaccount:ax-system:ax-server", true)
	if !changed {
		t.Fatal("expected first add to report changed=true")
	}
	if _, changedAgain := mutateCSVFlagArgs(added, "--authz-bootstrap-owners", "system:serviceaccount:ax-system:ax-server", true); changedAgain {
		t.Fatal("expected repeat add to be idempotent (changed=false)")
	}
	removed, changedRemove := mutateCSVFlagArgs(added, "--authz-bootstrap-owners", "system:serviceaccount:ax-system:ax-server", false)
	if !changedRemove {
		t.Fatal("expected remove to report changed=true")
	}
	if strings.Join(removed, "|") != strings.Join(base, "|") {
		t.Fatalf("got %v, want %v", removed, base)
	}
}

func TestMutateNamespacePolicyYAML(t *testing.T) {
	initial := "policies: []\n"
	withAX, changed, err := mutateNamespacePolicyYAML(initial, "ax-system", true)
	if err != nil || !changed {
		t.Fatalf("add ax-system: changed=%v err=%v", changed, err)
	}
	if !strings.Contains(withAX, "ax.io/credential: \"true\"") || !strings.Contains(withAX, "- ax-system") {
		t.Fatalf("unexpected policy after add:\n%s", withAX)
	}
	if _, changed2, err := mutateNamespacePolicyYAML(withAX, "ax-system", true); err != nil || changed2 {
		t.Fatalf("expected repeat add to be no-op: changed=%v err=%v", changed2, err)
	}
	afterRevoke, changed3, err := mutateNamespacePolicyYAML(withAX, "ax-system", false)
	if err != nil || !changed3 {
		t.Fatalf("expected revoke to change policy: changed=%v err=%v", changed3, err)
	}
	if strings.Contains(afterRevoke, "ax-system") || !strings.Contains(afterRevoke, "- default") {
		t.Fatalf("unexpected policy after revoke:\n%s", afterRevoke)
	}
}

func TestEnsureDigestPinnedValidatesReferences(t *testing.T) {
	const validDigest = "sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	for _, alreadyPinned := range []string{
		"ghcr.io/google/ax/ax-task-runner@" + validDigest,
		"ghcr.io/google/ax/ax-task-runner:v0.1.0@" + validDigest,
	} {
		got, err := EnsureDigestPinned(context.Background(), alreadyPinned, nil)
		if err != nil || got != alreadyPinned {
			t.Fatalf("EnsureDigestPinned(%q) = (%q, %v), want (%q, nil)", alreadyPinned, got, err, alreadyPinned)
		}
	}

	for _, bad := range []string{
		"",
		"@" + validDigest,
		"ghcr.io/google/ax/ax-task-runner:@" + validDigest,
		"ghcr.io/google/ax/ax-task-runner@sha256:not64hex",
		"ghcr.io/google/ax/ax-task-runner:v1\nevil: true",
	} {
		if _, err := EnsureDigestPinned(context.Background(), bad, nil); err == nil {
			t.Fatalf("EnsureDigestPinned(%q) succeeded, want validation error", bad)
		}
	}
}

type fakeExecRunner struct {
	calls [][]string
	runFn func(args []string) (executil.Result, error)
}

func (f *fakeExecRunner) LookPath(file string) (string, error) { return "/usr/bin/" + file, nil }
func (f *fakeExecRunner) Run(_ context.Context, spec executil.Spec) (executil.Result, error) {
	f.calls = append(f.calls, append([]string{spec.Name}, spec.Args...))
	return f.runFn(spec.Args)
}

func TestEnsurePostgresSecretCreateOnlySemantics(t *testing.T) {
	t.Run("transient_get_error_does_not_overwrite", func(t *testing.T) {
		r := &fakeExecRunner{
			runFn: func(_ []string) (executil.Result, error) {
				return executil.Result{Stderr: "Unable to connect to the server: i/o timeout"}, errors.New("exit status 1")
			},
		}
		k := &kubectlClient{runner: r, kubectl: "kubectl"}
		if err := ensurePostgresSecret(context.Background(), k, "ax-system"); err == nil {
			t.Fatal("expected error on transient get failure, got nil")
		}
		if len(r.calls) != 1 {
			t.Fatalf("expected exactly 1 kubectl call (no create/apply), got %d: %v", len(r.calls), r.calls)
		}
	})

	t.Run("not_found_uses_create_not_apply", func(t *testing.T) {
		r := &fakeExecRunner{
			runFn: func(args []string) (executil.Result, error) {
				if len(args) >= 3 && args[2] == "get" {
					return executil.Result{Stderr: `Error from server (NotFound): secrets "ax-postgres" not found`}, errors.New("exit status 1")
				}
				return executil.Result{}, nil
			},
		}
		k := &kubectlClient{runner: r, kubectl: "kubectl"}
		if err := ensurePostgresSecret(context.Background(), k, "ax-system"); err != nil {
			t.Fatalf("ensurePostgresSecret: %v", err)
		}
		if len(r.calls) != 2 || r.calls[1][1] != "create" {
			t.Fatalf("expected second call to be `kubectl create -f -`, got %v", r.calls)
		}
	})

	t.Run("already_exists_race_is_ignored", func(t *testing.T) {
		r := &fakeExecRunner{
			runFn: func(args []string) (executil.Result, error) {
				if len(args) >= 3 && args[2] == "get" {
					return executil.Result{Stderr: `Error from server (NotFound): secrets "ax-postgres" not found`}, errors.New("exit status 1")
				}
				return executil.Result{Stderr: `Error from server (AlreadyExists): secrets "ax-postgres" already exists`}, errors.New("exit status 1")
			},
		}
		k := &kubectlClient{runner: r, kubectl: "kubectl"}
		if err := ensurePostgresSecret(context.Background(), k, "ax-system"); err != nil {
			t.Fatalf("expected AlreadyExists race to return nil, got %v", err)
		}
	})
}

func TestReconcileCredProviderPolicyUsesMergePatch(t *testing.T) {
	r := &fakeExecRunner{
		runFn: func(args []string) (executil.Result, error) {
			// Both exists() and get -o json pass "get".
			if len(args) >= 3 && args[2] == "get" {
				return executil.Result{Stdout: `{"data":{"namespace-policy.yaml":"policies: []\n","extra-key":"keep-me"}}`}, nil
			}
			return executil.Result{}, nil
		},
	}
	k := &kubectlClient{runner: r, kubectl: "kubectl"}
	changed, present, err := reconcileCredProviderPolicy(context.Background(), k, "ax-system", true)
	if err != nil || !present || !changed {
		t.Fatalf("reconcileCredProviderPolicy = (changed=%v, present=%v, err=%v), want (true, true, nil)", changed, present, err)
	}
	lastCall := strings.Join(r.calls[len(r.calls)-1], " ")
	if !strings.Contains(lastCall, "patch configmap k8s-credential-provider-namespace-policy --type=merge") {
		t.Fatalf("expected merge patch on ConfigMap, got %q", lastCall)
	}
}

func TestResolveImageInputsGateAndEnvFallback(t *testing.T) {
	t.Setenv("AX_SERVER_IMAGE", "")
	t.Setenv("AX_DEFAULT_TASK_IMAGE", "")

	if _, _, err := ResolveImageInputs("", ""); err == nil || !strings.Contains(err.Error(), "--server-image") {
		t.Fatalf("expected --server-image gate error when unset, got: %v", err)
	}

	srvOnly, taskEmpty, err := ResolveImageInputs("ghcr.io/google/ax/ax-server:v0.1.0", "")
	if err != nil || srvOnly != "ghcr.io/google/ax/ax-server:v0.1.0" || taskEmpty != "" {
		t.Fatalf("expected server-only input to succeed with optional empty task image, got (%q, %q, %v)", srvOnly, taskEmpty, err)
	}

	t.Setenv("AX_SERVER_IMAGE", "ghcr.io/google/ax/ax-server:v0.1.0")
	t.Setenv("AX_DEFAULT_TASK_IMAGE", "ghcr.io/agentexecutor/claude-code:v1")
	srv, task, err := ResolveImageInputs("", "")
	if err != nil {
		t.Fatalf("ResolveImageInputs from env: %v", err)
	}
	if srv != "ghcr.io/google/ax/ax-server:v0.1.0" || task != "ghcr.io/agentexecutor/claude-code:v1" {
		t.Fatalf("got (%q, %q)", srv, task)
	}
}

func TestStopTunnel(t *testing.T) {
	// One kube context has a tunnel to each of two AX installations.
	t.Setenv("AX_HOME", t.TempDir())
	for _, ns := range []string{"ax-system", "ax-remote"} {
		if err := tunnel.SaveTunnel(&tunnel.TunnelInfo{Context: "kind-local", Namespace: ns}); err != nil {
			t.Fatal(err)
		}
	}

	stopTunnel("kind-local", "ax-remote")

	tunnels, err := tunnel.ListTunnels()
	if err != nil || len(tunnels) != 1 || tunnels[0].Namespace != "ax-system" {
		t.Errorf("expected only the tunnel to ax-system to be left, got %+v, error %v", tunnels, err)
	}
}
