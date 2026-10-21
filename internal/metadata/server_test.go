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

package metadata_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	"github.com/google/ax/internal/metadata"
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
		},
		Spec: &v1alpha1.TaskSpec{
			Image: "ghcr.io/test/img",
			Debug: true,
		},
		Status: &v1alpha1.TaskStatus{
			Id:    "task-task123-1",
			Phase: "Running",
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

	srv := metadata.NewServer(0, task, ws)
	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer srv.Stop(context.Background())

	// Wait briefly for listen
	time.Sleep(50 * time.Millisecond)

	// Test /healthz
	resp, err := http.Get("http://" + srv.ProbeAddr() + "/healthz")
	if err != nil {
		t.Fatalf("healthz request failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}

	// Test /readyz before workspace ready
	resp, err = http.Get("http://" + srv.ProbeAddr() + "/readyz")
	if err != nil {
		t.Fatalf("readyz request failed: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 Service Unavailable before setup, got %d", resp.StatusCode)
	}

	// Mark workspace ready
	srv.SetWorkspaceReady(true)

	// Test /readyz after workspace ready
	resp, err = http.Get("http://" + srv.ProbeAddr() + "/readyz")
	if err != nil {
		t.Fatalf("readyz request failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK after setup, got %d", resp.StatusCode)
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
	grpcConn, err := grpc.NewClient(srv.ProbeAddr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
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
		Spec:     &v1alpha1.TaskSpec{Image: "ghcr.io/test/img"},
	}

	srv := metadata.NewServer(0, task, nil)
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
	grpcConn, err := grpc.NewClient(srv.ProbeAddr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
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
		Spec:     &v1alpha1.TaskSpec{Image: "ghcr.io/test/img"},
	}

	srv := metadata.NewServer(0, task, nil)
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
		resp, err := http.Get("http://" + srv.ProbeAddr() + path)
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
