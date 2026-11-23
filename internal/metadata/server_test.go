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

package metadata

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"gopkg.in/yaml.v3"
)

func TestMetadataServer(t *testing.T) {
	task := &v1alpha1.Task{
		ApiVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindTask,
		Metadata: &v1alpha1.ObjectMeta{
			Name:     "task123",
			Atespace: "default",
			Uid:      "550e8400-e29b-41d4-a716-446655440000",
		},
		Spec: &v1alpha1.TaskSpec{
			Profile: &v1alpha1.Profile{Image: "ghcr.io/test/img"},
			Debug:   true,
		},
		Status: &v1alpha1.TaskStatus{
			State: v1alpha1.TaskState_TASK_STATE_WORKING,
		},
	}

	ws := &v1alpha1.Workspace{
		ApiVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindWorkspace,
		Metadata: &v1alpha1.ObjectMeta{
			Name:     "ws-default",
			Atespace: "default",
		},
		Spec: &v1alpha1.WorkspaceSpec{},
	}

	srv := NewServer(0, task, ws)
	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer srv.Stop(context.Background())

	// Wait briefly for listen
	time.Sleep(50 * time.Millisecond)

	// Test /healthz
	resp, err := http.Get("http://" + srv.probeAddr + "/healthz")
	if err != nil {
		t.Fatalf("healthz request failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}

	// Test /readyz before workspace ready
	resp, err = http.Get("http://" + srv.probeAddr + "/readyz")
	if err != nil {
		t.Fatalf("readyz request failed: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 Service Unavailable before setup, got %d", resp.StatusCode)
	}

	// Mark workspace ready
	srv.SetWorkspaceReady(true)

	// Test /readyz after workspace ready
	resp, err = http.Get("http://" + srv.probeAddr + "/readyz")
	if err != nil {
		t.Fatalf("readyz request failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK after setup, got %d", resp.StatusCode)
	}

	// Test /readyz?check=workspace gated independently from /readyz
	srv.SetWorkspaceCheckReady(false)
	resp, err = http.Get("http://" + srv.probeAddr + "/readyz")
	if err != nil {
		t.Fatalf("readyz request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on /readyz when checkReady=false, got %d", resp.StatusCode)
	}
	resp, err = http.Get("http://" + srv.probeAddr + "/readyz?check=workspace")
	if err != nil {
		t.Fatalf("readyz?check=workspace request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 on /readyz?check=workspace when checkReady=false, got %d", resp.StatusCode)
	}
	srv.SetWorkspaceCheckReady(true)
	resp, err = http.Get("http://" + srv.probeAddr + "/readyz?check=workspace")
	if err != nil {
		t.Fatalf("readyz?check=workspace request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on /readyz?check=workspace when checkReady=true, got %d", resp.StatusCode)
	}

	// Test /metadata/v1alpha1/ax/task
	resp, err = http.Get(srv.MetadataURL() + "/metadata/v1alpha1/ax/task")
	if err != nil {
		t.Fatalf("get task metadata failed: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var fetchedTask v1alpha1.Task
	if err := yaml.Unmarshal(body, &fetchedTask); err != nil {
		t.Fatalf("unmarshaling task yaml: %v", err)
	}
	if fetchedTask.Metadata.Name != "task123" {
		t.Errorf("expected task name 'task123', got %s", fetchedTask.Metadata.Name)
	}

	// Test /metadata/v1alpha1/ax/workspace returns the bound workspace
	resp, err = http.Get(srv.MetadataURL() + "/metadata/v1alpha1/ax/workspace")
	if err != nil {
		t.Fatalf("get workspace metadata failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ = io.ReadAll(resp.Body)
	var fetchedWS v1alpha1.Workspace
	if err := yaml.Unmarshal(body, &fetchedWS); err != nil {
		t.Fatalf("unmarshaling workspace yaml: %v", err)
	}
	if fetchedWS.Metadata.Name != "ws-default" {
		t.Errorf("expected workspace 'ws-default', got %s", fetchedWS.Metadata.Name)
	}

	// Test multiplexed gRPC ProcessService on the same port
	grpcConn, err := grpc.NewClient(srv.probeAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dialing gRPC: %v", err)
	}
	defer grpcConn.Close()

	procClient := ateenvv1alpha.NewProcessServiceClient(grpcConn)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	startResp, err := procClient.StartProcess(ctx, &ateenvv1alpha.StartProcessRequest{
		Command: []string{"echo", "hello-from-guest"},
	})
	if err != nil {
		t.Fatalf("StartProcess via multiplexed port failed: %v", err)
	}
	if startResp.GetProcessId() == "" {
		t.Errorf("expected non-empty process ID")
	}
}

func TestMetadataServer_GuestServicesRequireDebug(t *testing.T) {
	task := &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "no-debug", Atespace: "default"},
		Spec:     &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "ghcr.io/test/img"}},
	}

	srv := NewServer(0, task, nil)
	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer srv.Stop(context.Background())
	time.Sleep(100 * time.Millisecond)

	// HTTP metadata still works.
	resp, err := http.Get(srv.MetadataURL() + "/metadata/v1alpha1/ax/task")
	if err != nil {
		t.Fatalf("task metadata request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from task metadata, got %d", resp.StatusCode)
	}

	// Guest gRPC is not served.
	grpcConn, err := grpc.NewClient(srv.probeAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to create grpc client: %v", err)
	}
	defer grpcConn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = ateenvv1alpha.NewProcessServiceClient(grpcConn).StartProcess(ctx, &ateenvv1alpha.StartProcessRequest{
		Command: []string{"echo", "hi"},
	})
	if err == nil {
		t.Fatalf("expected StartProcess to fail when spec.debug is false")
	}
}

func TestMetadataServer_MetadataOnlyOnLoopback(t *testing.T) {
	task := &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "loopback", Atespace: "default"},
		Spec:     &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "ghcr.io/test/img"}},
	}

	srv := NewServer(0, task, nil)
	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer srv.Stop(context.Background())
	time.Sleep(100 * time.Millisecond)

	metadataHost, metadataPort, err := net.SplitHostPort(strings.TrimPrefix(srv.MetadataURL(), "http://"))
	if err != nil {
		t.Fatalf("parsing MetadataURL %q: %v", srv.MetadataURL(), err)
	}
	if metadataHost != "127.0.0.1" {
		t.Errorf("metadata listener bound to %q, want 127.0.0.1", metadataHost)
	}

	// The metadata endpoints are not served on the probe port.
	for _, path := range []string{"/metadata/v1alpha1/ax/task", "/metadata/v1alpha1/ax/workspace"} {
		resp, err := http.Get("http://" + srv.probeAddr + path)
		if err != nil {
			t.Fatalf("request to probe port failed: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s on the probe port: expected 404, got %d", path, resp.StatusCode)
		}
	}

	// The metadata port does not accept connections on non-loopback addresses.
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatalf("listing interface addresses: %v", err)
	}
	checked := false
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() || ipNet.IP.IsLinkLocalUnicast() || ipNet.IP.To4() == nil {
			continue
		}
		checked = true
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(ipNet.IP.String(), metadataPort), time.Second)
		if err == nil {
			conn.Close()
			t.Errorf("metadata port accepted a connection on %s", ipNet.IP)
		}
	}
	if !checked {
		t.Log("no non-loopback IPv4 address found; skipped the external address check")
	}
}

