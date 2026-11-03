package migration

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestRegisteredGuestTmpRequiresSeparateLateConsent(t *testing.T) {
	for _, scenario := range []string{"decline", "eof", "apply-fails", "transport-ten", "stopped-decline", "stopped-complete"} {
		t.Run(scenario, func(t *testing.T) {
			root := testkit.TempDir(t)
			if err := os.Mkdir(filepath.Join(root, "scripts"), 0700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(root, "scripts/guest-disk-tmp.sh"), []byte("policy source\n"), 0600)
			instance := ports.InstanceInfo{Project: "fixture", Name: "yard", Status: "Running", Config: map[string]string{"volatile.uuid": "uuid-original", "user.subyard.desired_power": "running"}}
			if scenario == "stopped-decline" || scenario == "stopped-complete" {
				instance.Status = "Stopped"
				instance.Config["user.subyard.desired_power"] = "stopped"
			}
			if scenario == "stopped-complete" {
				instance.Config["user.subyard.disk_tmp"] = "v1"
			}
			native := &testkit.Incus{Instances: map[string]ports.InstanceInfo{"fixture/yard": instance}, ExecSteps: []testkit.IncusExecStep{{}, {Result: ports.InstanceExecResult{ExitCode: 10}, Err: &ports.GuestExitError{Code: 10}}}}
			if scenario == "apply-fails" {
				native.ExecSteps = []testkit.IncusExecStep{{Result: ports.InstanceExecResult{ExitCode: 1}, Err: &ports.GuestExitError{Code: 1}}}
			}
			if scenario == "transport-ten" {
				native.ExecSteps[1].Err = errors.New("transport unavailable")
			}
			runtime := reconcileruntime.Runtime{RepositoryRoot: root, Incus: native, Executor: native, Yard: domain.Context{YardName: "default", IncusProject: "fixture", YardInstanceName: "yard"}}
			asks := 0
			err := migrateRegisteredDiskTmp(context.Background(), runtime, func(request domain.ConfirmationRequest) (bool, error) {
				asks++
				if request.Default != domain.ConfirmationDefaultNo || len(request.Consequences) == 0 {
					t.Fatal("late consent is not explicit")
				}
				if scenario == "eof" {
					return false, domain.ErrConfirmationRequired
				}
				return false, nil
			})
			expectedAsks := 1
			if scenario == "apply-fails" || scenario == "transport-ten" || scenario == "stopped-complete" {
				expectedAsks = 0
			}
			if asks != expectedAsks || len(native.PowerUpdates) != 0 || len(native.ConfigUpdates) != 0 {
				t.Fatalf("late boundary asks=%d power=%#v metadata=%#v", asks, native.PowerUpdates, native.ConfigUpdates)
			}
			if scenario == "stopped-complete" {
				if err != nil || len(native.ExecCalls) != 0 {
					t.Fatalf("completed stopped yard was touched: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("incomplete migration marked complete")
			}
			if strings.Contains(err.Error(), "%!w") {
				t.Fatal("nil cause was formatted as wrapped error", err)
			}
			if scenario == "decline" && (!errors.Is(err, domain.ErrOperationDeclined) || len(native.ExecCalls) != 2) {
				t.Fatalf("policy not applied before separate refusal: %v calls=%d", err, len(native.ExecCalls))
			}
			if scenario == "eof" && !errors.Is(err, domain.ErrConfirmationRequired) {
				t.Fatal(err)
			}
			if native.Instances["fixture/yard"].Config["volatile.uuid"] != "uuid-original" || native.Instances["fixture/yard"].Config["user.subyard.desired_power"] != instance.Config["user.subyard.desired_power"] {
				t.Fatal("migration changed identity or intent")
			}
		})
	}
}

type pendingTmpAssessment struct {
	*registeredNativeFixture
	step        testkit.IncusExecStep
	assessments int
	onlyInitial bool
}

func (n *pendingTmpAssessment) Exec(ctx context.Context, project, name string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	if request.Command[len(request.Command)-1] == "assess" {
		n.assessments++
		if n.onlyInitial && n.assessments > 1 {
			return ports.InstanceExecResult{}, errors.New("guest boot is not ready for a fresh safety assessment")
		}
		return n.step.Result, n.step.Err
	}
	return n.registeredNativeFixture.Exec(ctx, project, name, request)
}

func TestGuestTmpFreshnessDoesNotProbeGuestBeforeBootReadiness(t *testing.T) {
	_, owner, native, _ := pairedRegisteredOwners(t)
	check := &pendingTmpAssessment{registeredNativeFixture: native, onlyInitial: true}
	load := owner.LoadTargets
	owner.LoadTargets = func(ctx context.Context, view releasetransition.V2SettingsSnapshotView) ([]RegisteredYardTarget, error) {
		targets, err := load(ctx, view)
		for i := range targets {
			targets[i].Runtime.Executor = check
		}
		return targets, err
	}
	observed, err := owner.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
	if err != nil || observed.Converged {
		t.Fatalf("pre-journal assessment: %#v %v", observed, err)
	}
	if len(observed.Consequences) != 1 || !strings.Contains(observed.Consequences[0], "clear /tmp") || !strings.Contains(observed.Consequences[0], "stops and boots") {
		t.Error("policy future clearing is not disclosed", observed.Consequences)
	}
	if err := owner.Reconcile(context.Background(), releasetransition.ReleaseLinks{}); err != nil {
		t.Fatal("freshness guard probed boot readiness", err)
	}
	if check.assessments != 1 || native.grants != 1 {
		t.Fatalf("assessments=%d grants=%d", check.assessments, native.grants)
	}
}
func TestPendingGuestTmpAssessesSafetyBeforeAuthorization(t *testing.T) {
	for _, scenario := range []string{"disk", "memory", "unsafe", "transport", "mismatch", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			_, owner, native, _ := pairedRegisteredOwners(t)
			check := &pendingTmpAssessment{registeredNativeFixture: native}
			if scenario != "disk" {
				check.step.Result.ExitCode = 10
				check.step.Err = &ports.GuestExitError{Code: 10}
			}
			switch scenario {
			case "unsafe":
				check.step.Result.ExitCode = 1
				check.step.Err = &ports.GuestExitError{Code: 1}
			case "transport":
				check.step.Err = errors.New("transport unavailable")
			case "mismatch":
				check.step.Err = &ports.GuestExitError{Code: 1}
			case "cancelled":
				check.step.Err = errors.Join(context.Canceled, check.step.Err)
			}
			load := owner.LoadTargets
			owner.LoadTargets = func(ctx context.Context, view releasetransition.V2SettingsSnapshotView) ([]RegisteredYardTarget, error) {
				targets, err := load(ctx, view)
				for i := range targets {
					targets[i].Runtime.Executor = check
				}
				return targets, err
			}
			_, err := owner.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
			if (err == nil) != (scenario == "disk" || scenario == "memory") || check.assessments != 1 || native.grants != 0 {
				t.Fatalf("assessment: err=%v calls=%d grants=%d", err, check.assessments, native.grants)
			}
			for _, call := range native.ExecCalls {
				if call.Request.Command[len(call.Request.Command)-1] == "apply" {
					t.Fatal("inspection applied policy")
				}
			}
		})
	}
}

