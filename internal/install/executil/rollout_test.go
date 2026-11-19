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
	"fmt"
	"strings"
	"testing"
	"time"
)

// rolloutCluster fakes the kubectl calls of WaitRollout for one workload
// whose rollout never finishes.
type rolloutCluster struct {
	rolloutErr string   // stderr of `rollout status`; "" means it succeeds
	selector   string   // output of the matchLabels lookup
	pods       []string // output of `get pods -o json`, one per call; the last one repeats
	podCalls   int
	rollouts   []string // the --timeout argument of each `rollout status` call
}

func (c *rolloutCluster) run(spec Spec) (Result, error) {
	cmd := strings.Join(spec.Args, " ")
	switch {
	case strings.Contains(cmd, "rollout status"):
		c.rollouts = append(c.rollouts, spec.Args[len(spec.Args)-1])
		if c.rolloutErr == "" {
			return Result{}, nil
		}
		// Like OSRunner, the error quotes what the command printed.
		return Result{Stderr: c.rolloutErr}, fmt.Errorf("kubectl rollout status: exit status 1 (%s)", c.rolloutErr)
	case strings.Contains(cmd, "jsonpath={.spec.selector.matchLabels}"):
		return Result{Stdout: c.selector}, nil
	case strings.Contains(cmd, "get pods"):
		i := min(c.podCalls, len(c.pods)-1)
		c.podCalls++
		if i < 0 {
			return Result{Stdout: `{"items":[]}`}, nil
		}
		return Result{Stdout: c.pods[i]}, nil
	}
	return Result{}, fmt.Errorf("unexpected kubectl call: %s", cmd)
}

func waitingPod(name, image, reason, message string) string {
	return fmt.Sprintf(`{"items":[{"metadata":{"name":%q},"status":{"containerStatuses":[{"name":"main","image":%q,"state":{"waiting":{"reason":%q,"message":%q}}}]}}]}`,
		name, image, reason, message)
}

const (
	timedOut   = "error: timed out waiting for the condition"
	testTarget = "deployment/ax-server"
)

func testRollout(c *rolloutCluster, timeout time.Duration) error {
	return waitRollout(context.Background(), &fakeRunner{runFn: c.run}, RolloutOptions{
		Kubectl:     "kubectl",
		KubeContext: "ax-local",
		Namespace:   "ax-system",
		Target:      testTarget,
		Timeout:     timeout,
	}, time.Millisecond)
}

func TestWaitRolloutSucceeds(t *testing.T) {
	c := &rolloutCluster{}
	if err := testRollout(c, time.Minute); err != nil {
		t.Fatalf("WaitRollout: %v", err)
	}
	if len(c.rollouts) != 1 || c.podCalls != 0 {
		t.Errorf("got %d rollout calls and %d pod lookups, want 1 and 0", len(c.rollouts), c.podCalls)
	}
}

