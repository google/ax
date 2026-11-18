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

package runner

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/ax/internal/metadata"
	"github.com/google/ax/internal/runnerhttp"
	"github.com/google/ax/internal/substrate"
	"github.com/google/ax/internal/workspace"
	runnerv1 "github.com/google/ax/pkg/apis/runner/v1"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	// DefaultControlURL is the in-cluster URL where ax-task-runner claims
	// assignments and posts events when Config.ControlURL and AX_RUNNER_URL are
	// both unset.
	DefaultControlURL = "http://" + substrate.DefaultRunnerEndpoint
	// DefaultActorMetadataDir is where Substrate projects actor identity files
	// (such as actor-uid) inside every sandbox.
	DefaultActorMetadataDir = "/run/ate"
	// DefaultRunnerStateDir is the durable directory where ax-task-runner
	// persists its per-actor bearer token and last started run ID.
	DefaultRunnerStateDir = "/var/lib/ax"

	unprivilegedUID = 1000
	unprivilegedGID = 1000

	actorUIDFilename    = "actor-uid"
	runnerTokenFilename = "runner_token"
	runnerStateFilename = "state.json"

	defaultClaimInitialBackoff = 200 * time.Millisecond
	defaultClaimMaxBackoff     = 5 * time.Second

	// egressPlaceholderAuth triggers Substrate's egress gateway CredentialHeader
	// replacement, which swaps any present Authorization header for a signed
	// Actor JWT.
	egressPlaceholderAuth = "Bearer ax-placeholder"
)

var pullJSONUnmarshalOpts = protojson.UnmarshalOptions{DiscardUnknown: true}

type errHTTPStatus struct {
	code int
	body string
}

func (e errHTTPStatus) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.code, e.body)
}

type persistedRunnerState struct {
	ActorUID         string `json:"actor_uid"`
	LastStartedRunID string `json:"last_started_run_id,omitempty"`
}

type childExit struct {
	pid  int
	code int
	err  error
}

type supervisedChild struct {
	runID      string
	cmd        *exec.Cmd
	exited     chan childExit
	tail       *tailBuffer
	outputFile string
}

type pullAgent struct {
	cfg              Config
	controlURL       string
	actorMetadataDir string
	stateDir         string
	httpClient       *http.Client
	metaServer       *metadata.Server
	askpassPath      string
	caBundlePath     string
	initialBackoff   time.Duration
	maxBackoff       time.Duration

	actorUID         string
	runnerToken      string
	inMemoryRunID    string
	lastStartedRunID string
	claimNonce       string
	eventSeq         int64
	child            *supervisedChild
}

func runPull(ctx context.Context, cfg Config) error {
	port := cfg.Port
	if port <= 0 {
		port = DefaultPort
	}
	metadataPort := cfg.MetadataPort
	if metadataPort <= 0 {
		metadataPort = DefaultMetadataPort
	}
	controlURL := strings.TrimRight(cmp.Or(cfg.ControlURL, os.Getenv("AX_RUNNER_URL"), DefaultControlURL), "/")
	actorMetaDir := cmp.Or(cfg.ActorMetadataDir, DefaultActorMetadataDir)
	stateDir := cmp.Or(cfg.StateDir, DefaultRunnerStateDir)
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	initBackoff := cfg.ClaimRetryInitialBackoff
	if initBackoff <= 0 {
		initBackoff = defaultClaimInitialBackoff
	}
	maxBackoff := cfg.ClaimRetryMaxBackoff
	if maxBackoff <= 0 {
		maxBackoff = defaultClaimMaxBackoff
	}

	wakeupCh := make(chan struct{}, 1)
	triggerWakeup := func() {
		select {
		case wakeupCh <- struct{}{}:
		default:
		}
	}

	metaServer := metadata.NewServer(port, nil, nil, metadata.ServerOptions{
		WorkspacePath: DefaultWorkspacePath,
		MetadataPort:  metadataPort,
		OnReadyz:      triggerWakeup,
	})
	// Mark /readyz 200 OK immediately so Substrate's WakeupProbe succeeds
	// without deadlocking ResumeActor while the runner claims its assignment.
	metaServer.SetWorkspaceReady(true)
	if err := metaServer.Start(); err != nil {
		return fmt.Errorf("starting metadata server: %w", err)
	}
	defer func() {
		slog.Info("shutting down metadata server")
		_ = metaServer.Stop(context.Background())
	}()

	_ = os.Setenv("AX_METADATA_URL", metaServer.MetadataURL())
	caBundle := cmp.Or(cfg.CABundle, v1alpha1.EgressTrustBundlePath)
	askpass := linkGitAskpass(workspace.AXDir)
	setDefaultEnv(nil, caBundle, askpass)

	agent := &pullAgent{
		cfg:              cfg,
		controlURL:       controlURL,
		actorMetadataDir: actorMetaDir,
		stateDir:         stateDir,
		httpClient:       httpClient,
		metaServer:       metaServer,
		askpassPath:      askpass,
		caBundlePath:     caBundle,
		initialBackoff:   initBackoff,
		maxBackoff:       maxBackoff,
		claimNonce:       uuid.NewString(),
	}
	agent.syncActorIdentity()
	triggerWakeup()

	defer agent.stopChild()
	return agent.loop(ctx, wakeupCh)
}