func TestPendingGuestTmpRefusalNamesYardAndGuestReason(t *testing.T) {
	guestRefusal := func(stderr string) testkit.IncusExecStep {
		return testkit.IncusExecStep{Result: ports.InstanceExecResult{ExitCode: 1, Stderr: []byte(stderr)}, Err: &ports.GuestExitError{Code: 1}}
	}
	refuse := func(step testkit.IncusExecStep) error {
		t.Helper()
		_, owner, native, _ := pairedRegisteredOwners(t)
		check := &pendingTmpAssessment{registeredNativeFixture: native, step: step}
		load := owner.LoadTargets
		owner.LoadTargets = func(ctx context.Context, view releasetransition.V2SettingsSnapshotView) ([]RegisteredYardTarget, error) {
			targets, err := load(ctx, view)
			for i := range targets {
				targets[i].Runtime.Executor = check
			}
			return targets, err
		}
		_, err := owner.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
		return err
	}
	diagnostic := func(err error) (string, string) {
		t.Helper()
		var refusal interface {
			ActivationDiagnostic() (string, string)
			MigrationRefusal() bool
		}
		if !errors.As(err, &refusal) || !refusal.MigrationRefusal() {
			t.Fatalf("guest verdict is not a migration refusal: %v", err)
		}
		return refusal.ActivationDiagnostic()
	}

	message, retry := diagnostic(refuse(guestRefusal("guest-disk-tmp: unsafe guest temporary policy directory: /usr/local/lib owner 1000:1000 mode 755\nguest-disk-tmp: foreign or unsafe guest tmp.mount policy\n")))
	for _, want := range []string{"yard default", "/usr/local/lib owner 1000:1000", "foreign or unsafe guest tmp.mount policy"} {
		if !strings.Contains(message, want) {
			t.Errorf("message %q lacks %q", message, want)
		}
	}
	if strings.Contains(retry, "install a compatible release") {
		t.Errorf("retry sends the operator to a release: %q", retry)
	}

	// Guest bytes outside this leaf's own closed vocabulary never reach the operator.
	hostile := "guest-disk-tmp: bad\x1b[31m\nguest-disk-tmp: run $(reboot); rm -rf /\nplain text\n"
	message, _ = diagnostic(refuse(guestRefusal(hostile)))
	if !strings.Contains(message, "yard default") || !strings.Contains(message, "guest exit 1") || strings.Contains(message, "reboot") || strings.Contains(message, "plain") {
		t.Fatalf("unsafe guest text crossed the boundary: %q", message)
	}

	many := ""
	for index := range 12 {
		many += "guest-disk-tmp: finding " + string(rune('a'+index)) + "\n"
	}
	message, _ = diagnostic(refuse(guestRefusal(many + many)))
	if !strings.Contains(message, "and 4 more") || strings.Contains(message, "finding i") {
		t.Fatalf("findings not bounded and deduplicated: %q", message)
	}

	for name, step := range map[string]testkit.IncusExecStep{
		"transport": {Result: ports.InstanceExecResult{ExitCode: 1}, Err: errors.New("transport unavailable")},
		"mismatch":  {Result: ports.InstanceExecResult{ExitCode: 2}, Err: &ports.GuestExitError{Code: 1}},
		"cancelled": {Result: ports.InstanceExecResult{ExitCode: 1}, Err: errors.Join(context.Canceled, &ports.GuestExitError{Code: 1})},
	} {
		t.Run(name, func(t *testing.T) {
			err := refuse(step)
			var refusal interface{ MigrationRefusal() bool }
			if err == nil || errors.As(err, &refusal) || !strings.Contains(err.Error(), "yard default") {
				t.Fatalf("failed read became a guest verdict: %v", err)
			}
		})
	}
}

