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
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"gopkg.in/yaml.v3"
)

const (
	// workspaceCheckPath reports whether the runner has finished the work
	// pushed to it (see v1alpha1.RunnerSetupPath).
	workspaceCheckPath = "/readyz?check=workspace"
	// maxRunnerReplyBytes caps how much of a runner reply is read.
	maxRunnerReplyBytes = 64 << 10
)

// errSetupFailed marks the runner's report that workspace setup failed, as
// opposed to the runner being unreachable or still busy.
var errSetupFailed = errors.New("workspace setup failed")

// errNoRouter reports that an actor cannot be reached because no router URL
// is set. Retrying does not help.
var errNoRouter = errors.New("router URL is not set")

// actorRequest sends an HTTP request to port 80 of a running actor through
// atenet-router (routerURL), the only way into an actor, with the
// ate-target-actor header. It returns the status code and the start of the
// response body.
func actorRequest(ctx context.Context, client *http.Client, routerURL, atespace, actor, method, path string, body []byte) (int, string, error) {
	if routerURL == "" {
		return 0, "", errNoRouter
	}
	req, err := http.NewRequestWithContext(ctx, method, routerURL+path, bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/yaml")
	}
	req.Header.Set("ate-target-actor", atespace+"/"+actor)
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	reply, _ := io.ReadAll(io.LimitReader(resp.Body, maxRunnerReplyBytes))
	_ = resp.Body.Close()
	return resp.StatusCode, strings.TrimSpace(string(reply)), nil
}

// pushToRunner posts body to path on the actor's runner, retrying every
// interval until the runner accepts it or ctx is done, or fails at once with
// errNoRouter when the router's address is unset. A runner rejects a push
// for good (4xx) only when it already holds different work or cannot decode
// the body, so those replies are returned without retrying.
func pushToRunner(ctx context.Context, client *http.Client, routerURL, atespace, actor, path string, body []byte, interval time.Duration) error {
	for {
		code, reply, err := actorRequest(ctx, client, routerURL, atespace, actor, http.MethodPost, path, body)
		switch {
		case err == nil && code >= 200 && code < 300:
			return nil
		case err == nil && code >= 400 && code < 500:
			return fmt.Errorf("runner rejected %s: HTTP %d: %s", path, code, reply)
		case errors.Is(err, errNoRouter):
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
// errSetupFailed with the runner's reason), or until ctx is done. It fails at
// once with errNoRouter when the router's address is unset.
func waitWorkspaceReady(ctx context.Context, client *http.Client, routerURL, atespace, actor string, interval time.Duration) error {
	for {
		code, reply, err := actorRequest(ctx, client, routerURL, atespace, actor, http.MethodGet, workspaceCheckPath, nil)
		if errors.Is(err, errNoRouter) {
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
