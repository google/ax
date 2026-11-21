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

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"gopkg.in/yaml.v3"
)

const (
	// workspaceCheckPath reports whether the runner has finished the work
	// pushed to it (see v1alpha1.RunnerSetupPath).
	workspaceCheckPath = "/readyz?check=workspace"
	// maxRunnerReplyBytes caps how much of a runner reply is read. It fits a
	// v1alpha1.RunnerStatus with 64 KiB of output even when JSON escapes every
	// byte of it.
	maxRunnerReplyBytes = 512 << 10
)

// errSetupFailed marks the runner's report that workspace setup failed, as
// opposed to the runner being unreachable or still busy.
var errSetupFailed = errors.New("workspace setup failed")

// errNoAddress reports that an actor can be reached neither directly, because
// ResumeActor returned no address, nor through atenet-router, because
// ATENET_ROUTER_ADDR is unset. Retrying does not help.
var errNoAddress = errors.New("actor has no address")

// actorRequest sends an HTTP request to port 80 of a running actor: directly
// at workerAddr, the address ResumeActor returned, and through atenet-router
// (ATENET_ROUTER_ADDR) with the ate-target-actor header when workerAddr is
// empty or cannot be reached. It returns the status code and the start of the
// response body.
func actorRequest(ctx context.Context, client *http.Client, atespace, actor, workerAddr, method, path string, body []byte) (int, string, error) {
	var urls []string
	if workerAddr != "" {
		host, port := workerAddr, "80"
		if h, p, err := net.SplitHostPort(workerAddr); err == nil {
			host, port = h, p
		}
		urls = append(urls, "http://"+net.JoinHostPort(host, port)+path)
	}
	routerURL := routerBaseURL()
	if routerURL != "" {
		urls = append(urls, routerURL+path)
	}
	if len(urls) == 0 {
		return 0, "", fmt.Errorf("%w: %s/%s", errNoAddress, atespace, actor)
	}

	var errs []error
	for _, u := range urls {
		req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
		if err != nil {
			return 0, "", err
		}
		if len(body) > 0 {
			req.Header.Set("Content-Type", "application/yaml")
		}
		if routerURL != "" && strings.HasPrefix(u, routerURL) {
			req.Header.Set("ate-target-actor", atespace+"/"+actor)
		}
		resp, err := client.Do(req)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		reply, _ := io.ReadAll(io.LimitReader(resp.Body, maxRunnerReplyBytes))
		_ = resp.Body.Close()
		return resp.StatusCode, strings.TrimSpace(string(reply)), nil
	}
	return 0, "", errors.Join(errs...)
}

// routerBaseURL returns ATENET_ROUTER_ADDR as a base URL, or "" when unset.
func routerBaseURL() string {
	addr := os.Getenv("ATENET_ROUTER_ADDR")
	if addr == "" {
		return ""
	}
	if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
		if strings.HasSuffix(addr, ":443") {
			addr = "https://" + addr
		} else {
			addr = "http://" + addr
		}
	}
	return strings.TrimRight(addr, "/")
}

// pushToRunner posts body to path on the actor's runner, retrying every
// interval until the runner accepts it or ctx is done, or fails at once with
// errNoAddress for an actor without an address. A runner rejects a push
// for good (4xx) only when it already holds different work or cannot decode
// the body, so those replies are returned without retrying.
func pushToRunner(ctx context.Context, client *http.Client, atespace, actor, workerAddr, path string, body []byte, interval time.Duration) error {
	for {
		code, reply, err := actorRequest(ctx, client, atespace, actor, workerAddr, http.MethodPost, path, body)
		switch {
		case err == nil && code >= 200 && code < 300:
			return nil
		case err == nil && code >= 400 && code < 500:
			return fmt.Errorf("runner rejected %s: HTTP %d: %s", path, code, reply)
		case errors.Is(err, errNoAddress):
			return err
		case err == nil:
			err = fmt.Errorf("HTTP %d: %s", code, reply)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("pushing %s to actor %s/%s: %w (last error: %v)", path, atespace, actor, ctx.Err(), err)
		case <-time.After(interval):
		}
	}
}

// waitWorkspaceReady polls the runner's workspace check every interval until
// it reports the workspace ready (nil) or setup failed (an error wrapping
// errSetupFailed with the runner's reason), or until ctx is done. An actor
// without an address fails at once with errNoAddress.
func waitWorkspaceReady(ctx context.Context, client *http.Client, atespace, actor, workerAddr string, interval time.Duration) error {
	for {
		code, reply, err := actorRequest(ctx, client, atespace, actor, workerAddr, http.MethodGet, workspaceCheckPath, nil)
		if errors.Is(err, errNoAddress) {
			return err
		}
		if err == nil {
			switch code {
			case http.StatusOK:
				return nil
			case http.StatusInternalServerError:
				return fmt.Errorf("%w: %s", errSetupFailed, reply)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// taskStatus asks the actor's runner how its task ended. It returns nil until
// the task's command has exited, and for a runner too old to serve
// v1alpha1.RunnerStatusPath (404).
func taskStatus(ctx context.Context, client *http.Client, atespace, actor, workerAddr string) (*v1alpha1.RunnerStatus, error) {
	code, reply, err := actorRequest(ctx, client, atespace, actor, workerAddr, http.MethodGet, v1alpha1.RunnerStatusPath, nil)
	switch {
	case err != nil:
		return nil, err
	case code == http.StatusNoContent || code == http.StatusNotFound:
		return nil, nil
	case code != http.StatusOK:
		return nil, fmt.Errorf("runner replied HTTP %d to %s: %s", code, v1alpha1.RunnerStatusPath, reply)
	}
	var st v1alpha1.RunnerStatus
	if err := json.Unmarshal([]byte(reply), &st); err != nil {
		return nil, fmt.Errorf("decoding %s reply: %w", v1alpha1.RunnerStatusPath, err)
	}
	return &st, nil
}

// TaskStatus asks the runner of a running actor at workerAddr (empty to go
// through atenet-router) how its task ended, or returns nil while the task's
// command has not exited.
func (r *TaskReconciler) TaskStatus(ctx context.Context, atespace, actor, workerAddr string) (*v1alpha1.RunnerStatus, error) {
	return taskStatus(ctx, r.httpClient, atespace, actor, workerAddr)
}

// taskPushBody encodes what RunnerTaskPath takes: the task as YAML, followed by
// its bound workspace as a second document when it has one.
func taskPushBody(task *v1alpha1.Task, ws *v1alpha1.Workspace) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	if err := enc.Encode(task); err != nil {
		return nil, fmt.Errorf("encoding task: %w", err)
	}
	if ws != nil {
		if err := enc.Encode(ws); err != nil {
			return nil, fmt.Errorf("encoding workspace: %w", err)
		}
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
