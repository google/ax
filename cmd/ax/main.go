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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/ax/internal/guest"
	"github.com/google/ax/internal/local"
	"github.com/google/ax/internal/tunnel"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	opts := parseGlobalArgs(os.Args[1:])
	var (
		cmd         = opts.cmd
		cleanArgs   = opts.args
		atespace    = opts.atespace
		kubeContext = opts.kubeContext
		axNamespace = opts.namespace
	)

	if cmd == "" {
		printUsage()
		os.Exit(1)
	}

	// Commands that don't require an AX server connection
	switch cmd {
	case "version":
		fmt.Println("ax version v1alpha1 (standalone redis engine)")
		return
	case "help", "-h", "--help":
		printUsage()
		return
	case "ctx", "context":
		if err := runContext(opts, cleanArgs); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		return
	case "config":
		if err := runConfig(opts, cleanArgs); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		return
	case "tunnel":
		if err := runTunnel(cleanArgs); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		return
	case "ssh":
		// Answer help and usage errors before resolving the server, which may
		// start a port-forward to the cluster.
		if _, _, help, err := parseSSHArgs(cleanArgs); help || err != nil {
			fmt.Println(sshUsage)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
			return
		}
	}

	// Pick the control plane: the in-process local one by default, or AX in
	// a Kubernetes cluster (auto-tunneled) or at an explicit server.
	tgt, err := resolveTarget(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	var serverURL string
	cleanup := func() {}
	if tgt.local {
		configureLocalLogging()
		env, err := local.Open(local.Options{Progress: os.Stderr})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: starting local AX: %v\n", err)
			os.Exit(1)
		}
		cleanup = func() { _ = env.Close() }
		dialAX = func(string) (*grpc.ClientConn, error) { return env.Dial() }
		// Creating a task may pull its image first.
		applyTimeout = localApplyTimeout
	} else {
		kubeContext = tgt.kubeContext
		serverURL, err = tunnel.EnsureServerURL(tunnel.Options{
			ServerURL: tgt.serverURL,
			Context:   kubeContext,
			Namespace: axNamespace,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	}

	switch cmd {
	case "apply":
		var atespaceFlag *string
		if opts.atespaceExplicit {
			atespaceFlag = &atespace
		}
		err = runApply(serverURL, atespaceFlag, cleanArgs)
	case "get":
		err = runGet(serverURL, atespace, cleanArgs)
	case "describe":
		err = runDescribe(serverURL, atespace, cleanArgs)
	case "watch":
		err = runWatch(serverURL, atespace, cleanArgs)
	case "suspend":
		err = runSuspend(serverURL, atespace, cleanArgs)
	case "resume":
		err = runResume(serverURL, atespace, cleanArgs)
	case "delete":
		err = runDelete(serverURL, atespace, cleanArgs)
	case "ssh":
		err = runSSH(serverURL, atespace, kubeContext, cleanArgs)
	default:
		cleanup()
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		printUsage()
		os.Exit(1)
	}

	cleanup()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// configureLocalLogging quiets the embedded server's logs, which are written
// for a server log rather than a terminal; failures reach the user as command
// errors anyway. AX_DEBUG=1 shows everything.
func configureLocalLogging() {
	level := slog.LevelError + 4 // above every level the server uses
	if os.Getenv("AX_DEBUG") != "" {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
}

// globalArgs holds the command name, the flags shared by every command, and the
// remaining arguments for the command itself.
type globalArgs struct {
	cmd              string
	args             []string
	atespace         string
	atespaceExplicit bool
	server           string
	kubeContext      string
	namespace        string
}

// parseGlobalArgs extracts the command name and the global flags from args. It
// stops interpreting flags at "--", so everything after it (for example the
// command run by `ax ssh <task> -- ...`) is passed through to the command intact.
func parseGlobalArgs(args []string) globalArgs {
	g := globalArgs{atespace: "default", namespace: v1alpha1.DefaultSystemNamespace}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			g.args = append(g.args, args[i:]...)
			break
		} else if arg == "-a" || arg == "--atespace" {
			if i+1 < len(args) {
				g.atespace = args[i+1]
				g.atespaceExplicit = true
				i++
			}
		} else if strings.HasPrefix(arg, "--atespace=") {
			g.atespace = strings.TrimPrefix(arg, "--atespace=")
			g.atespaceExplicit = true
		} else if arg == "--server" {
			if i+1 < len(args) {
				g.server = args[i+1]
				i++
			}
		} else if strings.HasPrefix(arg, "--server=") {
			g.server = strings.TrimPrefix(arg, "--server=")
		} else if arg == "--context" {
			if i+1 < len(args) {
				g.kubeContext = args[i+1]
				i++
			}
		} else if strings.HasPrefix(arg, "--context=") {
			g.kubeContext = strings.TrimPrefix(arg, "--context=")
		} else if arg == "-n" || arg == "--namespace" {
			if i+1 < len(args) {
				g.namespace = args[i+1]
				i++
			}
		} else if strings.HasPrefix(arg, "--namespace=") {
			g.namespace = strings.TrimPrefix(arg, "--namespace=")
		} else if g.cmd == "" && !strings.HasPrefix(arg, "-") {
			g.cmd = arg
		} else {
			g.args = append(g.args, arg)
		}
	}
	return g
}

func printUsage() {
	fmt.Println(`AX CLI - Autonomous agent execution control

Usage:
  ax [command] [flags]

Available Commands:
  apply -f <file>         Apply resources (tasks, workspaces, models) from a file or stdin
  get tasks               List tasks
  get task <name>         Get a specific task
  get workspaces          List workspaces
  get workspace <name>    Get a specific workspace
  get models              List models
  get model <name>        Get a specific model
  describe task <name>    Show detailed information about a task
  describe workspace <name> Show detailed information about a workspace
  describe model <name>   Show detailed information about a model
  watch task <name>       Stream live status and condition updates for a task
  ssh <task-name> [-- cmd] Run a command or shell inside the running task container
                          (Ctrl-C is forwarded; press it twice quickly, or Ctrl-\, to exit locally)
  suspend task <name>     Suspend execution of a task and checkpoint state
  resume task <name>      Resume execution of a suspended task
  delete task <name>      Delete a task
  delete workspace <name> Delete a workspace
  delete model <name>     Delete a model
  ctx, context            Show where commands are sent (local, a Kubernetes context, or a server)
  config use-context <name> Choose where commands go: local, kube (follow kubectx), or a kube context
  config current-context  Print the context in use
  config get-contexts     List available contexts
  tunnel <list|stop>      Manage background tunnels to Kubernetes clusters
  version                 Print AX version

By default ax runs everything locally: state is kept in ~/.ax/db.sqlite and
tasks run as containers via Podman (or Docker; set $AX_CONTAINER_RUNTIME).

Flags:
  -a, --atespace string       Task atespace scope (default: "default")
  -n, --namespace string      Kubernetes namespace where AX is installed (default: "ax-system")
  --context string            "local" or a Kubernetes context with AX installed
                              (default: $AX_CONTEXT, then 'ax config use-context', then local)
  --server string             AX API server address (default: $AX_SERVER)`)
}

// dialAX connects to the AX API at serverURL. In local mode main replaces it
// with a connection to the in-process server.
var dialAX = func(serverURL string) (*grpc.ClientConn, error) {
	target := strings.TrimPrefix(serverURL, "http://")
	target = strings.TrimPrefix(target, "https://")
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", target, err)
	}
	return conn, nil
}

