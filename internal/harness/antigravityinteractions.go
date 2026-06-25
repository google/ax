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

package harness

// AntigravityInteractionsHarness drives an Antigravity agent through the Vertex
// GenAI Interactions API over HTTPS + Server-Sent Events, using the steps-based
// ("step_list") request format. It implements the Harness interface for the
// client-side ("local") environment: the agent runs server-side as the brain
// while the harness, on the client, drives the turn loop and executes every tool
// the agent yields.
//
// Tool execution (all internal to the harness):
//
//   - Built-in environment tools (view_file, run_command, list_dir, ...) are
//     executed against the local filesystem/shell.
//   - Third-party / MCP tools are executed via a ThirdPartyExecutor, the seam
//     through which the controller injects the caller's tool implementations. If
//     no executor is configured, no third-party tools are advertised.
//
// Neither kind of tool call is surfaced to the caller: Run drives the whole
// interaction to completion (initial turn -> resume -> resume -> ... -> final
// answer) and only forwards the agent's text output via Handler.OnMessage.
//
// Queue carries human input only -- the initial prompt and, in the future,
// "steering" messages injected mid-run. It never carries tool results (the
// harness produces those itself). Queued input is drained at each interaction
// gap (every resume point), which is the only place the harness can influence an
// otherwise atomic interaction.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/ax/proto"
	"github.com/google/uuid"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// cloudPlatformScope is the OAuth2 scope required to call Vertex AI.
const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// Compile-time interface assertions.
var _ Harness = (*AntigravityInteractionsHarness)(nil)
var _ Execution = (*antigravityInteractionsExecution)(nil)

const interactionsAPIVersion = "v1beta1"

// interactionsEndpoint is the public Vertex GenAI dataplane endpoint.
const interactionsEndpoint = "https://aiplatform.googleapis.com"

// Cloud project and location are read from these standard environment variables
// (see https://github.com/google/ax#authentication).
const (
	envCloudProject  = "GOOGLE_CLOUD_PROJECT"
	envCloudLocation = "GOOGLE_CLOUD_LOCATION"
)

// defaultLocation is used when GOOGLE_CLOUD_LOCATION is unset.
const defaultLocation = "global"

// AntigravityInteractionsConfig configures an AntigravityInteractionsHarness.
// Use NewAntigravityInteractionsHarness, which fills sensible defaults.
//
// Cloud project and location come from the standard GOOGLE_CLOUD_PROJECT and
// GOOGLE_CLOUD_LOCATION environment variables.
type AntigravityInteractionsConfig struct {
	// Agent is the Interactions API agent name to run.
	Agent string
	// SystemInstruction, if set, is sent as the interaction's system_instruction
	// (a free-form system prompt prepended to the agent's own instructions). It
	// is sent on every turn so it persists across resumes.
	SystemInstruction string
	// MaxTurns caps the number of interaction turns the harness will drive within
	// a single Run before giving up. Defaults to 100.
	MaxTurns int
	// Debug, if true, logs concise per-conversation tool activity to stderr: a
	// line for each function call (FC) the agent yields and each function result
	// (FR) the harness produces. Useful for observing the FC/FR exchange that is
	// otherwise internal to the harness.
	Debug bool

	// ThirdPartyExecutor executes third-party (non-built-in) function tool calls
	// and declares them to the agent. It is the seam for the controller to inject
	// the caller's tool implementations. If nil, the harness advertises no
	// third-party tools and any non-built-in call the agent attempts yields an
	// error result.
	ThirdPartyExecutor ThirdPartyExecutor

	// TokenSource overrides how the bearer token is obtained. If nil, the harness
	// builds an auto-refreshing source from Application Default Credentials.
	TokenSource oauth2.TokenSource
	// HTTPClient overrides the HTTP client. If nil, a default client with a long
	// timeout is used.
	HTTPClient *http.Client
}

func (c *AntigravityInteractionsConfig) withDefaults() {
	if c.MaxTurns == 0 {
		c.MaxTurns = 100
	}
}

