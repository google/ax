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

package axdeploy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	ateSystemNamespace       = "ate-system"
	credProviderPolicyCM     = "k8s-credential-provider-namespace-policy"
	credProviderDeployment   = "k8s-credential-provider"
	credentialLabelKey       = "ax.io/credential"
	credentialLabelVal       = "true"
	apiAccessNamespaceLabel  = "ate.dev/api-access"
	bootstrapOwnersFlag      = "--authz-bootstrap-owners"
	ingressAllowedSPIFFEFlag = "--ingress-allowed-spiffe-ids"
)

type namespacePolicyFile struct {
	Policies []atespacePolicy `yaml:"policies"`
}

type atespacePolicy struct {
	Atespace          string          `yaml:"atespace"`
	AllowedNamespaces []string        `yaml:"allowedNamespaces"`
	SecretSelector    *secretSelector `yaml:"secretSelector,omitempty"`
}

type secretSelector struct {
	MatchLabels map[string]string `yaml:"matchLabels,omitempty"`
}

// GrantSubstrateAccess idempotently labels axNamespace for ate-api-server
// NetworkPolicy ingress and registers ax-server's ServiceAccount / SPIFFE ID /
// namespace policy in Substrate. Deployments only roll when a flag or ConfigMap
// actually changes.
func GrantSubstrateAccess(ctx context.Context, k *kubectlClient, axNamespace string, rolloutTimeout time.Duration, out io.Writer) error {
	if err := ensureLabeledNamespace(ctx, k, axNamespace); err != nil {
		return err
	}

	principal := fmt.Sprintf("system:serviceaccount:%s:ax-server", axNamespace)
	if changed, err := reconcileDeploymentCSVFlag(ctx, k, "ate-api-server", "ate-api-server", bootstrapOwnersFlag, principal, true); err != nil {
		return err
	} else if changed {
		fmt.Fprintf(out, "==> Registered %s in ate-api-server %s; waiting for rollout...\n", principal, bootstrapOwnersFlag)
		if err := k.rolloutStatus(ctx, ateSystemNamespace, "deployment/ate-api-server", rolloutTimeout); err != nil {
			return err
		}
	}

	spiffeID := fmt.Sprintf("spiffe://cluster.local/ns/%s/sa/ax-server", axNamespace)
	if changed, err := reconcileDeploymentCSVFlag(ctx, k, "atenet-router", "atenet-router", ingressAllowedSPIFFEFlag, spiffeID, true); err != nil {
		return err
	} else if changed {
		fmt.Fprintf(out, "==> Registered %s in atenet-router %s; waiting for rollout...\n", spiffeID, ingressAllowedSPIFFEFlag)
		if err := k.rolloutStatus(ctx, ateSystemNamespace, "deployment/atenet-router", rolloutTimeout); err != nil {
			return err
		}
	}

	if changed, present, err := reconcileCredProviderPolicy(ctx, k, axNamespace, true); err != nil {
		return err
	} else if !present {
		fmt.Fprintf(out, "==> Note: %s/%s not present on cluster; skipping k8s-credential-provider policy update.\n", ateSystemNamespace, credProviderPolicyCM)
	} else if changed && k.exists(ctx, ateSystemNamespace, "deployment", credProviderDeployment) {
		fmt.Fprintf(out, "==> Updated %s for namespace %q; restarting %s...\n", credProviderPolicyCM, axNamespace, credProviderDeployment)
		if _, err := k.run(ctx, nil, "-n", ateSystemNamespace, "rollout", "restart", "deployment/"+credProviderDeployment); err != nil {
			return fmt.Errorf("restarting %s: %w", credProviderDeployment, err)
		}
		if err := k.rolloutStatus(ctx, ateSystemNamespace, "deployment/"+credProviderDeployment, rolloutTimeout); err != nil {
			return err
		}
	}

	return nil
}