func getAXClient(serverURL string) (v1alpha1.AXClient, *grpc.ClientConn, error) {
	conn, err := dialAX(serverURL)
	if err != nil {
		return nil, nil, err
	}
	return v1alpha1.NewAXClient(conn), conn, nil
}

// applyTimeout bounds `ax apply`. Local mode raises it to localApplyTimeout
// because creating a task may first pull its image.
var applyTimeout = 30 * time.Second

const localApplyTimeout = 15 * time.Minute

// A nil atespace leaves manifest atespaces unchanged and lets the server default missing ones.
func runApply(serverURL string, atespace *string, args []string) error {
	data, ok, err := manifestFromArgs(args)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("missing required flag: -f <file>")
	}

	client, conn, err := getAXClient(serverURL)
	if err != nil {
		return err
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), applyTimeout)
	defer cancel()

	// Manifests are parsed here and submitted one resource at a time through the
	// typed RPCs; the server never sees raw YAML.
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	for docIndex := 1; ; docIndex++ {
		var doc yaml.Node
		if err := decoder.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("decoding document %d: %w", docIndex, err)
		}
		if doc.Kind == 0 || (doc.Kind == yaml.DocumentNode && len(doc.Content) == 0) {
			continue // empty document, e.g. a trailing "---"
		}
		if doc.Kind == yaml.DocumentNode && len(doc.Content) == 1 {
			root := doc.Content[0]
			// yaml.v3 represents empty documents as implicitly tagged null scalars with no value.
			if root.Kind == yaml.ScalarNode && root.Tag == "!!null" && root.Value == "" && root.Style&yaml.TaggedStyle == 0 {
				continue
			}
		}

		kind, name, outcome, err := applyDocument(ctx, client, &doc, atespace)
		if err != nil {
			return fmt.Errorf("applying document %d: %w", docIndex, err)
		}
		fmt.Printf("%s.ax.io/%s %s\n", strings.ToLower(kind), name, outcome)
	}
}

