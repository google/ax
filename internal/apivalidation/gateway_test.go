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

package apivalidation_test

import (
	"strings"
	"testing"

	"github.com/google/ax/internal/apivalidation"
	"github.com/google/ax/pkg/apis/v1alpha1"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
)

func TestValidateGateway(t *testing.T) {
	http := func(effects *ateapipb.HttpRuleEffects, hostnames ...string) *ateapipb.EgressRule {
		return &ateapipb.EgressRule{Http: &ateapipb.HttpRule{Hostnames: hostnames, Effects: effects}}
	}
	https := func(effects *ateapipb.HttpRuleEffects, hostnames ...string) *ateapipb.EgressRule {
		return &ateapipb.EgressRule{Https: &ateapipb.HttpsRule{Hostnames: hostnames, Effects: effects}}
	}
	passthrough := func(ports *ateapipb.Ports, hostnames ...string) *ateapipb.EgressRule {
		return &ateapipb.EgressRule{TlsPassthrough: &ateapipb.TlsPassthroughRule{Hostnames: hostnames, Ports: ports}}
	}
	credential := &ateapipb.HttpRuleEffects{ReplaceHeaders: []*ateapipb.CredentialHeader{{
		Header:        "Authorization",
		CredentialUri: "ate-secret://k8s.io/default/default/token/value",
	}}}
	port443 := &ateapipb.Ports{Numbers: []int32{443}}
	tests := []struct {
		name    string
		rule    *ateapipb.EgressRule
		wantErr string
	}{
		{name: "host with a credential", rule: https(credential, "api.example.com")},
		{name: "subdomain wildcard", rule: https(nil, "*.example.com")},
		{name: "passthrough with port numbers", rule: passthrough(port443, "db.example.com")},
		{name: "passthrough with every port", rule: passthrough(&ateapipb.Ports{All: &ateapipb.AllPorts{}}, "db.example.com")},
		{name: "names that resemble denied ones", rule: https(nil, "notlocalhost", "1.example.com")},
		{name: "every host", rule: https(nil, "*"), wantErr: `spec.egress[0].https.hostnames[0]: "*" is not allowed: it matches every host`},
		{
			name:    "subdomain wildcard with a credential",
			rule:    https(credential, "*.example.com"),
			wantErr: `spec.egress[0].https.effects.replaceHeaders: not allowed with wildcard hostname "*.example.com"`,
		},
		{name: "credential over http", rule: http(credential, "api.example.com"), wantErr: "spec.egress[0].http.effects.replaceHeaders: not allowed on an http rule"},
		{name: "rule without a kind", rule: &ateapipb.EgressRule{}, wantErr: "spec.egress[0]: exactly one of http, https, or tlsPassthrough is required"},
		{
			name:    "rule with two kinds",
			rule:    &ateapipb.EgressRule{Http: &ateapipb.HttpRule{Hostnames: []string{"example.com"}}, Https: &ateapipb.HttpsRule{Hostnames: []string{"example.com"}}},
			wantErr: "exactly one of http, https, or tlsPassthrough is required",
		},
		{name: "passthrough without ports", rule: passthrough(nil, "db.example.com"), wantErr: "spec.egress[0].tlsPassthrough.ports: required"},
		{
			name:    "passthrough to localhost",
			rule:    passthrough(port443, "example.com", "localhost"),
			wantErr: `spec.egress[0].tlsPassthrough.hostnames[1]: "localhost" is not allowed: it would be the egress gateway's localhost`,
		},
		{name: "name under localhost", rule: http(nil, "app.localhost"), wantErr: "it would be the egress gateway's localhost"},
		{name: "ipv6 address", rule: https(nil, "fd00::1"), wantErr: `spec.egress[0].https.hostnames[0]: "fd00::1" is not allowed: IP addresses cannot be used`},
		{name: "ip address in short form", rule: https(nil, "127.1"), wantErr: "IP addresses cannot be used"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := apivalidation.ValidateGateway(&v1alpha1.Gateway{
				Metadata: &v1alpha1.ObjectMeta{Name: "gw"},
				Spec:     &v1alpha1.GatewaySpec{Egress: []*ateapipb.EgressRule{tt.rule}},
			})
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}
