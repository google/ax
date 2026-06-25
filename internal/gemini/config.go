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

package gemini

import "time"

// GeminiConfig is the configuration for a Gemini agent execution.
type GeminiConfig struct {
	Model        string        `json:"model,omitempty" yaml:"model,omitempty"`
	SystemPrompt string        `json:"system_prompt,omitempty" yaml:"system_prompt,omitempty"`
	MaxTokens    int32         `json:"max_tokens,omitempty" yaml:"max_tokens,omitempty"`
	Temperature  float32       `json:"temperature,omitempty" yaml:"temperature,omitempty"` // 0 means use model default
	Timeout      time.Duration `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	Tools        []string      `json:"tools,omitempty" yaml:"tools,omitempty"`
}