// applyDocument decodes one manifest by its kind and submits it with the matching
// RPC. It reports the kind, the resource name, and whether the resource was
// created, configured (spec changed), or unchanged, in the style of kubectl apply.
func applyDocument(ctx context.Context, client v1alpha1.AXClient, doc *yaml.Node, atespace *string) (kind, name, outcome string, err error) {
	var head struct {
		Kind string `yaml:"kind"`
	}
	if err := doc.Decode(&head); err != nil {
		return "", "", "", fmt.Errorf("reading kind: %w", err)
	}

	switch head.Kind {
	case v1alpha1.KindTask:
		var task v1alpha1.Task
		if err := doc.Decode(&task); err != nil {
			return "", "", "", err
		}
		if err := setApplyAtespace(task.Metadata, atespace); err != nil {
			return "", "", "", err
		}
		res, err := client.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: &task})
		if err != nil {
			return "", "", "", err
		}
		return head.Kind, res.GetMetadata().GetName(), "created", nil

	case v1alpha1.KindWorkspace:
		var ws v1alpha1.Workspace
		if err := doc.Decode(&ws); err != nil {
			return "", "", "", err
		}
		if err := setApplyAtespace(ws.Metadata, atespace); err != nil {
			return "", "", "", err
		}
		existing, err := client.GetWorkspace(ctx, &v1alpha1.GetWorkspaceRequest{Atespace: ws.GetMetadata().GetAtespace(), Name: ws.GetMetadata().GetName()})
		outcome, err := applyOutcome(err, existing.GetSpec(), ws.GetSpec())
		if err != nil {
			return "", "", "", err
		}
		res, err := client.UpdateWorkspace(ctx, &v1alpha1.UpdateWorkspaceRequest{Workspace: &ws})
		return head.Kind, res.GetMetadata().GetName(), outcome, err

	case v1alpha1.KindModel:
		var m v1alpha1.Model
		if err := doc.Decode(&m); err != nil {
			return "", "", "", err
		}
		if err := setApplyAtespace(m.Metadata, atespace); err != nil {
			return "", "", "", err
		}
		existing, err := client.GetModel(ctx, &v1alpha1.GetModelRequest{Atespace: m.GetMetadata().GetAtespace(), Name: m.GetMetadata().GetName()})
		outcome, err := applyOutcome(err, existing.GetSpec(), m.GetSpec())
		if err != nil {
			return "", "", "", err
		}
		res, err := client.UpdateModel(ctx, &v1alpha1.UpdateModelRequest{Model: &m})
		return head.Kind, res.GetMetadata().GetName(), outcome, err

	case "":
		return "", "", "", errors.New("missing kind")
	default:
		return "", "", "", fmt.Errorf("unsupported kind %q (expected Task, Workspace, or Model)", head.Kind)
	}
}

// setApplyAtespace fills a missing atespace or rejects a mismatch with an explicit flag.
func setApplyAtespace(meta *v1alpha1.ObjectMeta, atespace *string) error {
	if meta == nil || atespace == nil {
		return nil
	}
	if meta.Atespace != "" && meta.Atespace != *atespace {
		return fmt.Errorf("metadata.atespace %q does not match --atespace %q", meta.Atespace, *atespace)
	}
	meta.Atespace = *atespace
	return nil
}

// applyOutcome classifies an apply from the result of looking up the existing
// resource: "created" when it did not exist, "unchanged" when its spec already
// matches, and "configured" otherwise. Lookup failures other than NotFound are
// returned as errors.
func applyOutcome(lookupErr error, existingSpec, newSpec proto.Message) (string, error) {
	if lookupErr != nil {
		if status.Code(lookupErr) == codes.NotFound {
			return "created", nil
		}
		return "", fmt.Errorf("looking up existing resource: %w", lookupErr)
	}
	if proto.Equal(existingSpec, newSpec) {
		return "unchanged", nil
	}
	return "configured", nil
}

