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

package runnerhttp

import (
	"bytes"
	"cmp"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/ax/internal/store"
	runnerv1 "github.com/google/ax/pkg/apis/runner/v1"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type fakeTaskApplier struct {
	mu   sync.Mutex
	rows map[store.TaskKey]*store.TaskRow
}

func newFakeTaskApplier() *fakeTaskApplier {
	return &fakeTaskApplier{rows: make(map[store.TaskKey]*store.TaskRow)}
}

func (f *fakeTaskApplier) GetTaskRow(_ context.Context, key store.TaskKey) (*store.TaskRow, error) {
	key = key.Normalized()
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rows[key]
	if !ok {
		return nil, store.ErrNotFound
	}
	return r.Clone(), nil
}

func (f *fakeTaskApplier) Apply(_ context.Context, key store.TaskKey, fn store.ApplyFunc) (*store.TaskRow, error) {
	key = key.Normalized()
	f.mu.Lock()
	defer f.mu.Unlock()
	cur := f.rows[key].Clone()
	next, err := fn(cur, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if next == nil || (cur != nil && next == cur) {
		return cur.Clone(), nil
	}
	if next == store.DeleteTaskRow {
		delete(f.rows, key)
		return nil, nil
	}
	written := next.Clone()
	if cur == nil {
		written.Generation = 1
	} else {
		written.Generation = cur.Generation + 1
	}
	f.rows[key] = written
	return written.Clone(), nil
}

type fakeWorkspaceReader struct {
	ws *v1alpha1.Workspace
}

func (r fakeWorkspaceReader) GetWorkspace(_ context.Context, _, _ string) (*v1alpha1.Workspace, error) {
	if r.ws == nil {
		return nil, store.ErrNotFound
	}
	return proto.Clone(r.ws).(*v1alpha1.Workspace), nil
}

type fakeActorParker struct {
	mu     sync.Mutex
	parked []string
}

func (p *fakeActorParker) SuspendActor(_ context.Context, atespace, actorName string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.parked = append(p.parked, atespace+"/"+actorName)
	return nil
}

func postRunnerProto(t *testing.T, url, actorToken string, req, resp proto.Message) *http.Response {
	t.Helper()
	raw, err := protojson.Marshal(req)
	if err != nil {
		t.Fatalf("protojson.Marshal() error = %v", err)
	}
	httpReq, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("http.NewRequestWithContext() error = %v", err)
	}
	httpReq.Header.Set("Content-Type", contentTypeJSON)
	httpReq.Header.Set("Accept", contentTypeJSON)
	if actorToken != "" {
		httpReq.Header.Set(RunnerTokenHeader, actorToken)
	}
	httpResp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("Do(%s) error = %v", url, err)
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode == http.StatusOK && resp != nil {
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(httpResp.Body); err != nil {
			t.Fatalf("reading response body: %v", err)
		}
		if err := jsonUnmarshalOpts.Unmarshal(buf.Bytes(), resp); err != nil {
			t.Fatalf("protojson.Unmarshal() error = %v", err)
		}
	}
	return httpResp
}

func TestTaskClaimAndCompletionParksActor(t *testing.T) {
	applier := newFakeTaskApplier()
	applier.rows[store.TaskKey{Atespace: "default", Name: "task-1"}] = &store.TaskRow{
		Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Atespace: "default", Name: "task-1"},
			Spec:     &v1alpha1.TaskSpec{Commands: []string{"echo", "ok"}, Workspace: &v1alpha1.WorkspaceRef{Name: "ws-1"}},
			Status:   &v1alpha1.TaskStatus{State: v1alpha1.TaskState_TASK_STATE_QUEUED},
		},
		ActorUID: "uid-1",
		Runnable: true,
	}

	parker := &fakeActorParker{}
	srv := NewServer(Options{
		TaskApplier: applier,
		Store:       fakeWorkspaceReader{ws: &v1alpha1.Workspace{Metadata: &v1alpha1.ObjectMeta{Name: "ws-1"}}},
		ActorParker: parker,
		Verifier: ActorVerifierFunc(func(_ context.Context, r *http.Request) (ActorIdentity, error) {
			return ActorIdentity{
				Atespace:  "default",
				ActorName: cmp.Or(r.Header.Get("X-Test-Actor"), "task-1"),
				ActorUID:  cmp.Or(r.Header.Get("X-Test-UID"), "uid-1"),
			}, nil
		}),
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Claim RUN assignment.
	var claimResp runnerv1.ClaimAssignmentResponse
	res := postRunnerProto(t, ts.URL+ClaimAssignmentPath, "", &runnerv1.ClaimAssignmentRequest{Nonce: "n-1"}, &claimResp)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("ClaimAssignment status = %d, want 200", res.StatusCode)
	}
	if claimResp.GetMode() != runnerv1.AssignmentMode_ASSIGNMENT_MODE_RUN || claimResp.GetRunnerToken() == "" {
		t.Fatalf("unexpected claimResp: %+v", &claimResp)
	}

	// Wrong runner token returns 401.
	badRes := postRunnerProto(t, ts.URL+ReportEventsPath, "wrong-token", &runnerv1.ReportEventsRequest{
		RunId: claimResp.GetRunId(),
		Events: []*runnerv1.RunnerEvent{{
			Seq:   1,
			Event: &runnerv1.RunnerEvent_CommandStarted{CommandStarted: &runnerv1.CommandStarted{Pid: 10}},
		}},
	}, nil)
	if badRes.StatusCode != http.StatusUnauthorized {
		t.Fatalf("ReportEvents(bad token) status = %d, want 401", badRes.StatusCode)
	}

	// Valid CommandExited transitions task to COMPLETED and parks actor.
	var evResp runnerv1.ReportEventsResponse
	evRes := postRunnerProto(t, ts.URL+ReportEventsPath, claimResp.GetRunnerToken(), &runnerv1.ReportEventsRequest{
		RunId: claimResp.GetRunId(),
		Events: []*runnerv1.RunnerEvent{{
			Seq:   1,
			Event: &runnerv1.RunnerEvent_CommandExited{CommandExited: &runnerv1.CommandExited{ExitCode: 0, Output: "done"}},
		}},
	}, &evResp)
	if evRes.StatusCode != http.StatusOK {
		t.Fatalf("ReportEvents(CommandExited) status = %d, want 200", evRes.StatusCode)
	}
	if evResp.GetNext().GetMode() != runnerv1.AssignmentMode_ASSIGNMENT_MODE_IDLE {
		t.Fatalf("expected IDLE next assignment, got %v", evResp.GetNext().GetMode())
	}

	srv.WaitBackground()
	parker.mu.Lock()
	defer parker.mu.Unlock()
	if len(parker.parked) != 1 || parker.parked[0] != "default/task-1" {
		t.Fatalf("parked actors = %v, want [default/task-1]", parker.parked)
	}

	row, err := applier.GetTaskRow(t.Context(), store.TaskKey{Atespace: "default", Name: "task-1"})
	if err != nil {
		t.Fatalf("GetTaskRow() error = %v", err)
	}
	if row.Task.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_COMPLETED || row.Task.GetStatus().GetOutcome().GetOutput() != "done" {
		t.Fatalf("unexpected final task status: %+v", row.Task.GetStatus())
	}
}