func (a *pullAgent) loop(ctx context.Context, wakeupCh <-chan struct{}) error {
	for {
		var childDone <-chan childExit
		if a.child != nil {
			childDone = a.child.exited
		}

		select {
		case <-ctx.Done():
			return nil
		case exit := <-childDone:
			a.handleChildExited(ctx, exit)
		case <-wakeupCh:
			a.handleWakeup(ctx)
		}
	}
}

func (a *pullAgent) handleWakeup(ctx context.Context) {
	backoff := a.initialBackoff
	for {
		if ctx.Err() != nil {
			return
		}
		a.syncActorIdentity()

		resp, err := a.claimOnce(ctx)
		if err == nil {
			if tok := resp.GetRunnerToken(); tok != "" {
				a.persistRunnerToken(tok)
			}
			a.dispatchAssignment(ctx, resp)
			return
		}

		slog.Debug("runner assignment claim retry scheduled", "error", err, "backoff", backoff)
		if !a.sleepOrHandleExit(ctx, backoff) {
			return
		}
		backoff = min(backoff*2, a.maxBackoff)
	}
}

func (a *pullAgent) sleepOrHandleExit(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()

	var childDone <-chan childExit
	if a.child != nil {
		childDone = a.child.exited
	}
	select {
	case <-ctx.Done():
		return false
	case exit := <-childDone:
		a.handleChildExited(ctx, exit)
		return false
	case <-timer.C:
		return true
	}
}

func (a *pullAgent) dispatchAssignment(ctx context.Context, resp *runnerv1.ClaimAssignmentResponse) {
	switch resp.GetMode() {
	case runnerv1.AssignmentMode_ASSIGNMENT_MODE_SETUP:
		a.runWorkspaceSetup(ctx, resp.GetWorkspace())
	case runnerv1.AssignmentMode_ASSIGNMENT_MODE_RUN:
		if a.child != nil && a.inMemoryRunID == resp.GetRunId() {
			return
		}
		a.startTaskRun(ctx, resp)
	case runnerv1.AssignmentMode_ASSIGNMENT_MODE_RESUME:
		// Keep supervising the live child process.
	case runnerv1.AssignmentMode_ASSIGNMENT_MODE_IDLE:
		a.stopChild()
	}
}

