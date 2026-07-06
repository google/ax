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

package antigravityinteractions

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// executeTool runs a tool call the agent yielded and returns the result value to
// send back as a function_result step. The built-in environment tools enumerated
// in the switch below (file reads, command execution, and the file-mutation
// family) are executed internally against the local filesystem/shell; every
// other tool is dispatched to the configured ThirdPartyExecutor, or, if none is
// configured, returned as an error result. All execution is internal to the
// harness -- no tool call is surfaced to the caller.
//
// Argument names match the agent's tool schema (PascalCase). Mutation tools
// (move, delete_dir, create_file, edit_file, multi_edit_file, delete_file)
// require no success payload -- on success they return an empty result; on
// failure they return {"error": <message>}, which marks the step as failed.
func (h *AntigravityInteractionsHarness) executeTool(ctx context.Context, call capturedToolCall) any {
	switch call.name {
	case "view_file":
		return execViewFile(call)
	case "run_command":
		return execRunCommand(ctx, call)
	case "list_dir", "list_directory":
		return execListDir(call)
	case "move":
		return execMove(call)
	case "delete_dir":
		return execDeleteDir(call)
	case "create_file":
		return execCreateFile(call)
	case "edit_file":
		return execEditFile(call)
	case "multi_edit_file":
		return execMultiEditFile(call)
	case "delete_file":
		return execDeleteFile(call)
	default:
		if h.cfg.ThirdPartyExecutor == nil {
			return map[string]any{"error": fmt.Sprintf("no executor configured for third-party tool %q", call.name)}
		}
		return h.cfg.ThirdPartyExecutor.Execute(ctx, call.name, call.arguments)
	}
}

// ---------------------------------------------------------------------------
// Built-in environment tools: real local implementations.
//
// These run against the actual local filesystem/shell because they ARE the
// client-side environment. Argument names follow the agent's tool schema
// (view_file -> AbsolutePath, run_command -> CommandLine, list_dir ->
// DirectoryPath), and result field names follow what the server maps back into
// the step's own output (content, Output/ExitCode, results).
// ---------------------------------------------------------------------------

func execViewFile(call capturedToolCall) any {
	path := stringArg(call.arguments, "AbsolutePath")
	if path == "" {
		return map[string]any{"error": "view_file: missing required argument 'AbsolutePath'"}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]any{"error": fmt.Sprintf("view_file: %v", err)}
	}
	return map[string]any{"content": string(data)}
}

// runCommandTimeout bounds how long a single run_command may take, so a runaway
// command (e.g. `find /`, or `ping` without a count) cannot wedge the harness.
const runCommandTimeout = 60 * time.Second

func execRunCommand(ctx context.Context, call capturedToolCall) any {
	cmdLine := stringArg(call.arguments, "CommandLine")
	if cmdLine == "" {
		return map[string]any{"error": "run_command: missing required argument 'CommandLine'"}
	}

	// Bound the command's runtime so it cannot hang the harness.
	runCtx, cancel := context.WithTimeout(ctx, runCommandTimeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "/bin/sh", "-c", cmdLine)
	if cwd := stringArg(call.arguments, "Cwd"); cwd != "" {
		cmd.Dir = cwd
	}

	out, err := cmd.CombinedOutput()

	// Timed out: report a clear, non-zero result rather than blocking.
	if runCtx.Err() == context.DeadlineExceeded {
		return map[string]any{
			"Output":   fmt.Sprintf("%scommand timed out after %s", out, runCommandTimeout),
			"ExitCode": 124, // conventional timeout exit code
		}
	}

	exitCode := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			// Failed to start: surface as a non-zero exit + output.
			return map[string]any{"Output": fmt.Sprintf("%s%v", out, err), "ExitCode": 1}
		}
	}
	return map[string]any{"Output": string(out), "ExitCode": exitCode}
}

func execListDir(call capturedToolCall) any {
	dir := stringArg(call.arguments, "DirectoryPath")
	if dir == "" {
		return map[string]any{"error": "list_dir: missing required argument 'DirectoryPath'"}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return map[string]any{"error": fmt.Sprintf("list_dir: %v", err)}
	}
	results := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		var sizeBytes int64
		if info, err := e.Info(); err == nil {
			sizeBytes = info.Size()
		}
		results = append(results, map[string]any{
			"name":       e.Name(),
			"is_dir":     e.IsDir(),
			"size_bytes": sizeBytes,
		})
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i]["name"].(string) < results[j]["name"].(string)
	})
	return map[string]any{"results": results}
}

// ---------------------------------------------------------------------------
// Built-in file mutation tools.
//
// These mutate the real local filesystem. By contract, a successful mutation
// needs no result payload (the server renders success from the call's input
// args); a failure is reported as {"error": <message>}, which marks the step
// ERROR. Argument names follow the agent's tool schema (PascalCase).
// ---------------------------------------------------------------------------

// mutationOK is the empty success result for a mutation tool.
func mutationOK() any { return map[string]any{} }

func mutationErr(tool string, err error) any {
	return map[string]any{"error": fmt.Sprintf("%s: %v", tool, err)}
}

// execMove implements the "move" tool: SourcePath -> DestinationPath.
func execMove(call capturedToolCall) any {
	src := stringArg(call.arguments, "SourcePath")
	dst := stringArg(call.arguments, "DestinationPath")
	if src == "" || dst == "" {
		return mutationErr("move", fmt.Errorf("missing required argument 'SourcePath' and/or 'DestinationPath'"))
	}
	if err := os.Rename(src, dst); err != nil {
		return mutationErr("move", err)
	}
	return mutationOK()
}