func TestMetadataServer_Pushes(t *testing.T) {
	srv := NewServer(0, nil, nil, ServerOptions{AcceptPushes: true})
	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer srv.Stop(context.Background())
	base := "http://" + srv.probeAddr

	post := func(path, body string) int {
		t.Helper()
		resp, err := http.Post(base+path, "application/yaml", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	recv := func() Push {
		t.Helper()
		select {
		case p := <-srv.Pushes():
			return p
		case <-time.After(5 * time.Second):
			t.Fatal("no push delivered")
			return Push{}
		}
	}

	wsYAML := "metadata: {atespace: default, name: ws1, uid: w1}\nspec: {}\n"
	if code := post(v1alpha1.RunnerSetupPath, "{not yaml"); code != http.StatusBadRequest {
		t.Fatalf("bad setup body: got %d, want 400", code)
	}
	if code := post(v1alpha1.RunnerSetupPath, wsYAML); code != http.StatusAccepted {
		t.Fatalf("setup push: got %d, want 202", code)
	}
	if p := recv(); p.Task != nil || p.Workspace.GetMetadata().GetName() != "ws1" {
		t.Fatalf("setup push delivered %+v", p)
	}
	if code := post(v1alpha1.RunnerSetupPath, wsYAML); code != http.StatusOK {
		t.Fatalf("repeated setup push: got %d, want 200", code)
	}
	if code := post(v1alpha1.RunnerSetupPath, strings.Replace(wsYAML, "ws1", "ws2", 1)); code != http.StatusConflict {
		t.Fatalf("other setup push: got %d, want 409", code)
	}

	taskYAML := "metadata: {atespace: default, name: t1, uid: u1}\nspec: {futureField: 1}\n---\n" + wsYAML
	if code := post(v1alpha1.RunnerTaskPath, taskYAML); code != http.StatusAccepted {
		t.Fatalf("task push: got %d, want 202", code)
	}
	if p := recv(); p.Task.GetMetadata().GetName() != "t1" || p.Workspace.GetMetadata().GetName() != "ws1" {
		t.Fatalf("task push delivered %+v", p)
	}
	if code := post(v1alpha1.RunnerTaskPath, taskYAML); code != http.StatusOK {
		t.Fatalf("repeated task push: got %d, want 200", code)
	}
	if code := post(v1alpha1.RunnerTaskPath, strings.Replace(taskYAML, "uid: u1", "uid: u2", 1)); code != http.StatusConflict {
		t.Fatalf("task push of another uid: got %d, want 409", code)
	}
	if code := post(v1alpha1.RunnerSetupPath, wsYAML); code != http.StatusConflict {
		t.Fatalf("setup push while running a task: got %d, want 409", code)
	}

	srv.SetWorkspaceReady(true)
	srv.SetSetupFailed(errors.New("git clone failed"))
	resp, err := http.Get(base + "/readyz?check=workspace")
	if err != nil {
		t.Fatalf("readyz: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError || !strings.Contains(string(body), "git clone failed") {
		t.Fatalf("readyz?check=workspace after failure = %d %q, want 500 with the reason", resp.StatusCode, body)
	}
}

func TestMetadataServer_PushesOffByDefault(t *testing.T) {
	srv := NewServer(0, nil, nil)
	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer srv.Stop(context.Background())
	resp, err := http.Post("http://"+srv.probeAddr+v1alpha1.RunnerTaskPath, "application/yaml", strings.NewReader("metadata: {name: t1}\n"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusOK {
		t.Fatalf("task push without AcceptPushes: got %d, want it refused", resp.StatusCode)
	}
}
