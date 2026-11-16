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
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/google/ax/internal/install/executil"
	"github.com/google/ax/internal/tunnel"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"gopkg.in/yaml.v3"
)

// KindProvider implements Provider using Kubernetes-in-Docker (Kind) over
// Docker or Podman.
type KindProvider struct {
	Runner      executil.Runner
	ToolURLs    executil.ToolURLs
	Out         io.Writer
	AXNamespace string
}

// NewKindProvider constructs a KindProvider with sensible defaults.
func NewKindProvider(r executil.Runner, out io.Writer) *KindProvider {
	if r == nil {
		r = executil.OSRunner{}
	}
	if out == nil {
		out = io.Discard
	}
	return &KindProvider{
		Runner:      r,
		ToolURLs:    executil.DefaultToolURLs(),
		Out:         out,
		AXNamespace: v1alpha1.DefaultSystemNamespace,
	}
}

// Up creates or resumes the named Kind cluster and wires Kubernetes 1.36+
// certificate feature gates and node sysctls.
func (p *KindProvider) Up(ctx context.Context, opts UpOptions) (Info, error) {
	name := opts.Name
	if name == "" {
		name = DefaultName
	}
	nodeImage := opts.NodeImage
	if nodeImage == "" {
		nodeImage = DefaultNodeImage
	}

	rt, kindBin, kubectlBin, err := p.resolveTools(ctx)
	if err != nil {
		return Info{}, err
	}
	if err := executil.CheckContainerHost(ctx, p.Runner, rt, p.Out); err != nil {
		return Info{}, err
	}
	if err := p.verifyContextOwnership(ctx, kubectlBin, name); err != nil {
		return Info{}, err
	}

	exists, err := p.clusterExists(ctx, rt, kindBin, name)
	if err != nil {
		return Info{}, err
	}

	if !exists {
		fmt.Fprintf(p.Out, "==> Creating Kind cluster %q (%s)...\n", name, nodeImage)
		cfgYAML := BuildKindConfig()
		cfgFile, err := os.CreateTemp("", "ax-kind-config-*.yaml")
		if err != nil {
			return Info{}, fmt.Errorf("creating temporary kind config: %w", err)
		}
		cfgPath := cfgFile.Name()
		defer os.Remove(cfgPath)
		if _, err := cfgFile.WriteString(cfgYAML); err != nil {
			_ = cfgFile.Close()
			return Info{}, err
		}
		_ = cfgFile.Close()

		_, err = p.Runner.Run(ctx, executil.Spec{
			Name:   kindBin,
			Args:   []string{"create", "cluster", "--name", name, "--image", nodeImage, "--config", cfgPath},
			Env:    kindEnv(rt),
			Stdout: p.Out,
			Stderr: p.Out,
		})
		if err != nil {
			return Info{}, fmt.Errorf("creating kind cluster %q: %w", name, err)
		}
	} else {
		nodes, err := p.listNodes(ctx, rt, kindBin, name)
		if err != nil {
			return Info{}, err
		}
		anyStopped := false
		for _, node := range nodes {
			running, err := p.containerRunning(ctx, rt, node)
			if err == nil && !running {
				anyStopped = true
				break
			}
		}
		if anyStopped {
			fmt.Fprintf(p.Out, "==> Starting stopped Kind cluster %q...\n", name)
			args := append([]string{"start"}, nodes...)
			if _, err := p.Runner.Run(ctx, executil.Spec{Name: rt.Binary, Args: args}); err != nil {
				return Info{}, fmt.Errorf("starting kind node containers: %w", err)
			}
		} else {
			fmt.Fprintf(p.Out, "==> Kind cluster %q is already running.\n", name)
		}
		// Refresh kubeconfig in case API server port changed on container restart.
		_, _ = p.Runner.Run(ctx, executil.Spec{
			Name: kindBin,
			Args: []string{"export", "kubeconfig", "--name", name},
			Env:  kindEnv(rt),
		})
	}

	kubeCtx, err := p.ensureKubeContext(ctx, kubectlBin, name)
	if err != nil {
		return Info{}, err
	}

	// Wait for node readiness before running node-level sysctl / registry setup.
	if _, err := p.Runner.Run(ctx, executil.Spec{
		Name: kubectlBin,
		Args: []string{"--context", kubeCtx, "wait", "--for=condition=Ready", "node", "--all", "--timeout=120s"},
	}); err != nil {
		return Info{}, fmt.Errorf("waiting for Kind node readiness: %w", err)
	}

	nodes, err := p.listNodes(ctx, rt, kindBin, name)
	if err != nil {
		return Info{}, err
	}
	if err := p.configureNodes(ctx, rt, nodes); err != nil {
		return Info{}, err
	}

	return Info{
		Name:        name,
		KubeContext: kubeCtx,
	}, nil
}

