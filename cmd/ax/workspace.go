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
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/google/ax/pkg/apis/v1alpha1"
)

func printWorkspaceRow(w io.Writer, ws *v1alpha1.Workspace) {
	fmt.Fprintf(w, "%s\t%s\t%d\t%d\n",
		ws.GetMetadata().GetName(),
		ws.GetMetadata().GetAtespace(),
		len(ws.GetSpec().GetSetup().GetGit()),
		len(ws.GetSpec().GetMcp().GetServers()),
	)
}

func describeWorkspace(out io.Writer, ws *v1alpha1.Workspace) {
	fmt.Fprintf(out, "Name:         %s\n", ws.GetMetadata().GetName())
	fmt.Fprintf(out, "Atespace:     %s\n", ws.GetMetadata().GetAtespace())
	if phase := ws.GetStatus().GetPhase(); phase != "" {
		fmt.Fprintf(out, "Phase:        %s\n", phase)
	}
	if lastErr := ws.GetStatus().GetLastSetupError(); lastErr != "" {
		fmt.Fprintf(out, "Last Error:   %s\n", lastErr)
	}

	describeWorkspaceProfile(out, ws.GetSpec().GetProfile())
	describeWorkspaceSetup(out, ws.GetSpec().GetSetup())
	describeWorkspaceMCP(out, ws.GetSpec().GetMcp())

	if conds := ws.GetStatus().GetConditions(); len(conds) > 0 {
		fmt.Fprintln(out, "\nConditions:")
		w := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
		fmt.Fprintln(w, "  TYPE\tSTATUS\tREASON\tMESSAGE")
		for _, c := range conds {
			fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", c.GetType(), c.GetStatus(), c.GetReason(), c.GetMessage())
		}
		_ = w.Flush()
	}
}

func describeWorkspaceProfile(out io.Writer, prof *v1alpha1.Profile) {
	if prof == nil {
		return
	}
	if prof.GetImage() != "" {
		fmt.Fprintf(out, "Image:        %s\n", prof.GetImage())
	}
	if limits := prof.GetResources().GetLimits(); limits != nil && (limits.GetCpu() != "" || limits.GetMemory() != "") {
		fmt.Fprintf(out, "Resources:    cpu=%s memory=%s\n", limits.GetCpu(), limits.GetMemory())
	}
	if prof.GetAteActorTemplate() != "" {
		fmt.Fprintf(out, "ATE Template: %s\n", prof.GetAteActorTemplate())
	}
}

func describeWorkspaceSetup(out io.Writer, setup *v1alpha1.WorkspaceSetup) {
	if setup == nil {
		return
	}
	if repos := setup.GetGit(); len(repos) > 0 {
		fmt.Fprintln(out, "Git Repositories:")
		for _, g := range repos {
			ref := g.GetRef()
			if ref == "" {
				ref = "main"
			}
			label := g.GetName()
			if label == "" {
				label = g.GetRepo()
			}
			fmt.Fprintf(out, "  - %s (%s, ref: %s)\n", label, g.GetRepo(), ref)
		}
	}
	if files := setup.GetFiles(); len(files) > 0 {
		fmt.Fprintln(out, "Setup Files:")
		for _, f := range files {
			fmt.Fprintf(out, "  - %s\n", f.GetPath())
		}
	}
	if cmds := setup.GetCommands(); len(cmds) > 0 {
		fmt.Fprintln(out, "Setup Commands:")
		for _, c := range cmds {
			fmt.Fprintf(out, "  - %s\n", c)
		}
	}
	if skills := setup.GetSkills(); skills != nil {
		if regs := skills.GetRegistries(); len(regs) > 0 {
			fmt.Fprintln(out, "Skill Registries:")
			for _, r := range regs {
				fmt.Fprintf(out, "  - Provider: %s, Query: %q\n", r.GetProvider(), r.GetQuery())
			}
		}
		if path := skills.GetPath(); path != "" {
			fmt.Fprintf(out, "Skills Path:  %s\n", path)
		}
	}
	if gw := setup.GetGateway().GetName(); gw != "" {
		fmt.Fprintf(out, "Setup Gateway: %s\n", gw)
	}
}

func describeWorkspaceMCP(out io.Writer, mcp *v1alpha1.MCPConfig) {
	if mcp == nil {
		return
	}
	if regs := mcp.GetRegistries(); len(regs) > 0 {
		fmt.Fprintln(out, "MCP Registries:")
		for _, r := range regs {
			fmt.Fprintf(out, "  - Provider: %s, Query: %q\n", r.GetProvider(), r.GetQuery())
		}
	}
	if servers := mcp.GetServers(); len(servers) > 0 {
		fmt.Fprintln(out, "MCP Servers:")
		for _, s := range servers {
			if s.GetEndpoint() != "" {
				fmt.Fprintf(out, "  - %s: %s\n", s.GetName(), s.GetEndpoint())
			} else {
				fmt.Fprintf(out, "  - %s: %s %v\n", s.GetName(), s.GetCommand(), s.GetArgs())
			}
		}
	}
}