// cloudProject returns the Cloud project id from GOOGLE_CLOUD_PROJECT.
func cloudProject() string {
	return os.Getenv(envCloudProject)
}

// cloudLocation returns the Cloud location from GOOGLE_CLOUD_LOCATION, falling
// back to the default ("global").
func cloudLocation() string {
	if loc := os.Getenv(envCloudLocation); loc != "" {
		return loc
	}
	return defaultLocation
}

// AntigravityInteractionsHarness implements Harness by talking to the public
// Vertex GenAI Interactions API.
type AntigravityInteractionsHarness struct {
	cfg        AntigravityInteractionsConfig
	httpClient *http.Client

	// tsOnce guards lazy initialization of ts, the resolved OAuth2 token source.
	// It is resolved on first use (rather than in the constructor) so credential
	// errors surface to the caller of Run instead of at construction time.
	tsOnce sync.Once
	ts     oauth2.TokenSource
	tsErr  error
}

// NewAntigravityInteractionsHarness creates a harness from the given config,
// filling in defaults for unset fields.
func NewAntigravityInteractionsHarness(cfg AntigravityInteractionsConfig) *AntigravityInteractionsHarness {
	cfg.withDefaults()
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Minute}
	}
	return &AntigravityInteractionsHarness{cfg: cfg, httpClient: hc}
}

// Start implements Harness.Start.
func (h *AntigravityInteractionsHarness) Start(ctx context.Context, conversationID string) (Execution, error) {
	return &antigravityInteractionsExecution{
		harness:        h,
		conversationID: conversationID,
		id:             uuid.NewString(),
	}, nil
}

// antigravityInteractionsExecution implements Execution. It is long-lived
// across Run calls and owns the interaction chain (prevInteractionID). Queue
// may be called concurrently with Run to inject steering input, which is
// drained at each interaction gap.
type antigravityInteractionsExecution struct {
	harness        *AntigravityInteractionsHarness
	conversationID string
	id             string

	mu     sync.Mutex
	queued []*proto.Message
	closed bool

	// started is false until the initial turn has been sent.
	started bool
	// prevInteractionID chains resume turns (the interaction chain this Execution
	// owns).
	prevInteractionID string
}

// ID implements Execution.ID.
func (e *antigravityInteractionsExecution) ID() string { return e.id }

// Queue implements Execution.Queue. It carries human input only: the initial
// prompt, or steering messages injected mid-run. Tool results are NOT queued by
// the caller -- the harness executes all tools itself. Queued messages are
// drained at the next interaction gap within Run.
func (e *antigravityInteractionsExecution) Queue(ctx context.Context, msg ...*proto.Message) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return fmt.Errorf("execution session already closed")
	}
	e.queued = append(e.queued, msg...)
	return nil
}

// Close implements Execution.Close.
func (e *antigravityInteractionsExecution) Close(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	return nil
}

// drainQueue removes and returns any queued human input, converting it to
// user_input steps. Called at each interaction gap so newly-queued steering
// messages are folded into the next interaction.
func (e *antigravityInteractionsExecution) drainQueue() []any {
	e.mu.Lock()
	msgs := e.queued
	e.queued = nil
	e.mu.Unlock()
	return messagesToInputSteps(msgs)
}

func (e *antigravityInteractionsExecution) setPrevID(id string) {
	if id == "" {
		return
	}
	e.mu.Lock()
	e.prevInteractionID = id
	e.mu.Unlock()
}