func (a *pullAgent) runWorkspaceSetup(ctx context.Context, ws *v1alpha1.Workspace) {
	a.stopChild()
	wsPath := DefaultWorkspacePath
	slog.Info("executing pull-mode workspace setup", "workspace", ws.GetMetadata().GetName(), "path", wsPath)

	_, setupErr := workspace.SetupWorkspace(ctx, ws, wsPath)
	if setupErr == nil && a.shouldDropPrivileges() {
		if err := chownTree(wsPath, unprivilegedUID, unprivilegedGID); err != nil {
			slog.Warn("failed to chown pre-warmed workspace directory", "path", wsPath, "error", err)
		}
	}

	ev := &runnerv1.RunnerEvent{
		Seq:  1,
		Time: timestamppb.Now(),
		Event: &runnerv1.RunnerEvent_SetupFinished{
			SetupFinished: &runnerv1.SetupFinished{
				Ok:      setupErr == nil,
				Message: errorMessage(setupErr),
			},
		},
	}
	if setupErr != nil {
		slog.Error("workspace setup failed", "error", setupErr)
	}
	_, _ = a.reportEventsWithRetry(ctx, "", []*runnerv1.RunnerEvent{ev})
}

func (a *pullAgent) startTaskRun(ctx context.Context, resp *runnerv1.ClaimAssignmentResponse) {
	a.stopChild()

	runID := resp.GetRunId()
	task := resp.GetTask()
	ws := resp.GetWorkspace()
	goalText := resp.GetGoal().GetText()

	wsPath := a.resolveWorkdir(resp.GetExec().GetWorkdir(), task)
	axMetaDir := filepath.Join(wsPath, ".ax")
	if err := os.MkdirAll(axMetaDir, 0o755); err != nil {
		a.reportStartupFailure(ctx, runID, fmt.Errorf("creating %s: %w", axMetaDir, err))
		return
	}

	outFilePath := outputPath(wsPath)
	_ = os.Remove(outFilePath)

	taskForFiles := cloneTaskWithGoal(task, goalText)
	launcherEnvFiles, launcherErr := writeLauncherFiles(a.cfg.RunDir, wsPath, taskForFiles, ws)
	if launcherErr != nil {
		slog.Warn("failed to write launcher contract files in RunDir", "error", launcherErr)
		launcherEnvFiles.env = []string{OutputFileEnv + "=" + outFilePath}
	}

	// Record last_started_run_id on durable storage BEFORE spawning the child so
	// an unexpected container restart mid-run is detected reliably on re-claim.
	a.lastStartedRunID = runID
	a.persistState()

	a.metaServer.UpdateAssignment(taskForFiles, ws, goalText)
	setDefaultEnv(taskForFiles, a.caBundlePath, a.askpassPath)

	argv := resp.GetExec().GetArgv()
	if len(argv) == 0 {
		argv = launcherCommand(cmp.Or(a.cfg.LauncherPath, DefaultLauncherPath), wsPath, taskForFiles, launcherEnvFiles)
	}
	if len(argv) == 0 {
		a.eventSeq = 1
		_, _ = a.reportEventsWithRetry(ctx, runID, []*runnerv1.RunnerEvent{{
			Seq:   1,
			Time:  timestamppb.Now(),
			Event: &runnerv1.RunnerEvent_CommandExited{CommandExited: &runnerv1.CommandExited{ExitCode: 0}},
		}})
		return
	}

	dropPrivs := a.shouldDropPrivileges()
	if dropPrivs {
		ensureWritableForUser(wsPath, axMetaDir, unprivilegedUID, unprivilegedGID)
	}

	ring := newTailBuffer(MaxOutputBytes)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = wsPath
	cmd.Stdin = os.Stdin
	cmd.Stdout = io.MultiWriter(os.Stdout, ring)
	cmd.Stderr = io.MultiWriter(os.Stderr, ring)
	cmd.Env = a.buildChildEnv(taskForFiles, resp.GetExec().GetEnv(), launcherEnvFiles.env, wsPath, dropPrivs)
	sysAttr := &syscall.SysProcAttr{Setpgid: true}
	if dropPrivs {
		sysAttr.Credential = &syscall.Credential{
			Uid: unprivilegedUID,
			Gid: unprivilegedGID,
		}
	}
	cmd.SysProcAttr = sysAttr

	if err := cmd.Start(); err != nil {
		slog.Error("failed to start task command", "argv", argv, "error", err)
		a.eventSeq = 1
		_, _ = a.reportEventsWithRetry(ctx, runID, []*runnerv1.RunnerEvent{{
			Seq:  1,
			Time: timestamppb.Now(),
			Event: &runnerv1.RunnerEvent_CommandExited{CommandExited: &runnerv1.CommandExited{
				ExitCode: 127,
				Output:   err.Error(),
			}},
		}})
		return
	}

	pid := cmd.Process.Pid
	slog.Info("started pull-mode task command", "runID", runID, "pid", pid, "argv", argv)

	exited := make(chan childExit, 1)
	go func() {
		waitErr := cmd.Wait()
		code := -1
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		}
		exited <- childExit{pid: pid, code: code, err: waitErr}
	}()

	a.child = &supervisedChild{
		runID:      runID,
		cmd:        cmd,
		exited:     exited,
		tail:       ring,
		outputFile: outFilePath,
	}
	a.inMemoryRunID = runID
	a.eventSeq = 1

	// Best-effort single attempt for CommandStarted so CommandExited is never
	// blocked waiting on CommandStarted retries.
	_ = a.postEventsOnce(ctx, runID, []*runnerv1.RunnerEvent{{
		Seq:   a.eventSeq,
		Time:  timestamppb.Now(),
		Event: &runnerv1.RunnerEvent_CommandStarted{CommandStarted: &runnerv1.CommandStarted{Pid: int32(pid)}},
	}}, nil)
}