// Stop halts the Kind node containers (preserving cluster etcd and PVC state)
// and closes any background AX port-forward tunnel for the context.
func (p *KindProvider) Stop(ctx context.Context, name string) error {
	if name == "" {
		name = DefaultName
	}
	kubeCtx := KubeContextName(name)
	_ = tunnel.StopTunnelByContext(kubeCtx)
	_ = tunnel.StopTunnelByContext(name)
	_ = tunnel.StopTunnelByContext("kind-" + name)

	rt, kindBin, _, err := p.resolveTools(ctx)
	if err != nil {
		return err
	}
	nodes, err := p.listNodes(ctx, rt, kindBin, name)
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		return fmt.Errorf("local cluster %q does not exist", name)
	}
	fmt.Fprintf(p.Out, "==> Stopping Kind cluster %q (%s)...\n", name, strings.Join(nodes, ", "))
	args := append([]string{"stop"}, nodes...)
	if _, err := p.Runner.Run(ctx, executil.Spec{Name: rt.Binary, Args: args}); err != nil {
		return fmt.Errorf("stopping kind cluster %q: %w", name, err)
	}
	return nil
}

// Status inspects the Kind cluster, Substrate control plane, and AX server.
func (p *KindProvider) Status(ctx context.Context, name string) (Status, error) {
	if name == "" {
		name = DefaultName
	}
	kubeCtx := KubeContextName(name)
	st := Status{
		Name:            "N/A",
		Backend:         "kind",
		State:           StateAbsent,
		KubeContext:     "N/A",
		SubstrateStatus: "N/A",
		AXStatus:        "N/A",
	}

	rt, kindBin, kubectlBin, err := p.resolveTools(ctx)
	if err != nil {
		return st, err
	}
	nodes, err := p.listNodes(ctx, rt, kindBin, name)
	if err != nil || len(nodes) == 0 {
		return st, nil
	}
	st.Name = name
	st.KubeContext = kubeCtx
	st.Nodes = nodes

	allRunning := true
	for _, node := range nodes {
		running, err := p.containerRunning(ctx, rt, node)
		if err != nil || !running {
			allRunning = false
			break
		}
	}
	if !allRunning {
		st.State = StateStopped
		return st, nil
	}
	st.State = StateRunning

	// Probe Substrate control plane readiness.
	if res, err := p.Runner.Run(ctx, executil.Spec{
		Name: kubectlBin,
		Args: []string{"--context", kubeCtx, "-n", "ate-system", "get", "deployment", "ate-api-server", "-o", "jsonpath={.status.availableReplicas}/{.spec.replicas}"},
	}); err == nil && strings.TrimSpace(res.Stdout) != "" {
		st.SubstrateStatus = fmt.Sprintf("ate-api-server (%s available)", formatReplicaRatio(res.Stdout))
		if sysctlRes, sysctlErr := p.Runner.Run(ctx, executil.Spec{
			Name: rt.Binary,
			Args: []string{"exec", nodes[0], "sysctl", "-n", "net.ipv4.conf.all.proxy_arp"},
		}); sysctlErr == nil && strings.TrimSpace(sysctlRes.Stdout) == "0" {
			st.SubstrateStatus += " [degraded: node sysctls reset after restart; run 'ax local start' to restore]"
		}
	} else {
		st.SubstrateStatus = "NotInstalled"
	}

	axNS := p.AXNamespace
	if axNS == "" {
		axNS = v1alpha1.DefaultSystemNamespace
	}
	if res, err := p.Runner.Run(ctx, executil.Spec{
		Name: kubectlBin,
		Args: []string{"--context", kubeCtx, "-n", axNS, "get", "deployment", "ax-server", "-o", "jsonpath={.status.availableReplicas}/{.spec.replicas}"},
	}); err == nil && strings.TrimSpace(res.Stdout) != "" {
		st.AXStatus = fmt.Sprintf("ax-server (%s available in %s)", formatReplicaRatio(res.Stdout), axNS)
	} else {
		st.AXStatus = "NotInstalled"
	}

	return st, nil
}

func formatReplicaRatio(raw string) string {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, "/") {
		return "0" + s
	}
	return s
}

