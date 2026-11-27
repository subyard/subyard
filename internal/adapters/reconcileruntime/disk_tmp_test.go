package reconcileruntime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

func diskTmpFixtureScript(t *testing.T, root, source, name string) string {
	t.Helper()
	body, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	rewritten := regexp.MustCompile(`/tmp([/ "'\n)]|$)`).ReplaceAllString(string(body), root+"/tmp${1}")
	rewritten = strings.NewReplacer("/etc", root+"/etc", "/usr", root+"/usr", "/proc/", root+"/proc/").Replace(rewritten)
	script := filepath.Join(root, name)
	testkit.WriteFile(t, script, []byte(rewritten), 0700)
	return script
}

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
	script := diskTmpFixtureScript(t, root, "../../../scripts/guest-disk-tmp.sh", "policy.sh")
	for name, body := range map[string]string{
		"stat": `[ "${3:-}" != "${FOREIGN_PATH:-}" ] || OWNER=1000:1000
case "$2" in
  %u:%g) printf '%s\n' "${OWNER:-0:0}" ;;
  %a:%u:%g) printf '%s:%s\n' "$(/usr/bin/stat -c %a "$3")" "${OWNER:-0:0}" ;;
  *) exec /usr/bin/stat "$@" ;;
esac`,
		"findmnt": `filesystem=${FILESYSTEM:-ext4}
case " $* " in *" --uniq "*) filesystem=$(printf '%s\n' "$filesystem" | tail -n 1) ;; esac
printf '%s\n' "$filesystem"
[ "${FINDMNT_FAIL:-0}" = 0 ]`,
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
      *"-p LoadState"*) if [ -e "$FIXTURE_ROOT/etc/systemd/system/$2" ]; then echo loaded; else echo not-found; fi ;;
      *"-p TimeoutStartUSec"*|*"-p TimeoutStopUSec"*)
        deadline=30s; [ "$2" != subyard-tmp-age.service ] || deadline=15min
        echo "${TIMEOUT_OVERRIDE:-$deadline}" ;;
      *"-p TimeoutStartFailureMode"*|*"-p TimeoutStopFailureMode"*) echo kill ;;
      *"-p ExecStart"*|*"-p ExecStop"*)
        case "$2:$4" in
          subyard-tmp-shutdown.service:ExecStart) command="$FIXTURE_ROOT/usr/bin/true" ;;
          subyard-tmp-shutdown.service:ExecStop) command="$FIXTURE_ROOT/usr/local/lib/subyard-guest-tmp shutdown" ;;
          subyard-tmp-boot.service:ExecStart) command="$FIXTURE_ROOT/usr/local/lib/subyard-guest-tmp boot" ;;
          subyard-tmp-clean.service:ExecStart) command="$FIXTURE_ROOT/usr/local/lib/subyard-guest-tmp clean" ;;
          subyard-tmp-age.service:ExecStart) command="$FIXTURE_ROOT/usr/local/lib/subyard-guest-tmp-age" ;;
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
      target=sysinit.target; case "$unit" in *.timer) target=timers.target ;; esac
      mkdir -p "$FIXTURE_ROOT/etc/systemd/system/$target.wants"
      ln -sf "$FIXTURE_ROOT/etc/systemd/system/$unit" "$FIXTURE_ROOT/etc/systemd/system/$target.wants/$unit"
    done ;;
  is-active) [ "${INACTIVE:-}" != "$3" ] ;;
  daemon-reload|start) ;;
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
	if err != nil || strings.Contains(string(calls), "--remove") || strings.Contains(string(calls), "--boot") || strings.Contains(string(calls), "--clean") {
		t.Fatalf("live cleanup: %q %v", calls, err)
	}
	for _, filesystem := range []string{"tmpfs", "ramfs"} {
		output, err := run("check", "FILESYSTEM="+filesystem, "BOOT_RESULT=exit-code")
		var failure *exec.ExitError
		if !errors.As(err, &failure) || failure.ExitCode() != 10 {
			t.Fatalf("%s reboot: %v %s", filesystem, err, output)
		}
	}
	// A previous boot's cleanup result is not provisioning state: no apply can
	// change it, and runtime ageing removes what an interrupted cleanup left.
	for _, result := range []string{"exit-code", "timeout"} {
		if output, err := run("check", "BOOT_RESULT="+result); err != nil {
			t.Fatalf("boot cleanup %s blocked readiness: %v %s", result, err, output)
		}
	}
	for _, environment := range []string{"OWNER=1:0", "OVERRIDE=1", "MOUNT_EXIT=0", "MOUNT_EXIT=1", "FILESYSTEM=unknown", "INACTIVE=subyard-tmp-age.timer", "COMMAND_OVERRIDE=1", "TIMEOUT_OVERRIDE=infinity"} {
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

// findmnt --uniq skips overmounted entries; the final row models the visible
// filesystem. A failed query must refuse even if it emitted plausible output.
func TestDiskTmpStackedMountsUseVisibleFilesystem(t *testing.T) {
	for _, test := range []struct {
		name, filesystems, mountExit, queryFailure string
		want                                       int
	}{
		{"stacked-memory", "tmpfs\ntmpfs", "0", "0", 10},
		{"visible-memory", "ext4\ntmpfs", "0", "0", 10},
		{"visible-disk-root", "tmpfs\next4", "32", "0", 0},
		{"visible-disk-mount", "tmpfs\next4", "0", "0", 1},
		{"failed-query", "tmpfs\next4", "32", "1", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, _, run := diskTmpFixture(t)
			// Only /tmp is mounted; the disposable child stays an ordinary directory.
			testkit.WriteFile(t, filepath.Join(root, "bin/mountpoint"), []byte("#!/bin/sh\n"+
				`[ "$2" != "$FIXTURE_ROOT/tmp" ] || exit "${MOUNT_EXIT:-32}"`+"\nexit 32\n"), 0700)
			payload := filepath.Join(root, "tmp/active")
			testkit.WriteFile(t, payload, []byte("retain active workload data"), 0600)
			environment := []string{"FILESYSTEM=" + test.filesystems, "MOUNT_EXIT=" + test.mountExit, "FINDMNT_FAIL=" + test.queryFailure}
			assertExit := func(operation string, want int) []byte {
				t.Helper()
				output, err := run(operation, environment...)
				code := 0
				if err != nil {
					var exit *exec.ExitError
					if !errors.As(err, &exit) {
						t.Fatal(err)
					}
					code = exit.ExitCode()
				}
				if code != want {
					t.Fatalf("%s: exit %d, want %d: %s", operation, code, want, output)
				}
				return output
			}
			output := assertExit("assess", test.want)
			if test.name == "visible-disk-mount" && !strings.Contains(string(output), "disk-backed "+filepath.Join(root, "tmp")+" is its own mount") {
				t.Fatalf("wrong refusal for visible disk mount: %s", output)
			}
			if test.name == "failed-query" && !strings.Contains(string(output), "cannot read the "+filepath.Join(root, "tmp")+" filesystem") {
				t.Fatalf("query failure became a filesystem verdict: %s", output)
			}
			if _, err := os.Stat(filepath.Join(root, "calls")); !os.IsNotExist(err) {
				t.Fatalf("assessment wrote: %v", err)
			}
			if test.want == 10 {
				if output, err := run("apply", environment...); err != nil {
					t.Fatalf("live apply: %v: %s", err, output)
				}
			} else if output, err := run("apply"); err != nil {
				t.Fatalf("policy setup: %v: %s", err, output)
			}
			assertExit("check", test.want)
			if data, err := os.ReadFile(payload); err != nil || string(data) != "retain active workload data" {
				t.Fatalf("live data changed: %q %v", data, err)
			}
			// A native restart drops the transient stack; the masked mount and
			// installed cleanup policy then converge on the ordinary disk root.
			if output, err := run("check", "FILESYSTEM=ext4", "MOUNT_EXIT=32"); err != nil {
				t.Fatalf("after restart: %v: %s", err, output)
			}
		})
	}
}

