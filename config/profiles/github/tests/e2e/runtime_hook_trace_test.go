//go:build linux

package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

// Exercise the production Go launcher, including environment and native shebang
// execution. An unavailable/stopped yard legitimately skips the hook entirely.
func TestRuntimeHookTraceNativeLauncher(t *testing.T) {
	helper, err := filepath.Abs("runtime-hook-trace.py")
	if err != nil {
		t.Fatal(err)
	}
	wrapper, err := os.ReadFile(helper)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"running", "failure", "absent", "stopped"} {
		t.Run(scenario, func(t *testing.T) {
			root := testkit.TempDir(t)
			directory := filepath.Join(root, "config/profiles/synthetic")
			wrappers := filepath.Join(root, "wrappers")
			for _, path := range []string{directory, wrappers} {
				if err := os.MkdirAll(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			testkit.WriteFile(t, filepath.Join(wrappers, "bash"), wrapper, 0700)
			testkit.WriteFile(t, filepath.Join(directory, "profile.json"), []byte(`{"schema_version":1,"runtime":{"activation_id":"synthetic-runtime","handler":"runtime.sh"}}`), 0600)
			body := []byte("#!/usr/bin/env bash\nprintf '{\"state\":\"absent\"}\\n'\n")
			if scenario == "failure" {
				body = []byte("#!/usr/bin/env bash\nprintf 'git-fetch: native operation timed out; inspect owned timer state\\n' >&2\nexit 2\n")
			}
			source := filepath.Join(directory, "runtime.sh")
			testkit.WriteFile(t, source, body, 0700)
			log := filepath.Join(root, "hook.jsonl")
			instances := map[string]ports.InstanceInfo{}
			if scenario != "absent" {
				state := "Running"
				if scenario == "stopped" {
					state = "Stopped"
				}
				instances["p/i"] = ports.InstanceInfo{Status: state}
			}
			runtime := reconcileruntime.Runtime{RepositoryRoot: root, Incus: &testkit.Incus{Instances: instances}, Yard: domain.Context{IncusProject: "p", YardInstanceName: "i"}, Environment: append(os.Environ(),
				"PATH="+wrappers+":"+os.Getenv("PATH"), "SUBYARD_TRACE_REAL_BASH=/bin/bash", "SUBYARD_TRACE_HOOK_SOURCE="+source, "SUBYARD_TRACE_HOOK_LOG="+log, fmt.Sprintf("SUBYARD_TRACE_HOOK_SHA256=%x", sha256.Sum256(body)))}
			_, err := runtime.ObserveProfileRuntime(context.Background(), "synthetic-runtime")
			if (err != nil) != (scenario == "failure") {
				t.Fatalf("native outcome: %v", err)
			}
			output, err := exec.Command("python3", helper, "--verify", log).CombinedOutput()
			if err != nil {
				t.Fatalf("successful native launch/skip became capture failure: %v %s", err, output)
			}
			var capture struct {
				Observations int
				Status       string
			}
			if err := json.Unmarshal(output, &capture); err != nil {
				t.Fatal(err)
			}
			want := "observed"
			if scenario == "absent" || scenario == "stopped" {
				want = "not-observed"
			}
			count := 1
			if want == "not-observed" {
				count = 0
			}
			if capture.Status != want || capture.Observations != count {
				t.Fatalf("capture=%s want=%s", output, want)
			}
			if scenario == "failure" {
				raw, err := os.ReadFile(log)
				if err != nil {
					t.Fatal(err)
				}
				var terminal map[string]any
				for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte{'\n'}) {
					if err := json.Unmarshal(line, &terminal); err != nil {
						t.Fatal(err)
					}
				}
				if terminal["exit_code"] != float64(2) {
					t.Fatalf("lost native failure: %v", terminal)
				}
			}
		})
	}
}