// A private image on a cluster without registry credentials: the friction
// log's `ax local start` waited 305 s and printed only the kubectl timeout.
func TestWaitRolloutFailsFastOnImagePull(t *testing.T) {
	const image = "us-central1-docker.pkg.dev/p/r/ax-server@sha256:3329"
	c := &rolloutCluster{
		rolloutErr: timedOut,
		selector:   `{"app":"ax-server"}`,
		pods: []string{waitingPod("ax-server-abc", image, "ImagePullBackOff",
			`Back-off pulling image "`+image+`": ErrImagePull: failed to resolve reference: unexpected status: 403 Forbidden`)},
	}
	err := testRollout(c, time.Hour)
	if err == nil {
		t.Fatal("WaitRollout succeeded, want an image pull error")
	}
	if len(c.rollouts) != imagePullFailFastSlices {
		t.Errorf("gave up after %d slices, want %d", len(c.rollouts), imagePullFailFastSlices)
	}
	for _, want := range []string{
		"waiting for ax-system/deployment/ax-server rollout",
		"pod ax-server-abc, container main: ImagePullBackOff",
		"403 Forbidden",
		"no credentials for this registry",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

// A host whose containers have no outbound network: the friction log's
// `ax local start` waited 240 s on the first Substrate rollout.
func TestWaitRolloutNamesUnreachableRegistry(t *testing.T) {
	c := &rolloutCluster{
		rolloutErr: timedOut,
		selector:   `{"app":"podcertificate-controller"}`,
		pods: []string{waitingPod("podcert-1", "ghcr.io/agent-substrate/substrate/podcertcontroller:v1.0.0", "ErrImagePull",
			`failed to pull image: dial tcp 140.82.112.33:443: i/o timeout`)},
	}
	err := testRollout(c, time.Hour)
	if err == nil || !strings.Contains(err.Error(), "i/o timeout") || !strings.Contains(err.Error(), "cannot reach the registry") {
		t.Fatalf("error = %v, want the pull error and the egress hint", err)
	}
	if !strings.Contains(err.Error(), "(image ghcr.io/agent-substrate/substrate/podcertcontroller:v1.0.0)") {
		t.Errorf("error %q does not name the image", err)
	}
}

// One failed pull is not final: the kubelet retries it.
func TestWaitRolloutToleratesAPullThatRecovers(t *testing.T) {
	c := &rolloutCluster{
		rolloutErr: timedOut,
		selector:   `{"app":"ax-server"}`,
		pods: []string{
			waitingPod("ax-server-abc", "img", "ErrImagePull", "i/o timeout"),
			`{"items":[]}`,
			waitingPod("ax-server-abc", "img", "ErrImagePull", "i/o timeout"),
			waitingPod("ax-server-abc", "img", "ImagePullBackOff", "i/o timeout"),
			`{"items":[]}`,
		},
	}
	err := testRollout(c, 50*time.Millisecond)
	if err == nil {
		t.Fatal("WaitRollout succeeded, want a timeout")
	}
	if len(c.rollouts) <= imagePullFailFastSlices+1 {
		t.Errorf("gave up after %d slices; the pull failures were never %d in a row", len(c.rollouts), imagePullFailFastSlices)
	}
	if !strings.Contains(err.Error(), timedOut) {
		t.Errorf("error %q is not the kubectl timeout", err)
	}
}

// A crashing container may recover, so it does not end the wait early, but
// the timeout error names it.
func TestWaitRolloutTimeoutNamesWaitingContainers(t *testing.T) {
	c := &rolloutCluster{
		rolloutErr: timedOut,
		selector:   `{"app":"ax-server"}`,
		pods:       []string{waitingPod("ax-server-abc", "img", "CrashLoopBackOff", "back-off 10s restarting failed container")},
	}
	err := testRollout(c, 20*time.Millisecond)
	if err == nil {
		t.Fatal("WaitRollout succeeded, want a timeout")
	}
	if len(c.rollouts) <= imagePullFailFastSlices {
		t.Errorf("gave up after %d slices, want it to wait for the whole timeout", len(c.rollouts))
	}
	for _, want := range []string{"timed out after 20ms", "pod ax-server-abc, container main: CrashLoopBackOff: back-off 10s"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestWaitRolloutReturnsOtherErrorsAtOnce(t *testing.T) {
	c := &rolloutCluster{rolloutErr: `Error from server (NotFound): deployments.apps "ax-server" not found`}
	err := testRollout(c, time.Hour)
	if err == nil || !strings.Contains(err.Error(), "NotFound") {
		t.Fatalf("error = %v, want the kubectl error", err)
	}
	if len(c.rollouts) != 1 || c.podCalls != 0 {
		t.Errorf("got %d rollout calls and %d pod lookups, want 1 and 0", len(c.rollouts), c.podCalls)
	}
}

// Without a selector the pods of the workload are unknown, so WaitRollout
// behaves like `kubectl rollout status`.
func TestWaitRolloutWithoutSelector(t *testing.T) {
	c := &rolloutCluster{rolloutErr: timedOut, selector: ""}
	err := testRollout(c, 10*time.Millisecond)
	if err == nil || c.podCalls != 0 {
		t.Fatalf("error = %v with %d pod lookups, want a timeout and no lookup", err, c.podCalls)
	}
}

func TestParsePodProblems(t *testing.T) {
	const pods = `{"items":[
	  {"metadata":{"name":"starting"},"status":{"containerStatuses":[{"name":"main","state":{"waiting":{"reason":"ContainerCreating"}}}]}},
	  {"metadata":{"name":"running"},"status":{"containerStatuses":[{"name":"main","state":{"running":{}}}]}},
	  {"metadata":{"name":"unschedulable"},"status":{"conditions":[{"type":"PodScheduled","status":"False","reason":"Unschedulable","message":"0/1 nodes are available"}]}},
	  {"metadata":{"name":"init"},"status":{"initContainerStatuses":[{"name":"setup","image":"img","state":{"waiting":{"reason":"ImagePullBackOff","message":"Back-off"}}}],
	    "containerStatuses":[{"name":"main","state":{"waiting":{"reason":"PodInitializing"}}}]}}
	]}`
	got := parsePodProblems([]byte(pods))
	want := []podProblem{
		{Pod: "unschedulable", Reason: "Unschedulable", Message: "0/1 nodes are available"},
		{Pod: "init", Container: "setup", Image: "img", Reason: "ImagePullBackOff", Message: "Back-off"},
	}
	if len(got) != len(want) {
		t.Fatalf("parsePodProblems = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("problem %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if parsePodProblems([]byte("not json")) != nil {
		t.Error("parsePodProblems of invalid JSON is not nil")
	}
}

func TestRolloutSelectorSortsLabels(t *testing.T) {
	c := &rolloutCluster{selector: `{"b":"2","a":"1"}`}
	got := rolloutSelector(context.Background(), &fakeRunner{runFn: c.run}, RolloutOptions{Kubectl: "kubectl", Namespace: "ns", Target: testTarget})
	if got != "a=1,b=2" {
		t.Errorf("rolloutSelector = %q, want %q", got, "a=1,b=2")
	}
}