func runGet(serverURL, atespace string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("specify resource to get (e.g. 'ax get tasks' or 'ax get task <name>')")
	}

	resource := strings.ToLower(args[0])

	client, conn, err := getAXClient(serverURL)
	if err != nil {
		return err
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if (resource == "tasks" || resource == "task") && len(args) == 1 {
		resp, err := client.ListTasks(ctx, &v1alpha1.ListTasksRequest{Atespace: atespace})
		if err != nil {
			return fmt.Errorf("listing tasks: %w", err)
		}

		tasks := resp.Tasks

		w := tabwriter.NewWriter(os.Stdout, 0, 8, 3, ' ', 0)
		fmt.Fprintln(w, "NAME\tATESPACE\tPHASE\tACTOR\tWORKER-IP\tAGE")
		for _, t := range tasks {
			workerIP := ""
			actor := ""
			phase := "Pending"
			if t.Status != nil {
				workerIP = t.Status.WorkerIp
				actor = t.Status.Actor
				if t.Status.Phase != "" {
					phase = t.Status.Phase
				}
			}
			if workerIP == "" {
				workerIP = "<none>"
			}
			if actor == "" {
				actor = "<none>"
			}
			age := "<unknown>"
			name := ""
			tAtespace := ""
			if t.Metadata != nil {
				name = t.Metadata.Name
				tAtespace = t.Metadata.Atespace
				if t.Metadata.CreateTime != nil {
					age = formatAge(time.Since(t.Metadata.CreateTime.AsTime()))
				}
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
				name,
				tAtespace,
				phase,
				actor,
				workerIP,
				age,
			)
		}
		return w.Flush()
	}

	if (resource == "task" || resource == "tasks") && len(args) >= 2 {
		name := args[1]
		task, err := client.GetTask(ctx, &v1alpha1.GetTaskRequest{Atespace: atespace, Name: name})
		if err != nil {
			return fmt.Errorf("getting task %q: %w", name, err)
		}

		return yaml.NewEncoder(os.Stdout).Encode(task)
	}

	if (resource == "workspaces" || resource == "workspace") && len(args) == 1 {
		resp, err := client.ListWorkspaces(ctx, &v1alpha1.ListWorkspacesRequest{Atespace: atespace})
		if err != nil {
			return fmt.Errorf("listing workspaces: %w", err)
		}

		workspaces := resp.Workspaces

		w := tabwriter.NewWriter(os.Stdout, 0, 8, 3, ' ', 0)
		fmt.Fprintln(w, "NAME\tATESPACE\tGIT-REPOS\tMCP-SERVERS")
		for _, ws := range workspaces {
			gitCount := "0"
			mcpCount := "0"
			if ws.Spec != nil {
				gitCount = fmt.Sprintf("%d", len(ws.Spec.Git))
				if ws.Spec.Mcp != nil {
					mcpCount = fmt.Sprintf("%d", len(ws.Spec.Mcp.Servers))
				}
			}
			name := ""
			wsAtespace := ""
			if ws.Metadata != nil {
				name = ws.Metadata.Name
				wsAtespace = ws.Metadata.Atespace
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
				name,
				wsAtespace,
				gitCount,
				mcpCount,
			)
		}
		return w.Flush()
	}

	if (resource == "workspace" || resource == "workspaces") && len(args) >= 2 {
		name := args[1]
		ws, err := client.GetWorkspace(ctx, &v1alpha1.GetWorkspaceRequest{Atespace: atespace, Name: name})
		if err != nil {
			return fmt.Errorf("getting workspace %q: %w", name, err)
		}

		return yaml.NewEncoder(os.Stdout).Encode(ws)
	}

	if (resource == "models" || resource == "model") && len(args) == 1 {
		resp, err := client.ListModels(ctx, &v1alpha1.ListModelsRequest{Atespace: atespace})
		if err != nil {
			return fmt.Errorf("listing models: %w", err)
		}

		models := resp.Models

		w := tabwriter.NewWriter(os.Stdout, 0, 8, 3, ' ', 0)
		fmt.Fprintln(w, "NAME\tATESPACE\tPROVIDER\tMODEL")
		for _, m := range models {
			name := ""
			mAtespace := ""
			if m.Metadata != nil {
				name = m.Metadata.Name
				mAtespace = m.Metadata.Atespace
			}
			provider := ""
			modelName := ""
			if m.Spec != nil {
				provider = m.Spec.Provider
				modelName = m.Spec.Model
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
				name,
				mAtespace,
				provider,
				modelName,
			)
		}
		return w.Flush()
	}

	if (resource == "model" || resource == "models") && len(args) >= 2 {
		name := args[1]
		m, err := client.GetModel(ctx, &v1alpha1.GetModelRequest{Atespace: atespace, Name: name})
		if err != nil {
			return fmt.Errorf("getting model %q: %w", name, err)
		}

		return yaml.NewEncoder(os.Stdout).Encode(m)
	}

	return fmt.Errorf("unknown resource %q", resource)
}