// RevokeSubstrateAccess removes axNamespace's ax-server permissions from
// Substrate and drops the ate.dev/api-access label from axNamespace.
func RevokeSubstrateAccess(ctx context.Context, k *kubectlClient, axNamespace string, rolloutTimeout time.Duration, out io.Writer) error {
	principal := fmt.Sprintf("system:serviceaccount:%s:ax-server", axNamespace)
	if k.exists(ctx, ateSystemNamespace, "deployment", "ate-api-server") {
		if changed, err := reconcileDeploymentCSVFlag(ctx, k, "ate-api-server", "ate-api-server", bootstrapOwnersFlag, principal, false); err != nil {
			return err
		} else if changed {
			fmt.Fprintf(out, "==> Removed %s from ate-api-server; waiting for rollout...\n", principal)
			if err := k.rolloutStatus(ctx, ateSystemNamespace, "deployment/ate-api-server", rolloutTimeout); err != nil {
				return err
			}
		}
	}

	spiffeID := fmt.Sprintf("spiffe://cluster.local/ns/%s/sa/ax-server", axNamespace)
	if k.exists(ctx, ateSystemNamespace, "deployment", "atenet-router") {
		if changed, err := reconcileDeploymentCSVFlag(ctx, k, "atenet-router", "atenet-router", ingressAllowedSPIFFEFlag, spiffeID, false); err != nil {
			return err
		} else if changed {
			fmt.Fprintf(out, "==> Removed %s from atenet-router; waiting for rollout...\n", spiffeID)
			if err := k.rolloutStatus(ctx, ateSystemNamespace, "deployment/atenet-router", rolloutTimeout); err != nil {
				return err
			}
		}
	}

	if changed, present, err := reconcileCredProviderPolicy(ctx, k, axNamespace, false); err != nil {
		return err
	} else if present && changed && k.exists(ctx, ateSystemNamespace, "deployment", credProviderDeployment) {
		fmt.Fprintf(out, "==> Removed namespace %q from %s; restarting %s...\n", axNamespace, credProviderPolicyCM, credProviderDeployment)
		_, _ = k.run(ctx, nil, "-n", ateSystemNamespace, "rollout", "restart", "deployment/"+credProviderDeployment)
		_ = k.rolloutStatus(ctx, ateSystemNamespace, "deployment/"+credProviderDeployment, rolloutTimeout)
	}

	if k.exists(ctx, "", "namespace", axNamespace) {
		_, _ = k.run(ctx, nil, "label", "namespace", axNamespace, apiAccessNamespaceLabel+"-")
	}
	return nil
}

func ensureLabeledNamespace(ctx context.Context, k *kubectlClient, ns string) error {
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: %s
  labels:
    %s: "true"
`, ns, apiAccessNamespaceLabel)
	if _, err := k.run(ctx, strings.NewReader(manifest), "apply", "-f", "-"); err != nil {
		return fmt.Errorf("ensuring namespace %q with %s=true: %w", ns, apiAccessNamespaceLabel, err)
	}
	_, err := k.run(ctx, nil, "label", "namespace", ns, apiAccessNamespaceLabel+"=true", "--overwrite")
	return err
}

func reconcileDeploymentCSVFlag(ctx context.Context, k *kubectlClient, depName, containerName, flagName, entry string, add bool) (bool, error) {
	res, err := k.run(ctx, nil, "-n", ateSystemNamespace, "get", "deployment", depName, "-o", "json")
	if err != nil {
		return false, fmt.Errorf("reading %s/deployment/%s (is Agent Substrate installed on this cluster?): %w", ateSystemNamespace, depName, err)
	}

	var dep struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Name string   `json:"name"`
						Args []string `json:"args"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &dep); err != nil {
		return false, fmt.Errorf("decoding %s/deployment/%s: %w", ateSystemNamespace, depName, err)
	}

	containerIdx := -1
	var origArgs []string
	for i, c := range dep.Spec.Template.Spec.Containers {
		if c.Name == containerName {
			containerIdx = i
			origArgs = c.Args
			break
		}
	}
	if containerIdx == -1 {
		return false, fmt.Errorf("container %q not found in %s/deployment/%s", containerName, ateSystemNamespace, depName)
	}

	newArgs, changed := mutateCSVFlagArgs(origArgs, flagName, entry, add)
	if !changed {
		return false, nil
	}

	patch := []map[string]any{{
		"op":    "replace",
		"path":  fmt.Sprintf("/spec/template/spec/containers/%d/args", containerIdx),
		"value": newArgs,
	}}
	patchJSON, err := json.Marshal(patch)
	if err != nil {
		return false, err
	}
	if _, err := k.run(ctx, nil, "-n", ateSystemNamespace, "patch", "deployment", depName, "--type=json", "-p", string(patchJSON)); err != nil {
		return false, fmt.Errorf("patching %s on %s/deployment/%s: %w", flagName, ateSystemNamespace, depName, err)
	}
	return true, nil
}

