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

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// promptKey is the spec.config key holding the prompt, the positional argument
// of `claude [options] [prompt]`. Every other key is a claude flag.
const promptKey = "prompt"

// configKey matches a camelCase config key, e.g. model or maxTurns. The flag
// it names is the key with each capital letter turned into a dash and its
// lowercase, the spelling `claude --help` lists for every flag.
var configKey = regexp.MustCompile(`^[a-z][a-z0-9]*([A-Z][a-z0-9]+)*$`)

// flagName returns the claude flag named by the camelCase key k, e.g.
// --max-turns for maxTurns.
func flagName(k string) string {
	var b strings.Builder
	b.WriteString("--")
	for _, r := range k {
		if 'A' <= r && r <= 'Z' {
			b.WriteByte('-')
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// claudeArgs returns the arguments of a headless claude run described by
// config and the Workspace MCP servers in mcp.
//
// Each config key other than prompt names a claude flag in camelCase, so
// maxTurns is --max-turns, and its value maps to arguments by type: a string or number becomes --key=value, true becomes
// --key, false omits the flag, a list repeats --key=value for each element,
// and an object is passed as JSON, as --settings and --mcp-config accept.
// claude itself validates the flags and their values.
func claudeArgs(config *structpb.Struct, mcp *v1alpha1.MCPConfig) ([]string, error) {
	fields := config.GetFields()
	prompt, ok := fields[promptKey].GetKind().(*structpb.Value_StringValue)
	if !ok || prompt.StringValue == "" {
		return nil, errors.New("spec.config: prompt must be a non-empty string")
	}

	args := []string{claudeBinary, "--print"}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		if k != promptKey {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	for _, k := range keys {
		if !configKey.MatchString(k) {
			return nil, fmt.Errorf("spec.config: key %q must be a claude flag name in camelCase, e.g. maxTurns", k)
		}
		flagArgs, err := flagValues(flagName(k), fields[k])
		if err != nil {
			return nil, fmt.Errorf("spec.config.%s: %w", k, err)
		}
		args = append(args, flagArgs...)
	}

	if servers := mcpServers(mcp); len(servers) > 0 {
		data, err := json.Marshal(map[string]any{"mcpServers": servers})
		if err != nil {
			return nil, fmt.Errorf("encoding workspace MCP servers: %w", err)
		}
		args = append(args, "--mcp-config="+string(data))
	}

	// End option parsing so a prompt that starts with "-" is not taken as a flag.
	return append(args, "--", prompt.StringValue), nil
}

// flagValues converts one config value into arguments for flag. Values are
// attached with = so variadic flags such as --allowed-tools cannot consume the
// arguments that follow.
func flagValues(flag string, v *structpb.Value) ([]string, error) {
	switch kind := v.GetKind().(type) {
	case *structpb.Value_BoolValue:
		if kind.BoolValue {
			return []string{flag}, nil
		}
		return nil, nil
	case *structpb.Value_StructValue:
		data, err := kind.StructValue.MarshalJSON()
		if err != nil {
			return nil, err
		}
		return []string{flag + "=" + string(data)}, nil
	case *structpb.Value_ListValue:
		var args []string
		for _, elem := range kind.ListValue.GetValues() {
			s, err := scalar(elem)
			if err != nil {
				return nil, fmt.Errorf("list elements must be strings or numbers: %w", err)
			}
			args = append(args, flag+"="+s)
		}
		return args, nil
	default:
		s, err := scalar(v)
		if err != nil {
			return nil, err
		}
		return []string{flag + "=" + s}, nil
	}
}

func scalar(v *structpb.Value) (string, error) {
	switch kind := v.GetKind().(type) {
	case *structpb.Value_StringValue:
		return kind.StringValue, nil
	case *structpb.Value_NumberValue:
		return strconv.FormatFloat(kind.NumberValue, 'f', -1, 64), nil
	default:
		return "", fmt.Errorf("unsupported value %v", v.AsInterface())
	}
}

// mcpServers converts the Workspace's MCP servers, both the static ones and
// those listed under its registries, into the mcpServers object of Claude
// Code's MCP config. A server with an endpoint is reached over streamable
// HTTP; one with a command is started over stdio. Servers with neither, or
// with a name already taken, are skipped.
func mcpServers(mcp *v1alpha1.MCPConfig) map[string]any {
	all := slices.Clone(mcp.GetServers())
	for _, r := range mcp.GetRegistries() {
		all = append(all, r.GetServers()...)
	}
	servers := map[string]any{}
	for _, srv := range all {
		name := srv.GetName()
		if _, dup := servers[name]; dup || name == "" {
			slog.Warn("skipping MCP server with a missing or duplicate name", "name", name)
			continue
		}
		switch {
		case srv.GetEndpoint() != "":
			servers[name] = map[string]any{"type": "http", "url": srv.GetEndpoint()}
		case srv.GetCommand() != "":
			servers[name] = map[string]any{"type": "stdio", "command": srv.GetCommand(), "args": srv.GetArgs()}
		default:
			slog.Warn("skipping MCP server with neither an endpoint nor a command", "name", name)
		}
	}
	return servers
}

// withClaudeCommand returns task with spec.command set to the claude
// invocation described by spec.config and the MCP servers of ws, which may be
// nil. A task that already sets spec.command, or has no spec.config, is
// returned unchanged.
func withClaudeCommand(task *v1alpha1.Task, ws *v1alpha1.Workspace) (*v1alpha1.Task, error) {
	spec := task.GetSpec()
	if len(spec.GetCommand()) > 0 || spec.GetConfig() == nil {
		return task, nil
	}
	cmd, err := claudeArgs(spec.GetConfig(), ws.GetSpec().GetMcp())
	if err != nil {
		return nil, err
	}
	task = proto.Clone(task).(*v1alpha1.Task)
	task.Spec.Command = cmd
	return task, nil
}
