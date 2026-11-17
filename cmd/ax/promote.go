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
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/google/ax/internal/tunnel"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const promoteUsage = `Usage:
  ax promote task <name> --to <context> [flags]

Promote an existing Task and its bound Workspace and Gateway to a target cluster.

The global flags select the source cluster, which is only read unless
--suspend-source is set. On the target, a Gateway that already exists is reused
as it is, a Workspace that already exists must have the same spec, and the Task
is created and resumed. The sandbox's files and the Secrets a Gateway names are
not copied.

Flags:
  --to string             Target Kubernetes context (required unless --to-server is set)
  --to-server string      Target AX server address, used instead of --to
  --to-namespace string   Kubernetes namespace where AX runs on the target (default: "ax-system")
  --to-atespace string    Target atespace (default: the atespace of the Task)
  --replace               Delete an existing Task of the same name on the target before creating
  --suspend-source        Suspend the Task on the source before the target resumes it
  --no-resume             Create the Task on the target without resuming it
  --preflight-only        Check the source and the target without changing the target`

type promoteFlags struct {
	taskName      string
	to            string
	toServer      string
	toNamespace   string
	toAtespace    string
	replace       bool
	suspendSource bool
	noResume      bool
	preflightOnly bool
	help          bool
}

// parsePromoteArgs parses the arguments of `ax promote`: the task name, written
// `task <name>` or `<name>`, and the flags, in any order.
func parsePromoteArgs(args []string) (promoteFlags, error) {
	fs := flag.NewFlagSet("ax promote", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var out promoteFlags
	fs.StringVar(&out.to, "to", "", "target Kubernetes context")
	fs.StringVar(&out.toServer, "to-server", "", "target AX server address")
	fs.StringVar(&out.toNamespace, "to-namespace", "", "Kubernetes namespace where AX runs on the target")
	fs.StringVar(&out.toAtespace, "to-atespace", "", "target atespace")
	fs.BoolVar(&out.replace, "replace", false, "delete an existing task of the same name on the target")
	fs.BoolVar(&out.suspendSource, "suspend-source", false, "suspend the task on the source")
	fs.BoolVar(&out.noResume, "no-resume", false, "create the task on the target without resuming it")
	fs.BoolVar(&out.preflightOnly, "preflight-only", false, "check without changing the target")
	fs.BoolVar(&out.help, "h", false, "help")
	fs.BoolVar(&out.help, "help", false, "help")

	// The flag package stops at the first argument that is not a flag, so take
	// the names out one at a time and parse what follows each.
	var names []string
	for rest := args; ; {
		if err := fs.Parse(rest); err != nil {
			return promoteFlags{}, err
		}
		if fs.NArg() == 0 {
			break
		}
		names = append(names, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	if out.help {
		return out, nil
	}
	if len(names) == 2 && names[0] == "task" {
		names = names[1:]
	}
	if len(names) != 1 {
		return promoteFlags{}, fmt.Errorf("expected one task name, got %d", len(names))
	}
	out.taskName = names[0]
	if out.to == "" && out.toServer == "" {
		return promoteFlags{}, errors.New("missing --to <context>")
	}
	return out, nil
}

// runPromote copies a task, with the gateway and the workspace it binds, from
// the source to the target and resumes it there. The target is checked before
// it is changed, so a promotion that the target would refuse leaves the target
// as it was. The source is only read, unless --suspend-source asks for the
// task to be suspended there.
func runPromote(serverURL, atespace string, args []string) error {
	flags, err := parsePromoteArgs(args)
	if err != nil {
		return err
	}
	name := flags.taskName
	targetAtespace := flags.toAtespace
	if targetAtespace == "" {
		targetAtespace = atespace
	}

	// The environment describes the source, so it plays no part in reaching
	// the target.
	endResolve := activeTrace.startStep("resolve target ax-server address")
	targetURL, err := tunnel.EnsureServerURL(tunnel.Options{ServerURL: flags.toServer, Context: flags.to, Namespace: flags.toNamespace, IgnoreEnv: true})
	endResolve(err)
	if err != nil {
		return err
	}

	source, conn, err := getAXClient(serverURL)
	if err != nil {
		return err
	}
	defer conn.Close()
	target, targetConn, err := getAXClient(targetURL)
	if err != nil {
		return err
	}
	defer targetConn.Close()

	// Room for setting up the workspace and for resuming the task.
	ctx, cancel := context.WithTimeout(context.Background(), 2*lifecycleRPCTimeout)
	defer cancel()

	// A step is one resource and what promoting does to it, on the target
	// unless source is set. do is nil when the target already has what the task
	// needs. note, if any, is printed after the outcome.
	type step struct {
		resource, outcome string
		note              string
		source            bool
		do                func() error
	}
	var steps []step

	task, err := source.GetTask(ctx, &v1alpha1.GetTaskRequest{Atespace: atespace, Name: name})
	if err != nil {
		return fmt.Errorf("getting task %q: %w", name, err)
	}

	// A gateway the target already has is the target's own policy under that
	// name, so it is reused as it is.
	if gwName := task.GetSpec().GetGateway().GetName(); gwName != "" {
		_, err := target.GetGateway(ctx, &v1alpha1.GetGatewayRequest{Atespace: targetAtespace, Name: gwName})
		switch {
		case err == nil:
			steps = append(steps, step{resource: "gateway.ax.io/" + gwName, outcome: "reused"})
		case status.Code(err) == codes.NotFound:
			gw, err := source.GetGateway(ctx, &v1alpha1.GetGatewayRequest{Atespace: atespace, Name: gwName})
			if err != nil {
				return fmt.Errorf("getting gateway %q: %w", gwName, err)
			}
			gw.Metadata = manifestMetadata(gw.Metadata, targetAtespace)
			// The credentials a gateway injects are not copied with it.
			var note string
			if uris := credentialURIs(gw); len(uris) > 0 {
				note = fmt.Sprintf("Note: the target must provide the credentials that gateway %s injects: %s", gwName, strings.Join(uris, ", "))
			}
			steps = append(steps, step{resource: "gateway.ax.io/" + gwName, outcome: "created", note: note, do: func() error {
				_, err := target.CreateGateway(ctx, &v1alpha1.CreateGatewayRequest{Gateway: gw})
				return err
			}})
		default:
			return fmt.Errorf("getting gateway %q on the target: %w", gwName, err)
		}
	}

	if wsName := task.GetSpec().GetWorkspace().GetName(); wsName != "" {
		ws, err := source.GetWorkspace(ctx, &v1alpha1.GetWorkspaceRequest{Atespace: atespace, Name: wsName})
		if err != nil {
			return fmt.Errorf("getting workspace %q: %w", wsName, err)
		}
		existing, err := target.GetWorkspace(ctx, &v1alpha1.GetWorkspaceRequest{Atespace: targetAtespace, Name: wsName})
		outcome, err := applyOutcome(err, existing.GetSpec(), ws.GetSpec())
		if err != nil {
			return fmt.Errorf("workspace %q on the target: %w", wsName, err)
		}
		if outcome == "configured" {
			return fmt.Errorf("workspace %s/%s on the target has a different spec, and workspaces are immutable", targetAtespace, wsName)
		}
		ws.Metadata = manifestMetadata(ws.Metadata, targetAtespace)
		steps = append(steps, step{resource: "workspace.ax.io/" + wsName, outcome: outcome, do: func() error {
			// For a workspace the target already has, this waits until it is set up.
			_, err := target.CreateWorkspace(ctx, &v1alpha1.CreateWorkspaceRequest{Workspace: ws})
			return err
		}})
	}

	existing, err := target.GetTask(ctx, &v1alpha1.GetTaskRequest{Atespace: targetAtespace, Name: name})
	outcome := "created"
	switch {
	case status.Code(err) == codes.NotFound:
	case err != nil:
		return fmt.Errorf("getting task %q on the target: %w", name, err)
	case existing.GetMetadata().GetUid() == task.GetMetadata().GetUid():
		return fmt.Errorf("source and target must differ: task %s/%s on the target is the task being promoted", targetAtespace, name)
	case !flags.replace:
		return fmt.Errorf("task %s/%s already exists on target; pass --replace to recreate it", targetAtespace, name)
	default:
		outcome = "replaced"
	}
	// CreateTask would keep the status, such as the source's WorkspaceReady.
	task.Metadata, task.Status = manifestMetadata(task.Metadata, targetAtespace), nil
	steps = append(steps, step{resource: "task.ax.io/" + name, outcome: outcome, do: func() error {
		if outcome == "replaced" {
			if _, err := target.DeleteTask(ctx, &v1alpha1.DeleteTaskRequest{Atespace: targetAtespace, Name: name}); err != nil {
				return err
			}
		}
		_, err := target.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: task})
		return err
	}})
	if flags.suspendSource {
		// Once the target has the task and before it runs there, so that the
		// task never runs on both.
		steps = append(steps, step{resource: "task.ax.io/" + name, outcome: "suspended", source: true, do: func() error {
			_, err := source.SuspendTask(ctx, &v1alpha1.SuspendTaskRequest{Atespace: atespace, Name: name})
			return err
		}})
	}
	if !flags.noResume {
		steps = append(steps, step{resource: "task.ax.io/" + name, outcome: "resumed", do: func() error {
			_, err := target.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{Atespace: targetAtespace, Name: name})
			return err
		}})
	}

	for _, s := range steps {
		where, outcome := "target", s.outcome
		if s.source {
			where, outcome = "source", s.outcome+" on the source"
		}
		if flags.preflightOnly {
			fmt.Printf("%s %s (preflight only)\n", s.resource, outcome)
		} else {
			if s.do != nil {
				if err := s.do(); err != nil {
					return fmt.Errorf("%s on the %s: %w", s.resource, where, err)
				}
			}
			fmt.Printf("%s %s\n", s.resource, outcome)
		}
		if s.note != "" {
			fmt.Fprintln(os.Stderr, s.note)
		}
	}
	return nil
}

// credentialURIs returns the credentials that the gateway's rules inject.
func credentialURIs(gw *v1alpha1.Gateway) []string {
	var uris []string
	for _, rule := range gw.GetSpec().GetEgress() {
		for _, header := range rule.GetHttps().GetEffects().GetReplaceHeaders() {
			uris = append(uris, header.GetCredentialUri())
		}
	}
	return uris
}

// manifestMetadata returns the metadata a manifest sets, in the given
// atespace. The server that receives it assigns the rest.
func manifestMetadata(meta *v1alpha1.ObjectMeta, atespace string) *v1alpha1.ObjectMeta {
	return &v1alpha1.ObjectMeta{Name: meta.GetName(), Atespace: atespace, Labels: meta.GetLabels(), Owner: meta.GetOwner()}
}
