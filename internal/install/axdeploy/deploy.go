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
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/ax/deploy"
	"github.com/google/ax/internal/install/executil"
	"github.com/google/ax/internal/tunnel"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"gopkg.in/yaml.v3"
)

const (
	// DefaultServerImage is the default ax-server container image when
	// --server-image and $AX_SERVER_IMAGE are unset. Kept empty to gate zero-flag
	// installs until official org-hosted, @sha256-pinned release images exist.
	//
	// TODO(release): Populate with "ghcr.io/google/ax/ax-server:<tag>@sha256:<digest>"
	// once official multi-arch release images are published.
	DefaultServerImage = ""

	// DefaultTaskImage is the optional default container image configured on
	// ax-server (AX_DEFAULT_TASK_IMAGE) for tasks that omit spec.profile.image
	// when --default-task-image and $AX_DEFAULT_TASK_IMAGE are unset. When empty,
	// tasks must specify spec.profile.image explicitly.
	//
	// TODO(release): Populate with "ghcr.io/agentexecutor/<image>:<tag>@sha256:<digest>"
	// once the official published prebuilt default task image is live.
	DefaultTaskImage = ""
)

// InstallOptions configures `ax install`.
type InstallOptions struct {
	KubeContext      string
	Namespace        string
	ServerImage      string
	DefaultTaskImage string
	RolloutTimeout   time.Duration
	Runner           executil.Runner
	ToolURLs         executil.ToolURLs
	HTTPClient       *http.Client
	Out              io.Writer
}

// UninstallOptions configures `ax uninstall`.
type UninstallOptions struct {
	KubeContext    string
	Namespace      string
	RolloutTimeout time.Duration
	Runner         executil.Runner
	ToolURLs       executil.ToolURLs
	Out            io.Writer
}

// Install authorizes ax-server in Agent Substrate, resolves or builds AX
// container images (digest-pinning ax-task-runner), and deploys ax-postgres and
// ax-server into Namespace.
func Install(ctx context.Context, opts InstallOptions) error {
	r := opts.Runner
	if r == nil {
		r = executil.OSRunner{}
	}
	out := opts.Out
	if out == nil {
		out = io.Discard
	}
	ns := opts.Namespace
	if ns == "" {
		ns = v1alpha1.DefaultSystemNamespace
	}
	if err := deploy.ValidateNamespace(ns); err != nil {
		return err
	}
	if _, _, err := ResolveImageInputs(opts.ServerImage, opts.DefaultTaskImage); err != nil {
		return err
	}
	timeout := opts.RolloutTimeout
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}

	srvImg, taskImg, err := resolveImages(ctx, opts)
	if err != nil {
		return err
	}

	kubectlBin, err := executil.EnsureKubectl(ctx, r, opts.ToolURLs, out)
	if err != nil {
		return err
	}
	k := &kubectlClient{runner: r, kubectl: kubectlBin, context: opts.KubeContext}

	fmt.Fprintf(out, "==> Configuring Agent Substrate permissions for %s/ax-server...\n", ns)
	if err := GrantSubstrateAccess(ctx, k, ns, timeout, out); err != nil {
		return err
	}

	// Ensure ax-postgres Secret exists once (never overwrite on upgrades, and
	// intentionally omit ax.io/credential="true" so egress injection refuses it).
	if err := ensurePostgresSecret(ctx, k, ns); err != nil {
		return err
	}

	pgManifest, err := deploy.RenderPostgres(ns)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "==> Deploying ax-postgres in namespace %q...\n", ns)
	if _, err := k.run(ctx, bytes.NewReader(pgManifest), "apply", "-f", "-"); err != nil {
		return fmt.Errorf("applying ax-postgres: %w", err)
	}
	if err := k.rolloutStatus(ctx, ns, "statefulset/ax-postgres", timeout); err != nil {
		return err
	}

	srvManifest, err := deploy.RenderServer(deploy.ServerConfig{
		Namespace:        ns,
		ServerImage:      srvImg,
		DefaultTaskImage: taskImg,
	})
	if err != nil {
		return err
	}
	if taskImg != "" {
		fmt.Fprintf(out, "==> Deploying ax-server (%s) with default task image (%s)...\n", srvImg, taskImg)
	} else {
		fmt.Fprintf(out, "==> Deploying ax-server (%s)...\n", srvImg)
	}
	if _, err := k.run(ctx, bytes.NewReader(srvManifest), "apply", "-f", "-"); err != nil {
		return fmt.Errorf("applying ax-server: %w", err)
	}
	if err := k.rolloutStatus(ctx, ns, "deployment/ax-server", timeout); err != nil {
		return err
	}

	// Stop any stale port-forward tunnel so the next CLI call reconnects cleanly
	// to the newly rolled pod.
	stopTunnel(opts.KubeContext, ns)

	fmt.Fprintf(out, "==> AX is ready in namespace %q.\n", ns)
	return nil
}

