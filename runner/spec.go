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
	"fmt"
	"os"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"
)

// LoadSpec decodes YAML into out from file when set, otherwise from the named
// environment variable. It is not an error for neither to be present, or for
// the YAML to be blank. Unknown fields are
// ignored, so a runner image keeps working with a newer control plane.
func LoadSpec(file, envVar string, out proto.Message) error {
	var raw []byte
	switch {
	case file != "":
		data, err := os.ReadFile(file)
		if err != nil {
			return fmt.Errorf("reading %s: %w", file, err)
		}
		raw = data
	case os.Getenv(envVar) != "":
		raw = []byte(os.Getenv(envVar))
	default:
		return nil
	}
	var node yaml.Node
	if err := yaml.Unmarshal(raw, &node); err != nil {
		return fmt.Errorf("parsing %T: %w", out, err)
	}
	if len(node.Content) == 0 || node.Content[0].ShortTag() == "!!null" {
		return nil
	}
	if err := v1alpha1.UnmarshalYAMLLenient(node.Content[0], out); err != nil {
		return fmt.Errorf("parsing %T: %w", out, err)
	}
	return nil
}
