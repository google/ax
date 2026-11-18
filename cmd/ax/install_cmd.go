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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/google/ax/internal/install/axdeploy"
	"github.com/google/ax/internal/install/cluster"
	"github.com/google/ax/internal/install/executil"
	installsubstrate "github.com/google/ax/internal/install/substrate"
)

func formatImageFlagDefault(def, envVar string, required bool) string {
	if def != "" {
		return fmt.Sprintf("(default: %q)", def)
	}
	if required {
		return fmt.Sprintf("(required, or set $%s)", envVar)
	}
	return fmt.Sprintf("(optional, or set $%s)", envVar)
}

func localUsage() string {
	return fmt.Sprintf(`Usage:
  ax local <start|stop|status|delete> [flags]

Manage a self-contained local Kubernetes + Agent Substrate + AX environment.

Subcommands:
  start    Create or resume the local cluster, install Substrate + AX, and select its context
  status   Show health of the local cluster, Substrate control plane, and AX server
  stop     Stop the local cluster containers while preserving state
  delete   Destroy the local cluster and clean up its context

Flags:
  --name string                Local cluster name (default: %q)
  --workers int                Number of gVisor worker replicas (default: 1)
  --server-image string        ax-server image reference, tag or @sha256 %s
  --default-task-image string  Default task image when spec.profile.image is unset %s
  --runner-volume-image string Runner volume image mounted into tasks; empty runs each image's own runner %s
  -o, --output string          Output format of status; "json" also reports the container host
  -n, --namespace string       Kubernetes namespace for AX (default: "ax-system")`,
		cluster.DefaultName,
		formatImageFlagDefault(axdeploy.DefaultServerImage, "AX_SERVER_IMAGE", true),
		formatImageFlagDefault(axdeploy.DefaultTaskImage, "AX_DEFAULT_TASK_IMAGE", false),
		formatImageFlagDefault(axdeploy.DefaultRunnerVolumeImage, "AX_RUNNER_VOLUME_IMAGE", false),
	)
}

func installUsage() string {
	return fmt.Sprintf(`Usage:
  ax install [flags]

Install or upgrade AX (ax-postgres + ax-server) and register Substrate caller
permissions on an existing Kubernetes + Agent Substrate cluster.

Flags:
  --context string             Kubernetes context (defaults to active current-context)
  -n, --namespace string       Kubernetes namespace for AX (default: "ax-system")
  --server-image string        ax-server image reference, tag or @sha256 %s
  --default-task-image string  Default task image when spec.profile.image is unset %s
  --runner-volume-image string Runner volume image mounted into tasks; empty runs each image's own runner %s`,
		formatImageFlagDefault(axdeploy.DefaultServerImage, "AX_SERVER_IMAGE", true),
		formatImageFlagDefault(axdeploy.DefaultTaskImage, "AX_DEFAULT_TASK_IMAGE", false),
		formatImageFlagDefault(axdeploy.DefaultRunnerVolumeImage, "AX_RUNNER_VOLUME_IMAGE", false),
	)
}

const uninstallUsage = `Usage:
  ax uninstall [flags]

Remove AX workloads from a cluster (preserving ax-postgres PVCs by default) and
revoke ax-server's caller permissions in Agent Substrate.

Flags:
  --context string            Kubernetes context (defaults to active current-context)
  -n, --namespace string      Kubernetes namespace where AX is installed (default: "ax-system")`

// imageFlags holds independent full container image references shared by `ax local` and `ax install`.
// Each image may come from a distinct registry, repository, and release cadence.
type imageFlags struct {
	serverImage       string
	defaultTaskImage  string
	runnerVolumeImage string
}

func (f *imageFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.serverImage, "server-image", axdeploy.DefaultServerImage, "ax-server container image reference (repo:tag or repo@sha256:...)")
	fs.StringVar(&f.defaultTaskImage, "default-task-image", axdeploy.DefaultTaskImage, "default task container image when spec.profile.image is unset (repo:tag or repo@sha256:...)")
	fs.StringVar(&f.runnerVolumeImage, "runner-volume-image", axdeploy.DefaultRunnerVolumeImage, "runner volume image ax-server mounts into task containers; empty runs the runner baked into each task image (repo:tag or repo@sha256:...)")
}

type localFlags struct {
	imageFlags
	subcmd  string
	name    string
	workers int
	output  string
	help    bool
}

