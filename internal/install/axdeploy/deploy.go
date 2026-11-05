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
	"strings"
	"time"

	"github.com/google/ax/deploy"
	"github.com/google/ax/internal/install/executil"
	"github.com/google/ax/internal/tunnel"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"gopkg.in/yaml.v3"
)

const (
	// DefaultImageRepo is the default container registry path for AX images when
	// neither --image-repo nor --server-image / --task-runner-image is supplied.
	DefaultImageRepo = "ghcr.io/google/ax"
	// DefaultImageTag is the default AX image tag when --image-tag is unset.
	DefaultImageTag = "latest"
)

// InstallOptions configures `ax install`.
type InstallOptions struct {
	KubeContext     string
	Namespace       string
	ImageRepo       string
	ImageTag        string
	ServerImage     string
	TaskRunnerImage string
	FromSource      string
	RolloutTimeout  time.Duration
	Runner          executil.Runner
	ToolURLs        executil.ToolURLs
	HTTPClient      *http.Client
	Out             io.Writer
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
	timeout := opts.RolloutTimeout
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}

	kubectlBin, err := executil.EnsureKubectl(ctx, r, opts.ToolURLs, out)
	if err != nil {
		return err
	}
	k := &kubectlClient{runner: r, kubectl: kubectlBin, context: opts.KubeContext}

	srvImg, taskImg, err := resolveImages(ctx, opts, r, out)
	if err != nil {
		return err
	}

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
	fmt.Fprintf(out, "==> Deploying ax-server (%s) with default task image (%s)...\n", srvImg, taskImg)
	if _, err := k.run(ctx, bytes.NewReader(srvManifest), "apply", "-f", "-"); err != nil {
		return fmt.Errorf("applying ax-server: %w", err)
	}
	if err := k.rolloutStatus(ctx, ns, "deployment/ax-server", timeout); err != nil {
		return err
	}

	// Stop any stale port-forward tunnel so the next CLI call reconnects cleanly
	// to the newly rolled pod.
	if ctxName, err := tunnel.CurrentContext(opts.KubeContext); err == nil && ctxName != "" {
		_ = tunnel.StopTunnelByContext(ctxName)
	}

	fmt.Fprintf(out, "==> AX is ready in namespace %q.\n", ns)
	return nil
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

	if ctxName, err := tunnel.CurrentContext(opts.KubeContext); err == nil && ctxName != "" {
		_ = tunnel.StopTunnelByContext(ctxName)
	}

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
		{"-n", ns, "delete", "serviceaccount", "ax-server", "--ignore-not-found"},
		{"delete", "clusterrolebinding", roleName, "--ignore-not-found"},
		{"delete", "clusterrole", roleName, "--ignore-not-found"},
		{"-n", ns, "delete", "statefulset", "ax-postgres", "--ignore-not-found"},
		{"-n", ns, "delete", "service", "ax-postgres", "--ignore-not-found"},
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

func resolveImages(ctx context.Context, opts InstallOptions, r executil.Runner, out io.Writer) (string, string, error) {
	if opts.FromSource != "" {
		built, err := BuildAndPushFromSource(ctx, BuildOptions{
			SourceDir:  opts.FromSource,
			ImageRepo:  opts.ImageRepo,
			ImageTag:   opts.ImageTag,
			Runner:     r,
			HTTPClient: opts.HTTPClient,
			Out:        out,
		})
		if err != nil {
			return "", "", err
		}
		srvImg := built.ServerImage
		if opts.ServerImage != "" {
			pinned, err := EnsureDigestPinned(ctx, opts.ServerImage, opts.HTTPClient)
			if err != nil {
				return "", "", err
			}
			srvImg = pinned
		}
		taskImg := built.TaskRunnerImage
		if opts.TaskRunnerImage != "" {
			pinned, err := EnsureDigestPinned(ctx, opts.TaskRunnerImage, opts.HTTPClient)
			if err != nil {
				return "", "", err
			}
			taskImg = pinned
		}
		return srvImg, taskImg, nil
	}

	repo := strings.TrimRight(strings.TrimSpace(opts.ImageRepo), "/")
	if repo == "" {
		repo = DefaultImageRepo
	}
	tag := strings.TrimSpace(opts.ImageTag)
	if tag == "" {
		tag = DefaultImageTag
	}

	srvImg := strings.TrimSpace(opts.ServerImage)
	if srvImg == "" {
		srvImg = fmt.Sprintf("%s/ax-server:%s", repo, tag)
	}
	rawTaskImg := strings.TrimSpace(opts.TaskRunnerImage)
	if rawTaskImg == "" {
		rawTaskImg = fmt.Sprintf("%s/ax-task-runner:%s", repo, tag)
	}

	pinnedSrvImg, err := EnsureDigestPinned(ctx, srvImg, opts.HTTPClient)
	if err != nil {
		return "", "", err
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