func cloneTaskWithGoal(task *v1alpha1.Task, goalText string) *v1alpha1.Task {
	out := &v1alpha1.Task{}
	if task != nil {
		out = proto.Clone(task).(*v1alpha1.Task)
	}
	if goalText != "" && out.GetSpec().GetGoal().GetText() == "" {
		if out.Spec == nil {
			out.Spec = &v1alpha1.TaskSpec{}
		}
		out.Spec.Goal = &v1alpha1.Goal{Text: goalText}
	}
	return out
}

func (a *pullAgent) reportStartupFailure(ctx context.Context, runID string, err error) {
	slog.Error("failed preparing task workspace before command start", "runID", runID, "error", err)
	a.eventSeq = 1
	_, _ = a.reportEventsWithRetry(ctx, runID, []*runnerv1.RunnerEvent{{
		Seq:  1,
		Time: timestamppb.Now(),
		Event: &runnerv1.RunnerEvent_CommandExited{CommandExited: &runnerv1.CommandExited{
			ExitCode: 1,
			Output:   err.Error(),
		}},
	}})
}

func (a *pullAgent) handleChildExited(ctx context.Context, exit childExit) {
	child := a.child
	a.child = nil
	a.inMemoryRunID = ""
	if child == nil {
		return
	}

	if a.cfg.OnCommandExit != nil {
		a.cfg.OnCommandExit(CommandExit{Pid: exit.pid, ExitCode: exit.code, Err: exit.err})
	}

	output := readCapturedTaskOutput(child.outputFile, child.tail)
	a.eventSeq++
	ev := &runnerv1.RunnerEvent{
		Seq:  a.eventSeq,
		Time: timestamppb.Now(),
		Event: &runnerv1.RunnerEvent_CommandExited{
			CommandExited: &runnerv1.CommandExited{
				ExitCode: int32(exit.code),
				Output:   output,
			},
		},
	}

	resp, err := a.reportEventsWithRetry(ctx, child.runID, []*runnerv1.RunnerEvent{ev})
	if err == nil && resp.GetNext() != nil && resp.GetNext().GetMode() != runnerv1.AssignmentMode_ASSIGNMENT_MODE_IDLE {
		a.dispatchAssignment(ctx, resp.GetNext())
	}
}