func runDescribe(serverURL, atespace string, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: ax describe <task|workspace|model> <name>")
	}
	kind := strings.ToLower(args[0])
	name := args[1]

	client, conn, err := getAXClient(serverURL)
	if err != nil {
		return err
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if kind == "model" || kind == "models" {
		m, err := client.GetModel(ctx, &v1alpha1.GetModelRequest{Atespace: atespace, Name: name})
		if err != nil {
			return fmt.Errorf("getting model %q: %w", name, err)
		}

		mName := ""
		mAtespace := ""
		if m.Metadata != nil {
			mName = m.Metadata.Name
			mAtespace = m.Metadata.Atespace
		}
		fmt.Printf("Name:               %s\n", mName)
		fmt.Printf("Atespace:           %s\n", mAtespace)
		if m.Spec != nil {
			fmt.Printf("Provider:           %s\n", m.Spec.Provider)
			fmt.Printf("Model:              %s\n", m.Spec.Model)
			if params := m.Spec.GetParameters().AsMap(); len(params) > 0 {
				fmt.Println("Parameters:")
				keys := make([]string, 0, len(params))
				for k := range params {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					fmt.Printf("  %s: %v\n", k, params[k])
				}
			}
			if m.Spec.SecretKey != nil {
				if m.Spec.SecretKey.Name != "" && m.Spec.SecretKey.Key != "" && m.Spec.SecretKey.Name != m.Spec.SecretKey.Key {
					fmt.Printf("Secret Key:         %s (key: %s)\n", m.Spec.SecretKey.Name, m.Spec.SecretKey.Key)
				} else if m.Spec.SecretKey.Key != "" {
					fmt.Printf("Secret Key:         %s\n", m.Spec.SecretKey.Key)
				} else if m.Spec.SecretKey.Name != "" {
					fmt.Printf("Secret Key:         %s\n", m.Spec.SecretKey.Name)
				}
			}
		}
		return nil
	}

	if kind == "workspace" || kind == "workspaces" {
		ws, err := client.GetWorkspace(ctx, &v1alpha1.GetWorkspaceRequest{Atespace: atespace, Name: name})
		if err != nil {
			return fmt.Errorf("getting workspace %q: %w", name, err)
		}

		wsName := ""
		wsAtespace := ""
		if ws.Metadata != nil {
			wsName = ws.Metadata.Name
			wsAtespace = ws.Metadata.Atespace
		}
		fmt.Printf("Name:         %s\n", wsName)
		fmt.Printf("Atespace:     %s\n", wsAtespace)
		if ws.Spec != nil {
			if len(ws.Spec.Git) > 0 {
				fmt.Println("Git Repositories:")
				for _, g := range ws.Spec.Git {
					branch := g.Branch
					if branch == "" {
						branch = "main"
					}
					fmt.Printf("  - %s (%s, branch: %s)\n", g.Name, g.Repo, branch)
				}
			}
			if ws.Spec.Mcp != nil {
				if len(ws.Spec.Mcp.Registries) > 0 {
					fmt.Println("MCP Registries:")
					for _, r := range ws.Spec.Mcp.Registries {
						fmt.Printf("  - Provider: %s, Query: %q\n", r.Provider, r.Query)
					}
				}
				if len(ws.Spec.Mcp.Servers) > 0 {
					fmt.Println("MCP Servers:")
					for _, s := range ws.Spec.Mcp.Servers {
						if s.Endpoint != "" {
							fmt.Printf("  - %s: %s\n", s.Name, s.Endpoint)
						} else {
							fmt.Printf("  - %s: %s %v\n", s.Name, s.Command, s.Args)
						}
					}
				}
			}
			if ws.Spec.Skills != nil {
				if len(ws.Spec.Skills.Registries) > 0 {
					fmt.Println("Skill Registries:")
					for _, r := range ws.Spec.Skills.Registries {
						fmt.Printf("  - Provider: %s, Query: %q\n", r.Provider, r.Query)
					}
				}
				if ws.Spec.Skills.Path != "" {
					fmt.Printf("Skills Path:      %s\n", ws.Spec.Skills.Path)
				}
			}
		}
		return nil
	}

	task, err := client.GetTask(ctx, &v1alpha1.GetTaskRequest{Atespace: atespace, Name: name})
	if err != nil {
		return fmt.Errorf("getting task %q: %w", name, err)
	}

	taskName := ""
	taskAtespace := ""
	if task.Metadata != nil {
		taskName = task.Metadata.Name
		taskAtespace = task.Metadata.Atespace
	}
	phase := ""
	actor := ""
	workerIP := ""
	var conditions []*v1alpha1.Condition
	if task.Status != nil {
		phase = task.Status.Phase
		actor = task.Status.Actor
		workerIP = task.Status.WorkerIp
		conditions = task.Status.Conditions
	}

	fmt.Printf("Name:         %s\n", taskName)
	fmt.Printf("Atespace:     %s\n", taskAtespace)
	fmt.Printf("Phase:        %s\n", phase)
	fmt.Printf("Actor:        %s\n", actor)
	fmt.Printf("Worker IP:    %s\n", workerIP)
	if task.Spec != nil {
		if ref := task.Spec.GetWorkspace(); ref != nil {
			fmt.Printf("Workspace:    %s  path=%s", ref.Name, task.Spec.WorkspacePath())
			if ref.Goal != "" {
				fmt.Printf("  goal=%q", ref.Goal)
			}
			fmt.Println()
		}
		if task.Spec.Image != "" {
			fmt.Printf("Image:        %s\n", task.Spec.Image)
		}
		if len(task.Spec.Command) > 0 {
			fmt.Printf("Command:      %v\n", task.Spec.Command)
		}
	}

	if len(conditions) > 0 {
		fmt.Println("\nConditions:")
		w := tabwriter.NewWriter(os.Stdout, 0, 8, 2, ' ', 0)
		fmt.Fprintln(w, "  TYPE\tSTATUS\tREASON\tMESSAGE")
		for _, c := range conditions {
			fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", c.Type, c.Status, c.Reason, c.Message)
		}
		_ = w.Flush()
	}

	return nil
}