// stopTunnel stops the CLI's tunnel to AX in namespace ns of the kube context.
// The context's tunnels to AX in other namespaces stay.
func stopTunnel(kubeContext, ns string) {
	ctxName, err := tunnel.CurrentContext(kubeContext)
	if err != nil || ctxName == "" {
		return
	}
	if t, err := tunnel.GetTunnel(ctxName, ns); err == nil {
		_ = tunnel.StopTunnel(t)
	}
}

// Uninstall removes AX workloads and RBAC from Namespace (preserving
// ax-postgres PVCs and password Secret by default) and revokes ax-server's
// caller permissions in Agent Substrate.
func Uninstall(ctx context.Context, opts UninstallOptions) error {
	r := opts.Runner
	if r == nil {
		r = executil.OSRunner{}
	}
	out := opts.Out
	if out == nil {
		out = io.Discard
	}
	ns := opts.Namespace
	if ns == "" {
		ns = v1alpha1.DefaultSystemNamespace
	}
	if err := deploy.ValidateNamespace(ns); err != nil {
		return err
	}
	timeout := opts.RolloutTimeout
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}

	stopTunnel(opts.KubeContext, ns)

	kubectlBin, err := executil.EnsureKubectl(ctx, r, opts.ToolURLs, out)
	if err != nil {
		return err
	}
	k := &kubectlClient{runner: r, kubectl: kubectlBin, context: opts.KubeContext}

	fmt.Fprintf(out, "==> Removing AX workloads from namespace %q (preserving PostgreSQL PVC data)...\n", ns)
	roleName := deploy.ClusterRoleName(ns)
	for _, target := range [][]string{
		{"-n", ns, "delete", "deployment", "ax-server", "--ignore-not-found"},
		{"-n", ns, "delete", "service", "ax-server", "--ignore-not-found"},
		{"-n", ns, "delete", "configmap", "ax-otel-config", "--ignore-not-found"},
		{"-n", ns, "delete", "networkpolicy", "ax-server", "--ignore-not-found"},
		{"-n", ns, "delete", "serviceaccount", "ax-server", "--ignore-not-found"},
		{"delete", "clusterrolebinding", roleName, "--ignore-not-found"},
		{"delete", "clusterrole", roleName, "--ignore-not-found"},
		{"-n", ns, "delete", "statefulset", "ax-postgres", "--ignore-not-found"},
		{"-n", ns, "delete", "service", "ax-postgres", "--ignore-not-found"},
		{"-n", ns, "delete", "networkpolicy", "ax-postgres", "--ignore-not-found"},
	} {
		if _, err := k.run(ctx, nil, target...); err != nil {
			return fmt.Errorf("deleting %s: %w", strings.Join(target, " "), err)
		}
	}

	fmt.Fprintf(out, "==> Revoking %s/ax-server permissions in Agent Substrate...\n", ns)
	if err := RevokeSubstrateAccess(ctx, k, ns, timeout, out); err != nil {
		return err
	}

	fmt.Fprintf(out, "==> Uninstalled AX from namespace %q.\n", ns)
	return nil
}