// The real assess leaf and the real host filter share one vocabulary. Fixture paths
// stay relative, so findings keep the length they have on a guest and meet its bound.
func TestGuestTmpAssessFindingsSurviveHostFilter(t *testing.T) {
	source, err := os.ReadFile("../../scripts/guest-disk-tmp.sh")
	if err != nil {
		t.Fatal(err)
	}
	rewritten := regexp.MustCompile(`/tmp([/ "'\n)]|$)`).ReplaceAllString(string(source), "./tmp${1}")
	rewritten = strings.NewReplacer("/etc", "./etc", "/usr", "./usr").Replace(rewritten)
	assess := func(t *testing.T, setup func(string), environment ...string) (int, []byte) {
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
		testkit.WriteFile(t, filepath.Join(root, "policy.sh"), []byte(rewritten), 0700)
		for name, body := range map[string]string{
			"stat": `[ "${3:-}" != "${FOREIGN_PATH:-}" ] || OWNER=1000:1000
case "$2" in
  %u:%g) printf '%s\n' "${OWNER:-0:0}" ;;
  %a:%u:%g) printf '%s:%s\n' "$(/usr/bin/stat -c %a "$3")" "${OWNER:-0:0}" ;;
  *) exec /usr/bin/stat "$@" ;;
esac`,
			"findmnt":    `[ "${FINDMNT_FAIL:-0}" = 0 ] || exit 1; echo "${FILESYSTEM:-ext4}"`,
			"mountpoint": `exit "${MOUNT_EXIT:-32}"`,
			"systemctl": `[ "${SHOW_FAIL:-0}" = 0 ] || exit 1
[ "${OVERRIDE:-0}" = 0 ] || echo ./etc/systemd/system/subyard-tmp-shutdown.service.d/foreign.conf`,
		} {
			testkit.WriteFile(t, filepath.Join(root, "bin", name), []byte("#!/bin/sh\nset -eu\n"+body+"\n"), 0700)
		}
		if setup != nil {
			setup(root)
		}
		var stderr strings.Builder
		command := exec.Command("sh", filepath.Join(root, "policy.sh"), "assess")
		command.Dir, command.Stderr = root, &stderr
		command.Env = append(os.Environ(), "PATH="+filepath.Join(root, "bin")+":/usr/bin:/bin")
		command.Env = append(command.Env, environment...)
		code := 0
		if err := command.Run(); err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatal(err)
			}
			code = exit.ExitCode()
		}
		return code, []byte(stderr.String())
	}
	if code, stderr := assess(t, nil); code != 0 || len(stderr) != 0 {
		t.Fatalf("clean fixture: exit %d: %s", code, stderr)
	}
	symlink := func(target, path string) func(string) {
		return func(root string) {
			if err := os.Symlink(target, filepath.Join(root, path)); err != nil {
				t.Fatal(err)
			}
		}
	}
	mode := func(value os.FileMode) func(string) {
		return func(root string) {
			if err := os.Chmod(filepath.Join(root, "tmp"), value); err != nil {
				t.Fatal(err)
			}
		}
	}
	wants := "etc/systemd/system/sysinit.target.wants"
	for _, test := range []struct {
		name         string
		setup        func(string)
		environment  []string
		want         string
		needsNonRoot bool
	}{
		{name: "tmp owner", environment: []string{"FOREIGN_PATH=./tmp"}, want: "./tmp mode and owner are 1777:1000:1000 not 1777:0:0"},
		{name: "tmp mode", setup: mode(0755), want: "./tmp mode and owner are 755:0:0 not 1777:0:0"},
		{name: "tmp not writable", setup: mode(0555 | os.ModeSticky), want: "./tmp is not writable", needsNonRoot: true},
		{name: "tmp symlink", setup: func(root string) {
			if err := os.Remove(filepath.Join(root, "tmp")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("usr", filepath.Join(root, "tmp")); err != nil {
				t.Fatal(err)
			}
		}, want: "./tmp is not a real directory"},
		{name: "tmp.mount", setup: symlink("/foreign", "etc/systemd/system/tmp.mount"), want: "foreign or unsafe guest tmp.mount policy"},
		{name: "policy ancestor", environment: []string{"FOREIGN_PATH=./usr/local/lib"}, want: "unsafe guest temporary policy directory: ./usr/local/lib owner 1000:1000 mode"},
		{name: "policy file", setup: func(root string) {
			testkit.WriteFile(t, filepath.Join(root, "etc/subyard-guest-tmpfiles.conf"), []byte("foreign\n"), 0644)
		}, want: "foreign or unsafe guest temporary policy file: ./etc/subyard-guest-tmpfiles.conf"},
		{name: "unit inspection", environment: []string{"SHOW_FAIL=1"}, want: "cannot inspect guest temporary policy unit: subyard-tmp-shutdown.service"},
		{name: "unit override", environment: []string{"OVERRIDE=1"}, want: "foreign guest temporary policy unit override: subyard-tmp-shutdown.service"},
		{name: "wants directory", setup: func(root string) {
			if err := os.Mkdir(filepath.Join(root, wants), 0755); err != nil {
				t.Fatal(err)
			}
		}, environment: []string{"FOREIGN_PATH=./" + wants}, want: "unsafe guest temporary policy directory: ./" + wants + " owner 1000:1000 mode"},
		{name: "boot link", setup: func(root string) {
			if err := os.Mkdir(filepath.Join(root, wants), 0755); err != nil {
				t.Fatal(err)
			}
			symlink("/foreign", wants+"/subyard-tmp-boot.service")(root)
		}, want: "foreign guest temporary policy boot link: subyard-tmp-boot.service"},
		{name: "disposable directory", setup: func(root string) {
			testkit.WriteFile(t, filepath.Join(root, "tmp/subyard-disposable"), nil, 0600)
		}, want: "unsafe guest disposable temporary directory"},
		{name: "memory mount read", environment: []string{"FILESYSTEM=tmpfs", "MOUNT_EXIT=1"}, want: "cannot inspect the ./tmp mount: mountpoint exit 1"},
		{name: "disk mount", environment: []string{"MOUNT_EXIT=0"}, want: "disk-backed ./tmp is its own mount: mountpoint exit 0"},
		{name: "filesystem", environment: []string{"FILESYSTEM=unknown"}, want: "unsupported ./tmp filesystem: unknown"},
		{name: "filesystem read", environment: []string{"FINDMNT_FAIL=1"}, want: "cannot read the ./tmp filesystem"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.needsNonRoot && os.Getuid() == 0 {
				t.Skip("root writes through directory modes")
			}
			code, stderr := assess(t, test.setup, test.environment...)
			if code != 1 {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			findings := guestDiskTmpFindings(stderr)
			if reported := strings.Count(string(stderr), guestDiskTmpFindingPrefix); len(findings) != reported {
				t.Fatalf("host filter kept %d of %d findings: %q from:\n%s", len(findings), reported, findings, stderr)
			}
			message, _ := (&guestDiskTmpRefusal{yard: "default", exit: code, findings: findings}).ActivationDiagnostic()
			if !strings.Contains(message, test.want) || strings.Contains(message, "without a recognized reason") {
				t.Fatalf("operator reason lost: %q\nguest stderr:\n%s", message, stderr)
			}
		})
	}
}