func runWatch(serverURL, atespace string, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: ax watch task <name>")
	}
	name := args[1]

	client, conn, err := getAXClient(serverURL)
	if err != nil {
		return err
	}
	defer conn.Close()

	stream, err := client.WatchTask(context.Background(), &v1alpha1.WatchTaskRequest{Atespace: atespace, Name: name})
	if err != nil {
		return fmt.Errorf("watching task: %w", err)
	}

	fmt.Printf("Watching task %s/%s...\n", atespace, name)

	for {
		resp, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("receiving stream update: %w", err)
		}

		task := resp.Task
		if task != nil {
			phase := ""
			actor := ""
			workerIP := ""
			if task.Status != nil {
				phase = task.Status.Phase
				actor = task.Status.Actor
				workerIP = task.Status.WorkerIp
			}
			fmt.Printf("[%s] Phase: %-10s Actor: %-18s WorkerIP: %s\n",
				time.Now().Format("15:04:05"),
				phase,
				actor,
				workerIP,
			)
			if phase == "Completed" || phase == "Failed" {
				fmt.Printf("Task reached terminal phase %q.\n", phase)
				break
			}
		}
	}
	return nil
}

// runDelete removes one resource by kind and name.
func runDelete(serverURL, atespace string, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: ax delete <task|workspace|model> <name>")
	}
	kind, err := normalizeKind(args[0])
	if err != nil {
		return err
	}

	client, conn, err := getAXClient(serverURL)
	if err != nil {
		return err
	}
	defer conn.Close()

	timeout := 30 * time.Second
	if kind == v1alpha1.KindTask {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	return deleteResource(ctx, client, kind, atespace, args[1])
}

// deleteResource requests deletion and prints a confirmation.
func deleteResource(ctx context.Context, client v1alpha1.AXClient, kind, atespace, name string) error {
	lower := strings.ToLower(kind)

	var err error
	switch kind {
	case v1alpha1.KindTask:
		_, err = client.DeleteTask(ctx, &v1alpha1.DeleteTaskRequest{Atespace: atespace, Name: name})
	case v1alpha1.KindWorkspace:
		_, err = client.DeleteWorkspace(ctx, &v1alpha1.DeleteWorkspaceRequest{Atespace: atespace, Name: name})
	case v1alpha1.KindModel:
		_, err = client.DeleteModel(ctx, &v1alpha1.DeleteModelRequest{Atespace: atespace, Name: name})
	default:
		return fmt.Errorf("unsupported kind %q", kind)
	}
	if err != nil {
		return fmt.Errorf("deleting %s %s/%s: %w", lower, atespace, name, err)
	}

	fmt.Printf("%s.ax.io/%s deleted\n", lower, name)
	return nil
}

// normalizeKind maps user-typed kinds ("task", "tasks", "Task") to the canonical
// manifest kind, rejecting anything unknown.
func normalizeKind(kind string) (string, error) {
	switch strings.ToLower(strings.TrimSuffix(kind, "s")) {
	case "task":
		return v1alpha1.KindTask, nil
	case "workspace":
		return v1alpha1.KindWorkspace, nil
	case "model":
		return v1alpha1.KindModel, nil
	case "":
		return "", errors.New("missing kind")
	default:
		return "", fmt.Errorf("unsupported kind %q (expected task, workspace, or model)", kind)
	}
}

// manifestFromArgs returns the manifest named by -f/--file (or stdin for "-").
// ok is false when no -f flag is present.
func manifestFromArgs(args []string) (data []byte, ok bool, err error) {
	for i := 0; i < len(args); i++ {
		if args[i] != "-f" && args[i] != "--file" {
			continue
		}
		if i+1 >= len(args) {
			return nil, true, errors.New("-f requires a file path (or - for stdin)")
		}
		path := args[i+1]
		if path == "-" {
			data, err = io.ReadAll(os.Stdin)
		} else {
			data, err = os.ReadFile(path)
		}
		if err != nil {
			return nil, true, fmt.Errorf("reading %s: %w", path, err)
		}
		return data, true, nil
	}
	return nil, false, nil
}

// lifecycleRPCTimeout bounds suspend/resume RPCs, which wait for Substrate to
// snapshot or restore the task's sandbox. MicroVM-backed sandboxes can take
// well over a minute to snapshot/restore, so this is deliberately generous.
// The server renews its per-task Redis lock for as long as the RPC is in
// flight, so this deadline is not constrained by the lock TTL (see
// lock.RedisLocker).
const lifecycleRPCTimeout = 5 * time.Minute

