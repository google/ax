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

import "testing"

func TestResolveTarget(t *testing.T) {
	t.Setenv("AX_HOME", t.TempDir())
	t.Setenv("AX_SERVER", "")
	t.Setenv(contextEnv, "")

	check := func(name string, opts globalArgs, wantLocal bool, wantServer, wantKube string) {
		t.Helper()
		got, err := resolveTarget(opts)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.local != wantLocal || got.serverURL != wantServer || got.kubeContext != wantKube {
			t.Errorf("%s: got %+v, want local=%v server=%q kube=%q", name, got, wantLocal, wantServer, wantKube)
		}
	}

	check("default is local", globalArgs{}, true, "", "")
	check("--context local", globalArgs{kubeContext: "local"}, true, "", "")
	check("--context kube", globalArgs{kubeContext: "gke"}, false, "", "gke")
	check("--server wins", globalArgs{server: "http://x:8080", kubeContext: "gke"}, false, "http://x:8080", "")

	if err := saveConfig(cliConfig{Context: "saved"}); err != nil {
		t.Fatal(err)
	}
	check("saved context", globalArgs{}, false, "", "saved")
	check("flag beats saved", globalArgs{kubeContext: "local"}, true, "", "")

	t.Setenv(contextEnv, "local")
	check("$AX_CONTEXT beats saved", globalArgs{}, true, "", "")

	t.Setenv("KUBECONTEXT", "active-cluster")
	check("kube follows the active context", globalArgs{kubeContext: followKubeContext}, false, "", "active-cluster")

	t.Setenv("AX_SERVER", "http://env:8080")
	check("$AX_SERVER beats contexts", globalArgs{}, false, "http://env:8080", "")
}

func TestConfigUseContextRoundTrip(t *testing.T) {
	t.Setenv("AX_HOME", t.TempDir())
	t.Setenv("AX_SERVER", "")
	t.Setenv(contextEnv, "")

	if err := runConfig(globalArgs{}, []string{"use-context", "my-cluster"}); err != nil {
		t.Fatal(err)
	}
	got, _ := resolveTarget(globalArgs{})
	if got.kubeContext != "my-cluster" || got.name() != "my-cluster" {
		t.Errorf("after use-context my-cluster: %+v", got)
	}
	if err := runConfig(globalArgs{}, []string{"use-context", "local"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := resolveTarget(globalArgs{}); !got.local || got.name() != "local" {
		t.Errorf("after use-context local: %+v", got)
	}
	if err := runConfig(globalArgs{}, []string{"use-context"}); err == nil {
		t.Error("use-context without a name succeeded")
	}
	if err := runConfig(globalArgs{}, []string{"bogus"}); err == nil {
		t.Error("unknown config subcommand succeeded")
	}
	if err := runContext(globalArgs{}, []string{"use", "x"}); err == nil {
		t.Error("ax ctx use still accepted; it moved to ax config use-context")
	}
}
