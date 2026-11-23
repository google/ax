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

package executil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// rolloutPollSlice is how long one `kubectl rollout status` call waits before
// WaitRollout looks at the pods of the rollout.
const rolloutPollSlice = 15 * time.Second

// rolloutTimedOutMessage is what `kubectl rollout status --timeout` prints
// when the rollout did not finish in time.
const rolloutTimedOutMessage = "timed out waiting for the condition"

// restartHint introduces, in the timeout error of WaitRollout, the command
// that restarts a workload whose container runs but is not ready.
const restartHint = "to restart it, run: "

// runningNotReady is how the pod status describes a container that runs but
// fails its readiness probe. Kubernetes does not restart such a container.
const runningNotReady = "running but not ready"

// RolloutOptions names the workload WaitRollout waits for.
type RolloutOptions struct {
	Kubectl     string
	KubeContext string
	Namespace   string
	// Target is a workload such as "deployment/ax-server".
	Target  string
	Timeout time.Duration
	// Out receives the status of the pods while the rollout is not done. Nil
	// prints nothing.
	Out io.Writer
}

// WaitRollout waits for a Deployment, StatefulSet, or DaemonSet to finish
// rolling out, like `kubectl rollout status --timeout`.
//
// `kubectl rollout status` reports only "timed out waiting for the
// condition", while the reason (an image that cannot be pulled, a container
// that crashes or never becomes ready, a pod that cannot be scheduled) is in
// the status of the pods. WaitRollout reads that status every
// rolloutPollSlice. It prints it to Out when it changes, and puts the last
// one in the error when the wait times out. When a container runs but is not
// ready, the error also names the command that restarts the workload.
// WaitRollout never ends the wait early.
func WaitRollout(ctx context.Context, r Runner, opts RolloutOptions) error {
	return waitRollout(ctx, r, opts, rolloutPollSlice)
}

func waitRollout(ctx context.Context, r Runner, opts RolloutOptions, slice time.Duration) error {
	fail := func(err error) error {
		return fmt.Errorf("waiting for %s/%s rollout: %w", opts.Namespace, opts.Target, err)
	}
	deadline := time.Now().Add(opts.Timeout)
	var selector, printed string
	for {
		wait := min(slice, time.Until(deadline))
		// kubectl takes whole seconds, and --timeout=0s means no timeout.
		seconds := max(1, int(wait.Round(time.Second).Seconds()))
		res, err := r.Run(ctx, Spec{
			Name: opts.Kubectl,
			Args: kubectlArgs(opts, "rollout", "status", opts.Target, fmt.Sprintf("--timeout=%ds", seconds)),
		})
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return fail(ctx.Err())
		}
		if !strings.Contains(res.Stderr+" "+err.Error(), rolloutTimedOutMessage) {
			// Not a timeout (the workload is missing, or the request was
			// refused): waiting longer does not help.
			return fail(err)
		}

		if selector == "" {
			selector = rolloutSelector(ctx, r, opts)
		}
		var status string
		if selector != "" {
			status = rolloutPodStatus(ctx, r, opts, selector)
		}
		if status != "" && status != printed && opts.Out != nil {
			fmt.Fprintf(opts.Out, "    %s/%s is not ready: %s\n", opts.Namespace, opts.Target, status)
			printed = status
		}
		if time.Until(deadline) <= 0 {
			if status == "" {
				return fail(err)
			}
			msg := fmt.Sprintf("timed out after %s: %s", opts.Timeout, status)
			if strings.Contains(status, runningNotReady) {
				restart := append([]string{opts.Kubectl}, kubectlArgs(opts, "rollout", "restart", opts.Target)...)
				msg += "; " + restartHint + strings.Join(restart, " ")
			}
			return fail(errors.New(msg))
		}
	}
}

func kubectlArgs(opts RolloutOptions, args ...string) []string {
	var full []string
	if opts.KubeContext != "" {
		full = append(full, "--context", opts.KubeContext)
	}
	full = append(full, "-n", opts.Namespace)
	return append(full, args...)
}

// rolloutSelector returns the label selector of the pods the workload owns,
// or "" when it cannot be read.
func rolloutSelector(ctx context.Context, r Runner, opts RolloutOptions) string {
	res, err := r.Run(ctx, Spec{
		Name: opts.Kubectl,
		Args: kubectlArgs(opts, "get", opts.Target, "-o", "jsonpath={.spec.selector.matchLabels}"),
	})
	if err != nil {
		return ""
	}
	var labels map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &labels); err != nil || len(labels) == 0 {
		return ""
	}
	pairs := make([]string, 0, len(labels))
	for k, v := range labels {
		pairs = append(pairs, k+"="+v)
	}
	sort.Strings(pairs)
	return strings.Join(pairs, ",")
}

// rolloutPodStatus describes what the pods matching selector are waiting
// for. It returns "" when no pod is waiting or the pods cannot be read, so a
// failed lookup never hides the rollout error itself.
func rolloutPodStatus(ctx context.Context, r Runner, opts RolloutOptions, selector string) string {
	res, err := r.Run(ctx, Spec{
		Name: opts.Kubectl,
		Args: kubectlArgs(opts, "get", "pods", "-l", selector, "-o", "json"),
	})
	if err != nil {
		return ""
	}
	return describeWaitingPods([]byte(res.Stdout))
}

// describeWaitingPods lists, from the JSON of a pod list, each pod that is
// not scheduled and each container that is in the waiting state, with the
// reason and message Kubernetes gives for it, and each container that runs
// but is not ready. It leaves out the unready containers of a pod that is
// being deleted, and of init containers.
func describeWaitingPods(podListJSON []byte) string {
	type containerStatus struct {
		Name  string `json:"name"`
		Ready bool   `json:"ready"`
		State struct {
			Waiting *struct {
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"waiting"`
			Running *struct{} `json:"running"`
		} `json:"state"`
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name              string  `json:"name"`
				DeletionTimestamp *string `json:"deletionTimestamp"`
			} `json:"metadata"`
			Status struct {
				Conditions []struct {
					Type    string `json:"type"`
					Status  string `json:"status"`
					Reason  string `json:"reason"`
					Message string `json:"message"`
				} `json:"conditions"`
				InitContainerStatuses []containerStatus `json:"initContainerStatuses"`
				ContainerStatuses     []containerStatus `json:"containerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(podListJSON, &list); err != nil {
		return ""
	}
	withMessage := func(s, message string) string {
		if message = strings.TrimSpace(message); message != "" {
			return s + ": " + message
		}
		return s
	}
	var lines []string
	for _, pod := range list.Items {
		for _, c := range pod.Status.Conditions {
			if c.Type == "PodScheduled" && c.Status == "False" {
				lines = append(lines, withMessage(fmt.Sprintf("pod %s: %s", pod.Metadata.Name, c.Reason), c.Message))
			}
		}
		statuses := append(append([]containerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...)
		for _, c := range statuses {
			if w := c.State.Waiting; w != nil {
				lines = append(lines, withMessage(fmt.Sprintf("pod %s, container %s: %s", pod.Metadata.Name, c.Name, w.Reason), w.Message))
			}
		}
		if pod.Metadata.DeletionTimestamp != nil {
			continue
		}
		for _, c := range pod.Status.ContainerStatuses {
			if c.State.Running != nil && !c.Ready {
				lines = append(lines, fmt.Sprintf("pod %s, container %s: %s", pod.Metadata.Name, c.Name, runningNotReady))
			}
		}
	}
	return strings.Join(lines, "; ")
}