// Run implements Execution.Run. It drives the interaction to completion,
// executing every tool the agent yields internally (built-in env tools locally,
// third-party tools via the configured executor) and resuming until the agent
// produces a final answer (a turn with no tool calls) or the turn cap is hit.
//
// The agent's text output is forwarded via handler.OnMessage; handler.OnComplete
// is called once when the conversation finishes. At each interaction gap, any
// human input queued via Queue (steering) is folded into the next turn.
func (e *antigravityInteractionsExecution) Run(ctx context.Context, handler Handler) error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return fmt.Errorf("execution session already closed")
	}
	started := e.started
	prevID := e.prevInteractionID
	e.mu.Unlock()

	token, err := e.harness.token(ctx)
	if err != nil {
		return fmt.Errorf("obtaining access token: %w", err)
	}

	// Initial input for this Run: drain whatever is queued (the prompt on the
	// first Run, or steering input on later Runs).
	input := e.drainQueue()
	if !started {
		if len(input) == 0 {
			return fmt.Errorf("no input messages queued for the initial turn")
		}
		e.mu.Lock()
		e.started = true
		e.mu.Unlock()
	} else if len(input) == 0 {
		return fmt.Errorf("Run called with no queued input and no work pending")
	}

	res, err := e.harness.postTurn(ctx, token, e.harness.newRequest(input, prevID))
	if err != nil {
		return fmt.Errorf("interaction turn failed: %w", err)
	}
	prevID = res.interactionID
	e.setPrevID(prevID)

	for turn := 0; turn < e.harness.cfg.MaxTurns; turn++ {
		e.harness.debugTurn(e.conversationID, turn+1, len(res.toolCalls))
		if err := emitText(ctx, handler, e.id, res.modelText); err != nil {
			return err
		}

		// Execute every tool the agent yielded (built-in env tools and
		// third-party tools alike are executed internally) and collect the
		// results to send back.
		var next []any
		for i, call := range res.toolCalls {
			e.harness.debugCall(e.conversationID, i+1, len(res.toolCalls), call)
			out := e.harness.executeTool(ctx, call)
			e.harness.debugResult(e.conversationID, call.name, out)
			next = append(next, toolResultStep{
				Type:   "function_result",
				CallID: call.callID,
				Name:   call.name,
				Result: out,
			})
		}

		// Fold in any human input queued (steering) since the last gap.
		next = append(next, e.drainQueue()...)

		// If the agent yielded no tool calls and there is no queued input to send,
		// the conversation is complete.
		if len(next) == 0 {
			return handler.OnComplete(ctx, e.id)
		}

		res, err = e.harness.postTurn(ctx, token, e.harness.newRequest(next, prevID))
		if err != nil {
			return fmt.Errorf("resume turn failed: %w", err)
		}
		prevID = res.interactionID
		e.setPrevID(prevID)
	}

	// Hit the turn cap while still driving tools.
	return handler.OnComplete(ctx, e.id)
}

// emitText forwards non-empty model text to the handler as a Message.
func emitText(ctx context.Context, handler Handler, execID, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return handler.OnMessage(ctx, execID, &proto.Message{
		Role: "assistant",
		Content: &proto.Content{
			Type: &proto.Content_Text{Text: &proto.TextContent{Text: text}},
		},
	})
}

// messagesToInputSteps converts queued ax Messages (human input) into user_input
// steps. Only text content is supported as input today.
func messagesToInputSteps(msgs []*proto.Message) []any {
	var steps []any
	for _, m := range msgs {
		if t, ok := m.GetContent().GetType().(*proto.Content_Text); ok && t.Text.GetText() != "" {
			steps = append(steps, userInputStep{
				Type:    "user_input",
				Content: []textPart{{Type: "text", Text: t.Text.GetText()}},
			})
		}
	}
	return steps
}

// ---------------------------------------------------------------------------
// Wire types and helpers (steps-based Interactions API).
// ---------------------------------------------------------------------------

// textPart is a single Content part within a user_input step.
type textPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// userInputStep is the steps-based representation of a user prompt.
type userInputStep struct {
	Type    string     `json:"type"` // "user_input"
	Content []textPart `json:"content"`
}

// toolResultStep is one tool result returned on a resume turn: a flat Step with
// "type":"function_result", the matching call_id, the tool name, and the result
// object under "result".
type toolResultStep struct {
	Type   string `json:"type"` // "function_result"
	CallID string `json:"call_id"`
	Name   string `json:"name"`
	Result any    `json:"result"`
}