// Delete destroys the Kind cluster and removes its kubeconfig context.
func (p *KindProvider) Delete(ctx context.Context, name string) error {
	if name == "" {
		name = DefaultName
	}
	kubeCtx := KubeContextName(name)
	_ = tunnel.StopTunnelByContext(kubeCtx)
	_ = tunnel.StopTunnelByContext(name)
	_ = tunnel.StopTunnelByContext("kind-" + name)

	rt, kindBin, kubectlBin, err := p.resolveTools(ctx)
	if err != nil {
		return err
	}

	rawCtx := "kind-" + name
	ownedContexts, activeDeleted := p.ownedContextsForCluster(ctx, kubectlBin, name, kubeCtx, rawCtx)

	fmt.Fprintf(p.Out, "==> Deleting Kind cluster %q...\n", name)
	if _, err := p.Runner.Run(ctx, executil.Spec{
		Name:   kindBin,
		Args:   []string{"delete", "cluster", "--name", name},
		Env:    kindEnv(rt),
		Stdout: p.Out,
		Stderr: p.Out,
	}); err != nil {
		return fmt.Errorf("deleting kind cluster %q: %w", name, err)
	}

	for _, cName := range ownedContexts {
		_, _ = p.Runner.Run(ctx, executil.Spec{
			Name: kubectlBin,
			Args: []string{"config", "delete-context", cName},
		})
	}
	if activeDeleted {
		_, _ = p.Runner.Run(ctx, executil.Spec{
			Name: kubectlBin,
			Args: []string{"config", "unset", "current-context"},
		})
	}
	return nil
}

// BuildKindConfig renders the Kind v1alpha4 cluster configuration required by
// Agent Substrate v1.0+ (K8s >= 1.36 certificate feature gates and parallel
// image pulls). Note that `ax local` runs gVisor (`systrap`) workers and never
// bind-mounts or mutates host `/dev/kvm`.
func BuildKindConfig() string {
	return `kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
featureGates:
  ClusterTrustBundle: true
  ClusterTrustBundleProjection: true
  PodCertificateRequest: true
runtimeConfig:
  "certificates.k8s.io/v1beta1": "true"
networking:
  ipFamily: ipv4
kubeadmConfigPatches:
- |
  kind: KubeletConfiguration
  serializeImagePulls: false
  maxParallelImagePulls: 4
`
}

func (p *KindProvider) resolveTools(ctx context.Context) (executil.ContainerRuntime, string, string, error) {
	rt, err := executil.ResolveContainerRuntime(ctx, p.Runner)
	if err != nil {
		return executil.ContainerRuntime{}, "", "", err
	}
	kindBin, err := executil.EnsureKind(ctx, p.Runner, p.ToolURLs, p.Out)
	if err != nil {
		return executil.ContainerRuntime{}, "", "", err
	}
	kubectlBin, err := executil.EnsureKubectl(ctx, p.Runner, p.ToolURLs, p.Out)
	if err != nil {
		return executil.ContainerRuntime{}, "", "", err
	}
	return rt, kindBin, kubectlBin, nil
}

func kindEnv(rt executil.ContainerRuntime) []string {
	if rt.IsPodman {
		return []string{"KIND_EXPERIMENTAL_PROVIDER=podman"}
	}
	return nil
}

func (p *KindProvider) clusterExists(ctx context.Context, rt executil.ContainerRuntime, kindBin, name string) (bool, error) {
	res, err := p.Runner.Run(ctx, executil.Spec{
		Name: kindBin,
		Args: []string{"get", "clusters"},
		Env:  kindEnv(rt),
	})
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		if strings.TrimSpace(line) == name {
			return true, nil
		}
	}
	return false, nil
}

func (p *KindProvider) listNodes(ctx context.Context, rt executil.ContainerRuntime, kindBin, name string) ([]string, error) {
	res, err := p.Runner.Run(ctx, executil.Spec{
		Name: kindBin,
		Args: []string{"get", "nodes", "--name", name},
		Env:  kindEnv(rt),
	})
	if err != nil {
		return nil, err
	}
	var nodes []string
	for _, line := range strings.Split(res.Stdout, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			nodes = append(nodes, trimmed)
		}
	}
	return nodes, nil
}

func (p *KindProvider) containerRunning(ctx context.Context, rt executil.ContainerRuntime, container string) (bool, error) {
	res, err := p.Runner.Run(ctx, executil.Spec{
		Name: rt.Binary,
		Args: []string{"inspect", "-f", "{{.State.Running}}", container},
	})
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(res.Stdout) == "true", nil
}

func (p *KindProvider) ownedContextsForCluster(ctx context.Context, kubectlBin, name, kubeCtx, rawCtx string) ([]string, bool) {
	res, err := p.Runner.Run(ctx, executil.Spec{
		Name: kubectlBin,
		Args: []string{"config", "view", "-o", "yaml"},
	})
	if err != nil {
		return nil, false
	}
	var cfg struct {
		CurrentContext string `yaml:"current-context"`
		Contexts       []struct {
			Name    string `yaml:"name"`
			Context struct {
				Cluster string `yaml:"cluster"`
			} `yaml:"context"`
		} `yaml:"contexts"`
	}
	if err := yaml.Unmarshal([]byte(res.Stdout), &cfg); err != nil {
		return nil, false
	}
	var owned []string
	cur := strings.TrimSpace(cfg.CurrentContext)
	activeDeleted := cur == rawCtx
	for _, c := range cfg.Contexts {
		if strings.TrimSpace(c.Context.Cluster) != rawCtx {
			continue
		}
		if c.Name == kubeCtx || c.Name == name {
			if c.Name != rawCtx {
				owned = append(owned, c.Name)
			}
			if cur == c.Name {
				activeDeleted = true
			}
		}
	}
	return owned, activeDeleted
}

