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

package e2e_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/google/ax/internal/substrate"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"go.ate.dev/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var runE2E = flag.Bool("e2e", false, "run end-to-end tests against a live Kind/Substrate/AX cluster")

const (
	// ateSystemNamespace is the namespace Substrate's control plane is installed into.
	ateSystemNamespace = "ate-system"
	// ateAPIAudience is the token audience and TLS authority of the Substrate Control API.
	ateAPIAudience = "api.ate-system.svc"

	// axCommandTimeout bounds a single `ax` CLI invocation.
	axCommandTimeout = 2 * time.Minute
	// readyTimeout bounds waiting for port-forwards and health endpoints.
	readyTimeout = 15 * time.Second
)

// suiteEnv holds the connections shared by every test in the suite. It is
// populated once by TestMain.
type suiteEnv struct {
	Kubeconfig        string
	KubeContext       string
	AXSystemNamespace string
	AXBin             string
	AXServerURL       string

	AXClient        v1alpha1.AXClient
	SubstrateClient *substrate.Client

	// axHome isolates ax CLI state (tunnels, config) from the user's ~/.ax.
	axHome  string
	closers []func()
}

var env *suiteEnv

func TestMain(m *testing.M) {
	flag.Parse()
	if !*runE2E && os.Getenv("AX_E2E") != "true" {
		fmt.Println("Skipping AX E2E tests (pass -args --e2e or set AX_E2E=true to run)")
		os.Exit(0)
	}

	var err error
	env, err = newSuiteEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "E2E setup failed: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	env.Close()
	os.Exit(code)
}

// newSuiteEnv verifies the cluster provisioned by hack/setup-e2e-kind.sh is
// healthy and connects to both ax-server and the Substrate Control API.
func newSuiteEnv() (_ *suiteEnv, err error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	repoRoot := filepath.Clean(filepath.Join(wd, "..", ".."))
	repoTmp := filepath.Join(repoRoot, ".tmp")

	// Pick up defaults written by hack/setup-e2e-kind.sh; explicit env vars win.
	if err := loadEnvFileIfUnset(filepath.Join(repoTmp, "e2e.env")); err != nil {
		return nil, err
	}

	clusterName := getenvDefault("KIND_CLUSTER_NAME", "ax-e2e")
	s := &suiteEnv{
		Kubeconfig:        getenvDefault("KUBECONFIG", filepath.Join(repoTmp, "kubeconfig-"+clusterName)),
		KubeContext:       getenvDefault("KUBE_CONTEXT", "kind-"+clusterName),
		AXSystemNamespace: getenvDefault("AX_SYSTEM_NAMESPACE", v1alpha1.DefaultSystemNamespace),
		AXBin:             getenvDefault("AX_BIN", filepath.Join(repoTmp, "bin", "ax")),
	}
	defer func() {
		if err != nil {
			s.Close()
		}
	}()

	suiteTmp, err := os.MkdirTemp("", "ax-e2e-")
	if err != nil {
		return nil, err
	}
	s.closers = append(s.closers, func() { _ = os.RemoveAll(suiteTmp) })
	s.axHome = filepath.Join(suiteTmp, "ax-home")

	if err := s.verifyCluster(); err != nil {
		return nil, err
	}
	if err := ensureAXBinary(repoRoot, s.AXBin); err != nil {
		return nil, err
	}
	if err := s.connectAXServer(); err != nil {
		return nil, err
	}
	if err := s.connectSubstrate(filepath.Join(suiteTmp, "ateapi.token")); err != nil {
		return nil, err
	}
	return s, nil
}

// Close releases all suite resources in reverse order of acquisition.
func (s *suiteEnv) Close() {
	for i := len(s.closers) - 1; i >= 0; i-- {
		s.closers[i]()
	}
	s.closers = nil
}

func (s *suiteEnv) verifyCluster() error {
	const hint = "run ./hack/setup-e2e-kind.sh first"
	for _, ns := range []string{ateSystemNamespace, s.AXSystemNamespace} {
		if _, err := s.kubectl("get", "namespace", ns); err != nil {
			return fmt.Errorf("namespace %q not found (%s): %w", ns, hint, err)
		}
	}
	for ns, deploy := range map[string]string{
		ateSystemNamespace:  "ate-api-server",
		s.AXSystemNamespace: "ax-server",
	} {
		if _, err := s.kubectl("-n", ns, "wait", "--for=condition=Available", "deployment/"+deploy, "--timeout=30s"); err != nil {
			return fmt.Errorf("deployment %s/%s not available (%s): %w", ns, deploy, hint, err)
		}
	}
	return nil
}

// ensureAXBinary builds the ax CLI for the host if it does not already exist.
func ensureAXBinary(repoRoot, path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	cmd := exec.Command("go", "build", "-o", path, "./cmd/ax")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("building ax CLI: %w\n%s", err, out)
	}
	return nil
}