func runSuspend(serverURL, atespace string, args []string) error {
	name := ""
	if len(args) == 1 {
		name = args[0]
	} else if len(args) >= 2 {
		if args[0] == "task" || args[0] == "tasks" {
			name = args[1]
		} else {
			name = args[0]
		}
	} else {
		return fmt.Errorf("usage: ax suspend task <name>")
	}

	client, conn, err := getAXClient(serverURL)
	if err != nil {
		return err
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), lifecycleRPCTimeout)
	defer cancel()

	if _, err := client.SuspendTask(ctx, &v1alpha1.SuspendTaskRequest{Atespace: atespace, Name: name}); err != nil {
		return fmt.Errorf("suspending task: %w", err)
	}

	fmt.Printf("task.ax.io/%s suspended\n", name)
	return nil
}

func runResume(serverURL, atespace string, args []string) error {
	name := ""
	if len(args) == 1 {
		name = args[0]
	} else if len(args) >= 2 {
		if args[0] == "task" || args[0] == "tasks" {
			name = args[1]
		} else {
			name = args[0]
		}
	} else {
		return fmt.Errorf("usage: ax resume task <name>")
	}

	client, conn, err := getAXClient(serverURL)
	if err != nil {
		return err
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), lifecycleRPCTimeout)
	defer cancel()

	if _, err := client.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{Atespace: atespace, Name: name}); err != nil {
		return fmt.Errorf("resuming task: %w", err)
	}

	fmt.Printf("task.ax.io/%s resumed\n", name)
	return nil
}

func formatAge(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func runTunnel(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("specify tunnel action: 'ax tunnel list' or 'ax tunnel stop [context]'")
	}

	switch args[0] {
	case "list":
		tunnels, err := tunnel.ListTunnels()
		if err != nil {
			return err
		}
		if len(tunnels) == 0 {
			fmt.Println("No active tunnels found in ~/.ax/tunnels.")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 8, 2, ' ', 0)
		fmt.Fprintln(w, "CONTEXT\tLOCAL PORT\tREMOTE SERVICE\tPID\tSTATUS\tCREATED")
		for _, t := range tunnels {
			status := "Active"
			if !tunnel.IsTunnelHealthy(t.Port) {
				status = "Stale"
			}
			fmt.Fprintf(w, "%s\t%d\t%s/%s\t%d\t%s\t%s\n",
				t.Context, t.Port, t.Namespace, t.Service, t.PID, status, t.CreatedAt.Format("2006-01-02 15:04:05"))
		}
		return w.Flush()

	case "stop":
		if len(args) > 1 {
			ctxName := args[1]
			if err := tunnel.StopTunnelByContext(ctxName); err != nil {
				return fmt.Errorf("stopping tunnel for context %q: %w", ctxName, err)
			}
			fmt.Printf("Tunnel stopped for context %q\n", ctxName)
			return nil
		}
		cur, err := tunnel.CurrentContext("")
		if err != nil || cur == "" {
			return fmt.Errorf("no current context found to stop tunnel")
		}
		if err := tunnel.StopTunnelByContext(cur); err != nil {
			return fmt.Errorf("stopping tunnel for context %q: %w", cur, err)
		}
		fmt.Printf("Tunnel stopped for context %q\n", cur)
		return nil

	default:
		return fmt.Errorf("unknown tunnel subcommand: %s (available: list, stop)", args[0])
	}
}

const sshUsage = `Usage:
  ax ssh <task-name> [-- command...]

Run a command inside a running task's container, or /bin/sh when no command is
given. The task must be Running and have spec.debug: true.

Examples:
  ax ssh task123
  ax ssh task123 -- ls -la /workspace
  ax ssh -a my-atespace task123 -- python3 main.py`

// parseSSHArgs splits the arguments of `ax ssh` into the task name and the command
// to run, defaulting the command to /bin/sh. help reports a -h or --help given in
// place of the task name. Arguments after the task name form the command, passed
// verbatim, with or without a separating "--" directly after the task name.
func parseSSHArgs(args []string) (taskName string, command []string, help bool, err error) {
	if len(args) == 0 || args[0] == "--" {
		return "", nil, false, errors.New("missing task name")
	}
	if args[0] == "-h" || args[0] == "--help" {
		return "", nil, true, nil
	}
	if strings.HasPrefix(args[0], "-") {
		return "", nil, false, fmt.Errorf("unknown flag %q", args[0])
	}

	taskName = args[0]
	// Only a "--" directly after the task name is the separator. Any later "--"
	// belongs to the remote command (for example `git log -- file`).
	rest := args[1:]
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}
	command = append(command, rest...)
	if len(command) == 0 {
		command = []string{"/bin/sh"}
	}
	return taskName, command, false, nil
}

