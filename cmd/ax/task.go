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
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"gopkg.in/yaml.v3"
)

func printTaskRow(w io.Writer, t *v1alpha1.Task) {
	actor := t.GetStatus().GetActor()
	if actor == "" {
		actor = "<none>"
	}
	age := "<unknown>"
	if ct := t.GetMetadata().GetCreateTime(); ct != nil {
		age = formatAge(time.Since(ct.AsTime()))
	}
	fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
		t.GetMetadata().GetName(),
		t.GetMetadata().GetAtespace(),
		v1alpha1.FormatTaskState(t),
		actor,
		age,
	)
}

func describeTask(out io.Writer, task *v1alpha1.Task) {
	meta := task.GetMetadata()
	st := task.GetStatus()
	spec := task.GetSpec()

	fmt.Fprintf(out, "Name:         %s\n", meta.GetName())
	fmt.Fprintf(out, "Atespace:     %s\n", meta.GetAtespace())
	if uid := meta.GetUid(); uid != "" {
		fmt.Fprintf(out, "UID:          %s\n", uid)
	}
	fmt.Fprintf(out, "State:        %s\n", v1alpha1.FormatTaskState(task))
	if detail := st.GetStatusDetail(); detail != nil && (detail.GetReason() != v1alpha1.TaskStatusReason_TASK_STATUS_REASON_UNSPECIFIED || detail.GetDetail() != "") {
		fmt.Fprintf(out, "Detail:       %s (%s)\n", detail.GetReason(), detail.GetDetail())
	}
	fmt.Fprintf(out, "Actor:        %s\n", st.GetActor())
	actorUID := st.GetActorUid()
	if actorUID == "" {
		actorUID = "<none>"
	}
	fmt.Fprintf(out, "Actor UID:    %s\n", actorUID)
	if tpl := st.GetAteActorTemplate(); tpl != "" {
		fmt.Fprintf(out, "Template:     %s\n", tpl)
	}
	if attempt := st.GetAttempt(); attempt > 0 {
		fmt.Fprintf(out, "Attempt:      %d\n", attempt)
	}

	if spec != nil {
		if sid := spec.GetSessionId(); sid != "" {
			fmt.Fprintf(out, "Session ID:   %s\n", sid)
		}
		if ref := spec.GetWorkspace(); ref != nil {
			fmt.Fprintf(out, "Workspace:    %s  path=%s\n", ref.GetName(), spec.WorkspacePath())
		}
		if gw := spec.GetGateway().GetName(); gw != "" {
			fmt.Fprintf(out, "Gateway:      %s\n", gw)
		}
		describeWorkspaceProfile(out, spec.GetProfile())
		if cmds := spec.GetCommands(); len(cmds) > 0 {
			fmt.Fprintf(out, "Commands:     %v\n", cmds)
		}
		if goal := spec.GetGoal().GetSpec(); goal != "" {
			fmt.Fprintf(out, "Goal:         %s\n", goal)
		}
		if config := spec.GetConfig().AsMap(); len(config) > 0 {
			if cfgYAML, err := yaml.Marshal(config); err == nil {
				fmt.Fprintln(out, "Config:")
				for _, line := range strings.Split(strings.TrimRight(string(cfgYAML), "\n"), "\n") {
					fmt.Fprintf(out, "  %s\n", line)
				}
			}
		}
	}

	if outcome := st.GetOutcome(); outcome != nil {
		fmt.Fprintf(out, "Outcome:      %s (exitCode=%d)\n", outcome.GetResult(), outcome.GetExitCode())
		if output := outcome.GetOutput(); output != "" {
			fmt.Fprintln(out, "Output:")
			for _, line := range strings.Split(strings.TrimRight(output, "\n"), "\n") {
				fmt.Fprintf(out, "  %s\n", line)
			}
		}
	}

	if conditions := st.GetConditions(); len(conditions) > 0 {
		fmt.Fprintln(out, "\nConditions:")
		w := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
		fmt.Fprintln(w, "  TYPE\tSTATUS\tREASON\tMESSAGE")
		for _, c := range conditions {
			fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", c.GetType(), c.GetStatus(), c.GetReason(), c.GetMessage())
		}
		_ = w.Flush()
	}
}