func parseLocalArgs(args []string) (localFlags, error) {
	if len(args) == 0 {
		return localFlags{help: true}, nil
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		return localFlags{help: true}, nil
	}

	subcmd := args[0]
	fs := flag.NewFlagSet("ax local "+subcmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	out := localFlags{subcmd: subcmd}
	fs.StringVar(&out.name, "name", cluster.DefaultName, "local cluster name")
	fs.IntVar(&out.workers, "workers", 1, "worker replicas")
	out.register(fs)
	fs.StringVar(&out.output, "o", "", "output format of status: json")
	fs.StringVar(&out.output, "output", "", "output format of status: json")
	fs.BoolVar(&out.help, "h", false, "help")
	fs.BoolVar(&out.help, "help", false, "help")

	if err := fs.Parse(args[1:]); err != nil {
		return localFlags{}, err
	}
	if fs.NArg() > 0 {
		return localFlags{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if out.output != "" && out.output != "json" {
		return localFlags{}, fmt.Errorf("unsupported output format %q (expected: json)", out.output)
	}
	if out.output != "" && subcmd != "status" {
		return localFlags{}, fmt.Errorf("-o is only supported by 'ax local status'")
	}
	return out, nil
}

func runLocal(axNamespace string, args []string) error {
	parsed, err := parseLocalArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, localUsage())
		return err
	}
	if parsed.help {
		fmt.Println(localUsage())
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	provider := cluster.NewKindProvider(nil, os.Stdout)
	provider.AXNamespace = axNamespace

	switch parsed.subcmd {
	case "start":
		// Fail fast if image inputs are gated before provisioning a Kind cluster.
		srvImg, taskImg, runnerImg, err := axdeploy.ResolveImageInputs(parsed.serverImage, parsed.defaultTaskImage, parsed.runnerVolumeImage)
		if err != nil {
			return err
		}
		// Step 1: Provision or resume the local Kind cluster and select its kubeconfig context.
		info, err := provider.Up(ctx, cluster.UpOptions{
			Name: parsed.name,
		})
		if err != nil {
			return err
		}
		// Step 2: Bootstrap Agent Substrate (PKI, mTLS Postgres, control plane, and gVisor workers).
		if err := installsubstrate.Bootstrap(ctx, installsubstrate.BootstrapOptions{
			KubeContext:    info.KubeContext,
			AXNamespace:    axNamespace,
			WorkerReplicas: parsed.workers,
			Out:            os.Stdout,
		}); err != nil {
			return err
		}
		// Step 3: Resolve AX image digests, register Substrate caller permissions, and deploy AX.
		if err := axdeploy.Install(ctx, axdeploy.InstallOptions{
			KubeContext:       info.KubeContext,
			Namespace:         axNamespace,
			ServerImage:       srvImg,
			DefaultTaskImage:  taskImg,
			RunnerVolumeImage: runnerImg,
			Out:               os.Stdout,
		}); err != nil {
			return err
		}
		fmt.Printf("\nLocal environment %q is ready (active context: %s).\n", info.Name, info.KubeContext)
		return nil

	case "status":
		if parsed.output == "json" {
			// Tool download progress goes to stderr, so stdout is only the report.
			provider.Out = os.Stderr
			return writeLocalStatusJSON(ctx, os.Stdout, provider.Runner, provider, parsed.name)
		}
		st, err := provider.Status(ctx, parsed.name)
		if err != nil {
			return err
		}
		return formatLocalStatus(os.Stdout, st)

	case "stop":
		if err := provider.Stop(ctx, parsed.name); err != nil {
			return err
		}
		fmt.Printf("Local cluster %q stopped.\n", parsed.name)
		return nil

	case "delete":
		if err := provider.Delete(ctx, parsed.name); err != nil {
			return err
		}
		fmt.Printf("Local cluster %q deleted.\n", parsed.name)
		return nil

	default:
		fmt.Fprintln(os.Stderr, localUsage())
		return fmt.Errorf("unknown subcommand %q (expected: start, stop, status, delete)", parsed.subcmd)
	}
}

func formatLocalStatus(out io.Writer, st cluster.Status) error {
	w := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
	clusterLabel := "N/A"
	if st.State != cluster.StateAbsent && st.Name != "" && st.Name != "N/A" {
		clusterLabel = fmt.Sprintf("%s (%s)", st.Name, st.Backend)
	}
	fmt.Fprintf(w, "Cluster:\t%s\n", clusterLabel)
	fmt.Fprintf(w, "State:\t%s\n", st.State)
	fmt.Fprintf(w, "Kube Context:\t%s\n", st.KubeContext)
	fmt.Fprintf(w, "Substrate:\t%s\n", st.SubstrateStatus)
	fmt.Fprintf(w, "AX Server:\t%s\n", st.AXStatus)
	return w.Flush()
}

type installFlags struct {
	imageFlags
	help bool
}

func parseInstallArgs(args []string) (installFlags, error) {
	fs := flag.NewFlagSet("ax install", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var out installFlags
	out.register(fs)
	fs.BoolVar(&out.help, "h", false, "help")
	fs.BoolVar(&out.help, "help", false, "help")

	if err := fs.Parse(args); err != nil {
		return installFlags{}, err
	}
	if fs.NArg() > 0 {
		return installFlags{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return out, nil
}

func runInstall(kubeContext, axNamespace string, args []string) error {
	parsed, err := parseInstallArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, installUsage())
		return err
	}
	if parsed.help {
		fmt.Println(installUsage())
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return axdeploy.Install(ctx, axdeploy.InstallOptions{
		KubeContext:       kubeContext,
		Namespace:         axNamespace,
		ServerImage:       parsed.serverImage,
		DefaultTaskImage:  parsed.defaultTaskImage,
		RunnerVolumeImage: parsed.runnerVolumeImage,
		Out:               os.Stdout,
	})
}

func runUninstall(kubeContext, axNamespace string, args []string) error {
	for _, a := range args {
		if a == "-h" || a == "--help" {
			fmt.Println(uninstallUsage)
			return nil
		}
	}
	if len(args) > 0 {
		return fmt.Errorf("unexpected argument %q", args[0])
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return axdeploy.Uninstall(ctx, axdeploy.UninstallOptions{
		KubeContext: kubeContext,
		Namespace:   axNamespace,
		Out:         os.Stdout,
	})
}

// localStatusReport is the output of `ax local status -o json`. AX Studio
// parses it, so fields are only added, never renamed or removed.
type localStatusReport struct {
	Name            string          `json:"name"`
	Backend         string          `json:"backend"`
	State           cluster.State   `json:"state"`
	KubeContext     string          `json:"kubeContext"`
	Nodes           []string        `json:"nodes"`
	SubstrateStatus string          `json:"substrateStatus"`
	AXStatus        string          `json:"axStatus"`
	Host            localHostReport `json:"host"`
}

// localHostReport describes the container engine that runs the local cluster,
// and whether its Linux host (or macOS Linux VM) can run one.
type localHostReport struct {
	// Runtime is "docker" or "podman", or "" when no running engine was found.
	Runtime       string `json:"runtime"`
	KernelVersion string `json:"kernelVersion"`
	MemTotalBytes int64  `json:"memTotalBytes"`
	Rootless      bool   `json:"rootless"`
	// Error is why `ax local start` would refuse this host; "" if it would not.
	Error string `json:"error"`
	// Warning is a problem that does not stop `ax local start`.
	Warning string `json:"warning"`
}

// inspectLocalHost reports the container engine and applies the checks that
// `ax local start` applies before it creates a cluster.
func inspectLocalHost(ctx context.Context, r executil.Runner, hostOS string) localHostReport {
	rt, err := executil.ResolveContainerRuntime(ctx, r)
	if err != nil {
		return localHostReport{Error: err.Error()}
	}
	host := localHostReport{Runtime: "docker"}
	if rt.IsPodman {
		host.Runtime = "podman"
	}
	info, ok := executil.InspectContainerHost(ctx, r, rt)
	if !ok {
		return host
	}
	host.KernelVersion = info.KernelVersion
	host.MemTotalBytes = info.MemTotalBytes
	host.Rootless = info.Rootless
	var warn bytes.Buffer
	if err := executil.ValidateContainerHost(info, rt.IsPodman, hostOS, &warn); err != nil {
		host.Error = err.Error()
	}
	host.Warning = strings.TrimSpace(strings.TrimPrefix(warn.String(), "==> Warning: "))
	return host
}

// writeLocalStatusJSON writes the report for the local cluster called name.
// A host with no running container engine is a report, not an error: the
// cluster is then NotFound and host.error says why.
func writeLocalStatusJSON(ctx context.Context, out io.Writer, r executil.Runner, provider cluster.Provider, name string) error {
	host := inspectLocalHost(ctx, r, runtime.GOOS)
	st := cluster.Status{
		Name:            "N/A",
		Backend:         "kind",
		State:           cluster.StateAbsent,
		KubeContext:     "N/A",
		SubstrateStatus: "N/A",
		AXStatus:        "N/A",
	}
	if host.Runtime != "" {
		var err error
		if st, err = provider.Status(ctx, name); err != nil {
			return err
		}
	}
	nodes := st.Nodes
	if nodes == nil {
		nodes = []string{}
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(localStatusReport{
		Name:            st.Name,
		Backend:         st.Backend,
		State:           st.State,
		KubeContext:     st.KubeContext,
		Nodes:           nodes,
		SubstrateStatus: st.SubstrateStatus,
		AXStatus:        st.AXStatus,
		Host:            host,
	})
}