func TestSetupCoordinatorLifecycle(t *testing.T) {
	srv := NewServer(Options{
		Verifier: ActorVerifierFunc(func(_ context.Context, _ *http.Request) (ActorIdentity, error) {
			return ActorIdentity{
				Atespace:  "default",
				ActorName: "ax-ws-setup-abc",
				ActorUID:  "setup-uid",
			}, nil
		}),
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	if err := srv.RegisterSetup(t.Context(), "default", "ax-ws-setup-abc", "setup-uid", &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "ws-1"},
	}); err != nil {
		t.Fatalf("RegisterSetup() error = %v", err)
	}
	defer func() { _ = srv.DeleteSetup(t.Context(), "default", "ax-ws-setup-abc") }()

	var claimResp runnerv1.ClaimAssignmentResponse
	res := postRunnerProto(t, ts.URL+ClaimAssignmentPath, "", &runnerv1.ClaimAssignmentRequest{}, &claimResp)
	if res.StatusCode != http.StatusOK || claimResp.GetMode() != runnerv1.AssignmentMode_ASSIGNMENT_MODE_SETUP {
		t.Fatalf("setup claim status=%d mode=%v", res.StatusCode, claimResp.GetMode())
	}

	var evResp runnerv1.ReportEventsResponse
	evRes := postRunnerProto(t, ts.URL+ReportEventsPath, "", &runnerv1.ReportEventsRequest{
		Events: []*runnerv1.RunnerEvent{{
			Seq:   1,
			Event: &runnerv1.RunnerEvent_SetupFinished{SetupFinished: &runnerv1.SetupFinished{Ok: true}},
		}},
	}, &evResp)
	if evRes.StatusCode != http.StatusOK {
		t.Fatalf("setup ReportEvents status = %d, want 200", evRes.StatusCode)
	}

	if err := srv.AwaitSetup(t.Context(), "default", "ax-ws-setup-abc"); err != nil {
		t.Fatalf("AwaitSetup() error = %v", err)
	}
}

