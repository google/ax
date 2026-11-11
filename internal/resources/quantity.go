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

package resources

import (
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
)

// MaxCPULimitCores is the exclusive upper bound Substrate places on a cpu limit.
const MaxCPULimitCores = 1000

// ParseCPUMillicores parses a Kubernetes CPU quantity string into millicores,
// rounding up sub-millicore fractions.
func ParseCPUMillicores(cpu string) (int64, error) {
	q, err := parseQuantity(cpu)
	if err != nil {
		return 0, fmt.Errorf("invalid quantity %q: %w", cpu, err)
	}
	if q.Sign() <= 0 {
		return 0, fmt.Errorf("%q must be greater than zero", cpu)
	}
	if q.Cmp(big.NewRat(MaxCPULimitCores, 1)) >= 0 {
		return 0, fmt.Errorf("%q must be less than %d cores", cpu, MaxCPULimitCores)
	}
	milli := new(big.Rat).Mul(q, big.NewRat(1000, 1))
	v, ok := ratCeilInt64(milli)
	if !ok || v >= MaxCPULimitCores*1000 {
		return 0, fmt.Errorf("%q must be less than %d cores", cpu, MaxCPULimitCores)
	}
	return v, nil
}

// ParseMemoryBytes parses a Kubernetes memory quantity string into bytes,
// rounding up fractional bytes.
func ParseMemoryBytes(memory string) (int64, error) {
	q, err := parseQuantity(memory)
	if err != nil {
		return 0, fmt.Errorf("invalid quantity %q: %w", memory, err)
	}
	if q.Sign() <= 0 {
		return 0, fmt.Errorf("%q must be greater than zero", memory)
	}
	v, ok := ratCeilInt64(q)
	if !ok {
		return 0, fmt.Errorf("invalid quantity %q: value overflows int64", memory)
	}
	return v, nil
}

func ratCeilInt64(q *big.Rat) (int64, bool) {
	num := new(big.Int).Add(q.Num(), q.Denom())
	num.Sub(num, big.NewInt(1))
	quo := new(big.Int).Quo(num, q.Denom())
	if !quo.IsInt64() {
		return 0, false
	}
	return quo.Int64(), true
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