// FunctionTool is a client-declared function tool declaration. It is the element
// type a ThirdPartyExecutor returns from Declarations.
type FunctionTool struct {
	Type   string `json:"type"` // "function"
	Name   string `json:"name"`
	Desc   string `json:"description,omitempty"`
	Params any    `json:"parameters,omitempty"`
}

// environmentConfig is the JSON shape of the Interaction.environment oneof. This
// harness always uses {"type":"local"} (the client-side environment).
type environmentConfig struct {
	Type string `json:"type"`
}

type interactionRequest struct {
	Stream                bool               `json:"stream"`
	Background            bool               `json:"background"`
	Store                 bool               `json:"store"`
	Agent                 string             `json:"agent"`
	SystemInstruction     string             `json:"system_instruction,omitempty"`
	Environment           *environmentConfig `json:"environment,omitempty"`
	PreviousInteractionID string             `json:"previous_interaction_id,omitempty"`
	Input                 []any              `json:"input,omitempty"`
	Tools                 []FunctionTool     `json:"tools,omitempty"`
}

// capturedToolCall is a tool call the agent yielded during a turn.
type capturedToolCall struct {
	callID    string
	name      string
	arguments map[string]any
}

// turnResult is what we extracted from streaming one turn.
type turnResult struct {
	interactionID string
	toolCalls     []capturedToolCall
	modelText     string
}

// Debug output layout. When Debug is enabled, each Run turn is logged as a
// hierarchical block so the function-call / function-result (FC/FR) exchange is
// easy to follow:
//
//	[harness:e2e-conv] ==== turn 1 (2 function calls) ============
//	[harness:e2e-conv]   FC 1/2  list_dir
//	[harness:e2e-conv]            DirectoryPath: .
//	[harness:e2e-conv]            explanation:  Listing the current directory.
//	[harness:e2e-conv]   FR      list_dir
//	[harness:e2e-conv]            results: [...]
//	[harness:e2e-conv]   FC 2/2  run_command
//	[harness:e2e-conv]            CommandLine: pwd
//	[harness:e2e-conv]   FR      run_command
//	[harness:e2e-conv]            ExitCode: 0
//	[harness:e2e-conv]            Output:   /home/user/project
const (
	debugIndentCall   = "  "          // FC / FR lines, under the turn header
	debugIndentDetail = "           " // params and result values, under FC / FR
)

// debugln writes a single debug line (prefixed with the conversation id) to
// stderr when Debug is enabled.
func (h *AntigravityInteractionsHarness) debugln(conversationID, line string) {
	if !h.cfg.Debug {
		return
	}
	fmt.Fprintf(os.Stderr, "[harness:%s] %s\n", conversationID, line)
}

// debugTurn logs the separator header that begins a turn.
func (h *AntigravityInteractionsHarness) debugTurn(conversationID string, turn, numCalls int) {
	if !h.cfg.Debug {
		return
	}
	plural := "s"
	if numCalls == 1 {
		plural = ""
	}
	header := fmt.Sprintf("==== turn %d (%d function call%s) ", turn, numCalls, plural)
	// Pad the header with '=' to a fixed width for an easy visual break.
	if pad := 56 - len(header); pad > 0 {
		header += strings.Repeat("=", pad)
	}
	h.debugln(conversationID, header)
}

// debugCall logs a function call (FC): a header line plus one indented line per
// argument (keys sorted for stable output).
func (h *AntigravityInteractionsHarness) debugCall(conversationID string, idx, total int, call capturedToolCall) {
	if !h.cfg.Debug {
		return
	}
	h.debugln(conversationID, fmt.Sprintf("%sFC %d/%d  %s", debugIndentCall, idx, total, call.name))
	h.debugKeyValues(conversationID, call.arguments)
}

// debugResult logs a function result (FR): a header line plus the indented
// result value(s).
func (h *AntigravityInteractionsHarness) debugResult(conversationID, name string, result any) {
	if !h.cfg.Debug {
		return
	}
	h.debugln(conversationID, fmt.Sprintf("%sFR      %s", debugIndentCall, name))
	if m, ok := result.(map[string]any); ok {
		h.debugKeyValues(conversationID, m)
		return
	}
	h.debugln(conversationID, debugIndentDetail+debugTruncate(fmt.Sprintf("%v", result)))
}

