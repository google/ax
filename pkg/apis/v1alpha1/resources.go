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

package v1alpha1

import (
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
)

// Resource requirements.
//
// spec.resources.limits is copied onto the Substrate ActorTemplate, which
// enforces the same rules as below: cpu and memory only, each quantity greater
// than zero, and a cpu limit below 1000 cores. Checking them here means a bad
// value fails at apply time instead of when the template is created.
// Substrate sizes a sandbox by limits alone, so requests are rejected rather
// than accepted and ignored.

// MaxCPULimitCores is the exclusive upper bound Substrate places on a cpu limit.
const MaxCPULimitCores = 1000

// ValidateResources reports the first problem with a task's resource
// requirements. A nil or empty value is valid: the sandbox is then sized by its
// worker's defaults.
func ValidateResources(reqs *ResourceReqs) error {
	if req := reqs.GetRequests(); req.GetCpu() != "" || req.GetMemory() != "" {
		return errors.New("spec.resources.requests: not supported, only spec.resources.limits is applied to the sandbox")
	}
	limits := reqs.GetLimits()
	if cpu := limits.GetCpu(); cpu != "" {
		q, err := parseQuantity(cpu)
		if err != nil {
			return fmt.Errorf("spec.resources.limits.cpu: invalid quantity %q: %w", cpu, err)
		}
		if q.Sign() <= 0 {
			return fmt.Errorf("spec.resources.limits.cpu: %q must be greater than zero", cpu)
		}
		if q.Cmp(big.NewRat(MaxCPULimitCores, 1)) >= 0 {
			return fmt.Errorf("spec.resources.limits.cpu: %q must be less than %d cores", cpu, MaxCPULimitCores)
		}
	}
	if memory := limits.GetMemory(); memory != "" {
		q, err := parseQuantity(memory)
		if err != nil {
			return fmt.Errorf("spec.resources.limits.memory: invalid quantity %q: %w", memory, err)
		}
		if q.Sign() <= 0 {
			return fmt.Errorf("spec.resources.limits.memory: %q must be greater than zero", memory)
		}
	}
	return nil
}

// quantityRegexp follows the Kubernetes resource.Quantity grammar: a decimal
// number followed by an optional binary SI suffix (Ki..Ei), decimal SI suffix
// (n, u, m, k, M, G, T, P, E) or decimal exponent (e3, E-2).
var quantityRegexp = regexp.MustCompile(`^([+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+))([eE][+-]?[0-9]+|Ki|Mi|Gi|Ti|Pi|Ei|[numkMGTPE])?$`)

var quantitySuffixes = map[string]*big.Rat{
	"n":  big.NewRat(1, 1_000_000_000),
	"u":  big.NewRat(1, 1_000_000),
	"m":  big.NewRat(1, 1000),
	"k":  big.NewRat(1000, 1),
	"M":  big.NewRat(1_000_000, 1),
	"G":  big.NewRat(1_000_000_000, 1),
	"T":  big.NewRat(1_000_000_000_000, 1),
	"P":  big.NewRat(1_000_000_000_000_000, 1),
	"E":  big.NewRat(1_000_000_000_000_000_000, 1),
	"Ki": big.NewRat(1<<10, 1),
	"Mi": big.NewRat(1<<20, 1),
	"Gi": big.NewRat(1<<30, 1),
	"Ti": big.NewRat(1<<40, 1),
	"Pi": big.NewRat(1<<50, 1),
	"Ei": big.NewRat(1<<60, 1),
}

// parseQuantity evaluates a Kubernetes quantity string to its exact value in
// base units (cores for cpu, bytes for memory).
func parseQuantity(s string) (*big.Rat, error) {
	m := quantityRegexp.FindStringSubmatch(s)
	if m == nil {
		return nil, errors.New("must be a number with an optional suffix such as 500m, 2, 4Gi or 1e3")
	}
	value, ok := new(big.Rat).SetString(m[1])
	if !ok {
		return nil, errors.New("not a number")
	}
	switch suffix := m[2]; {
	case suffix == "":
	case suffix[0] == 'e' || suffix[0] == 'E':
		exp, err := strconv.Atoi(suffix[1:])
		if err != nil || exp > 64 || exp < -64 {
			return nil, errors.New("exponent out of range")
		}
		pow := new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(abs(exp))), nil))
		if exp < 0 {
			pow.Inv(pow)
		}
		value.Mul(value, pow)
	default:
		value.Mul(value, quantitySuffixes[suffix])
	}
	return value, nil
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
