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
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// rolloutCluster fakes the kubectl calls of WaitRollout for one workload.
type rolloutCluster struct {
	rolloutErr string   // stderr of `rollout status`; "" means it succeeds
	selector   string   // output of the matchLabels lookup
	pods       []string // output of `get pods -o json`, one per call; the last one repeats
	podCalls   int
	rollouts   int
}

func (c *rolloutCluster) run(spec Spec) (Result, error) {
	cmd := strings.Join(spec.Args, " ")
	switch {
	case strings.Contains(cmd, "rollout status"):
		c.rollouts++
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

func waitingPod(name, reason, message string) string {
	return fmt.Sprintf(`{"items":[{"metadata":{"name":%q},"status":{"containerStatuses":[{"name":"main","state":{"waiting":{"reason":%q,"message":%q}}}]}}]}`,
		name, reason, message)
}

const (
	timedOut   = "error: timed out waiting for the condition"
	testTarget = "deployment/ax-server"
)

func testRollout(c *rolloutCluster, timeout time.Duration) (string, error) {
	var out bytes.Buffer
	err := waitRollout(context.Background(), &fakeRunner{runFn: c.run}, RolloutOptions{
		Kubectl:     "kubectl",
		KubeContext: "ax-local",
		Namespace:   "ax-system",
		Target:      testTarget,
		Timeout:     timeout,
		Out:         &out,
	}, time.Millisecond)
	return out.String(), err
}

func TestWaitRolloutSucceeds(t *testing.T) {
	c := &rolloutCluster{}
	out, err := testRollout(c, time.Minute)
	if err != nil {
		t.Fatalf("WaitRollout: %v", err)
	}
	if c.rollouts != 1 || c.podCalls != 0 || out != "" {
		t.Errorf("got %d rollout calls, %d pod lookups, and output %q; want 1, 0, and none", c.rollouts, c.podCalls, out)
	}
}

// An image the cluster cannot pull: the friction log's `ax local start`
// printed only the kubectl timeout.
func TestWaitRolloutTimeoutQuotesPodStatus(t *testing.T) {
	const message = `Back-off pulling image "r/ax-server@sha256:3329": ErrImagePull: unexpected status: 403 Forbidden`
	c := &rolloutCluster{
		rolloutErr: timedOut,
		selector:   `{"app":"ax-server"}`,
		pods:       []string{waitingPod("ax-server-abc", "ImagePullBackOff", message)},
	}
	out, err := testRollout(c, 20*time.Millisecond)
	if err == nil {
		t.Fatal("WaitRollout succeeded, want a timeout")
	}
	const status = "pod ax-server-abc, container main: ImagePullBackOff: " + message
	if want := "waiting for ax-system/deployment/ax-server rollout: timed out after 20ms: " + status; err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
	// The status did not change between slices, so it is printed once.
	if want := "    ax-system/deployment/ax-server is not ready: " + status + "\n"; out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
	if c.rollouts < 2 {
		t.Errorf("made %d rollout calls, want it to wait for the whole timeout", c.rollouts)
	}
}

// https://github.com/google/ax/issues/697: the envoy container of
// atenet-router ran but was not ready for 16 hours, and `ax local start`
// printed only the kubectl timeout.
func TestWaitRolloutTimeoutNamesUnreadyContainer(t *testing.T) {
	c := &rolloutCluster{
		rolloutErr: timedOut,
		selector:   `{"app":"ax-server"}`,
		pods: []string{`{"items":[{"metadata":{"name":"ax-server-abc"},"status":{"containerStatuses":[` +
			`{"name":"main","ready":false,"state":{"running":{"startedAt":"2026-11-20T14:00:00Z"}}}]}}]}`},
	}
	out, err := testRollout(c, 20*time.Millisecond)
	if err == nil {
		t.Fatal("WaitRollout succeeded, want a timeout")
	}
	const status = "pod ax-server-abc, container main: running but not ready"
	want := "waiting for ax-system/deployment/ax-server rollout: timed out after 20ms: " + status +
		"; to restart it, run: kubectl --context ax-local -n ax-system rollout restart deployment/ax-server"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
	if want := "    ax-system/deployment/ax-server is not ready: " + status + "\n"; out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
}

func TestWaitRolloutPrintsStatusWhenItChanges(t *testing.T) {
	c := &rolloutCluster{
		rolloutErr: timedOut,
		selector:   `{"app":"ax-server"}`,
		pods: []string{
			waitingPod("ax-server-abc", "ContainerCreating", ""),
			waitingPod("ax-server-abc", "ContainerCreating", ""),
			waitingPod("ax-server-abc", "CrashLoopBackOff", "back-off 10s restarting failed container"),
		},
	}
	out, err := testRollout(c, 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "CrashLoopBackOff: back-off 10s") {
		t.Fatalf("error = %v, want the last pod status", err)
	}
	want := "    ax-system/deployment/ax-server is not ready: pod ax-server-abc, container main: ContainerCreating\n" +
		"    ax-system/deployment/ax-server is not ready: pod ax-server-abc, container main: CrashLoopBackOff: back-off 10s restarting failed container\n"
	if out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
}

func TestWaitRolloutReturnsOtherErrorsAtOnce(t *testing.T) {
	c := &rolloutCluster{rolloutErr: `Error from server (NotFound): deployments.apps "ax-server" not found`}
	_, err := testRollout(c, time.Hour)
	if err == nil || !strings.Contains(err.Error(), "NotFound") {
		t.Fatalf("error = %v, want the kubectl error", err)
	}
	if c.rollouts != 1 || c.podCalls != 0 {
		t.Errorf("got %d rollout calls and %d pod lookups, want 1 and 0", c.rollouts, c.podCalls)
	}
}

// Without a selector the pods of the workload are unknown, so WaitRollout
// behaves like `kubectl rollout status`.
func TestWaitRolloutWithoutSelector(t *testing.T) {
	c := &rolloutCluster{rolloutErr: timedOut, selector: ""}
	out, err := testRollout(c, 10*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), timedOut) || c.podCalls != 0 || out != "" {
		t.Fatalf("error = %v, %d pod lookups, output %q; want the kubectl timeout, no lookup, no output", err, c.podCalls, out)
	}
}

func TestDescribeWaitingPods(t *testing.T) {
	const pods = `{"items":[
	  {"metadata":{"name":"running"},"status":{"containerStatuses":[{"name":"main","ready":true,"state":{"running":{}}}]}},
	  {"metadata":{"name":"unschedulable"},"status":{"conditions":[{"type":"PodScheduled","status":"False","reason":"Unschedulable","message":"0/1 nodes are available"}]}},
	  {"metadata":{"name":"init"},"status":{"initContainerStatuses":[{"name":"setup","state":{"waiting":{"reason":"ImagePullBackOff","message":"Back-off"}}}],
	    "containerStatuses":[{"name":"main","state":{"waiting":{"reason":"PodInitializing"}}}]}},
	  {"metadata":{"name":"router"},"status":{"containerStatuses":[
	    {"name":"atenet-router","ready":true,"state":{"running":{}}},
	    {"name":"envoy","ready":false,"state":{"running":{}}}]}},
	  {"metadata":{"name":"sidecar"},"status":{"initContainerStatuses":[{"name":"sdsmint","ready":false,"state":{"running":{}}}],
	    "containerStatuses":[{"name":"main","ready":true,"state":{"running":{}}}]}},
	  {"metadata":{"name":"leaving","deletionTimestamp":"2026-11-21T06:35:00Z"},"status":{"containerStatuses":[{"name":"main","ready":false,"state":{"running":{}}}]}}
	]}`
	want := "pod unschedulable: Unschedulable: 0/1 nodes are available; " +
		"pod init, container setup: ImagePullBackOff: Back-off; " +
		"pod init, container main: PodInitializing; " +
		"pod router, container envoy: running but not ready"
	if got := describeWaitingPods([]byte(pods)); got != want {
		t.Errorf("describeWaitingPods = %q, want %q", got, want)
	}
	if got := describeWaitingPods([]byte("not json")); got != "" {
		t.Errorf("describeWaitingPods of invalid JSON = %q, want none", got)
	}
}

func TestRolloutSelectorSortsLabels(t *testing.T) {
	c := &rolloutCluster{selector: `{"b":"2","a":"1"}`}
	got := rolloutSelector(context.Background(), &fakeRunner{runFn: c.run}, RolloutOptions{Kubectl: "kubectl", Namespace: "ns", Target: testTarget})
	if got != "a=1,b=2" {
		t.Errorf("rolloutSelector = %q, want %q", got, "a=1,b=2")
	}
}
