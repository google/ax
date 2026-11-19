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
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	// rolloutPollSlice is how long one `kubectl rollout status` call waits
	// before WaitRollout looks at the pods of the rollout.
	rolloutPollSlice = 15 * time.Second

	// imagePullFailFastSlices is how many slices in a row a container must be
	// unable to pull its image before WaitRollout gives up on the rollout.
	// The kubelet retries a pull with a growing delay, so one failed pull is
	// not final, but a pull that has failed for this long does not recover
	// within the rollout timeout in practice.
	imagePullFailFastSlices = 3
)

// rolloutTimedOutMessage is what `kubectl rollout status --timeout` prints
// when the rollout did not finish in time.
const rolloutTimedOutMessage = "timed out waiting for the condition"

// RolloutOptions names the workload WaitRollout waits for.
type RolloutOptions struct {
	Kubectl     string
	KubeContext string
	Namespace   string
	// Target is a workload such as "deployment/ax-server".
	Target  string
	Timeout time.Duration
}

// WaitRollout waits for a Deployment, StatefulSet, or DaemonSet to finish
// rolling out, like `kubectl rollout status --timeout`.
//
// `kubectl rollout status` reports only "timed out waiting for the
// condition", while the reason (an image that cannot be pulled, a container
// that crashes, a pod that cannot be scheduled) is in the status of the pods.
// WaitRollout reads that status: it returns early when a container has been
// unable to pull its image for imagePullFailFastSlices slices, and on a
// timeout its error says what each pod that is not running is waiting for.
func WaitRollout(ctx context.Context, r Runner, opts RolloutOptions) error {
	return waitRollout(ctx, r, opts, rolloutPollSlice)
}

func waitRollout(ctx context.Context, r Runner, opts RolloutOptions, slice time.Duration) error {
	fail := func(err error) error {
		return fmt.Errorf("waiting for %s/%s rollout: %w", opts.Namespace, opts.Target, err)
	}
	deadline := time.Now().Add(opts.Timeout)
	pullFailures := map[string]int{}
	var selector string
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
		var problems []podProblem
		if selector != "" {
			problems = rolloutPodProblems(ctx, r, opts, selector)
		}
		stuck := stuckImagePulls(problems, pullFailures)
		if len(stuck) > 0 {
			return fail(fmt.Errorf("%s", describePodProblems(stuck)))
		}
		if time.Until(deadline) <= 0 {
			if len(problems) == 0 {
				return fail(err)
			}
			return fail(fmt.Errorf("timed out after %s: %s", opts.Timeout, describePodProblems(problems)))
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

// podProblem is one reason a pod of a rollout is not running.
type podProblem struct {
	Pod       string
	Container string // "" when the pod as a whole is the problem
	Image     string
	Reason    string
	Message   string
}

func (p podProblem) key() string { return p.Pod + "/" + p.Container }

// isImagePull reports whether the container cannot start because its image
// cannot be pulled.
func (p podProblem) isImagePull() bool {
	switch p.Reason {
	case "ErrImagePull", "ImagePullBackOff", "InvalidImageName", "ErrImageNeverPull":
		return true
	}
	return false
}

// rolloutPodProblems lists what the pods matching selector are waiting for.
// It returns nil when the pods cannot be read, so a failed lookup never hides
// the rollout error itself.
func rolloutPodProblems(ctx context.Context, r Runner, opts RolloutOptions, selector string) []podProblem {
	res, err := r.Run(ctx, Spec{
		Name: opts.Kubectl,
		Args: kubectlArgs(opts, "get", "pods", "-l", selector, "-o", "json"),
	})
	if err != nil {
		return nil
	}
	return parsePodProblems([]byte(res.Stdout))
}

func parsePodProblems(podListJSON []byte) []podProblem {
	type containerStatus struct {
		Name  string `json:"name"`
		Image string `json:"image"`
		State struct {
			Waiting *struct {
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"waiting"`
		} `json:"state"`
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
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
		return nil
	}
	var problems []podProblem
	for _, pod := range list.Items {
		for _, c := range pod.Status.Conditions {
			if c.Type == "PodScheduled" && c.Status == "False" {
				problems = append(problems, podProblem{Pod: pod.Metadata.Name, Reason: c.Reason, Message: c.Message})
			}
		}
		statuses := append(append([]containerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...)
		for _, c := range statuses {
			w := c.State.Waiting
			// ContainerCreating and PodInitializing are normal steps of a
			// start, not a reason it fails.
			if w == nil || w.Reason == "" || w.Reason == "ContainerCreating" || w.Reason == "PodInitializing" {
				continue
			}
			problems = append(problems, podProblem{
				Pod:       pod.Metadata.Name,
				Container: c.Name,
				Image:     c.Image,
				Reason:    w.Reason,
				Message:   w.Message,
			})
		}
	}
	return problems
}

// stuckImagePulls counts, in failures, how many slices in a row each
// container has been unable to pull its image, and returns the containers
// that reached imagePullFailFastSlices.
func stuckImagePulls(problems []podProblem, failures map[string]int) []podProblem {
	seen := map[string]bool{}
	var stuck []podProblem
	for _, p := range problems {
		if !p.isImagePull() {
			continue
		}
		seen[p.key()] = true
		failures[p.key()]++
		if failures[p.key()] >= imagePullFailFastSlices {
			stuck = append(stuck, p)
		}
	}
	for key := range failures {
		if !seen[key] {
			delete(failures, key)
		}
	}
	return stuck
}

func describePodProblems(problems []podProblem) string {
	lines := make([]string, 0, len(problems))
	for _, p := range problems {
		var b strings.Builder
		fmt.Fprintf(&b, "pod %s", p.Pod)
		if p.Container != "" {
			fmt.Fprintf(&b, ", container %s", p.Container)
		}
		fmt.Fprintf(&b, ": %s", p.Reason)
		if p.isImagePull() && p.Image != "" && !strings.Contains(p.Message, p.Image) {
			fmt.Fprintf(&b, " (image %s)", p.Image)
		}
		if msg := strings.TrimSpace(p.Message); msg != "" {
			fmt.Fprintf(&b, ": %s", msg)
		}
		if p.isImagePull() {
			if hint := imagePullHint(p.Message); hint != "" {
				fmt.Fprintf(&b, " [%s]", hint)
			}
		}
		lines = append(lines, b.String())
	}
	return strings.Join(lines, "; ")
}

// imagePullHint turns the kubelet's pull error into the likely cause.
func imagePullHint(message string) string {
	msg := strings.ToLower(message)
	contains := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(msg, s) {
				return true
			}
		}
		return false
	}
	switch {
	case contains("401", "403", "unauthorized", "forbidden", "denied"):
		return "the cluster's nodes have no credentials for this registry; an 'ax local' cluster pulls every image anonymously, so the image must be public"
	case contains("i/o timeout", "no such host", "connection refused", "network is unreachable", "no route to host", "temporary failure in name resolution"):
		return "the cluster's nodes cannot reach the registry; check that containers have outbound network access (IPv4 forwarding on the container host, a proxy, or a firewall)"
	case contains("not found", "manifest unknown"):
		return "the registry has no such image; check the image reference"
	}
	return ""
}
