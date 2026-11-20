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

package cluster

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"slices"
	"strings"

	"github.com/google/ax/internal/install/executil"
)

// registryContainer returns the container that serves endpoint
// (UpOptions.LocalRegistry) and the port it listens on. The nodes reach a
// container on their network by its name, so the host of endpoint is the name
// of the container.
func registryContainer(endpoint string) (name, port string) {
	if name, port, err := net.SplitHostPort(endpoint); err == nil {
		return name, port
	}
	return endpoint, "80"
}

// loopbackRegistries returns the registries of images that only the host can
// reach: localhost and loopback addresses, with their port. The result is
// sorted and has no duplicates.
func loopbackRegistries(images []string) []string {
	var registries []string
	for _, image := range images {
		registry, _, ok := strings.Cut(strings.TrimSpace(image), "/")
		if !ok {
			continue
		}
		host := registry
		if h, _, err := net.SplitHostPort(registry); err == nil {
			host = h
		}
		if ip := net.ParseIP(host); host == "localhost" || (ip != nil && ip.IsLoopback()) {
			registries = append(registries, registry)
		}
	}
	slices.Sort(registries)
	return slices.Compact(registries)
}

// checkLocalRegistry fails when images are on a loopback registry and the
// container that serves them to the cluster at endpoint is not running.
func (p *KindProvider) checkLocalRegistry(ctx context.Context, rt executil.ContainerRuntime, registries []string, endpoint string) error {
	if len(registries) == 0 {
		return nil
	}
	if endpoint == "" {
		return fmt.Errorf("the local cluster cannot pull the images on %s: no registry serves the images of localhost to it", strings.Join(registries, ", "))
	}
	container, port := registryContainer(endpoint)
	if running, err := p.containerRunning(ctx, rt, container); err == nil && running {
		return nil
	}
	hostPort := "80"
	if _, registryPort, err := net.SplitHostPort(registries[0]); err == nil {
		hostPort = registryPort
	}
	return fmt.Errorf("the local cluster pulls the images on %s from a registry container named %q, which is not running: start it, for example with '%s run -d -p 127.0.0.1:%s:%s --name %s registry:2', and push the images to it",
		strings.Join(registries, ", "), container, filepath.Base(rt.Binary), hostPort, port, container)
}

// configureLocalRegistry makes the images on the loopback registries pullable
// from the cluster. It connects the container that serves endpoint to the
// network of the nodes, and makes containerd on each node pull those images
// from endpoint.
func (p *KindProvider) configureLocalRegistry(ctx context.Context, rt executil.ContainerRuntime, nodes, registries []string, endpoint string) error {
	if len(registries) == 0 || len(nodes) == 0 {
		return nil
	}
	container, _ := registryContainer(endpoint)
	fmt.Fprintf(p.Out, "==> Serving images on %s from the %q container...\n", strings.Join(registries, ", "), container)

	nodeNetworks, err := p.containerNetworks(ctx, rt, nodes[0])
	if err != nil {
		return err
	}
	registryNetworks, err := p.containerNetworks(ctx, rt, container)
	if err != nil {
		return err
	}
	for _, network := range nodeNetworks {
		if slices.Contains(registryNetworks, network) {
			continue
		}
		if _, err := p.Runner.Run(ctx, executil.Spec{
			Name: rt.Binary,
			Args: []string{"network", "connect", network, container},
		}); err != nil {
			return fmt.Errorf("connecting container %s to network %s: %w", container, network, err)
		}
	}

	hosts := fmt.Sprintf("[host.\"http://%s\"]\n", endpoint)
	for _, node := range nodes {
		for _, registry := range registries {
			if _, err := p.Runner.Run(ctx, executil.Spec{
				Name:  rt.Binary,
				Args:  []string{"exec", "-i", node, "sh", "-c", `mkdir -p "$1" && cat > "$1/hosts.toml"`, "sh", "/etc/containerd/certs.d/" + registry},
				Stdin: strings.NewReader(hosts),
			}); err != nil {
				return fmt.Errorf("configuring registry %s on node %s: %w", registry, node, err)
			}
		}
	}
	return nil
}

func (p *KindProvider) containerNetworks(ctx context.Context, rt executil.ContainerRuntime, container string) ([]string, error) {
	res, err := p.Runner.Run(ctx, executil.Spec{
		Name: rt.Binary,
		Args: []string{"inspect", "-f", "{{range $name, $network := .NetworkSettings.Networks}}{{println $name}}{{end}}", container},
	})
	if err != nil {
		return nil, fmt.Errorf("reading the networks of container %s: %w", container, err)
	}
	return strings.Fields(res.Stdout), nil
}