func (p *KindProvider) contextCluster(ctx context.Context, kubectlBin, ctxName string) (string, bool, error) {
	res, err := p.Runner.Run(ctx, executil.Spec{
		Name: kubectlBin,
		Args: []string{"config", "view", "-o", "yaml"},
	})
	if err != nil {
		return "", false, err
	}
	var cfg struct {
		Contexts []struct {
			Name    string `yaml:"name"`
			Context struct {
				Cluster string `yaml:"cluster"`
			} `yaml:"context"`
		} `yaml:"contexts"`
	}
	if err := yaml.Unmarshal([]byte(res.Stdout), &cfg); err != nil {
		return "", false, err
	}
	for _, c := range cfg.Contexts {
		if c.Name == ctxName {
			return strings.TrimSpace(c.Context.Cluster), true, nil
		}
	}
	return "", false, nil
}

// verifyContextOwnership ensures that aliasing `kind-<name>` to `targetCtx` will
// not clobber a pre-existing kubeconfig context pointing at a different cluster.
func (p *KindProvider) verifyContextOwnership(ctx context.Context, kubectlBin, name string) error {
	rawCtx := "kind-" + name
	targetCtx := KubeContextName(name)
	if targetCtx == rawCtx {
		return nil
	}
	existingCluster, exists, err := p.contextCluster(ctx, kubectlBin, targetCtx)
	if err != nil || !exists || existingCluster == "" || existingCluster == rawCtx {
		return nil
	}
	return fmt.Errorf("kubeconfig context %q already exists and points to cluster %q (expected %q); pass --name to choose a different local cluster name", targetCtx, existingCluster, rawCtx)
}

func (p *KindProvider) ensureKubeContext(ctx context.Context, kubectlBin, name string) (string, error) {
	rawCtx := "kind-" + name
	targetCtx := KubeContextName(name)
	if targetCtx == rawCtx {
		_, err := p.Runner.Run(ctx, executil.Spec{
			Name: kubectlBin,
			Args: []string{"config", "use-context", targetCtx},
		})
		return targetCtx, err
	}
	if err := p.verifyContextOwnership(ctx, kubectlBin, name); err != nil {
		return "", err
	}

	// Alias "kind-ax-local" -> "ax-local" and drop the duplicate "kind-ax-local"
	// entry so `ax ctx` and `ax ctx ax-local` behave cleanly and idempotently.
	if _, err := p.Runner.Run(ctx, executil.Spec{
		Name: kubectlBin,
		Args: []string{"config", "set-context", targetCtx, "--cluster=" + rawCtx, "--user=" + rawCtx},
	}); err != nil {
		return "", fmt.Errorf("setting kubeconfig context %q: %w", targetCtx, err)
	}
	if _, err := p.Runner.Run(ctx, executil.Spec{
		Name: kubectlBin,
		Args: []string{"config", "use-context", targetCtx},
	}); err != nil {
		return "", fmt.Errorf("switching kubeconfig context to %q: %w", targetCtx, err)
	}
	_, _ = p.Runner.Run(ctx, executil.Spec{
		Name: kubectlBin,
		Args: []string{"config", "delete-context", rawCtx},
	})
	return targetCtx, nil
}

func (p *KindProvider) configureNodes(ctx context.Context, rt executil.ContainerRuntime, nodes []string) error {
	for _, node := range nodes {
		if _, err := p.Runner.Run(ctx, executil.Spec{
			Name: rt.Binary,
			Args: []string{"exec", node, "sysctl", "net.ipv4.conf.all.proxy_arp=1"},
		}); err != nil {
			return fmt.Errorf("enabling proxy_arp on node %s: %w", node, err)
		}
		_, _ = p.Runner.Run(ctx, executil.Spec{
			Name: rt.Binary,
			Args: []string{"exec", node, "sysctl", "-e", "net.ipv6.conf.all.proxy_ndp=1"},
		})
		_, _ = p.Runner.Run(ctx, executil.Spec{
			Name: rt.Binary,
			Args: []string{"exec", node, "sysctl", "-w", "fs.inotify.max_user_instances=8192"},
		})
	}
	return nil
}