// debugKeyValues logs a map as one "key: value" line per entry, indented under
// an FC/FR header, with keys sorted for stable output.
func (h *AntigravityInteractionsHarness) debugKeyValues(conversationID string, m map[string]any) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		h.debugln(conversationID, fmt.Sprintf("%s%s: %s", debugIndentDetail, k, debugTruncate(fmt.Sprintf("%v", m[k]))))
	}
}

// debugTruncate collapses newlines and caps very long values so a single debug
// line stays readable.
func debugTruncate(s string) string {
	s = strings.ReplaceAll(s, "\n", "\\n")
	const max = 200
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

func (h *AntigravityInteractionsHarness) interactionsURL() string {
	return fmt.Sprintf("%s/%s/projects/%s/locations/%s/interactions",
		interactionsEndpoint, interactionsAPIVersion, cloudProject(), cloudLocation())
}

// token returns a bearer access token from the harness's OAuth2 token source.
// The source is resolved once (lazily) and auto-refreshes thereafter.
func (h *AntigravityInteractionsHarness) token(ctx context.Context) (string, error) {
	h.tsOnce.Do(func() {
		if h.cfg.TokenSource != nil {
			h.ts = h.cfg.TokenSource
			return
		}
		h.ts, h.tsErr = newTokenSource(ctx)
	})
	if h.tsErr != nil {
		return "", h.tsErr
	}
	tok, err := h.ts.Token()
	if err != nil {
		return "", fmt.Errorf("obtaining access token: %w", err)
	}
	return tok.AccessToken, nil
}

// newTokenSource builds an auto-refreshing OAuth2 token source for Vertex AI
// from Application Default Credentials.
func newTokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	creds, err := google.FindDefaultCredentials(ctx, cloudPlatformScope)
	if err != nil {
		return nil, fmt.Errorf("finding application default credentials: %w", err)
	}
	return creds.TokenSource, nil
}

// newRequest builds an interactionRequest common to every turn. The environment
// is always the client-side ("local") environment -- this harness exists to
// execute the agent's built-in env tools locally. Tools are re-declared on every
// turn so they stay known to the agent across resumes.
func (h *AntigravityInteractionsHarness) newRequest(input []any, previousID string) interactionRequest {
	var tools []FunctionTool
	if h.cfg.ThirdPartyExecutor != nil {
		tools = h.cfg.ThirdPartyExecutor.Declarations()
	}
	return interactionRequest{
		Stream:                true,
		Background:            true,
		Store:                 true,
		Agent:                 h.cfg.Agent,
		SystemInstruction:     h.cfg.SystemInstruction,
		Environment:           &environmentConfig{Type: "local"},
		PreviousInteractionID: previousID,
		Input:                 input,
		Tools:                 tools,
	}
}

// postTurn POSTs the request and streams the SSE response for one turn.
func (h *AntigravityInteractionsHarness) postTurn(ctx context.Context, token string, reqBody interactionRequest) (*turnResult, error) {
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, h.interactionsURL(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Goog-User-Project", cloudProject())

	resp, err := h.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b := new(bytes.Buffer)
		if _, err := b.ReadFrom(resp.Body); err != nil {
			return nil, fmt.Errorf("HTTP %d (failed to read error body: %v)", resp.StatusCode, err)
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, b.String())
	}
	return h.parseStreamedTurn(resp.Body)
}