func (a *pullAgent) stopChild() {
	if a.child == nil {
		return
	}
	child := a.child
	a.child = nil
	a.inMemoryRunID = ""

	rawExited := make(chan error, 1)
	go func() {
		res := <-child.exited
		rawExited <- res.err
	}()
	_ = stopCommand(child.cmd, rawExited)
}

func (a *pullAgent) resolveWorkdir(execWorkdir string, task *v1alpha1.Task) string {
	dir := execWorkdir
	if dir == "" {
		dir = task.GetSpec().WorkspacePath()
	}
	if (dir == "" || dir == v1alpha1.DefaultWorkspacePath) && DefaultWorkspacePath != v1alpha1.DefaultWorkspacePath {
		return DefaultWorkspacePath
	}
	if dir == "" {
		return DefaultWorkspacePath
	}
	return dir
}

func (a *pullAgent) shouldDropPrivileges() bool {
	if a.cfg.DropPrivileges != nil {
		return *a.cfg.DropPrivileges && os.Geteuid() == 0
	}
	return os.Geteuid() == 0
}

func (a *pullAgent) buildChildEnv(task *v1alpha1.Task, execEnv map[string]string, launcherEnv []string, wsPath string, droppedToUID1000 bool) []string {
	env := commandEnv(task, a.metaServer.MetadataURL())
	for k, v := range execEnv {
		if k != "" {
			env = append(env, k+"="+v)
		}
	}
	env = append(env, launcherEnv...)
	if droppedToUID1000 && execEnv["HOME"] == "" && !hasTaskEnv(task, "HOME") {
		if curHome := os.Getenv("HOME"); curHome == "" || curHome == "/root" {
			env = append(env, "HOME="+wsPath)
		}
	}
	return env
}

func hasTaskEnv(task *v1alpha1.Task, key string) bool {
	for _, e := range task.GetSpec().GetEnv() {
		if e.GetName() == key {
			return true
		}
	}
	return false
}

func (a *pullAgent) syncActorIdentity() {
	uidBytes, err := os.ReadFile(filepath.Join(a.actorMetadataDir, actorUIDFilename))
	diskUID := strings.TrimSpace(string(uidBytes))
	if err != nil || diskUID == "" {
		return
	}

	if a.actorUID == "" {
		a.actorUID = diskUID
		a.loadDurableStateForActor(diskUID)
		return
	}
	if a.actorUID != diskUID {
		slog.Info("detected new actor incarnation; resetting durable runner state", "prevActorUID", a.actorUID, "newActorUID", diskUID)
		a.stopChild()
		a.wipeDurableState()
		a.actorUID = diskUID
		a.claimNonce = uuid.NewString()
	}
}

func (a *pullAgent) loadDurableStateForActor(expectedUID string) {
	statePath := filepath.Join(a.stateDir, runnerStateFilename)
	tokenPath := filepath.Join(a.stateDir, runnerTokenFilename)

	rawState, err := os.ReadFile(statePath)
	if err != nil {
		a.wipeDurableState()
		return
	}
	var st persistedRunnerState
	if err := json.Unmarshal(rawState, &st); err != nil || st.ActorUID != expectedUID {
		a.wipeDurableState()
		return
	}
	a.lastStartedRunID = st.LastStartedRunID
	if tokBytes, err := os.ReadFile(tokenPath); err == nil {
		a.runnerToken = strings.TrimSpace(string(tokBytes))
	}
}

func (a *pullAgent) wipeDurableState() {
	a.runnerToken = ""
	a.lastStartedRunID = ""
	if a.stateDir == "" {
		return
	}
	_ = os.Remove(filepath.Join(a.stateDir, runnerTokenFilename))
	_ = os.Remove(filepath.Join(a.stateDir, runnerStateFilename))
}

func (a *pullAgent) persistRunnerToken(tok string) {
	a.runnerToken = tok
	if a.stateDir == "" {
		return
	}
	if err := os.MkdirAll(a.stateDir, 0o700); err != nil {
		slog.Warn("failed creating runner state dir", "dir", a.stateDir, "error", err)
		return
	}
	p := filepath.Join(a.stateDir, runnerTokenFilename)
	if err := os.WriteFile(p, []byte(tok), 0o600); err != nil {
		slog.Warn("failed persisting runner token", "path", p, "error", err)
	}
}

