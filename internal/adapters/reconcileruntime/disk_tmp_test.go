package reconcileruntime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

// The shell leaf arrives over stdin in production. Rewrite only its fixed guest
// paths into a private filesystem fixture; no command can publish host policy.
func diskTmpFixture(t *testing.T) (string, string, func(string, ...string) ([]byte, error)) {
	t.Helper()
	root := testkit.TempDir(t)
	for _, directory := range []string{"bin", "tmp", "etc/systemd/system", "usr/local/lib"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(root, "tmp"), 0777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile("../../../scripts/guest-disk-tmp.sh")
	if err != nil {
		t.Fatal(err)
	}
	rewritten := regexp.MustCompile(`/tmp([/ "'\n)]|$)`).ReplaceAllString(string(source), root+"/tmp${1}")
	rewritten = strings.NewReplacer("/etc", root+"/etc", "/usr", root+"/usr").Replace(rewritten)
	script := filepath.Join(root, "policy.sh")
	testkit.WriteFile(t, script, []byte(rewritten), 0700)
	for name, body := range map[string]string{
		"stat": `case "$2" in
  %u:%g) printf '%s\n' "${OWNER:-0:0}" ;;
  %a:%u:%g) printf '%s:%s\n' "$(/usr/bin/stat -c %a "$3")" "${OWNER:-0:0}" ;;
  *) exec /usr/bin/stat "$@" ;;
esac`,
		"findmnt":    `echo "${FILESYSTEM:-ext4}"`,
		"mountpoint": `exit "${MOUNT_EXIT:-32}"`,
		"systemd-tmpfiles": `printf '%s\n' "$*" >> "$FIXTURE_ROOT/calls"
case "$1" in --create) mkdir -p "$FIXTURE_ROOT/tmp/subyard-disposable"; chmod 1777 "$FIXTURE_ROOT/tmp/subyard-disposable" ;; esac`,
		"sleep": `:`,
		"systemctl": `case "$1" in
  is-system-running)
    count=$(cat "$FIXTURE_ROOT/count" 2>/dev/null || echo 0); count=$((count + 1)); echo "$count" > "$FIXTURE_ROOT/count"
    if [ "${BOOT_WAIT:-0}" = 1 ] && [ "$count" -lt 3 ]; then echo starting; exit 1; fi
    echo "${SYSTEM_STATE:-running}"; [ "${SYSTEM_STATE:-running}" = running ] ;;
  show)
    case "$*" in
      *"-p Result"*) echo "${BOOT_RESULT:-success}" ;;
      *"-p TimeoutStartUSec"*|*"-p TimeoutStopUSec"*) echo "${TIMEOUT_OVERRIDE:-30s}" ;;
      *"-p TimeoutStartFailureMode"*|*"-p TimeoutStopFailureMode"*) echo kill ;;
      *"-p ExecStart"*|*"-p ExecStop"*)
        case "$2:$4" in
          subyard-tmp-shutdown.service:ExecStart) command="$FIXTURE_ROOT/usr/bin/true" ;;
          subyard-tmp-shutdown.service:ExecStop) command="$FIXTURE_ROOT/usr/local/lib/subyard-guest-tmp shutdown" ;;
          subyard-tmp-boot.service:ExecStart) command="$FIXTURE_ROOT/usr/local/lib/subyard-guest-tmp boot" ;;
          subyard-tmp-clean.service:ExecStart) command="$FIXTURE_ROOT/usr/local/lib/subyard-guest-tmp clean" ;;
          *) exit 90 ;;
        esac
        [ "${COMMAND_OVERRIDE:-0}" = 0 ] || command=/usr/bin/false
        echo "{ path=${command%% *} ; argv[]=$command ; ignore_errors=no ; pid=0 ; }" ;;
      *) case "${OVERRIDE:-0}" in 1) echo /etc/systemd/system/subyard-tmp-shutdown.service.d/foreign.conf ;; 2) echo /usr/lib/systemd/system/service.d/vendor.conf ;; esac ;;
    esac ;;
  mask) ln -sf /dev/null "$FIXTURE_ROOT/etc/systemd/system/tmp.mount" ;;
  enable)
    shift
    for unit do
      target=sysinit.target; [ "$unit" != subyard-tmp-clean.timer ] || target=timers.target
      mkdir -p "$FIXTURE_ROOT/etc/systemd/system/$target.wants"
      ln -sf "$FIXTURE_ROOT/etc/systemd/system/$unit" "$FIXTURE_ROOT/etc/systemd/system/$target.wants/$unit"
    done ;;
  daemon-reload|start|is-active) ;;
  *) exit 90 ;;
esac`,
	} {
		testkit.WriteFile(t, filepath.Join(root, "bin", name), []byte("#!/bin/sh\nset -eu\n"+body+"\n"), 0700)
	}
	run := func(mode string, environment ...string) ([]byte, error) {
		command := exec.Command("sh", script, mode)
		command.Env = append(os.Environ(), "PATH="+filepath.Join(root, "bin")+":/usr/bin:/bin", "FIXTURE_ROOT="+root)
		command.Env = append(command.Env, environment...)
		return command.CombinedOutput()
	}
	return root, script, run
}

func TestDiskTmpGuestPolicy(t *testing.T) {
	root, _, run := diskTmpFixture(t)
	payload := filepath.Join(root, "tmp", ".active")
	testkit.WriteFile(t, payload, []byte("keep"), 0600)
	for range 2 {
		if output, err := run("apply"); err != nil {
			t.Fatalf("apply: %v: %s", err, output)
		}
		if output, err := run("check"); err != nil {
			t.Fatalf("check: %v: %s", err, output)
		}
	}
	if data, err := os.ReadFile(payload); err != nil || string(data) != "keep" {
		t.Fatalf("live data changed: %q %v", data, err)
	}
	calls, err := os.ReadFile(filepath.Join(root, "calls"))
	if err != nil || strings.Contains(string(calls), "--remove") || strings.Contains(string(calls), "--boot") {
		t.Fatalf("live cleanup: %q %v", calls, err)
	}
	for _, filesystem := range []string{"tmpfs", "ramfs"} {
		output, err := run("check", "FILESYSTEM="+filesystem, "BOOT_RESULT=exit-code")
		var failure *exec.ExitError
		if !errors.As(err, &failure) || failure.ExitCode() != 10 {
			t.Fatalf("%s reboot: %v %s", filesystem, err, output)
		}
	}
	for _, environment := range []string{"OWNER=1:0", "OVERRIDE=1", "MOUNT_EXIT=0", "MOUNT_EXIT=1", "FILESYSTEM=unknown", "BOOT_RESULT=exit-code", "COMMAND_OVERRIDE=1", "TIMEOUT_OVERRIDE=infinity"} {
		if output, err := run("check", environment); err == nil {
			t.Fatalf("unsafe check accepted %s: %s", environment, output)
		}
	}
	if output, err := run("apply", "OVERRIDE=2"); err != nil {
		t.Fatalf("vendor dropin refused: %v %s", err, output)
	}
	policy := filepath.Join(root, "etc/subyard-guest-tmpfiles.conf")
	if err := os.Chmod(policy, 0666); err != nil {
		t.Fatal(err)
	}
	if output, err := run("apply"); err == nil {
		t.Fatalf("unsafe policy accepted: %s", output)
	}
	if err := os.Chmod(policy, 0644); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, policy, []byte("foreign\n"), 0644)
	if output, err := run("apply"); err == nil {
		t.Fatalf("foreign policy replaced: %s", output)
	}
	if data, _ := os.ReadFile(policy); string(data) != "foreign\n" {
		t.Fatal("foreign policy was changed")
	}
}