func (s *suiteEnv) connectAXServer() error {
	port, err := s.startPortForward(s.AXSystemNamespace, "svc/ax-server", 8080)
	if err != nil {
		return fmt.Errorf("port-forwarding ax-server: %w", err)
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	s.AXServerURL = "http://" + addr

	if err := waitForHTTPHealth(s.AXServerURL+"/healthz", readyTimeout); err != nil {
		return fmt.Errorf("waiting for ax-server /healthz: %w", err)
	}

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("connecting to ax-server gRPC: %w", err)
	}
	s.closers = append(s.closers, func() { _ = conn.Close() })
	s.AXClient = v1alpha1.NewAXClient(conn)
	return nil
}

// connectSubstrate connects directly to the Substrate Control API so tests can
// verify the Actors and ActorTemplates AX creates on their behalf.
func (s *suiteEnv) connectSubstrate(tokenFile string) error {
	port, err := s.startPortForward(ateSystemNamespace, "svc/api", 443)
	if err != nil {
		return fmt.Errorf("port-forwarding %s/svc/api: %w", ateSystemNamespace, err)
	}

	// Authenticate as ax-server's service account, just like ax-server does.
	token, err := s.kubectl("create", "token", "ax-server", "-n", s.AXSystemNamespace,
		"--audience="+ateAPIAudience, "--duration=1h")
	if err != nil {
		return fmt.Errorf("minting Substrate Control API token: %w", err)
	}
	if err := os.WriteFile(tokenFile, []byte(strings.TrimSpace(token)), 0o600); err != nil {
		return fmt.Errorf("writing Substrate token file: %w", err)
	}

	client, err := substrate.NewClientWithOptions(substrate.ClientOptions{
		Target:      fmt.Sprintf("127.0.0.1:%d", port),
		Authority:   ateAPIAudience,
		InsecureTLS: true,
		TokenFile:   tokenFile,
	})
	if err != nil {
		return fmt.Errorf("connecting to Substrate Control API: %w", err)
	}
	s.closers = append(s.closers, func() { _ = client.Close() })
	s.SubstrateClient = client

	ctx, cancel := context.WithTimeout(context.Background(), readyTimeout)
	defer cancel()
	if err := client.EnsureAtespace(ctx, defaultAtespace); err != nil {
		return fmt.Errorf("verifying Substrate Control API readiness: %w", err)
	}
	return nil
}

// kubectlArgs prefixes args with the suite's kubeconfig and context.
func (s *suiteEnv) kubectlArgs(args ...string) []string {
	return append([]string{"--kubeconfig", s.Kubeconfig, "--context", s.KubeContext}, args...)
}

func (s *suiteEnv) kubectl(args ...string) (string, error) {
	out, err := exec.Command("kubectl", s.kubectlArgs(args...)...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("kubectl %s: %w (%s)", strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return string(out), nil
}

// startPortForward runs `kubectl port-forward` in the background and returns
// the local port once it accepts connections. The process is stopped by Close.
func (s *suiteEnv) startPortForward(namespace, resource string, remotePort int) (int, error) {
	localPort, err := freeLocalPort()
	if err != nil {
		return 0, err
	}

	cmd := exec.Command("kubectl", s.kubectlArgs(
		"-n", namespace, "port-forward", resource, fmt.Sprintf("%d:%d", localPort, remotePort))...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	stop := func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}

	addr := fmt.Sprintf("127.0.0.1:%d", localPort)
	err = poll(readyTimeout, 150*time.Millisecond, func() error {
		conn, err := net.DialTimeout("tcp", addr, 250*time.Millisecond)
		if err != nil {
			return err
		}
		return conn.Close()
	})
	if err != nil {
		stop()
		return 0, fmt.Errorf("port-forward to %s/%s did not become ready: %w (%s)", namespace, resource, err, stderr.String())
	}
	s.closers = append(s.closers, stop)
	return localPort, nil
}

func freeLocalPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func waitForHTTPHealth(url string, timeout time.Duration) error {
	client := &http.Client{Timeout: time.Second}
	return poll(timeout, 200*time.Millisecond, func() error {
		resp, err := client.Get(url)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
		}
		return nil
	})
}

// poll calls fn every interval until it returns nil or timeout elapses, in
// which case the last error is returned.
func poll(timeout, interval time.Duration, fn func() error) error {
	deadline := time.Now().Add(timeout)
	for {
		err := fn()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(interval)
	}
}

// ---------------------------------------------------------------------------
// ax CLI helpers
// ---------------------------------------------------------------------------

// axCommand returns an `ax` invocation whose environment points at the E2E
// cluster, with AX_SERVER and AX_HOME scrubbed so the caller's shell does not
// leak in. The command is killed after axCommandTimeout.
func (s *suiteEnv) axCommand(axHome string, args ...string) (*exec.Cmd, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), axCommandTimeout)
	cmd := exec.CommandContext(ctx, s.AXBin, args...)
	for _, kv := range os.Environ() {
		switch k, _, _ := strings.Cut(kv, "="); k {
		case "AX_SERVER", "AX_HOME", "KUBECONFIG":
			continue
		}
		cmd.Env = append(cmd.Env, kv)
	}
	cmd.Env = append(cmd.Env, "KUBECONFIG="+s.Kubeconfig, "AX_HOME="+axHome)
	return cmd, cancel
}

