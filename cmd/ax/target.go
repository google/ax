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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/google/ax/internal/container"
	"github.com/google/ax/internal/local"
	"github.com/google/ax/internal/tunnel"
	"gopkg.in/yaml.v3"
)

// localContext is the AX context name for the built-in local control plane.
const localContext = "local"

// followKubeContext is the AX context name that follows the active
// Kubernetes context (kubectx / current-context), tunneling to AX there.
const followKubeContext = "kube"

// contextEnv overrides the configured AX context for one invocation.
const contextEnv = "AX_CONTEXT"

// cliConfig is persisted in ~/.ax/config.yaml.
type cliConfig struct {
	// Context is "local" (the default) or the name of a Kubernetes context
	// with AX installed.
	Context string `yaml:"context,omitempty"`
}

func configPath() (string, error) {
	home, err := local.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "config.yaml"), nil
}

func loadConfig() (cliConfig, error) {
	var cfg cliConfig
	path, err := configPath()
	if err != nil {
		return cfg, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parsing %s: %w", path, err)
	}
	return cfg, nil
}

func saveConfig(cfg cliConfig) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// target is where commands are sent.
type target struct {
	// local runs the control plane in-process.
	local bool
	// serverURL is set for an explicit server.
	serverURL string
	// kubeContext is set when AX is reached through a Kubernetes context.
	kubeContext string
	// source explains how the target was chosen, for `ax ctx`.
	source string
}

// resolveTarget picks the control plane, in order of precedence: --server,
// $AX_SERVER, --context, $AX_CONTEXT, the context saved by `ax config use-context`, and
// finally the local control plane.
func resolveTarget(opts globalArgs) (target, error) {
	if opts.server != "" {
		return target{serverURL: opts.server, source: "--server flag"}, nil
	}
	if env := os.Getenv("AX_SERVER"); env != "" {
		return target{serverURL: env, source: "$AX_SERVER"}, nil
	}
	name, source := opts.kubeContext, "--context flag"
	if name == "" {
		name, source = os.Getenv(contextEnv), "$"+contextEnv
	}
	if name == "" {
		cfg, err := loadConfig()
		if err != nil {
			return target{}, err
		}
		name, source = cfg.Context, "`ax config use-context`"
	}
	if name == "" {
		return target{local: true, source: "default"}, nil
	}
	if name == localContext {
		return target{local: true, source: source}, nil
	}
	if name == followKubeContext {
		cur, err := tunnel.CurrentContext("")
		if err != nil || cur == "" {
			return target{}, fmt.Errorf("AX context %q follows the active Kubernetes context, but none is set (kubectl config current-context)", followKubeContext)
		}
		return target{kubeContext: cur, source: source + ", following the active Kubernetes context"}, nil
	}
	return target{kubeContext: name, source: source}, nil
}

const configUsage = `Usage:
  ax config use-context <local|kube|kube-context>   Choose where ax sends commands
  ax config current-context                         Print the AX context in use
  ax config get-contexts                            List the contexts ax can use

Contexts:
  local          The built-in local control plane (default)
  kube           Follow the active Kubernetes context (kubectl config current-context)
  <kube-context> A specific Kubernetes context with AX installed`

// runConfig implements `ax config`, modeled on `kubectl config`. The choice is
// stored in ~/.ax/config.yaml; the kubeconfig is never modified.
func runConfig(opts globalArgs, args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Println(configUsage)
		return nil
	}
	switch args[0] {
	case "use-context":
		if len(args) != 2 {
			return fmt.Errorf("usage: ax config use-context <local|kube|kube-context>")
		}
		name := args[1]
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		cfg.Context = name
		if name == localContext {
			cfg.Context = "" // local is the default; keep the file minimal
		}
		if err := saveConfig(cfg); err != nil {
			return err
		}
		fmt.Printf("Switched to context %q.\n", name)
		return nil

	case "current-context":
		t, err := resolveTarget(opts)
		if err != nil {
			return err
		}
		fmt.Println(t.name())
		return nil

	case "get-contexts":
		t, err := resolveTarget(opts)
		if err != nil {
			return err
		}
		cfg, _ := loadConfig()
		configured := cfg.Context
		if configured == "" {
			configured = localContext
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 8, 3, ' ', 0)
		fmt.Fprintln(w, "CURRENT\tNAME\tDESCRIPTION")
		mark := func(name string) string {
			if name == configured {
				return "*"
			}
			return ""
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", mark(localContext), localContext, "local control plane (~/.ax, container runtime)")
		follow := "follow the active Kubernetes context"
		if cur, err := tunnel.CurrentContext(""); err == nil && cur != "" {
			follow += " (" + cur + ")"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", mark(followKubeContext), followKubeContext, follow)
		for _, name := range kubeContexts() {
			fmt.Fprintf(w, "%s\t%s\t%s\n", mark(name), name, "Kubernetes context")
		}
		if err := w.Flush(); err != nil {
			return err
		}
		if t.source != "`ax config use-context`" && t.source != "default" {
			fmt.Printf("\nNote: %s overrides the saved context; commands go to %q.\n", t.source, t.name())
		}
		return nil

	default:
		return fmt.Errorf("unknown config subcommand %q\n\n%s", args[0], configUsage)
	}
}

// kubeContexts lists the contexts in the kubeconfig, or nothing when kubectl
// is unavailable.
func kubeContexts() []string {
	out, err := exec.Command("kubectl", "config", "get-contexts", "-o", "name").Output()
	if err != nil {
		return nil
	}
	var names []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			names = append(names, l)
		}
	}
	return names
}

// name is the context name `ax config current-context` prints.
func (t target) name() string {
	switch {
	case t.serverURL != "":
		return t.serverURL
	case t.local:
		return localContext
	default:
		return t.kubeContext
	}
}

// runContext implements `ax ctx`: a summary of where commands go and how.
func runContext(opts globalArgs, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("ax ctx takes no arguments; use 'ax config use-context <name>' to switch")
	}

	t, err := resolveTarget(opts)
	if err != nil {
		return err
	}
	switch {
	case t.serverURL != "":
		fmt.Printf("AX Context:   server %s (from %s)\n", t.serverURL, t.source)
	case t.local:
		home, _ := local.Home()
		fmt.Printf("AX Context:   local (from %s)\n", t.source)
		fmt.Printf("Database:     %s\n", local.DBPath(home))
		if rt, err := container.Detect(); err == nil {
			fmt.Printf("Runtime:      %s\n", rt.Bin)
		} else {
			fmt.Printf("Runtime:      none (%v)\n", err)
		}
	default:
		fmt.Printf("AX Context:   %s (Kubernetes, from %s)\n", t.kubeContext, t.source)
		if tun, err := tunnel.GetTunnel(t.kubeContext); err == nil && tun != nil {
			state := "Healthy"
			if !tunnel.IsTunnelHealthy(tun.Port) {
				state = "Stale / Not responding"
			}
			fmt.Printf("AX Tunnel:    http://127.0.0.1:%d -> %s/svc/%s:8080 (PID: %d, %s)\n",
				tun.Port, tun.Namespace, tun.Service, tun.PID, state)
		} else {
			fmt.Println("AX Tunnel:    No background tunnel running (will auto-connect on next command)")
		}
	}
	return nil
}