// ResolveImageInputs resolves the server and optional default task image
// references from explicit flags, environment overrides ($AX_SERVER_IMAGE /
// $AX_DEFAULT_TASK_IMAGE), or package defaults (DefaultServerImage /
// DefaultTaskImage), failing fast only when the server image is unset.
func ResolveImageInputs(serverImage, defaultTaskImage string) (string, string, error) {
	srvImg := strings.TrimSpace(serverImage)
	if srvImg == "" {
		srvImg = strings.TrimSpace(os.Getenv("AX_SERVER_IMAGE"))
	}
	if srvImg == "" {
		srvImg = DefaultServerImage
	}
	if srvImg == "" {
		return "", "", fmt.Errorf("--server-image (or $AX_SERVER_IMAGE) is required until official release images are published")
	}

	rawTaskImg := strings.TrimSpace(defaultTaskImage)
	if rawTaskImg == "" {
		rawTaskImg = strings.TrimSpace(os.Getenv("AX_DEFAULT_TASK_IMAGE"))
	}
	if rawTaskImg == "" {
		rawTaskImg = DefaultTaskImage
	}
	return srvImg, rawTaskImg, nil
}

func resolveImages(ctx context.Context, opts InstallOptions) (string, string, error) {
	srvImg, rawTaskImg, err := ResolveImageInputs(opts.ServerImage, opts.DefaultTaskImage)
	if err != nil {
		return "", "", err
	}

	pinnedSrvImg, err := EnsureDigestPinned(ctx, srvImg, opts.HTTPClient)
	if err != nil {
		return "", "", err
	}
	if rawTaskImg == "" {
		return pinnedSrvImg, "", nil
	}
	pinnedTaskImg, err := EnsureDigestPinned(ctx, rawTaskImg, opts.HTTPClient)
	if err != nil {
		return "", "", err
	}
	return pinnedSrvImg, pinnedTaskImg, nil
}

func generateRandomPassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

type kubectlClient struct {
	runner  executil.Runner
	kubectl string
	context string
}

func (k *kubectlClient) run(ctx context.Context, stdin io.Reader, args ...string) (executil.Result, error) {
	var full []string
	if k.context != "" {
		full = append(full, "--context", k.context)
	}
	full = append(full, args...)
	return k.runner.Run(ctx, executil.Spec{
		Name:  k.kubectl,
		Args:  full,
		Stdin: stdin,
	})
}

func (k *kubectlClient) exists(ctx context.Context, namespace, kind, name string) bool {
	var args []string
	if namespace != "" {
		args = append(args, "-n", namespace)
	}
	args = append(args, "get", kind, name, "-o", "name")
	_, err := k.run(ctx, nil, args...)
	return err == nil
}

func (k *kubectlClient) rolloutStatus(ctx context.Context, namespace, target string, timeout time.Duration) error {
	_, err := k.run(ctx, nil, "-n", namespace, "rollout", "status", target, fmt.Sprintf("--timeout=%ds", int(timeout.Seconds())))
	if err != nil {
		return fmt.Errorf("waiting for %s/%s rollout: %w", namespace, target, err)
	}
	return nil
}

func ensurePostgresSecret(ctx context.Context, k *kubectlClient, namespace string) error {
	res, err := k.run(ctx, nil, "-n", namespace, "get", "secret", "ax-postgres", "-o", "name")
	if err == nil {
		return nil
	}
	lowerErr := strings.ToLower(res.Stderr + " " + err.Error())
	if !strings.Contains(lowerErr, "notfound") && !strings.Contains(lowerErr, "not found") {
		return fmt.Errorf("checking existing secret/ax-postgres in namespace %q: %w", namespace, err)
	}

	pw, err := generateRandomPassword()
	if err != nil {
		return err
	}
	return k.createPostgresSecret(ctx, namespace, pw)
}

func (k *kubectlClient) createPostgresSecret(ctx context.Context, namespace, password string) error {
	obj := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"type":       "Opaque",
		"metadata": map[string]any{
			"name":      "ax-postgres",
			"namespace": namespace,
		},
		"data": map[string]string{
			"password": base64.StdEncoding.EncodeToString([]byte(password)),
		},
	}
	raw, err := yaml.Marshal(obj)
	if err != nil {
		return err
	}
	// Use `create` rather than `apply` so Kubernetes enforces create-only
	// semantics at the API server level and never overwrites an existing password.
	res, err := k.run(ctx, bytes.NewReader(raw), "create", "-f", "-")
	if err != nil {
		lowerErr := strings.ToLower(res.Stderr + " " + err.Error())
		if strings.Contains(lowerErr, "alreadyexists") || strings.Contains(lowerErr, "already exists") {
			return nil
		}
		return fmt.Errorf("creating secret/ax-postgres in namespace %q: %w", namespace, err)
	}
	return nil
}
