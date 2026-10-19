package reconcileruntime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestDiskTmpGuestPolicy(t *testing.T) {
	root := testkit.TempDir(t)
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	for name, script := range map[string]string{
		"stat":     `case "$*" in "-c %u:%g "*) echo 0:0 ;; *) echo "$MODE:0:0" ;; esac`,
		"readlink": `echo "$MASK"`,
		"findmnt":  `echo "$FILESYSTEM"`,
		"chmod":    `test "$*" = "1777 /tmp"; echo "$*" >> "$CALLS"`,
		"systemctl": `case "$*" in
  is-system-running) echo running ;;
  "mask tmp.mount") echo "$*" >> "$CALLS" ;;
  *) exit 1 ;;
esac`,
	} {
		testkit.WriteFile(t, filepath.Join(bin, name), []byte("#!/bin/sh\nset -eu\n"+script+"\n"), 0700)
	}
	for _, test := range []struct {
		mode, filesystem, mask, permissions string
		exit                                int
	}{
		{"apply", "tmpfs", "/dev/null", "1777", 0},
		{"apply", "tmpfs", "/dev/null", "1777", 0}, // Safe repeat: no stop/unmount.
		{"check", "tmpfs", "/dev/null", "1777", 10},
		{"check", "ramfs", "/dev/null", "1777", 10},
		{"check", "ext4", "/dev/null", "1777", 0},
		{"check", "btrfs", "/dev/null", "1777", 0},
		{"check", "ext4", "", "1777", 1},
		{"check", "ext4", "/dev/null", "777", 1},
		{"check", "unknown", "/dev/null", "1777", 1},
	} {
		command := exec.Command("sh", "../../../scripts/guest-disk-tmp.sh", test.mode)
		command.Env = append(os.Environ(), "PATH="+bin+":/usr/bin:/bin", "MODE="+test.permissions,
			"FILESYSTEM="+test.filesystem, "MASK="+test.mask, "CALLS="+filepath.Join(root, "calls"))
		output, err := command.CombinedOutput()
		code := 0
		if err != nil {
			var failure *exec.ExitError
			if !errors.As(err, &failure) {
				t.Fatal(err)
			}
			code = failure.ExitCode()
		}
		if code != test.exit {
			t.Fatalf("%+v: exit=%d output=%s", test, code, output)
		}
	}
	calls, err := os.ReadFile(filepath.Join(root, "calls"))
	if err != nil || string(calls) != strings.Repeat("1777 /tmp\nmask tmp.mount\n", 2) {
		t.Fatalf("policy writes = %q, err=%v", calls, err)
	}
}

