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

package runner

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/google/ax/pkg/apis/v1alpha1"
)

// caBundleEnv are the variables through which common tools find their CA
// certificates.
var caBundleEnv = []string{"SSL_CERT_FILE", "CURL_CA_BUNDLE", "REQUESTS_CA_BUNDLE", "NODE_EXTRA_CA_CERTS", "GIT_SSL_CAINFO"}

// gitAskpass is the name under which git starts this program to ask for the
// credentials of an HTTPS remote. The program answers with a placeholder, to
// be replaced with the real ones outside the sandbox. git asks only after a
// server demands credentials, so public repositories are unaffected.
const gitAskpass = "git-askpass"

// git reads the answer from the program's standard output, so the program
// answers here, before main can print or fail.
func init() {
	if len(os.Args) > 0 && filepath.Base(os.Args[0]) == gitAskpass {
		fmt.Println("ax-placeholder")
		os.Exit(0)
	}
}

// linkGitAskpass links dir/git-askpass to the running program and returns the
// link's path, or "" if the link cannot be made.
func linkGitAskpass(dir string) string {
	link := filepath.Join(dir, gitAskpass)
	exe, err := os.Executable()
	if err == nil {
		err = os.MkdirAll(dir, 0o755)
	}
	if err == nil {
		_ = os.Remove(link)
		err = os.Symlink(exe, link)
	}
	if err != nil {
		slog.Warn("failed to link git-askpass", "path", link, "error", err)
		return ""
	}
	return link
}

// setDefaultEnv sets the variables the runner provides to every task, except
// those the task's spec.env sets:
//   - the CA variables, pointed at caBundle if that file exists, so that TLS
//     intercepted on its way out of the sandbox verifies;
//   - GIT_ASKPASS, pointed at askpass unless that is empty, and
//     GIT_TERMINAL_PROMPT, so that git never waits for someone to type.
func setDefaultEnv(task *v1alpha1.Task, caBundle, askpass string) {
	env := map[string]string{"GIT_TERMINAL_PROMPT": "0"}
	if askpass != "" {
		env["GIT_ASKPASS"] = askpass
	}
	if _, err := os.Stat(caBundle); err == nil {
		for _, name := range caBundleEnv {
			env[name] = caBundle
		}
	}
	for _, e := range task.GetSpec().GetEnv() {
		delete(env, e.GetName())
	}
	for name, value := range env {
		_ = os.Setenv(name, value)
	}
}
