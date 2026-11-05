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
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/google/ax/internal/install/executil"
	"github.com/google/ax/internal/tunnel"
	"github.com/google/ax/pkg/apis/v1alpha1"
)

const kindRegistryName = "kind-registry"

// KindProvider implements Provider using Kubernetes-in-Docker (Kind) over
// Docker or Podman.
type KindProvider struct {
	Runner      executil.Runner
	ToolURLs    executil.ToolURLs
	Out         io.Writer
	AXNamespace string
	ProbeKVM    func(ctx context.Context, rt executil.ContainerRuntime) bool
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
// certificate feature gates, node sysctls, and (optionally) a loopback local
// container registry.
func (p *KindProvider) Up(ctx context.Context, opts UpOptions) (Info, error) {
	name := opts.Name
	if name == "" {
		name = DefaultName
	}
	nodeImage := opts.NodeImage
	if nodeImage == "" {
		nodeImage = DefaultNodeImage
	}
	regPort := opts.RegistryPort
	if regPort <= 0 {
		regPort = DefaultRegistryPort
	}

	rt, kindBin, kubectlBin, err := p.resolveTools(ctx)
	if err != nil {
		return Info{}, err
	}

	var regAddr string
	if opts.WithRegistry {
		if err := p.ensureLocalRegistry(ctx, rt, regPort); err != nil {
			return Info{}, err
		}
		regAddr = fmt.Sprintf("localhost:%d", regPort)
	}

	hasKVM := p.hasKVM(ctx, rt)
	exists, err := p.clusterExists(ctx, rt, kindBin, name)
	if err != nil {
		return Info{}, err
	}

	if !exists {
		fmt.Fprintf(p.Out, "==> Creating Kind cluster %q (%s)...\n", name, nodeImage)
		cfgYAML := BuildKindConfig(hasKVM)
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
	if err := p.configureNodes(ctx, rt, nodes, hasKVM); err != nil {
		return Info{}, err
	}

	if opts.WithRegistry {
		if err := p.wireRegistryToCluster(ctx, rt, kubectlBin, kubeCtx, nodes, regPort); err != nil {
			return Info{}, err
		}
	}

	return Info{
		Name:         name,
		KubeContext:  kubeCtx,
		RegistryAddr: regAddr,
		HasKVM:       hasKVM,
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
		Name:            name,
		Backend:         "kind",
		State:           StateAbsent,
		KubeContext:     kubeCtx,
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

	if regRunning, err := p.containerRunning(ctx, rt, kindRegistryName); err == nil && regRunning {
		st.RegistryAddr = fmt.Sprintf("127.0.0.1:%d", DefaultRegistryPort)
	}

	// Probe Substrate control plane readiness.
	if res, err := p.Runner.Run(ctx, executil.Spec{
		Name: kubectlBin,
		Args: []string{"--context", kubeCtx, "-n", "ate-system", "get", "deployment", "ate-api-server", "-o", "jsonpath={.status.availableReplicas}/{.spec.replicas}"},
	}); err == nil && strings.TrimSpace(res.Stdout) != "" {
		st.SubstrateStatus = fmt.Sprintf("ate-api-server (%s available)", strings.TrimSpace(res.Stdout))
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
		st.AXStatus = fmt.Sprintf("ax-server (%s available in %s)", strings.TrimSpace(res.Stdout), axNS)
	} else {
		st.AXStatus = "NotInstalled"
	}

	return st, nil
}

// Delete destroys the Kind cluster, removes its kubeconfig context, and removes
// the local registry container only if no other Kind clusters remain.
func (p *KindProvider) Delete(ctx context.Context, name string) error {
	if name == "" {
		name = DefaultName
	}
	kubeCtx := KubeContextName(name)
	_ = tunnel.StopTunnelByContext(kubeCtx)
	_ = tunnel.StopTunnelByContext("kind-" + name)

	rt, kindBin, kubectlBin, err := p.resolveTools(ctx)
	if err != nil {
		return err
	}

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

	if name == DefaultName {
		_, _ = p.Runner.Run(ctx, executil.Spec{
			Name: kubectlBin,
			Args: []string{"config", "delete-context", DefaultName},
		})
	}

	// Only tear down the shared kind-registry if no other Kind clusters remain.
	if res, err := p.Runner.Run(ctx, executil.Spec{
		Name: kindBin,
		Args: []string{"get", "clusters"},
		Env:  kindEnv(rt),
	}); err == nil && strings.TrimSpace(res.Stdout) == "" {
		_, _ = p.Runner.Run(ctx, executil.Spec{
			Name: rt.Binary,
			Args: []string{"rm", "-f", kindRegistryName},
		})
	}
	return nil
}

// BuildKindConfig renders the Kind v1alpha4 cluster configuration required by
// Agent Substrate v1.0+ (K8s >= 1.36 certificate feature gates, parallel image
// pulls, and optional /dev/kvm passthrough for Kata microVMs).
func BuildKindConfig(hasKVM bool) string {
	var b strings.Builder
	b.WriteString(`kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
`)
	if hasKVM {
		b.WriteString(`  extraMounts:
  - hostPath: /dev/kvm
    containerPath: /dev/kvm
`)
	}
	b.WriteString(`featureGates:
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
`)
	return b.String()
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

func (p *KindProvider) hasKVM(ctx context.Context, rt executil.ContainerRuntime) bool {
	if p.ProbeKVM != nil {
		return p.ProbeKVM(ctx, rt)
	}
	if runtime.GOOS == "linux" {
		_, err := os.Stat("/dev/kvm")
		return err == nil
	}
	_, err := p.Runner.Run(ctx, executil.Spec{
		Name: rt.Binary,
		Args: []string{"run", "--rm", "--device", "/dev/kvm", "busybox", "true"},
	})
	return err == nil
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

	// Alias "kind-local" -> "local" and drop the duplicate "kind-local" entry so
	// `ax ctx` and `ax ctx local` behave cleanly and idempotently.
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

func (p *KindProvider) configureNodes(ctx context.Context, rt executil.ContainerRuntime, nodes []string, hasKVM bool) error {
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
		if hasKVM {
			_, _ = p.Runner.Run(ctx, executil.Spec{
				Name: rt.Binary,
				Args: []string{"exec", node, "chmod", "666", "/dev/kvm"},
			})
		}
	}
	return nil
}

func (p *KindProvider) ensureLocalRegistry(ctx context.Context, rt executil.ContainerRuntime, port int) error {
	running, err := p.containerRunning(ctx, rt, kindRegistryName)
	if err == nil && running {
		portRes, portErr := p.Runner.Run(ctx, executil.Spec{
			Name: rt.Binary,
			Args: []string{"port", kindRegistryName},
		})
		if portErr == nil && strings.Contains(portRes.Stdout, fmt.Sprintf(":%d", port)) {
			return nil
		}
		_, _ = p.Runner.Run(ctx, executil.Spec{Name: rt.Binary, Args: []string{"rm", "-f", kindRegistryName}})
	} else if err == nil && !running {
		// Container exists in Stopped state; try starting it first.
		if _, startErr := p.Runner.Run(ctx, executil.Spec{Name: rt.Binary, Args: []string{"start", kindRegistryName}}); startErr == nil {
			if portRes, portErr := p.Runner.Run(ctx, executil.Spec{Name: rt.Binary, Args: []string{"port", kindRegistryName}}); portErr == nil && strings.Contains(portRes.Stdout, fmt.Sprintf(":%d", port)) {
				return nil
			}
		}
		_, _ = p.Runner.Run(ctx, executil.Spec{Name: rt.Binary, Args: []string{"rm", "-f", kindRegistryName}})
	}

	fmt.Fprintf(p.Out, "==> Starting loopback local registry %q on 127.0.0.1:%d...\n", kindRegistryName, port)
	if _, err := p.Runner.Run(ctx, executil.Spec{
		Name: rt.Binary,
		Args: []string{
			"run", "-d", "--restart=always",
			"--label", "created-by=ax-installer",
			"-p", fmt.Sprintf("127.0.0.1:%d:5000", port),
			"-p", fmt.Sprintf("[::1]:%d:5000", port),
			"--network", "bridge",
			"--name", kindRegistryName,
			"registry:3",
		},
	}); err != nil {
		return fmt.Errorf("starting local registry on 127.0.0.1:%d: %w", port, err)
	}

	client := &http.Client{Timeout: time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d/v2/", port)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := client.Get(url); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return nil
}

func (p *KindProvider) wireRegistryToCluster(ctx context.Context, rt executil.ContainerRuntime, kubectlBin, kubeCtx string, nodes []string, port int) error {
	regDir := fmt.Sprintf("/etc/containerd/certs.d/localhost:%d", port)
	hostsTOML := fmt.Sprintf("[host.\"http://%s:5000\"]\n", kindRegistryName)
	for _, node := range nodes {
		if _, err := p.Runner.Run(ctx, executil.Spec{
			Name: rt.Binary,
			Args: []string{"exec", node, "mkdir", "-p", regDir},
		}); err != nil {
			return fmt.Errorf("creating containerd registry dir on %s: %w", node, err)
		}
		if _, err := p.Runner.Run(ctx, executil.Spec{
			Name:  rt.Binary,
			Args:  []string{"exec", "-i", node, "cp", "/dev/stdin", regDir + "/hosts.toml"},
			Stdin: strings.NewReader(hostsTOML),
		}); err != nil {
			return fmt.Errorf("writing hosts.toml on %s: %w", node, err)
		}
	}

	// Attach kind-registry to the "kind" Docker network if not already attached.
	_, _ = p.Runner.Run(ctx, executil.Spec{
		Name: rt.Binary,
		Args: []string{"network", "connect", "kind", kindRegistryName},
	})

	cmYAML := fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: local-registry-hosting
  namespace: kube-public
data:
  localRegistryHosting.v1: |
    host: "localhost:%d"
    help: "https://kind.sigs.k8s.io/docs/user/local-registry/"
`, port)
	_, err := p.Runner.Run(ctx, executil.Spec{
		Name:  kubectlBin,
		Args:  []string{"--context", kubeCtx, "apply", "-f", "-"},
		Stdin: strings.NewReader(cmYAML),
	})
	return err
}