func TestDiskTmpPolicyWaitsForGuestBoot(t *testing.T) {
	for _, completed := range []string{"running", "degraded"} {
		t.Run(completed, func(t *testing.T) {
			root := testkit.TempDir(t)
			bin := filepath.Join(root, "bin")
			if err := os.Mkdir(bin, 0700); err != nil {
				t.Fatal(err)
			}
			for name, script := range map[string]string{
				"stat":  `echo 0:0`,
				"sleep": `:`,
				"chmod": `[ "$(cat "$COUNT")" -ge 3 ]; echo chmod >> "$CALLS"`,
				"systemctl": `case "$*" in
  is-system-running)
    count=$(cat "$COUNT" 2>/dev/null || echo 0); count=$((count + 1)); echo "$count" > "$COUNT"
    case "$count" in
      1) echo 'Failed to connect to system scope bus via local transport: No such file or directory' >&2; echo unknown; exit 1 ;;
      2) echo starting; exit 1 ;;
      *) echo "$COMPLETED"; [ "$COMPLETED" = running ] ;;
    esac ;;
  "mask tmp.mount")
    [ "$(cat "$COUNT")" -ge 3 ]; echo mask >> "$CALLS" ;;
  *) exit 1 ;;
esac`,
			} {
				testkit.WriteFile(t, filepath.Join(bin, name), []byte("#!/bin/sh\nset -eu\n"+script+"\n"), 0700)
			}
			run := func() ([]byte, error) {
				command := exec.Command("sh", "../../../scripts/guest-disk-tmp.sh", "apply")
				command.Env = append(os.Environ(), "PATH="+bin+":/usr/bin:/bin", "COMPLETED="+completed,
					"COUNT="+filepath.Join(root, "count"), "CALLS="+filepath.Join(root, "calls"))
				return command.CombinedOutput()
			}
			if output, err := run(); err != nil {
				t.Fatalf("exec-ready guest did not finish boot: %v: %s", err, output)
			}
			calls, err := os.ReadFile(filepath.Join(root, "calls"))
			if err != nil || string(calls) != "chmod\nmask\n" {
				t.Fatalf("policy writes = %q, err=%v", calls, err)
			}
			// A terminal manager state is an error, rather than boot progress.
			testkit.WriteFile(t, filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\necho maintenance\nexit 1\n"), 0700)
			if output, err := run(); err == nil || !strings.Contains(string(output), "state=maintenance exit=1") {
				t.Fatalf("terminal boot state was not reported: %v: %s", err, output)
			}
			// A failed native deadline must prevent policy writes entirely.
			testkit.WriteFile(t, filepath.Join(bin, "timeout"), []byte("#!/bin/sh\ntest \"$1 $2\" = '--kill-after=5 120' || exit 2\nexit 124\n"), 0700)
			if output, err := run(); err == nil || !strings.Contains(string(output), "guest boot did not complete") {
				t.Fatalf("boot timeout was not reported: %v: %s", err, output)
			}
			after, err := os.ReadFile(filepath.Join(root, "calls"))
			if err != nil || string(after) != string(calls) {
				t.Fatalf("boot timeout mutated policy: %q, err=%v", after, err)
			}
		})
	}
}

func TestDiskTmpReadinessDistinguishesReboot(t *testing.T) {
	for _, kind := range []domain.YardKind{domain.YardContainer, domain.YardVM} {
		for _, code := range []int{0, 1, 10} {
			incus := &testkit.Incus{Reconcile: ports.ReconcileState{InstanceFound: true,
				Instance: ports.InstanceInfo{Status: "Running"}}}
			runtime := Runtime{RepositoryRoot: "../../..", Incus: incus, Executor: incus,
				Yard: domain.Context{YardName: "minimal", YardKind: kind}, Profiles: []profile.Definition{}}
			for _, diagnostic := range []bool{false, true} {
				incus.ExecSteps = []testkit.IncusExecStep{{Result: ports.InstanceExecResult{ExitCode: code}}}
				ready, err := runtime.diskTmpReadiness(context.Background(), diagnostic)
				if ready != (code == 0) {
					t.Fatalf("kind=%s code=%d ready=%v", kind, code, ready)
				}
				var reboot diskTmpRebootError
				if errors.As(err, &reboot) != (code == 10 && diagnostic) {
					t.Fatalf("kind=%s code=%d diagnostic=%v err=%v", kind, code, diagnostic, err)
				}
				if err != nil {
					message, next := reboot.ActivationDiagnostic()
					if !strings.Contains(message, "active /tmp is memory-backed") || !strings.Contains(next, "yard -Y minimal stop") {
						t.Fatal(message, next)
					}
				}
			}
		}
	}
}

func TestDiskTmpPostApplyAndStoppedPolicy(t *testing.T) {
	incus := &testkit.Incus{Reconcile: ports.ReconcileState{InstanceFound: true,
		Instance: ports.InstanceInfo{Status: "Running"}},
		ExecSteps: []testkit.IncusExecStep{{Result: ports.InstanceExecResult{ExitCode: 10}}}}
	runtime := Runtime{RepositoryRoot: "../../..", Incus: incus, Executor: incus,
		Yard: domain.Context{YardName: "minimal"}}
	ready, err := runtime.VerifyStageWithDiagnostic(context.Background(), ports.ReconcileStageProvision)
	var diagnostic interface{ ActivationDiagnostic() (string, string) }
	if ready || !errors.As(err, &diagnostic) || !strings.Contains(err.Error(), "yard -Y minimal stop") {
		t.Fatalf("post-apply readiness lost diagnostic: ready=%v err=%v", ready, err)
	}
	incus.Reconcile.Instance = ports.InstanceInfo{Status: "Stopped", Config: map[string]string{
		"user.subyard.managed": "true", "user.subyard.initialized": "true", "user.subyard.desired_power": "stopped",
	}}
	for _, policy := range []string{"", "v1"} {
		incus.Reconcile.Instance.Config["user.subyard.disk_tmp"] = policy
		ready, err := runtime.diskTmpReadiness(context.Background(), false)
		if err != nil || ready != (policy == "v1") {
			t.Fatalf("stopped policy=%q ready=%v err=%v", policy, ready, err)
		}
	}
}
