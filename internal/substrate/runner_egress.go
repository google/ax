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

package substrate

import (
	"net"
	"strconv"
	"strings"

	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
)

const (
	// DefaultRunnerEndpoint is the in-cluster host:port of ax-server's :8081
	// pull-based runner control service.
	DefaultRunnerEndpoint = "ax-server-runner.ax-system.svc.cluster.local:8081"
	// DefaultRunnerAudience is the expected OIDC audience for Actor JWTs
	// injected by Substrate's egress gateway on requests to DefaultRunnerEndpoint.
	DefaultRunnerAudience = "ax-runner"

	defaultActorJWTExpirationSeconds int64 = 300
)

// RunnerEgressRule builds the built-in cleartext HTTP EgressRule allowing an
// actor to reach ax-server's internal runner control service (runnerHostPort,
// defaulting to DefaultRunnerEndpoint) while instructing Substrate's egress
// gateway to replace the actor's placeholder Authorization header with a
// verified Actor JWT bound to audience (defaulting to DefaultRunnerAudience)
// and to extraAudiences.
func RunnerEgressRule(runnerHostPort, audience string, extraAudiences ...string) *ateapipb.EgressRule {
	if runnerHostPort == "" {
		runnerHostPort = DefaultRunnerEndpoint
	}
	if audience == "" {
		audience = DefaultRunnerAudience
	}
	host := runnerHostPort
	port := 8081
	if h, portStr, err := net.SplitHostPort(runnerHostPort); err == nil {
		host = h
		if p, err := strconv.Atoi(portStr); err == nil && p >= 1 && p <= 65535 {
			port = p
		}
	}
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))

	return &ateapipb.EgressRule{
		Http: &ateapipb.HttpRule{
			Hostnames: []string{host},
			Ports:     &ateapipb.Ports{Numbers: []int32{int32(port)}},
			Effects: &ateapipb.HttpRuleEffects{
				ReplaceHeaders: []*ateapipb.CredentialHeader{{
					Header: "Authorization",
					Prefix: "Bearer ",
					ActorJwt: &ateapipb.ActorJwtSource{
						Audiences:         append([]string{audience}, extraAudiences...),
						ExpirationSeconds: defaultActorJWTExpirationSeconds,
					},
				}},
			},
		},
	}
}