// mutateCSVFlagArgs adds or removes entry from a comma-separated `--flag=a,b`
// argument slice while preserving order and all unrelated flags.
func mutateCSVFlagArgs(args []string, flagName, entry string, add bool) ([]string, bool) {
	prefix := flagName + "="
	var values []string
	var otherArgs []string
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			for _, item := range strings.Split(strings.TrimPrefix(a, prefix), ",") {
				item = strings.TrimSpace(item)
				if item != "" && !slices.Contains(values, item) {
					values = append(values, item)
				}
			}
			continue
		}
		otherArgs = append(otherArgs, a)
	}

	if add {
		if slices.Contains(values, entry) {
			return args, false
		}
		values = append(values, entry)
	} else {
		idx := slices.Index(values, entry)
		if idx == -1 {
			return args, false
		}
		values = slices.Delete(values, idx, idx+1)
	}

	out := slices.Clone(otherArgs)
	if len(values) > 0 {
		out = append(out, prefix+strings.Join(values, ","))
	}
	return out, !slices.Equal(args, out)
}

func reconcileCredProviderPolicy(ctx context.Context, k *kubectlClient, axNamespace string, add bool) (changed, present bool, err error) {
	if !k.exists(ctx, ateSystemNamespace, "configmap", credProviderPolicyCM) {
		return false, false, nil
	}
	res, err := k.run(ctx, nil, "-n", ateSystemNamespace, "get", "configmap", credProviderPolicyCM, "-o", "json")
	if err != nil {
		return false, true, err
	}
	var cm struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &cm); err != nil {
		return false, true, err
	}
	origRaw := cm.Data["namespace-policy.yaml"]
	updatedRaw, mutated, err := mutateNamespacePolicyYAML(origRaw, axNamespace, add)
	if err != nil {
		return false, true, err
	}
	if !mutated {
		return false, true, nil
	}

	obj := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      credProviderPolicyCM,
			"namespace": ateSystemNamespace,
		},
		"data": map[string]string{
			"namespace-policy.yaml": updatedRaw,
		},
	}
	manifest, err := yaml.Marshal(obj)
	if err != nil {
		return false, true, err
	}
	if _, err := k.run(ctx, bytes.NewReader(manifest), "apply", "-f", "-"); err != nil {
		return false, true, fmt.Errorf("updating %s/%s: %w", ateSystemNamespace, credProviderPolicyCM, err)
	}
	return true, true, nil
}

// mutateNamespacePolicyYAML adds or removes axNamespace in the default atespace
// grant gated by `ax.io/credential: "true"`.
func mutateNamespacePolicyYAML(raw, axNamespace string, add bool) (string, bool, error) {
	var parsed namespacePolicyFile
	if strings.TrimSpace(raw) != "" {
		if err := yaml.Unmarshal([]byte(raw), &parsed); err != nil {
			return "", false, fmt.Errorf("parsing namespace-policy.yaml: %w", err)
		}
	}

	matchIdx := -1
	for i, p := range parsed.Policies {
		if p.Atespace == "default" &&
			p.SecretSelector != nil &&
			len(p.SecretSelector.MatchLabels) == 1 &&
			p.SecretSelector.MatchLabels[credentialLabelKey] == credentialLabelVal {
			matchIdx = i
			break
		}
	}

	if add {
		if matchIdx == -1 {
			nsList := []string{"default"}
			if axNamespace != "" && axNamespace != "default" {
				nsList = []string{axNamespace, "default"}
			}
			parsed.Policies = append(parsed.Policies, atespacePolicy{
				Atespace:          "default",
				AllowedNamespaces: nsList,
				SecretSelector: &secretSelector{
					MatchLabels: map[string]string{credentialLabelKey: credentialLabelVal},
				},
			})
		} else {
			nsList := parsed.Policies[matchIdx].AllowedNamespaces
			changed := false
			if !slices.Contains(nsList, axNamespace) {
				nsList = append(nsList, axNamespace)
				changed = true
			}
			if !slices.Contains(nsList, "default") {
				nsList = append(nsList, "default")
				changed = true
			}
			if !changed {
				return raw, false, nil
			}
			parsed.Policies[matchIdx].AllowedNamespaces = nsList
		}
	} else {
		if matchIdx == -1 {
			return raw, false, nil
		}
		if axNamespace == "default" {
			return raw, false, nil
		}
		nsList := parsed.Policies[matchIdx].AllowedNamespaces
		idx := slices.Index(nsList, axNamespace)
		if idx == -1 {
			return raw, false, nil
		}
		parsed.Policies[matchIdx].AllowedNamespaces = slices.Delete(nsList, idx, idx+1)
	}

	outBytes, err := yaml.Marshal(&parsed)
	if err != nil {
		return "", false, err
	}
	return string(outBytes), string(outBytes) != raw, nil
}