func TestDiskTmpAssessmentIsReadOnlyAndRejectsForeignPolicy(t *testing.T) {
	root, _, run := diskTmpFixture(t)
	sentinel := filepath.Join(root, "tmp", "active")
	testkit.WriteFile(t, sentinel, []byte("retain active workload data"), 0600)
	for _, filesystem := range []string{"ext4", "tmpfs", "ramfs", "unknown"} {
		output, err := run("assess", "FILESYSTEM="+filesystem)
		expected := 0
		if filesystem == "tmpfs" || filesystem == "ramfs" {
			expected = 10
		}
		if filesystem == "unknown" {
			expected = 1
		}
		code := 0
		var exit *exec.ExitError
		if err != nil {
			if !errors.As(err, &exit) {
				t.Fatal(err)
			}
			code = exit.ExitCode()
		}
		if code != expected {
			t.Fatalf("%s: exit %d want %d: %s", filesystem, code, expected, output)
		}
	}
	for _, relative := range []string{"calls", "count", "etc/subyard-guest-tmpfiles.conf", "etc/systemd/system/tmp.mount"} {
		if _, err := os.Lstat(filepath.Join(root, relative)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("assessment wrote or waited for boot", relative, err)
		}
	}
	for _, relative := range []string{"etc/systemd/system/tmp.mount", "etc/subyard-guest-tmpfiles.conf"} {
		path := filepath.Join(root, relative)
		testkit.WriteFile(t, path, []byte("foreign policy\n"), 0644)
		if output, err := run("assess"); err == nil {
			t.Fatalf("foreign %s accepted: %s", relative, output)
		}
		if payload, _ := os.ReadFile(path); string(payload) != "foreign policy\n" {
			t.Fatal("foreign policy changed")
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	if payload, err := os.ReadFile(sentinel); err != nil || string(payload) != "retain active workload data" {
		t.Fatal("assessment changed active data", err)
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

// Rolling back to the published release keeps its policy working: the files it
// owns are unchanged and it neither refuses nor removes the additive ageing policy.
func TestDiskTmpPolicyRollsBackToPublishedRelease(t *testing.T) {
	root, _, run := diskTmpFixture(t)
	published := diskTmpFixtureScript(t, root, "testdata/v0.32.0-guest-disk-tmp.sh", "published.sh")
	runPublished := func(mode string) ([]byte, error) {
		command := exec.Command("sh", published, mode)
		command.Env = append(os.Environ(), "PATH="+filepath.Join(root, "bin")+":/usr/bin:/bin", "FIXTURE_ROOT="+root)
		return command.CombinedOutput()
	}
	if output, err := runPublished("apply"); err != nil {
		t.Fatalf("published apply: %v: %s", err, output)
	}
	// Units that are not installed are reported as such, not as overrides.
	output, err := run("check")
	findings := GuestDiskTmpFindings(output)
	if err == nil || !slices.Contains(findings, "guest temporary policy unit is not loaded: subyard-tmp-age.service") ||
		strings.Contains(string(output), "overridden") || strings.Contains(string(output), "setting differs") || len(findings) > 8 {
		t.Fatalf("published policy satisfied the ageing policy: %v: %s", err, output)
	}
	// Fixture paths exceed the host filter's length, so read the guest output.
	if strings.Count(string(output), "guest temporary policy file missing: ") != 4 {
		t.Fatalf("missing ageing files not named: %s", output)
	}
	if output, err := run("apply"); err != nil {
		t.Fatalf("upgrade apply: %v: %s", err, output)
	}
	if output, err := run("check"); err != nil {
		t.Fatalf("upgrade check: %v: %s", err, output)
	}
	for _, mode := range []string{"assess", "apply", "check"} {
		if output, err := runPublished(mode); err != nil {
			t.Fatalf("published %s after upgrade: %v: %s", mode, err, output)
		}
	}
	if output, err := run("check"); err != nil {
		t.Fatalf("check after published apply: %v: %s", err, output)
	}
}

func TestDiskTmpCheckReportsEveryIndependentFinding(t *testing.T) {
	_, _, run := diskTmpFixture(t)
	if output, err := run("apply"); err != nil {
		t.Fatalf("apply: %v: %s", err, output)
	}
	output, err := run("check", "OVERRIDE=1", "TIMEOUT_OVERRIDE=infinity", "INACTIVE=subyard-tmp-age.timer", "MOUNT_EXIT=0")
	var failure *exec.ExitError
	if !errors.As(err, &failure) || failure.ExitCode() != 1 {
		t.Fatalf("check: %v %s", err, output)
	}
	findings := GuestDiskTmpFindings(output)
	for _, want := range []string{
		"foreign guest temporary policy unit override: subyard-tmp-age.timer",
		"guest temporary policy unit setting differs: subyard-tmp-age.service TimeoutStartUSec",
		"guest temporary policy unit setting differs: subyard-tmp-boot.service TimeoutStartUSec",
		"guest temporary policy unit is not active: subyard-tmp-age.timer",
		"tmp is its own mount",
	} {
		if !slices.ContainsFunc(findings, func(finding string) bool { return strings.HasSuffix(finding, want) }) {
			t.Errorf("finding %q missing from %q", want, findings)
		}
	}
	if reported := strings.Count(string(output), "guest-disk-tmp: "); reported != len(findings) {
		t.Fatalf("host filter kept %d of %d findings:\n%s", len(findings), reported, output)
	}
	// A memory-backed root with another finding is not merely a pending reboot.
	output, err = run("check", "FILESYSTEM=tmpfs", "INACTIVE=subyard-tmp-age.timer")
	if !errors.As(err, &failure) || failure.ExitCode() != 1 ||
		!strings.Contains(string(output), "tmp is memory-backed and needs a yard restart") {
		t.Fatalf("tmpfs with a finding: %v %s", err, output)
	}
}

// Runtime ageing keeps every top-level entry a live process references through
// its working directory, open files or mappings, and ages the rest natively.
func TestDiskTmpAgeingKeepsReferencedEntries(t *testing.T) {
	root, _, run := diskTmpFixture(t)
	if output, err := run("apply"); err != nil {
		t.Fatalf("apply: %v: %s", err, output)
	}
	tmp := filepath.Join(root, "tmp")
	// Native tmpfiles cannot see backdated change times, so the wrapper ages
	// this fixture by access and modification time only.
	testkit.WriteFile(t, filepath.Join(root, "bin", "systemd-tmpfiles"), []byte(`#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$FIXTURE_ROOT/calls"
if [ "$1" = --cat-config ]; then cat "$FIXTURE_ROOT/vendor.conf"; exit; fi
[ "$1" = --clean ] || exit 0
shift
for config do sed 's/ 2d$/ aAmM:2d/' "$config" > "$config.native"; set -- "$@" "$config.native"; shift; done
exec /usr/bin/systemd-tmpfiles --clean "$@"
`), 0o700)
	// Packages may declare their own /tmp exclusions; tmpfiles skips the whole
	// subtree of a path with its own line, so either type keeps its contents.
	testkit.WriteFile(t, filepath.Join(root, "vendor.conf"), []byte("# vendor\nx "+tmp+"/podman-run-*\nX\t"+tmp+"/vendor  1777 - - -\n"+
		"x /var/tmp/podman-run-*\nD! "+tmp+"/.X11-unix 1777 root root 10d\n"), 0o644)
	// Names are bytes whatever the service locale; non-ASCII bytes widen to "?".
	weird, accented, invalid := "we ird*\n%b", "caf\u00e9", "a\xffb"
	entries := []string{"stale/file", "stale-file", "cwd/file", "open/file", "open/sibling", weird + "/file", accented + "/file", invalid + "/file",
		"mapped\nlibrary/lib.so", "podman-run-1/file", "vendor/old", "sockets/listen", "fresh/file", "subyard-disposable/old"}
	for _, entry := range entries {
		path := filepath.Join(tmp, entry)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, path, []byte("data"), 0o600)
	}
	past := time.Now().Add(-72 * time.Hour)
	for _, entry := range entries {
		if entry == "fresh/file" {
			continue
		}
		for _, path := range []string{filepath.Join(tmp, entry), filepath.Dir(filepath.Join(tmp, entry))} {
			if path != tmp {
				if err := os.Chtimes(path, past, past); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	for link, target := range map[string]string{
		"proc/1/cwd": "/", "proc/1/root": "/",
		"proc/20/cwd": filepath.Join(tmp, "cwd"), "proc/20/fd/3": filepath.Join(tmp, "open/file") + " (deleted)",
		"proc/21/fd/4": filepath.Join(tmp, weird, "file"), "proc/21/fd/5": filepath.Join(tmp, invalid, "file"),
		"proc/22/exe": "/usr/bin/true", "proc/22/cwd": filepath.Join(tmp, accented), "proc/self": "22",
		"proc/25/ns/net": "net:[2]", "proc/24/ns/net": "net:[2]", "proc/26/ns/net": "net:[2]",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, link)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	// Mappings name their file in text, escaping a newline as \012.
	if err := os.MkdirAll(filepath.Join(root, "proc/23"), 0o755); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "proc/23/maps"), []byte(
		"7f00-7f10 r-xp 00000000 08:01 1234                       "+filepath.Join(tmp, `mapped\012library`, "lib.so")+" (deleted)\n"+
			"7f10-7f20 r--p 00000000 08:01 1235                       /usr/lib/libc.so.6\n"), 0o600)
	// Each network namespace has its own socket table, read through any process
	// still in it: process 24 exits before its table is read.
	if err := os.MkdirAll(filepath.Join(root, "proc/25/net"), 0o755); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "proc/25/net/unix"), []byte("Num       RefCount Protocol Flags    Type St Inode Path\n"+
		"0000000000000000: 00000002 00000000 00010000 0001 01  1234 "+filepath.Join(tmp, "sockets/listen")+"\n"+
		"0000000000000001: 00000002 00000000 00010000 0001 01 12350 @abstract\n"), 0o644)
	runtimeDirectory := filepath.Join(root, "run")
	if err := os.Mkdir(runtimeDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	age := func() ([]byte, error) {
		command := exec.Command("sh", filepath.Join(root, "usr/local/lib/subyard-guest-tmp-age"))
		command.Env = append(os.Environ(), "PATH="+filepath.Join(root, "bin")+":/usr/bin:/bin", "FIXTURE_ROOT="+root, "RUNTIME_DIRECTORY="+runtimeDirectory,
			"LC_ALL=", "LANG=C.UTF-8")
		return command.CombinedOutput()
	}
	// Without the process view nothing ages.
	if err := os.Rename(filepath.Join(root, "proc/1"), filepath.Join(root, "proc/hidden")); err != nil {
		t.Fatal(err)
	}
	if output, err := age(); err == nil || !strings.Contains(string(output), "cannot inspect process references") {
		t.Fatalf("ageing without a process view: %v %s", err, output)
	}
	if _, err := os.Stat(filepath.Join(tmp, "stale/file")); err != nil {
		t.Fatalf("ageing removed data without a process view: %v", err)
	}
	if err := os.Rename(filepath.Join(root, "proc/hidden"), filepath.Join(root, "proc/1")); err != nil {
		t.Fatal(err)
	}
	// A process whose references cannot be read stops ageing.
	if os.Getuid() != 0 {
		unreadable := filepath.Join(root, "proc/24/fd")
		if err := os.MkdirAll(unreadable, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(unreadable, 0); err != nil {
			t.Fatal(err)
		}
		if output, err := age(); err == nil || !strings.Contains(string(output), "cannot inspect process references") {
			t.Fatalf("ageing with an unreadable process: %v %s", err, output)
		}
		if err := os.Chmod(unreadable, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(tmp, "stale/file")); err != nil {
			t.Fatalf("ageing removed data with an unreadable process: %v", err)
		}
	}
	if output, err := age(); err != nil {
		t.Fatalf("ageing: %v %s", err, output)
	}
	for _, entry := range []string{"stale/file", "stale", "stale-file"} {
		if _, err := os.Lstat(filepath.Join(tmp, entry)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("unreferenced old entry retained: %s %v", entry, err)
		}
	}
	for _, entry := range []string{"cwd/file", "open/file", "open/sibling", weird + "/file", accented + "/file", invalid + "/file",
		"mapped\nlibrary/lib.so", "podman-run-1/file", "vendor/old", "sockets/listen", "fresh/file", "subyard-disposable/old"} {
		if _, err := os.Lstat(filepath.Join(tmp, entry)); err != nil {
			t.Errorf("referenced, fresh or separately owned entry removed: %q %v", entry, err)
		}
	}
	if info, err := os.Stat(tmp); err != nil || info.Mode().Perm() != 0o777 || info.Mode()&os.ModeSticky == 0 {
		t.Fatalf("ageing changed the root: %v %v", info, err)
	}
	exclusions, err := os.ReadFile(filepath.Join(runtimeDirectory, "in-use.conf"))
	if err != nil {
		t.Fatal(err)
	}
	want := "x " + tmp + "/a?b\nx " + tmp + "/caf??\nx " + tmp + "/cwd\nx " + tmp + "/mapped*\nx " + tmp + "/open\nx " + tmp + "/sockets*\nx " + tmp + "/we?ird???b\n" +
		"x " + tmp + "/podman-run-*\nX " + tmp + "/vendor\nx " + tmp + "/.X11-unix\n"
	if string(exclusions) != want {
		t.Fatalf("exclusions:\n got %q\nwant %q", exclusions, want)
	}
	testkit.WriteFile(t, filepath.Join(root, "etc/subyard-guest-tmp-age.conf"), []byte("e /tmp - - - 0\n"), 0o644)
	if output, err := age(); err == nil || !strings.Contains(string(output), "foreign policy content") {
		t.Fatalf("foreign ageing policy accepted: %v %s", err, output)
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

func TestDiskTmpPendingDistinguishesReboot(t *testing.T) {
	for _, kind := range []domain.YardKind{domain.YardContainer, domain.YardVM} {
		for _, code := range []int{0, 1, 10} {
			incus := &testkit.Incus{}
			runtime := Runtime{RepositoryRoot: "../../..", Incus: incus, Executor: incus,
				Yard: domain.Context{YardName: "minimal", YardKind: kind}, Profiles: []profile.Definition{}}
			incus.ExecSteps = []testkit.IncusExecStep{{Result: ports.InstanceExecResult{ExitCode: code}}}
			pending, err := runtime.diskTmpPending(context.Background())
			if err != nil || (pending == nil) != (code == 0) {
				t.Fatalf("kind=%s code=%d pending=%v err=%v", kind, code, pending, err)
			}
			var reboot diskTmpRebootError
			if errors.As(pending, &reboot) != (code == 10) {
				t.Fatalf("kind=%s code=%d pending=%v", kind, code, pending)
			}
			if code == 10 {
				message, next := reboot.ActivationDiagnostic()
				if !strings.Contains(message, "active /tmp is memory-backed") || !strings.Contains(next, "yard -Y minimal stop") {
					t.Fatal(message, next)
				}
			}
		}
	}
}

func TestDiskTmpAssessmentReportsEveryIndependentFinding(t *testing.T) {
	root, _, run := diskTmpFixture(t)
	policy := filepath.Join(root, "etc/subyard-guest-tmpfiles.conf")
	testkit.WriteFile(t, policy, []byte("foreign\n"), 0644)
	if err := os.Symlink("/foreign", filepath.Join(root, "etc/systemd/system/tmp.mount")); err != nil {
		t.Fatal(err)
	}
	environment := []string{"FOREIGN_PATH=" + filepath.Join(root, "usr/local/lib"), "FILESYSTEM=unknown"}
	want := []string{
		"foreign or unsafe guest tmp.mount policy",
		"unsafe guest temporary policy directory: " + filepath.Join(root, "usr/local/lib") + " owner 1000:1000",
		"foreign or unsafe guest temporary policy file: " + policy,
		"unsupported " + filepath.Join(root, "tmp") + " filesystem: unknown",
	}
	output, err := run("assess", environment...)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("assessment: %v: %s", err, output)
	}
	position := 0
	for _, finding := range want {
		index := strings.Index(string(output[position:]), "guest-disk-tmp: "+finding)
		if index < 0 {
			t.Fatalf("finding %q missing or out of order:\n%s", finding, output)
		}
		position += index + len(finding)
	}
	if again, _ := run("assess", environment...); string(again) != string(output) {
		t.Fatalf("finding order is unstable:\n%s\n%s", output, again)
	}
	if data, _ := os.ReadFile(policy); string(data) != "foreign\n" {
		t.Fatal("assessment changed foreign policy")
	}
	if _, err := os.Lstat(filepath.Join(root, "calls")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("assessment ran a host-changing command", err)
	}
	// A memory-backed guest with a real finding is refused, not offered a restart.
	if output, err := run("assess", "FOREIGN_PATH="+filepath.Join(root, "usr/local/lib"), "FILESYSTEM=tmpfs"); !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("unsafe memory-backed guest: %v: %s", err, output)
	}
}
