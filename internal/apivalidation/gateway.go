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

package apivalidation

import (
	"fmt"
	"net/netip"
	"strings"

	"github.com/google/ax/pkg/apis/v1alpha1"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
)

// ValidateGateway reports the first problem with a gateway that would make it
// unusable. It is called by the API server before saving. Agent Substrate
// checks the rest of each egress rule later, when a task that binds the
// gateway is created.
//
// TODO: Validate everything that Substrate validates.
func ValidateGateway(g *v1alpha1.Gateway) error {
	if err := ValidateObjectMeta(g.GetMetadata()); err != nil {
		return err
	}
	for i, rule := range g.GetSpec().GetEgress() {
		kind, hostnames, effects, ports := egressRuleFields(rule)
		// A rule kind added by a newer Substrate would skip the checks below.
		if kind == "" {
			return fmt.Errorf("spec.egress[%d]: exactly one of http, https, or tlsPassthrough is required", i)
		}
		field := fmt.Sprintf("spec.egress[%d].%s", i, kind)
		credential := len(effects.GetReplaceHeaders()) > 0
		if credential && kind == "http" {
			return fmt.Errorf("%s.effects.replaceHeaders: not allowed on an http rule: the credential would be sent in cleartext", field)
		}
		for j, name := range hostnames {
			if reason := deniedEgressHostname(name); reason != "" {
				return fmt.Errorf("%s.hostnames[%d]: %q is not allowed: %s", field, j, name, reason)
			}
			if credential && strings.HasPrefix(name, "*.") {
				return fmt.Errorf("%s.effects.replaceHeaders: not allowed with wildcard hostname %q: name each host that gets the credential", field, name)
			}
		}
		// Unlike http and https, passthrough has no default port.
		if kind == "tlsPassthrough" && ports.GetAll() == nil && len(ports.GetNumbers()) == 0 {
			return fmt.Errorf("%s.ports: required, set numbers or all", field)
		}
	}
	return nil
}

// egressRuleFields returns the kind of rule that is set, named as in a
// manifest, and the fields the checks read. kind is empty unless the rule sets
// exactly one kind.
func egressRuleFields(rule *ateapipb.EgressRule) (kind string, hostnames []string, effects *ateapipb.HttpRuleEffects, ports *ateapipb.Ports) {
	http, https, passthrough := rule.GetHttp(), rule.GetHttps(), rule.GetTlsPassthrough()
	switch {
	case http != nil && https == nil && passthrough == nil:
		return "http", http.GetHostnames(), http.GetEffects(), http.GetPorts()
	case https != nil && http == nil && passthrough == nil:
		return "https", https.GetHostnames(), https.GetEffects(), https.GetPorts()
	case passthrough != nil && http == nil && https == nil:
		return "tlsPassthrough", passthrough.GetHostnames(), nil, passthrough.GetPorts()
	}
	return "", nil, nil, nil
}

// deniedEgressHostname returns why a hostname is not allowed, or "" if it is.
func deniedEgressHostname(name string) string {
	switch {
	case name == "*":
		return "it matches every host, including hosts inside the cluster"
	case isIPAddress(name):
		return "IP addresses cannot be used, name the host instead"
	case name == "localhost" || strings.HasSuffix(name, ".localhost"):
		return "it would be the egress gateway's localhost; a task reaches its own without a rule"
	}
	return ""
}

// isIPAddress reports whether name is an IP address. As in Agent Substrate, a
// name whose last label is a number counts as one.
func isIPAddress(name string) bool {
	if _, err := netip.ParseAddr(name); err == nil {
		return true
	}
	last := name[strings.LastIndexByte(name, '.')+1:]
	return last != "" && strings.Trim(last, "0123456789") == ""
}
