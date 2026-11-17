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

package tunnel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/ax/internal/install/executil"
	"gopkg.in/yaml.v3"
)

// ContextEntry describes a configured kubeconfig context.
type ContextEntry struct {
	Name    string
	Cluster string
	User    string
	Current bool
}

type kubeConfig struct {
	CurrentContext string `yaml:"current-context"`
	Contexts       []struct {
		Name    string `yaml:"name"`
		Context struct {
			Cluster string `yaml:"cluster"`
			User    string `yaml:"user"`
		} `yaml:"context"`
	} `yaml:"contexts"`
	Clusters []struct {
		Name    string `yaml:"name"`
		Cluster struct {
			Server                   string `yaml:"server"`
			CertificateAuthorityData string `yaml:"certificate-authority-data"`
			CertificateAuthority     string `yaml:"certificate-authority"`
		} `yaml:"cluster"`
	} `yaml:"clusters"`
}

func kubeconfigPaths() []string {
	if p := os.Getenv("KUBECONFIG"); p != "" {
		var out []string
		for _, item := range filepath.SplitList(p) {
			if item != "" {
				out = append(out, item)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		return []string{filepath.Join(home, ".kube", "config")}
	}
	return nil
}

func resolveKubectl(ctx context.Context) (string, error) {
	return executil.EnsureKubectl(ctx, executil.OSRunner{}, executil.DefaultToolURLs(), os.Stderr)
}

func kubectlIfInstalled() string {
	if p, err := exec.LookPath("kubectl"); err == nil {
		return p
	}
	if dir, err := executil.BinDir(); err == nil {
		candidate := filepath.Join(dir, "kubectl")
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return candidate
		}
	}
	return ""
}

// CurrentContext returns the active Kubernetes context, checking:
// 1. Explicitly passed override
// 2. KUBECONTEXT environment variable
// 3. ~/.kube/config (or $KUBECONFIG) current-context (fast parse via YAML)
// 4. Fallback to 'kubectl config current-context'
func CurrentContext(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	if env := os.Getenv("KUBECONTEXT"); env != "" {
		return env, nil
	}
	if ctx, parsed := readCurrentContext(kubeconfigPaths()); parsed {
		return ctx, nil
	}

	kubectlBin := kubectlIfInstalled()
	if kubectlBin == "" {
		return "", nil
	}
	out, err := exec.Command(kubectlBin, "config", "current-context").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func readCurrentContext(paths []string) (string, bool) {
	var active string
	known := make(map[string]bool)
	parsed := false

	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var cfg kubeConfig
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			continue
		}
		parsed = true
		if active == "" && cfg.CurrentContext != "" {
			active = cfg.CurrentContext
		}
		for _, c := range cfg.Contexts {
			known[c.Name] = true
		}
	}
	if !known[active] {
		return "", parsed
	}
	return active, parsed
}

// ListContexts returns all contexts defined in the user's kubeconfig files,
// sorted by name, with Current set on the active context.
func ListContexts(overrideCurrent string) ([]ContextEntry, error) {
	active, _ := CurrentContext(overrideCurrent)
	seen := make(map[string]ContextEntry)

	for _, path := range kubeconfigPaths() {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var cfg kubeConfig
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			continue
		}
		for _, c := range cfg.Contexts {
			if c.Name == "" {
				continue
			}
			if _, exists := seen[c.Name]; !exists {
				seen[c.Name] = ContextEntry{
					Name:    c.Name,
					Cluster: c.Context.Cluster,
					User:    c.Context.User,
					Current: c.Name == active,
				}
			}
		}
	}

	out := make([]ContextEntry, 0, len(seen))
	for _, entry := range seen {
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// SetCurrentContext switches `current-context` in kubeconfig to target. When
// kubectl is already installed locally it delegates to `kubectl config
// use-context`; otherwise it validates and updates kubeconfig directly in Go so
// switching contexts works offline (and in hermetic unit tests) without
// downloading kubectl.
func SetCurrentContext(target string) error {
	target = strings.TrimSpace(target)
	if target == "" {
		return fmt.Errorf("context name must not be empty")
	}
	if kubectlBin := kubectlIfInstalled(); kubectlBin != "" {
		out, err := exec.Command(kubectlBin, "config", "use-context", target).CombinedOutput()
		if err != nil {
			msg := strings.TrimSpace(string(out))
			if msg != "" {
				return fmt.Errorf("%s", msg)
			}
			return fmt.Errorf("switching kubeconfig context to %q: %w", target, err)
		}
		return nil
	}
	return setCurrentContextYAML(target)
}

func setCurrentContextYAML(target string) error {
	entries, err := ListContexts("")
	if err != nil {
		return err
	}
	found := false
	for _, e := range entries {
		if e.Name == target {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("no context exists with the name: %q", target)
	}

	paths := kubeconfigPaths()
	if len(paths) == 0 {
		return fmt.Errorf("cannot locate kubeconfig path")
	}
	primary := paths[0]
	raw, err := os.ReadFile(primary)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return err
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("unexpected kubeconfig YAML root")
	}
	mapping := doc.Content[0]
	updated := false
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == "current-context" {
			mapping.Content[i+1].Value = target
			updated = true
			break
		}
	}
	if !updated {
		mapping.Content = append(mapping.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "current-context"},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: target},
		)
	}
	encoded, err := yaml.Marshal(&doc)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(primary), ".kubeconfig-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(encoded); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, primary); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

// clusterFingerprint returns a short SHA-256 hex digest of the target context's
// Kubernetes cluster endpoint and CA certificate from kubeconfig. Including
// this fingerprint in router TLS cache keys naturally invalidates cached mTLS
// credentials whenever a cluster is deleted and recreated under the same
// context name (e.g. `ax local delete` -> `ax local start`), without requiring
// callers to manually clear cache files.
func clusterFingerprint(ctxName string) string {
	if ctxName == "" {
		ctxName, _ = CurrentContext("")
	}
	if ctxName == "" {
		return ""
	}

	paths := kubeconfigPaths()
	var parsed []struct {
		path string
		cfg  kubeConfig
	}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var cfg kubeConfig
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			continue
		}
		parsed = append(parsed, struct {
			path string
			cfg  kubeConfig
		}{path: p, cfg: cfg})
	}

	var clusterName string
	for _, item := range parsed {
		for _, c := range item.cfg.Contexts {
			if c.Name == ctxName {
				clusterName = c.Context.Cluster
				break
			}
		}
		if clusterName != "" {
			break
		}
	}
	if clusterName == "" {
		return ""
	}

	for _, item := range parsed {
		for _, cl := range item.cfg.Clusters {
			if cl.Name != clusterName {
				continue
			}
			server := strings.TrimSpace(cl.Cluster.Server)
			ca := strings.TrimSpace(cl.Cluster.CertificateAuthorityData)
			if ca == "" && strings.TrimSpace(cl.Cluster.CertificateAuthority) != "" {
				caPath := strings.TrimSpace(cl.Cluster.CertificateAuthority)
				if !filepath.IsAbs(caPath) {
					caPath = filepath.Join(filepath.Dir(item.path), caPath)
				}
				if caBytes, err := os.ReadFile(caPath); err == nil {
					ca = strings.TrimSpace(string(caBytes))
				} else {
					ca = caPath
				}
			}
			sum := sha256.Sum256([]byte(server + "\n" + ca))
			return hex.EncodeToString(sum[:8])
		}
	}
	return ""
}