func runSSH(serverURL, atespace, kubeContext string, args []string) error {
	taskName, cmdToRun, _, err := parseSSHArgs(args)
	if err != nil {
		return err
	}

	client, conn, err := getAXClient(serverURL)
	if err != nil {
		return err
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	task, err := client.GetTask(ctx, &v1alpha1.GetTaskRequest{
		Atespace: atespace,
		Name:     taskName,
	})
	if err != nil {
		return fmt.Errorf("fetching task %q: %w", taskName, err)
	}

	if task == nil || task.Status == nil {
		return fmt.Errorf("task %q has no status available", taskName)
	}

	if task.Status.Phase != "Running" {
		return fmt.Errorf("task %q is in phase %q (must be Running to ssh)", taskName, task.Status.Phase)
	}

	if !task.GetSpec().GetDebug() {
		return fmt.Errorf("task %q does not expose guest services; set spec.debug: true and re-apply to enable ax ssh", taskName)
	}

	workerIP := task.Status.WorkerIp
	if workerIP == "" {
		return fmt.Errorf("task %q has no worker IP assigned", taskName)
	}

	// The runner serves guest services on port 80 unless the worker IP says otherwise.
	host := workerIP
	port := 80
	if h, p, err := net.SplitHostPort(workerIP); err == nil {
		host = h
		if parsedPort, err := strconv.Atoi(p); err == nil {
			port = parsedPort
		}
	}

	targetActor := fmt.Sprintf("%s/%s", task.Metadata.Atespace, task.Status.Actor)
	var (
		guestClient *guest.Client
		cleanup     func()
	)

	// 1. First, check if worker IP is directly reachable (e.g. within cluster or local mesh)
	d := net.Dialer{Timeout: 500 * time.Millisecond}
	guestEndpoint := net.JoinHostPort(host, fmt.Sprint(port))
	if testConn, dialErr := d.Dial("tcp", guestEndpoint); dialErr == nil {
		_ = testConn.Close()
		guestClient, err = guest.Dial(guestEndpoint)
		if err != nil {
			return fmt.Errorf("connecting to guest at %s: %w", guestEndpoint, err)
		}
	} else {
		// 2. Connect via the Substrate atenet-router service in ate-system (port 80)
		localPort, pfCleanup, err := tunnel.PortForward(context.Background(), kubeContext, "ate-system", "svc/atenet-router", 80)
		if err != nil {
			return fmt.Errorf("establishing port-forward to atenet-router: %w", err)
		}
		cleanup = pfCleanup
		routerEndpoint := fmt.Sprintf("127.0.0.1:%d", localPort)
		guestClient, err = guest.DialTarget(routerEndpoint, targetActor)
		if err != nil {
			return fmt.Errorf("connecting to guest via atenet-router at %s: %w", routerEndpoint, err)
		}
	}

	if cleanup != nil {
		defer cleanup()
	}
	defer guestClient.Close()

	// A bare shell on a terminal runs with -i, so it prompts and Ctrl-C stops the
	// command it is running rather than the shell itself.
	if stdinIsTerminal() && len(cmdToRun) == 1 && cmdToRun[0] == "/bin/sh" {
		cmdToRun = []string{"/bin/sh", "-i"}
	}

	// Ctrl-C is passed on to the remote command instead of ending ax ssh. Pressing
	// it twice in quick succession gives up on the session and exits locally, so
	// a hung connection never traps the terminal.
	rawSigs := make(chan os.Signal, 4)
	signal.Notify(rawSigs, os.Interrupt)
	defer signal.Stop(rawSigs)
	sigs := escalateInterrupts(rawSigs, interruptExitWindow, func() {
		fmt.Fprintln(os.Stderr, "\nax ssh: interrupted twice, exiting")
		os.Exit(130)
	})

	exitCode, err := guestClient.Exec(context.Background(), guest.ExecOptions{
		Command: cmdToRun,
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Signals: sigs,
	})
	if err != nil {
		return err
	}

	if exitCode != 0 {
		os.Exit(exitCode)
	}

	return nil
}

// interruptExitWindow is how soon a second Ctrl-C must follow the first to end
// ax ssh locally instead of being forwarded to the remote command.
const interruptExitWindow = time.Second

// escalateInterrupts relays each signal from in to the returned channel, except
// that a signal arriving within window of the previous one calls exit instead.
// This keeps a single Ctrl-C going to the remote command while a repeated one
// still works as an escape hatch.
func escalateInterrupts(in <-chan os.Signal, window time.Duration, exit func()) <-chan os.Signal {
	out := make(chan os.Signal, 1)
	go func() {
		var last time.Time
		for sig := range in {
			now := time.Now()
			if !last.IsZero() && now.Sub(last) < window {
				exit()
				return
			}
			last = now
			select {
			case out <- sig:
			default:
			}
		}
	}()
	return out
}

// stdinIsTerminal reports whether standard input is a terminal rather than a
// pipe or file.
func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
