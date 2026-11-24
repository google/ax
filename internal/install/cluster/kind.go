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
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
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
	registries := loopbackRegistries(opts.Images)
	if err := p.checkLocalRegistry(ctx, rt, registries, opts.LocalRegistry); err != nil {
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
	if err := p.configureLocalRegistry(ctx, rt, nodes, registries, opts.LocalRegistry); err != nil {
		return Info{}, err
	}
	p.warnIfNoEgress(ctx, rt, nodes)

	return Info{
		Name:        name,
		KubeContext: kubeCtx,
	}, nil
}

// egressProbeURL is the registry that serves Agent Substrate's images, the
// first images a new cluster pulls.
const egressProbeURL = "https://ghcr.io/v2/"

// warnIfNoEgress warns when a Kind node cannot reach the image registry.
//
// The host checks (executil.CheckContainerHost) cannot see this: the host
// itself usually has network access, and what fails is traffic from a
// container on the Kind network, for example on a host with IPv4 forwarding
// off. Without the warning the first sign is an image pull that fails inside
// the cluster while `ax local start` waits for a rollout.
//
// It is a warning and not an error, because a cluster that gets its images
// from a registry mirror works without reaching the registry itself.
func (p *KindProvider) warnIfNoEgress(ctx context.Context, rt executil.ContainerRuntime, nodes []string) {
	if len(nodes) == 0 {
		return
	}
	node := nodes[0]
	// The request runs in the node's network namespace, like the node's image
	// pulls. Any HTTP answer, such as 401, means the registry is reachable.
	res, err := p.Runner.Run(ctx, executil.Spec{
		Name: rt.Binary,
		Args: []string{"exec", node, "curl", "--silent", "--show-error", "--output", "/dev/null",
			"--connect-timeout", "5", "--max-time", "10", egressProbeURL},
	})
	if err == nil || ctx.Err() != nil {
		return
	}
	if res.ExitCode == 126 || res.ExitCode == 127 {
		// The node image has no curl: nothing was learned.
		return
	}
	detail := strings.TrimSpace(res.Stderr)
	if i := strings.LastIndex(detail, "\n"); i >= 0 {
		detail = strings.TrimSpace(detail[i+1:])
	}
	if detail == "" {
		detail = err.Error()
	}
	cause := "Check for a proxy that containers do not use, or a firewall."
	if !rt.IsPodman {
		if fwd, fwdErr := p.Runner.Run(ctx, executil.Spec{
			Name: rt.Binary,
			Args: []string{"info", "--format", "{{.IPv4Forwarding}}"},
		}); fwdErr == nil && strings.TrimSpace(fwd.Stdout) == "false" {
			cause = "The Docker host has IPv4 forwarding off ('docker info' reports IPv4Forwarding: false), so its containers have no outbound network: turn on net.ipv4.ip_forward on the host, or use a host that allows it."
		}
	}
	fmt.Fprintf(p.Out, "==> Warning: Kind node %s cannot reach %s (%s). The cluster pulls its images through this node, so the install is likely to fail on image pulls. %s\n",
		node, egressProbeURL, detail, cause)
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

	// Probe Substrate control plane and dataplane readiness.
	if res, err := p.Runner.Run(ctx, executil.Spec{
		Name: kubectlBin,
		Args: []string{"--context", kubeCtx, "-n", "ate-system", "get", "deployment", "ate-api-server", "-o", "jsonpath={.status.availableReplicas}/{.spec.replicas}"},
	}); err == nil && strings.TrimSpace(res.Stdout) != "" {
		// When all is well the line is "ate-api-server (1/1 available)".
		// Otherwise it names only what is wrong, for example
		// "degraded: atenet-router (0/1 available)".
		apiServer := fmt.Sprintf("ate-api-server (%s available)", formatReplicaRatio(res.Stdout))
		var unavailable []string
		if !replicaRatioAvailable(res.Stdout) {
			unavailable = append(unavailable, apiServer)
		}
		st.SubstrateStatus = apiServer
		if note := p.substrateDegradedNote(ctx, rt, kubectlBin, kubeCtx, nodes[0], unavailable); note != "" {
			st.SubstrateStatus = "degraded: " + note
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

// replicaRatioAvailable reports whether raw, "<available>/<wanted>" with an
// empty <available> for none, has every wanted replica available.
func replicaRatioAvailable(raw string) bool {
	available, wanted, _ := strings.Cut(strings.TrimSpace(raw), "/")
	w, err := strconv.Atoi(wanted)
	if err != nil {
		return true
	}
	a, _ := strconv.Atoi(available)
	return a >= w
}

// substrateWorkload names a Kubernetes workload that Agent Substrate needs in
// order to run and reach actors on a local cluster. ate-api-server is omitted
// because Status already prints its replica ratio at the start of
// SubstrateStatus.
type substrateWorkload struct {
	namespace string
	kind      string // lowercase Kubernetes resource kind ("deployment", "statefulset", "daemonset")
	name      string
}

// substrateWorkloads lists the workloads installed by
// internal/install/substrate.Bootstrap, other than ate-api-server and the
// telemetry stack in otel-system, which Substrate runs without.
//
// The list has to follow the manifests in internal/install/substrate/manifests:
// a workload that a new Substrate version adds is not looked at until it is
// listed here. TestSubstrateWorkloadsMatchManifests fails when the two differ.
var substrateWorkloads = []substrateWorkload{
	{namespace: "podcertificate-controller-system", kind: "deployment", name: "podcertificate-controller"},
	{namespace: "ate-system", kind: "statefulset", name: "postgres"},
	{namespace: "ate-system", kind: "deployment", name: "ate-idp-server"},
	{namespace: "ate-system", kind: "deployment", name: "ate-controller"},
	{namespace: "ate-system", kind: "deployment", name: "atenet-router"},
	{namespace: "ate-system", kind: "deployment", name: "atenet-egress"},
	{namespace: "ate-system", kind: "deployment", name: "k8s-credential-provider"},
	{namespace: "ate-system", kind: "deployment", name: "rustfs"},
	{namespace: "ate-system", kind: "daemonset", name: "atelet"},
	{namespace: "ate-workers", kind: "deployment", name: "ax-workers"},
}

// substrateDegradedNote returns what SubstrateStatus says after "degraded: ",
// or "" when nothing is wrong: unavailable (ate-api-server, when it is not
// fully available) and the other Substrate workloads that are not, and the
// node sysctls when a restart of the node container reset them.
//
// It names the unready workloads and no command. A workload is usually
// unready because its pods are still starting or being restarted, and
// `ax local start` does not restart a pod that stays unready.
func (p *KindProvider) substrateDegradedNote(ctx context.Context, rt executil.ContainerRuntime, kubectlBin, kubeCtx, node string, unavailable []string) string {
	var problems []string
	if res, err := p.Runner.Run(ctx, executil.Spec{
		Name: kubectlBin,
		Args: []string{"--context", kubeCtx, "get", "deployments,statefulsets,daemonsets", "-A", "-o", "json"},
	}); err == nil {
		unavailable = append(unavailable, unreadySubstrateWorkloads(res.Stdout)...)
	}
	if len(unavailable) > 0 {
		problems = append(problems, strings.Join(unavailable, ", "))
	}
	if sysctlRes, sysctlErr := p.Runner.Run(ctx, executil.Spec{
		Name: rt.Binary,
		Args: []string{"exec", node, "sysctl", "-n", "net.ipv4.conf.all.proxy_arp"},
	}); sysctlErr == nil && strings.TrimSpace(sysctlRes.Stdout) == "0" {
		problems = append(problems, "node sysctls reset after restart; run 'ax local start' to restore")
	}
	return strings.Join(problems, "; ")
}

// unreadySubstrateWorkloads reads the JSON of a list of Deployments,
// StatefulSets and DaemonSets, and describes each workload of
// substrateWorkloads in it that has fewer ready pods than it wants, as
// "<name> (<ready>/<wanted> available)". A workload that
// is not in the list is left out.
func unreadySubstrateWorkloads(rawJSON string) []string {
	var list struct {
		Items []struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Spec struct {
				Replicas *int `json:"replicas"`
			} `json:"spec"`
			Status struct {
				AvailableReplicas      int `json:"availableReplicas"`
				DesiredNumberScheduled int `json:"desiredNumberScheduled"`
				NumberAvailable        int `json:"numberAvailable"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(rawJSON), &list); err != nil {
		return nil
	}

	var out []string
	for _, w := range substrateWorkloads {
		for _, item := range list.Items {
			if item.Metadata.Namespace != w.namespace || strings.ToLower(item.Kind) != w.kind || item.Metadata.Name != w.name {
				continue
			}
			var ready, wanted int
			if w.kind == "daemonset" {
				wanted, ready = item.Status.DesiredNumberScheduled, item.Status.NumberAvailable
			} else {
				// Kubernetes defaults spec.replicas to 1.
				wanted, ready = 1, item.Status.AvailableReplicas
				if item.Spec.Replicas != nil {
					wanted = *item.Spec.Replicas
				}
			}
			if ready < wanted {
				out = append(out, fmt.Sprintf("%s (%d/%d available)", w.name, ready, wanted))
			}
			break
		}
	}
	return out
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
