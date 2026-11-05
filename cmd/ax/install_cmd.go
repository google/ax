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
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"

	"github.com/google/ax/internal/install/axdeploy"
	"github.com/google/ax/internal/install/cluster"
	installsubstrate "github.com/google/ax/internal/install/substrate"
	"github.com/google/ax/internal/tunnel"
)

const localUsage = `Usage:
  ax local <up|stop|status|delete> [flags]

Manage a self-contained local Kubernetes + Agent Substrate + AX environment.

Subcommands:
  up       Create or resume the local cluster, install Substrate + AX, and select its context
  status   Show health of the local cluster, Substrate control plane, and AX server
  stop     Stop the local cluster containers while preserving state
  delete   Destroy the local cluster and clean up its context

Flags:
  --name string               Local cluster name (default: "local")
  --workers int               Number of gVisor worker replicas (default: 2)
  --from-source string        Build and push ax-server + ax-task-runner from local AX repo path
  --image-repo string         Container image repository override for AX images
  --image-tag string          Container image tag override for AX images
  --server-image string       Explicit ax-server image reference
  --task-runner-image string  Explicit ax-task-runner image reference
  -n, --namespace string      Kubernetes namespace for AX (default: "ax-system")`

const installUsage = `Usage:
  ax install [flags]

Install or upgrade AX (ax-postgres + ax-server) and register Substrate caller
permissions on an existing Kubernetes + Agent Substrate cluster.

Flags:
  --context string            Kubernetes context (defaults to active current-context)
  -n, --namespace string      Kubernetes namespace for AX (default: "ax-system")
  --from-source string        Build and push ax-server + ax-task-runner from local AX repo path
  --image-repo string         Container image repository (default: "ghcr.io/google/ax")
  --image-tag string          Container image tag (default: "latest")
  --server-image string       Explicit ax-server image reference
  --task-runner-image string  Explicit ax-task-runner image reference`

const uninstallUsage = `Usage:
  ax uninstall [flags]

Remove AX workloads from a cluster (preserving ax-postgres PVCs by default) and
revoke ax-server's caller permissions in Agent Substrate.

Flags:
  --context string            Kubernetes context (defaults to active current-context)
  -n, --namespace string      Kubernetes namespace where AX is installed (default: "ax-system")`

type localFlags struct {
	subcmd          string
	name            string
	workers         int
	fromSource      string
	imageRepo       string
	imageTag        string
	serverImage     string
	taskRunnerImage string
	help            bool
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
	fs.IntVar(&out.workers, "workers", 2, "worker replicas")
	fs.StringVar(&out.fromSource, "from-source", "", "AX source directory")
	fs.StringVar(&out.imageRepo, "image-repo", "", "AX image repository")
	fs.StringVar(&out.imageTag, "image-tag", "", "AX image tag")
	fs.StringVar(&out.serverImage, "server-image", "", "ax-server image")
	fs.StringVar(&out.taskRunnerImage, "task-runner-image", "", "ax-task-runner image")
	fs.BoolVar(&out.help, "h", false, "help")
	fs.BoolVar(&out.help, "help", false, "help")

	if err := fs.Parse(args[1:]); err != nil {
		return localFlags{}, err
	}
	if fs.NArg() > 0 {
		return localFlags{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return out, nil
}

func runLocal(axNamespace string, args []string) error {
	parsed, err := parseLocalArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, localUsage)
		return err
	}
	if parsed.help {
		fmt.Println(localUsage)
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	provider := cluster.NewKindProvider(nil, os.Stdout)
	provider.AXNamespace = axNamespace

	switch parsed.subcmd {
	case "up":
		info, err := provider.Up(ctx, cluster.UpOptions{
			Name:         parsed.name,
			WithRegistry: parsed.fromSource != "",
		})
		if err != nil {
			return err
		}
		if err := installsubstrate.Bootstrap(ctx, installsubstrate.BootstrapOptions{
			KubeContext:    info.KubeContext,
			AXNamespace:    axNamespace,
			WorkerReplicas: parsed.workers,
			Out:            os.Stdout,
		}); err != nil {
			return err
		}
		imgRepo := parsed.imageRepo
		if imgRepo == "" && parsed.fromSource != "" {
			imgRepo = info.RegistryAddr
		}
		if err := axdeploy.Install(ctx, axdeploy.InstallOptions{
			KubeContext:     info.KubeContext,
			Namespace:       axNamespace,
			ImageRepo:       imgRepo,
			ImageTag:        parsed.imageTag,
			ServerImage:     parsed.serverImage,
			TaskRunnerImage: parsed.taskRunnerImage,
			FromSource:      parsed.fromSource,
			Out:             os.Stdout,
		}); err != nil {
			return err
		}
		fmt.Printf("\nLocal environment %q is ready (active context: %s).\n", info.Name, info.KubeContext)
		return nil

	case "status":
		st, err := provider.Status(ctx, parsed.name)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 8, 2, ' ', 0)
		fmt.Fprintf(w, "Cluster:\t%s (%s)\n", st.Name, st.Backend)
		fmt.Fprintf(w, "State:\t%s\n", st.State)
		fmt.Fprintf(w, "Kube Context:\t%s\n", st.KubeContext)
		if st.RegistryAddr != "" {
			fmt.Fprintf(w, "Local Registry:\t%s\n", st.RegistryAddr)
		}
		fmt.Fprintf(w, "Substrate:\t%s\n", st.SubstrateStatus)
		fmt.Fprintf(w, "AX Server:\t%s\n", st.AXStatus)
		return w.Flush()

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
		fmt.Fprintln(os.Stderr, localUsage)
		return fmt.Errorf("unknown subcommand %q (expected: up, stop, status, delete)", parsed.subcmd)
	}
}