// execDeleteDir implements the "delete_dir" tool: removes DirectoryPath. Force
// allows recursive removal; otherwise only an empty directory is removed.
func execDeleteDir(call capturedToolCall) any {
	dir := stringArg(call.arguments, "DirectoryPath")
	if dir == "" {
		return mutationErr("delete_dir", fmt.Errorf("missing required argument 'DirectoryPath'"))
	}
	var err error
	if boolArg(call.arguments, "Force") {
		err = os.RemoveAll(dir)
	} else {
		err = os.Remove(dir) // fails if not empty / not a dir
	}
	if err != nil {
		return mutationErr("delete_dir", err)
	}
	return mutationOK()
}

// execCreateFile implements the "create_file" tool: writes Content to TargetFile.
// Overwrite must be true to replace an existing file.
func execCreateFile(call capturedToolCall) any {
	path := stringArg(call.arguments, "TargetFile")
	if path == "" {
		return mutationErr("create_file", fmt.Errorf("missing required argument 'TargetFile'"))
	}
	if !boolArg(call.arguments, "Overwrite") {
		if _, err := os.Stat(path); err == nil {
			return mutationErr("create_file", fmt.Errorf("file %q already exists and Overwrite is false", path))
		}
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return mutationErr("create_file", err)
		}
	}
	if err := os.WriteFile(path, []byte(stringArg(call.arguments, "Content")), 0o644); err != nil {
		return mutationErr("create_file", err)
	}
	return mutationOK()
}

// execEditFile implements the "edit_file" tool: replaces the first occurrence of
// TargetContent with ReplacementContent in TargetFile.
func execEditFile(call capturedToolCall) any {
	path := stringArg(call.arguments, "TargetFile")
	target := stringArg(call.arguments, "TargetContent")
	repl := stringArg(call.arguments, "ReplacementContent")
	if path == "" || target == "" {
		return mutationErr("edit_file", fmt.Errorf("missing required argument 'TargetFile' and/or 'TargetContent'"))
	}
	return applyReplacements(path, "edit_file", []replacement{{target: target, repl: repl}})
}

// execMultiEditFile implements the "multi_edit_file" tool: applies a list of
// search/replace ReplacementChunks to TargetFile, in order.
func execMultiEditFile(call capturedToolCall) any {
	path := stringArg(call.arguments, "TargetFile")
	if path == "" {
		return mutationErr("multi_edit_file", fmt.Errorf("missing required argument 'TargetFile'"))
	}
	chunks, ok := call.arguments["ReplacementChunks"].([]any)
	if !ok {
		return mutationErr("multi_edit_file", fmt.Errorf("missing or invalid 'ReplacementChunks' array"))
	}
	reps := make([]replacement, 0, len(chunks))
	for _, c := range chunks {
		m, ok := c.(map[string]any)
		if !ok {
			return mutationErr("multi_edit_file", fmt.Errorf("each ReplacementChunk must be an object"))
		}
		reps = append(reps, replacement{
			target: stringArg(m, "TargetContent"),
			repl:   stringArg(m, "ReplacementContent"),
		})
	}
	return applyReplacements(path, "multi_edit_file", reps)
}

// execDeleteFile implements the "delete_file" tool: removes TargetFile.
func execDeleteFile(call capturedToolCall) any {
	path := stringArg(call.arguments, "TargetFile")
	if path == "" {
		return mutationErr("delete_file", fmt.Errorf("missing required argument 'TargetFile'"))
	}
	if err := os.Remove(path); err != nil {
		return mutationErr("delete_file", err)
	}
	return mutationOK()
}

// replacement is a single search/replace edit.
type replacement struct {
	target string
	repl   string
}

// applyReplacements reads path, applies each search/replace (first occurrence,
// in order), and writes the result back. Each target must be found.
func applyReplacements(path, tool string, reps []replacement) any {
	data, err := os.ReadFile(path)
	if err != nil {
		return mutationErr(tool, err)
	}
	content := string(data)
	for i, r := range reps {
		if r.target == "" {
			return mutationErr(tool, fmt.Errorf("chunk %d has empty TargetContent", i))
		}
		idx := strings.Index(content, r.target)
		if idx < 0 {
			return mutationErr(tool, fmt.Errorf("chunk %d: TargetContent not found in %q", i, path))
		}
		content = content[:idx] + r.repl + content[idx+len(r.target):]
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return mutationErr(tool, err)
	}
	return mutationOK()
}

// ---------------------------------------------------------------------------
// Third-party / MCP function tools.
//
// Third-party tools are executed INTERNALLY by the harness, via a
// ThirdPartyExecutor. The executor both declares the tools (advertised to the
// agent in the request's "tools" field) and executes calls to them.
//
// The executor is the seam for the controller to inject the caller's own tool
// implementations (and their declarations). If no executor is configured, the
// harness advertises no third-party tools and reports an error result for any
// non-built-in call the agent attempts.
// ---------------------------------------------------------------------------

// ThirdPartyExecutor declares and executes third-party (non-built-in) function
// tools. The harness owns when it is called; an implementation just needs to
// describe its tools and produce a result for a given call.
type ThirdPartyExecutor interface {
	// Declarations returns the tool declarations advertised to the agent.
	Declarations() []FunctionTool
	// Execute runs the named tool with the given arguments and returns the result
	// value to send back to the agent (wrapped into the function_result step).
	Execute(ctx context.Context, name string, args map[string]any) any
}

// ---------------------------------------------------------------------------
// Argument helpers.
// ---------------------------------------------------------------------------

func stringArg(args map[string]any, name string) string {
	if args == nil {
		return ""
	}
	if v, ok := args[name].(string); ok {
		return v
	}
	return ""
}

func boolArg(args map[string]any, name string) bool {
	if args == nil {
		return false
	}
	if v, ok := args[name].(bool); ok {
		return v
	}
	return false
}
