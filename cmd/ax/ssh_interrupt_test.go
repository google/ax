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

package main

import (
	"os"
	"testing"
	"time"
)

func TestEscalateInterruptsForwardsFirstAndExitsOnSecond(t *testing.T) {
	in := make(chan os.Signal, 2)
	exited := make(chan struct{})
	out := escalateInterrupts(in, time.Minute, func() { close(exited) })

	in <- os.Interrupt
	select {
	case <-out:
	case <-time.After(2 * time.Second):
		t.Fatal("first interrupt was not forwarded")
	}
	select {
	case <-exited:
		t.Fatal("exited after a single interrupt")
	default:
	}

	in <- os.Interrupt
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("second interrupt did not exit")
	}
	select {
	case <-out:
		t.Error("second interrupt was forwarded as well")
	default:
	}
}

func TestEscalateInterruptsSlowSecondIsForwarded(t *testing.T) {
	in := make(chan os.Signal, 2)
	out := escalateInterrupts(in, 10*time.Millisecond, func() { t.Error("exited on a slow second interrupt") })

	in <- os.Interrupt
	<-out
	time.Sleep(50 * time.Millisecond)
	in <- os.Interrupt
	select {
	case <-out:
	case <-time.After(2 * time.Second):
		t.Fatal("second interrupt outside the window was not forwarded")
	}
}