type installFlags struct {
	fromSource      string
	imageRepo       string
	imageTag        string
	serverImage     string
	taskRunnerImage string
	help            bool
}

func parseInstallArgs(args []string) (installFlags, error) {
	fs := flag.NewFlagSet("ax install", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var out installFlags
	fs.StringVar(&out.fromSource, "from-source", "", "AX source directory")
	fs.StringVar(&out.imageRepo, "image-repo", "", "AX image repository")
	fs.StringVar(&out.imageTag, "image-tag", "", "AX image tag")
	fs.StringVar(&out.serverImage, "server-image", "", "ax-server image")
	fs.StringVar(&out.taskRunnerImage, "task-runner-image", "", "ax-task-runner image")
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
		fmt.Fprintln(os.Stderr, installUsage)
		return err
	}
	if parsed.help {
		fmt.Println(installUsage)
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return axdeploy.Install(ctx, axdeploy.InstallOptions{
		KubeContext:     kubeContext,
		Namespace:       axNamespace,
		ImageRepo:       parsed.imageRepo,
		ImageTag:        parsed.imageTag,
		ServerImage:     parsed.serverImage,
		TaskRunnerImage: parsed.taskRunnerImage,
		FromSource:      parsed.fromSource,
		Out:             os.Stdout,
	})
}

func runUninstall(kubeContext, axNamespace string, args []string) error {
	for _, a := range args {
		if a == "-h" || a == "--help" {
			fmt.Println(uninstallUsage)
			return nil
		}
		return fmt.Errorf("unexpected argument %q", a)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return axdeploy.Uninstall(ctx, axdeploy.UninstallOptions{
		KubeContext: kubeContext,
		Namespace:   axNamespace,
		Out:         os.Stdout,
	})
}

func runContextCommand(kubeContext string, args []string, out io.Writer) error {
	if out == nil {
		out = os.Stdout
	}
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintln(out, "Usage:\n  ax ctx              List Kubernetes contexts and active AX tunnel status\n  ax ctx <name>       Switch active Kubernetes context in kubeconfig")
		return nil
	}
	if len(args) > 1 {
		return fmt.Errorf("too many arguments (usage: ax ctx [context-name])")
	}
	if len(args) == 1 {
		target := args[0]
		if err := tunnel.SetCurrentContext(target); err != nil {
			return err
		}
		fmt.Fprintf(out, "Switched to context %q.\n", target)
		return nil
	}

	entries, err := tunnel.ListContexts(kubeContext)
	if err != nil || len(entries) == 0 {
		ctxName, ctxErr := tunnel.CurrentContext(kubeContext)
		if ctxErr != nil || ctxName == "" {
			fmt.Fprintln(out, "No active Kubernetes context detected.")
			fmt.Fprintln(out, "Fallback server: http://localhost:8080 (or set $AX_SERVER / --server)")
			return nil
		}
		fmt.Fprintf(out, "Active Kubernetes Context: %s\n", ctxName)
		return nil
	}

	w := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
	fmt.Fprintln(w, "CURRENT\tNAME\tCLUSTER\tAX TUNNEL")
	for _, e := range entries {
		cur := ""
		if e.Current {
			cur = "*"
		}
		tunDesc := "-"
		if t, err := tunnel.GetTunnel(e.Name); err == nil && t != nil {
			if tunnel.IsTunnelHealthy(t.Port) {
				tunDesc = fmt.Sprintf("127.0.0.1:%d -> %s/%s (Healthy)", t.Port, t.Namespace, t.Service)
			} else {
				tunDesc = fmt.Sprintf("127.0.0.1:%d (Stale)", t.Port)
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", cur, e.Name, e.Cluster, tunDesc)
	}
	return w.Flush()
}
