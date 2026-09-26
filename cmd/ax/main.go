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
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/ax/internal/guest"
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

	var (
		cmd            string
		cleanArgs      []string
		atespace       = "default"
		explicitServer = ""
		kubeContext    = ""
		axNamespace    = "ax-system"
	)

	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-a" || arg == "--atespace" {
			if i+1 < len(args) {
				atespace = args[i+1]
				i++
			}
		} else if strings.HasPrefix(arg, "--atespace=") {
			atespace = strings.TrimPrefix(arg, "--atespace=")
		} else if arg == "--server" {
			if i+1 < len(args) {
				explicitServer = args[i+1]
				i++
			}
		} else if strings.HasPrefix(arg, "--server=") {
			explicitServer = strings.TrimPrefix(arg, "--server=")
		} else if arg == "--context" {
			if i+1 < len(args) {
				kubeContext = args[i+1]
				i++
			}
		} else if strings.HasPrefix(arg, "--context=") {
			kubeContext = strings.TrimPrefix(arg, "--context=")
		} else if arg == "-n" || arg == "--namespace" {
			if i+1 < len(args) {
				axNamespace = args[i+1]
				i++
			}
		} else if strings.HasPrefix(arg, "--namespace=") {
			axNamespace = strings.TrimPrefix(arg, "--namespace=")
		} else if cmd == "" && !strings.HasPrefix(arg, "-") {
			cmd = arg
		} else {
			cleanArgs = append(cleanArgs, arg)
		}
	}

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
		if err := runContext(kubeContext); err != nil {
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
	}

	// Resolve the AX server URL (auto-tunneling to active kube context if not explicitly set)
	serverURL, err := tunnel.EnsureServerURL(tunnel.Options{
		ServerURL: explicitServer,
		Context:   kubeContext,
		Namespace: axNamespace,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	switch cmd {
	case "apply":
		err = runApply(serverURL, cleanArgs)
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
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		printUsage()
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
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
  suspend task <name>     Suspend execution of a task and checkpoint state
  resume task <name>      Resume execution of a suspended task
  delete task <name>      Delete a task
  delete workspace <name> Delete a workspace
  delete model <name>     Delete a model
  ctx, context            Show active Kubernetes context and AX connection
  tunnel <list|stop>      Manage background tunnels to Kubernetes clusters
  version                 Print AX version

Flags:
  -a, --atespace string       Task atespace scope (default: "default")
  -n, --namespace string      Kubernetes namespace where AX is installed (default: "ax-system")
  --context string            Kubernetes context (defaults to active kubectx / current-context)
  --server string             AX API server address (default: auto-detected from kube context or $AX_SERVER)`)
}

func getAXClient(serverURL string) (v1alpha1.AXClient, *grpc.ClientConn, error) {
	target := strings.TrimPrefix(serverURL, "http://")
	target = strings.TrimPrefix(target, "https://")
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, fmt.Errorf("connecting to %s: %w", target, err)
	}
	return v1alpha1.NewAXClient(conn), conn, nil
}

func runApply(serverURL string, args []string) error {
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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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

		kind, name, outcome, err := applyDocument(ctx, client, &doc)
		if err != nil {
			return fmt.Errorf("applying document %d: %w", docIndex, err)
		}
		fmt.Printf("%s.ax.io/%s %s\n", strings.ToLower(kind), name, outcome)
	}
}

// applyDocument decodes one manifest by its kind and submits it with the matching
// Update RPC. It reports the kind, the resource name, and whether the resource was
// created, configured (spec changed), or unchanged, in the style of kubectl apply.
func applyDocument(ctx context.Context, client v1alpha1.AXClient, doc *yaml.Node) (kind, name, outcome string, err error) {
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
				if t.Metadata.CreationTimestamp != nil {
					age = formatAge(time.Since(t.Metadata.CreationTimestamp.AsTime()))
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
		if refs := task.Spec.WorkspaceRefs(); len(refs) > 0 {
			paths := task.Spec.WorkspacePaths()
			fmt.Println("Workspaces:")
			for i, ref := range refs {
				fmt.Printf("  - %s  path=%s", ref.Name, paths[i])
				if ref.Goal != "" {
					fmt.Printf("  goal=%q", ref.Goal)
				}
				fmt.Println()
			}
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
			if phase == "Running" || phase == "Completed" || phase == "Failed" {
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

	// Task deletion waits for the controller to tear down the actor, which can take
	// a while; the other kinds are removed synchronously.
	timeout := 15 * time.Second
	if kind == v1alpha1.KindTask {
		timeout = deleteTaskTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	return deleteResource(ctx, client, kind, atespace, args[1])
}

const (
	deleteTaskTimeout  = 5 * time.Minute
	deletePollInterval = 500 * time.Millisecond
)

// deleteResource requests deletion, then blocks until the resource is really gone
// and prints a kubectl-style confirmation.
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

	if err := waitForDeletion(ctx, client, kind, atespace, name); err != nil {
		return err
	}
	fmt.Printf("%s.ax.io/%s deleted\n", lower, name)
	return nil
}

// waitForDeletion polls until the resource returns NotFound or ctx expires.
func waitForDeletion(ctx context.Context, client v1alpha1.AXClient, kind, atespace, name string) error {
	lookup := func() error {
		var err error
		switch kind {
		case v1alpha1.KindTask:
			_, err = client.GetTask(ctx, &v1alpha1.GetTaskRequest{Atespace: atespace, Name: name})
		case v1alpha1.KindWorkspace:
			_, err = client.GetWorkspace(ctx, &v1alpha1.GetWorkspaceRequest{Atespace: atespace, Name: name})
		case v1alpha1.KindModel:
			_, err = client.GetModel(ctx, &v1alpha1.GetModelRequest{Atespace: atespace, Name: name})
		}
		return err
	}

	announced := false
	for {
		err := lookup()
		if status.Code(err) == codes.NotFound {
			return nil
		}
		if err != nil {
			return fmt.Errorf("checking %s %s/%s after delete: %w", strings.ToLower(kind), atespace, name, err)
		}
		if !announced {
			fmt.Fprintf(os.Stderr, "waiting for %s %s/%s to be deleted...\n", strings.ToLower(kind), atespace, name)
			announced = true
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for %s %s/%s to be deleted; it is still being torn down (check `ax describe` and the controller logs)", strings.ToLower(kind), atespace, name)
		case <-time.After(deletePollInterval):
		}
	}
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

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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

func runContext(kubeContext string) error {
	ctxName, err := tunnel.CurrentContext(kubeContext)
	if err != nil || ctxName == "" {
		fmt.Println("No active Kubernetes context detected.")
		fmt.Println("Fallback server: http://localhost:8080 (or set $AX_SERVER / --server)")
		return nil
	}

	fmt.Printf("Active Kubernetes Context: %s\n", ctxName)
	if t, err := tunnel.GetTunnel(ctxName); err == nil && t != nil {
		status := "Healthy"
		if !tunnel.IsTunnelHealthy(t.Port) {
			status = "Stale / Not responding"
		}
		fmt.Printf("AX Tunnel:                 http://127.0.0.1:%d -> %s/svc/%s:8080 (PID: %d, %s)\n",
			t.Port, t.Namespace, t.Service, t.PID, status)
	} else {
		fmt.Println("AX Tunnel:                 No background tunnel running (will auto-connect on next command)")
	}
	return nil
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

func runSSH(serverURL, atespace, kubeContext string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: ax ssh <task-name> [-- command...]")
	}

	taskName := args[0]
	var cmdToRun []string
	for i := 1; i < len(args); i++ {
		if args[i] == "--" {
			cmdToRun = args[i+1:]
			break
		} else {
			cmdToRun = append(cmdToRun, args[i])
		}
	}

	if len(cmdToRun) == 0 {
		cmdToRun = []string{"/bin/sh"}
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

	exitCode, err := guestClient.Exec(context.Background(), guest.ExecOptions{
		Command: cmdToRun,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
	})
	if err != nil {
		return err
	}

	if exitCode != 0 {
		os.Exit(exitCode)
	}

	return nil
}