// runAX runs `ax --server <url> --context <context> [-a atespace] args...` and returns its
// combined output.
func (s *suiteEnv) runAX(atespace string, args ...string) (string, error) {
	fullArgs := []string{"--server", s.AXServerURL, "--context", s.KubeContext}
	if atespace != "" {
		fullArgs = append(fullArgs, "-a", atespace)
	}
	cmd, cancel := s.axCommand(s.axHome, append(fullArgs, args...)...)
	defer cancel()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (s *suiteEnv) mustRunAX(t *testing.T, atespace string, args ...string) string {
	t.Helper()
	out, err := s.runAX(atespace, args...)
	if err != nil {
		t.Fatalf("ax %s failed: %v\nOutput:\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// ssh runs a shell script inside the task's sandbox via `ax ssh`.
func (s *suiteEnv) ssh(t *testing.T, atespace, task, script string) string {
	t.Helper()
	return s.mustRunAX(t, atespace, "ssh", task, "--", "sh", "-c", script)
}

// writeManifest writes manifest to a temp file and returns its path.
func writeManifest(t *testing.T, manifest string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "manifest.yaml")
	if err := os.WriteFile(f, []byte(manifest), 0o644); err != nil {
		t.Fatalf("writing manifest: %v", err)
	}
	return f
}

// apply runs `ax apply -f` on manifest and returns the CLI output.
func (s *suiteEnv) apply(t *testing.T, atespace, manifest string) string {
	t.Helper()
	return s.mustRunAX(t, atespace, "apply", "-f", writeManifest(t, manifest))
}

// deleteOnCleanup registers a best-effort `ax delete <kind> <name>` to run
// when the test finishes. Resources are deleted in reverse registration order.
func (s *suiteEnv) deleteOnCleanup(t *testing.T, atespace, kind, name string) {
	t.Cleanup(func() {
		if out, err := s.runAX(atespace, "delete", kind, name); err != nil && !strings.Contains(out, "NotFound") {
			t.Logf("cleanup: ax delete %s %s: %v\n%s", kind, name, err, out)
		}
	})
}

// ---------------------------------------------------------------------------
// Assertions and fixtures
// ---------------------------------------------------------------------------

// renderYAML executes tmpl (a text/template) with data.
func renderYAML(t *testing.T, tmpl string, data any) string {
	t.Helper()
	var b strings.Builder
	if err := template.Must(template.New("manifest").Option("missingkey=error").Parse(tmpl)).Execute(&b, data); err != nil {
		t.Fatalf("rendering manifest: %v", err)
	}
	return b.String()
}

func uniqueName(prefix string) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return prefix + "-" + hex.EncodeToString(b[:])
}

// assertContains fails the test if out does not contain every want.
func assertContains(t *testing.T, what, out string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %s to contain %q, got:\n%s", what, want, out)
		}
	}
}

func (s *suiteEnv) getTask(t *testing.T, atespace, name string) *v1alpha1.Task {
	t.Helper()
	task, err := s.AXClient.GetTask(t.Context(), &v1alpha1.GetTaskRequest{Name: name, Atespace: atespace})
	if err != nil {
		t.Fatalf("GetTask(%s/%s): %v", atespace, name, err)
	}
	return task
}

func assertPhase(t *testing.T, task *v1alpha1.Task, want string) {
	t.Helper()
	if got := task.GetStatus().GetPhase(); got != want {
		t.Fatalf("task %s: phase = %q, want %q", task.GetMetadata().GetName(), got, want)
	}
}

func assertCondition(t *testing.T, task *v1alpha1.Task, condType, wantStatus, wantReason string) {
	t.Helper()
	for _, c := range task.GetStatus().GetConditions() {
		if c.GetType() != condType {
			continue
		}
		if c.GetStatus() != wantStatus || (wantReason != "" && c.GetReason() != wantReason) {
			t.Fatalf("condition %s: got status=%q reason=%q (message=%q), want status=%q reason=%q",
				condType, c.GetStatus(), c.GetReason(), c.GetMessage(), wantStatus, wantReason)
		}
		return
	}
	t.Fatalf("condition %q not found on task %s (conditions: %+v)", condType, task.GetMetadata().GetName(), task.GetStatus().GetConditions())
}

// getActor fetches the Substrate Actor backing a task and checks its state.
func (s *suiteEnv) getActor(t *testing.T, atespace, name string, wantState ateapipb.ActorState) *ateapipb.Actor {
	t.Helper()
	actor, err := s.SubstrateClient.GetActor(t.Context(), atespace, name)
	if err != nil {
		t.Fatalf("Substrate GetActor(%s/%s): %v", atespace, name, err)
	}
	if got := actor.GetStatus().GetState(); got != wantState {
		t.Fatalf("Substrate actor %s/%s: state = %v, want %v", atespace, name, got, wantState)
	}
	return actor
}

func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// loadEnvFileIfUnset sets variables from a file of `export KEY="value"` lines,
// skipping keys already present in the environment. A missing file is not an error.
func loadEnvFileIfUnset(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if os.Getenv(k) == "" {
			_ = os.Setenv(k, strings.Trim(strings.TrimSpace(v), `"'`))
		}
	}
	return scanner.Err()
}
