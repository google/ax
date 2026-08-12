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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/google/ax/cmd/ax/internal"
)

// runConfigMenu shows the /config menu and returns the (possibly updated) config.
// An updated config is sent on subsequent requests.
func runConfigMenu(d *internal.Display, agentConfig []byte) ([]byte, error) {
	for {
		action, err := d.PromptForConfigAction()
		if err != nil {
			if errors.Is(err, internal.ErrUserAborted) {
				return agentConfig, nil // Esc/Ctrl+C on the menu cancels /config.
			}
			return agentConfig, err
		}

		switch action {
		case "edit":
			cfg, done, err := editAgentConfig(d, agentConfig)
			if err != nil {
				return agentConfig, err
			}
			if done {
				return cfg, nil
			}
		case "load":
			cfg, done, err := loadAgentConfig(d)
			if err != nil {
				return agentConfig, err
			}
			if done {
				return cfg, nil
			}
		default: // "cancel" or anything else
			return agentConfig, nil
		}
	}
}

// editAgentConfig opens the JSON editor pre-filled with the current config. It
// returns the updated config with done=true if the config was updated, or
// done=false (config ignored) if the user cancelled back to the menu. Invalid
// JSON is reported and the editor re-opens with the user's draft so they can fix
// it.
func editAgentConfig(d *internal.Display, agentConfig []byte) ([]byte, bool, error) {
	draft := prettyAgentConfig(agentConfig)
	for {
		edited, err := d.PromptForConfigEdit(draft)
		if err != nil {
			if errors.Is(err, internal.ErrUserAborted) {
				return nil, false, nil // Back to the menu.
			}
			return nil, false, err
		}
		normalized, err := normalizeAgentConfigJSON(edited)
		if err != nil {
			d.ShowNotice(fmt.Sprintf("Invalid config: %v", err))
			draft = edited // Preserve the user's input so they can fix it.
			continue
		}
		return normalized, true, nil
	}
}

// loadAgentConfig lets the user pick a JSON file and loads it. It returns the
// loaded config with done=true, or done=false (config ignored) if the user
// cancelled back to the menu or the file could not be used.
func loadAgentConfig(d *internal.Display) ([]byte, bool, error) {
	path, err := d.PromptForConfigFile()
	if err != nil {
		if errors.Is(err, internal.ErrUserAborted) {
			return nil, false, nil // Back to the menu.
		}
		return nil, false, err
	}
	b, err := os.ReadFile(strings.TrimSpace(path))
	if err != nil {
		d.ShowNotice(fmt.Sprintf("Failed to read file: %v", err))
		return nil, false, nil
	}
	normalized, err := normalizeAgentConfigJSON(string(b))
	if err != nil {
		d.ShowNotice(fmt.Sprintf("Invalid config: %v", err))
		return nil, false, nil
	}
	return normalized, true, nil
}

// normalizeAgentConfigJSON trims and validates the given JSON config, returning
// the bytes to send on the wire. Empty input clears the config (returns nil). The
// config must be a JSON object.
func normalizeAgentConfigJSON(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		return nil, err
	}
	return []byte(s), nil
}

// prettyAgentConfig returns an indented JSON rendering of the config bytes for
// display, falling back to the raw bytes if they cannot be parsed.
func prettyAgentConfig(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, b, "", "  "); err != nil {
		return string(b)
	}
	return buf.String()
}