func TestDiskTmpPolicyWaitsForGuestBoot(t *testing.T) {
	for _, completed := range []string{"running", "degraded"} {
		t.Run(completed, func(t *testing.T) {
			root, _, run := diskTmpFixture(t)
			if output, err := run("apply", "BOOT_WAIT=1", "SYSTEM_STATE="+completed); err != nil {
				t.Fatalf("boot: %v %s", err, output)
			}
			if data, _ := os.ReadFile(filepath.Join(root, "count")); string(data) != "3\n" {
				t.Fatalf("boot was not awaited: %q", data)
			}
			if output, err := run("apply", "SYSTEM_STATE=maintenance"); err == nil || !strings.Contains(string(output), "state=maintenance") {
				t.Fatalf("terminal boot: %v %s", err, output)
			}
			testkit.WriteFile(t, filepath.Join(root, "bin/timeout"), []byte("#!/bin/sh\nexit 124\n"), 0700)
			if output, err := run("apply"); err == nil || !strings.Contains(string(output), "guest boot did not complete") {
				t.Fatalf("boot deadline: %v %s", err, output)
			}
		})
	}
}

func TestDiskTmpCleanupRejectsManualShutdownAndUnsafeRoot(t *testing.T) {
	root, _, run := diskTmpFixture(t)
	if output, err := run("apply"); err != nil {
		t.Fatalf("apply: %v %s", err, output)
	}
	helper := filepath.Join(root, "usr/local/lib/subyard-guest-tmp")
	invoke := func(mode string, environment ...string) ([]byte, error) {
		command := exec.Command("sh", helper, mode)
		command.Env = append(os.Environ(), "PATH="+filepath.Join(root, "bin")+":/usr/bin:/bin", "FIXTURE_ROOT="+root)
		command.Env = append(command.Env, environment...)
		return command.CombinedOutput()
	}
	if output, err := invoke("shutdown"); err == nil || !strings.Contains(string(output), "outside native shutdown") {
		t.Fatalf("manual shutdown: %v %s", err, output)
	}
	if output, err := invoke("shutdown", "SYSTEM_STATE=stopping"); err != nil {
		t.Fatalf("native shutdown: %v %s", err, output)
	}
	if output, err := invoke("clean", "MOUNT_EXIT=0"); err == nil || !strings.Contains(string(output), "root is mounted") {
		t.Fatalf("mounted root: %v %s", err, output)
	}
	if output, err := invoke("clean", "MOUNT_EXIT=1"); err == nil {
		t.Fatalf("unknown mount status: %s", output)
	}
}