func TestOIDCVerifierES256(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}

	var oidcSrv *httptest.Server
	oidcSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{"jwks_uri": oidcSrv.URL + "/jwks"})
		case "/jwks":
			rawPub, err := priv.PublicKey.Bytes()
			if err != nil {
				t.Fatalf("PublicKey.Bytes() error = %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"keys": []map[string]string{{
					"kty": "EC",
					"kid": "k1",
					"crv": "P-256",
					"x":   base64.RawURLEncoding.EncodeToString(rawPub[1:33]),
					"y":   base64.RawURLEncoding.EncodeToString(rawPub[33:65]),
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer oidcSrv.Close()

	now := time.Unix(1700000000, 0).UTC()
	verifier, err := NewOIDCVerifier(OIDCVerifierConfig{
		Issuer:     oidcSrv.URL,
		Audience:   "ax-runner",
		HTTPClient: oidcSrv.Client(),
		Now:        func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewOIDCVerifier() error = %v", err)
	}

	signJWT := func(claims map[string]any) string {
		hdr, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": "k1"})
		pay, _ := json.Marshal(claims)
		input := base64.RawURLEncoding.EncodeToString(hdr) + "." + base64.RawURLEncoding.EncodeToString(pay)
		h := crypto.SHA256.New()
		_, _ = h.Write([]byte(input))
		r, s, err := ecdsa.Sign(rand.Reader, priv, h.Sum(nil))
		if err != nil {
			t.Fatalf("ecdsa.Sign() error = %v", err)
		}
		sig := make([]byte, 64)
		r.FillBytes(sig[:32])
		s.FillBytes(sig[32:])
		return input + "." + base64.RawURLEncoding.EncodeToString(sig)
	}

	tok := signJWT(map[string]any{
		"iss":       oidcSrv.URL,
		"sub":       "actor/default/my-task",
		"aud":       "ax-runner",
		"exp":       now.Add(5 * time.Minute).Unix(),
		"actor_uid": "uid-123",
	})
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "/", nil)
	req.Header.Set("Authorization", "Bearer "+tok)

	id, err := verifier.VerifyActorRequest(t.Context(), req)
	if err != nil {
		t.Fatalf("VerifyActorRequest() error = %v", err)
	}
	if id.Atespace != "default" || id.ActorName != "my-task" || id.ActorUID != "uid-123" {
		t.Fatalf("unexpected identity: %+v", id)
	}

	expiredTok := signJWT(map[string]any{
		"iss":       oidcSrv.URL,
		"sub":       "actor/default/my-task",
		"aud":       "ax-runner",
		"exp":       now.Add(-5 * time.Minute).Unix(),
		"actor_uid": "uid-123",
	})
	req2, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "/", nil)
	req2.Header.Set("Authorization", "Bearer "+expiredTok)
	if _, err := verifier.VerifyActorRequest(t.Context(), req2); err == nil {
		t.Fatalf("expected expired JWT to fail verification")
	}
}
