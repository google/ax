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

// Package axdeploy builds/resolves AX container images, configures Agent
// Substrate caller authorization, and installs or uninstalls AX on any
// Kubernetes cluster running Agent Substrate.
package axdeploy

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
)

// EnsureDigestPinned returns imageRef unchanged if it already carries a valid
// @sha256:... digest, or resolves the tag's manifest digest via crane (using
// local Docker/credential-helper keychains for private registries and automatic
// loopback HTTP fallback for local Kind registries).
func EnsureDigestPinned(ctx context.Context, imageRef string, client *http.Client) (string, error) {
	imageRef = strings.TrimSpace(imageRef)
	if imageRef == "" {
		return "", fmt.Errorf("image reference must not be empty")
	}

	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return "", fmt.Errorf("invalid image reference %q: %w", imageRef, err)
	}
	if _, ok := ref.(name.Digest); ok {
		return imageRef, nil
	}

	opts := []crane.Option{crane.WithContext(ctx)}
	if client != nil && client.Transport != nil {
		opts = append(opts, crane.WithTransport(client.Transport))
	}
	digest, err := crane.Digest(imageRef, opts...)
	if err != nil {
		return "", fmt.Errorf("resolving digest for %q: %w (if pre-built AX images are not published there yet, pass --from-source <ax-repo-dir>)", imageRef, err)
	}
	return imageRef + "@" + digest, nil
}