func TestDiskTmpNativeTmpfiles(t *testing.T) {
	root := testkit.TempDir(t)
	tmp := filepath.Join(root, "tmp")
	leaf := filepath.Join(tmp, "subyard-disposable")
	if err := os.MkdirAll(leaf, 0700); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(tmp)
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, "tmpfiles.conf")
	testkit.WriteFile(t, config, []byte("D! "+tmp+" 1777 - - -\nD "+leaf+" 1777 - - mM:1d\n"), 0600)
	for _, p := range []string{filepath.Join(tmp, ".hidden"), filepath.Join(leaf, "payload")} {
		testkit.WriteFile(t, p, []byte("remove"), 0600)
	}
	run := func(arguments ...string) {
		t.Helper()
		command := exec.Command("systemd-tmpfiles", append(arguments, config)...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("native tmpfiles: %v %s", err, output)
		}
	}
	run("--remove", "--create", "--boot")
	for _, p := range []string{filepath.Join(tmp, ".hidden"), filepath.Join(leaf, "payload")} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("payload retained: %s %v", p, err)
		}
	}
	after, err := os.Stat(tmp)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("root changed: %v", err)
	}
	old, fresh, unknown := filepath.Join(leaf, "old"), filepath.Join(leaf, "fresh"), filepath.Join(tmp, "unknown")
	for _, p := range []string{old, fresh, unknown} {
		testkit.WriteFile(t, p, []byte("data"), 0600)
	}
	past := time.Now().Add(-48 * time.Hour)
	for _, p := range []string{old, unknown} {
		if err := os.Chtimes(p, past, past); err != nil {
			t.Fatal(err)
		}
	}
	run("--clean", "--prefix="+leaf)
	if _, err := os.Lstat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old disposable retained: %v", err)
	}
	for _, p := range []string{fresh, unknown} {
		if _, err := os.Stat(p); err != nil {
			t.Fatal(err)
		}
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