// parseStreamedTurn parses the event:/data: Server-Sent Events stream for one
// interaction turn and extracts the tool calls the agent yielded and the model's
// text output.
//
// Function-call streaming works per step index: a step.start announces the call
// (id, name, sometimes inline arguments), then step.delta "arguments_delta"
// events carry the arguments as a JSON STRING. The server re-emits the SAME call
// (same id) at several consecutive indices while it streams, and each emission
// carries the COMPLETE arguments JSON (a snapshot, not an incremental fragment).
// So we track state per step index, always REPLACING the latest arguments
// snapshot, then dedupe by id at the end -- concatenating snapshots would
// corrupt the JSON.
func (h *AntigravityInteractionsHarness) parseStreamedTurn(body io.Reader) (*turnResult, error) {
	turn := &turnResult{}

	// pendingCall accumulates one function call as it streams in.
	type pendingCall struct {
		id       string
		name     string
		argsJSON string         // latest complete arguments JSON snapshot
		args     map[string]any // inline arguments, if provided directly
	}
	callsByStepIndex := map[int]*pendingCall{}
	var stepIndexOrder []int

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		eventJSON := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if eventJSON == "[DONE]" {
			break
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(eventJSON), &event); err != nil {
			continue
		}
		switch event["event_type"] {
		case "interaction.created", "interaction.completed", "interaction.status_update":
			if interaction, ok := event["interaction"].(map[string]any); ok {
				if id, ok := interaction["id"].(string); ok && id != "" {
					turn.interactionID = id
				}
			}
			if id, ok := event["interaction_id"].(string); ok && id != "" {
				turn.interactionID = id
			}

		case "step.start":
			// encoding/json decodes JSON numbers as float64; convert the step
			// index to int at this boundary so the maps are keyed by a plain int.
			stepIndexF, _ := event["index"].(float64)
			stepIndex := int(stepIndexF)
			if step, ok := event["step"].(map[string]any); ok && step["type"] == "function_call" {
				call := callsByStepIndex[stepIndex]
				if call == nil {
					call = &pendingCall{}
					callsByStepIndex[stepIndex] = call
					stepIndexOrder = append(stepIndexOrder, stepIndex)
				}
				if id, ok := step["id"].(string); ok && id != "" {
					call.id = id
				}
				if name, ok := step["name"].(string); ok && name != "" {
					call.name = name
				}
				if inlineArgs, ok := step["arguments"].(map[string]any); ok && len(inlineArgs) > 0 {
					call.args = inlineArgs
				}
			}
		case "step.delta":
			stepIndexF, _ := event["index"].(float64)
			stepIndex := int(stepIndexF)
			if delta, ok := event["delta"].(map[string]any); ok {
				switch delta["type"] {
				case "arguments_delta":
					if call := callsByStepIndex[stepIndex]; call != nil {
						if argsSnapshot, ok := delta["arguments"].(string); ok && argsSnapshot != "" {
							call.argsJSON = argsSnapshot // snapshot: replace, not append
						}
					}
				case "text":
					if textChunk, ok := delta["text"].(string); ok {
						// Text deltas are mostly incremental, but the server
						// periodically re-sends a full cumulative restatement of the
						// text so far (e.g. when finalizing a turn before a tool
						// call). Detect that case and replace rather than append, so
						// the restated text is not duplicated.
						if strings.HasPrefix(textChunk, turn.modelText) && turn.modelText != "" {
							turn.modelText = textChunk
						} else {
							turn.modelText += textChunk
						}
					}
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	// Collapse the per-step-index pending calls into unique tool calls, in
	// first-seen order, deduped by id. A call missing a name or id is an
	// incomplete streaming artifact and is skipped.
	seenCallIDs := map[string]bool{}
	for _, stepIndex := range stepIndexOrder {
		call := callsByStepIndex[stepIndex]
		if call.name == "" || call.id == "" || seenCallIDs[call.id] {
			continue
		}
		seenCallIDs[call.id] = true
		if call.args == nil && call.argsJSON != "" {
			var parsedArgs map[string]any
			if err := json.Unmarshal([]byte(call.argsJSON), &parsedArgs); err == nil {
				call.args = parsedArgs
			}
		}
		if call.args == nil {
			call.args = map[string]any{}
		}
		turn.toolCalls = append(turn.toolCalls, capturedToolCall{
			callID: call.id, name: call.name, arguments: call.args,
		})
	}
	return turn, nil
}