func (a *pullAgent) persistState() {
	if a.stateDir == "" || a.actorUID == "" {
		return
	}
	if err := os.MkdirAll(a.stateDir, 0o700); err != nil {
		slog.Warn("failed creating runner state dir", "dir", a.stateDir, "error", err)
		return
	}
	raw, err := json.Marshal(persistedRunnerState{
		ActorUID:         a.actorUID,
		LastStartedRunID: a.lastStartedRunID,
	})
	if err != nil {
		return
	}
	p := filepath.Join(a.stateDir, runnerStateFilename)
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		slog.Warn("failed persisting runner state", "path", p, "error", err)
	}
}

func (a *pullAgent) claimOnce(ctx context.Context) (*runnerv1.ClaimAssignmentResponse, error) {
	req := &runnerv1.ClaimAssignmentRequest{
		InMemoryRunId:    a.inMemoryRunID,
		LastStartedRunId: a.lastStartedRunID,
		ChildAlive:       a.child != nil,
		Nonce:            a.claimNonce,
	}
	var resp runnerv1.ClaimAssignmentResponse
	if err := a.postProto(ctx, runnerhttp.ClaimAssignmentPath, req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (a *pullAgent) reportEventsWithRetry(ctx context.Context, runID string, events []*runnerv1.RunnerEvent) (*runnerv1.ReportEventsResponse, error) {
	backoff := a.initialBackoff
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var resp runnerv1.ReportEventsResponse
		err := a.postEventsOnce(ctx, runID, events, &resp)
		if err == nil {
			return &resp, nil
		}
		var httpErr errHTTPStatus
		if errors.As(err, &httpErr) && httpErr.code == http.StatusBadRequest {
			return nil, err
		}
		slog.Debug("runner report events retry scheduled", "error", err, "backoff", backoff)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
		backoff = min(backoff*2, a.maxBackoff)
	}
}

func (a *pullAgent) postEventsOnce(ctx context.Context, runID string, events []*runnerv1.RunnerEvent, out *runnerv1.ReportEventsResponse) error {
	req := &runnerv1.ReportEventsRequest{
		RunId:  runID,
		Events: events,
	}
	var resp proto.Message
	if out != nil {
		resp = out
	}
	return a.postProto(ctx, runnerhttp.ReportEventsPath, req, resp)
}

func (a *pullAgent) postProto(ctx context.Context, path string, req, resp proto.Message) error {
	raw, err := proto.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshaling request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.controlURL+path, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/protobuf")
	httpReq.Header.Set("Accept", "application/protobuf")
	httpReq.Header.Set("Authorization", egressPlaceholderAuth)
	if a.runnerToken != "" {
		httpReq.Header.Set(runnerhttp.RunnerTokenHeader, a.runnerToken)
	}

	httpResp, err := a.httpClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer httpResp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(httpResp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}
	if httpResp.StatusCode != http.StatusOK {
		return errHTTPStatus{code: httpResp.StatusCode, body: strings.TrimSpace(string(body))}
	}
	if resp == nil || len(body) == 0 {
		return nil
	}
	ct := strings.ToLower(strings.TrimSpace(strings.Split(httpResp.Header.Get("Content-Type"), ";")[0]))
	if ct == "application/json" {
		return pullJSONUnmarshalOpts.Unmarshal(body, resp)
	}
	return proto.Unmarshal(body, resp)
}

func ensureWritableForUser(wsPath, axMetaDir string, uid, gid int) {
	_ = chownTree(axMetaDir, uid, gid)
	info, err := os.Stat(wsPath)
	if err == nil && info.IsDir() {
		_ = os.Chown(wsPath, uid, gid)
	}
}

func chownTree(root string, uid, gid int) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		return os.Chown(p, uid, gid)
	})
}

func errorMessage(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
