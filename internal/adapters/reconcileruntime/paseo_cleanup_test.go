package reconcileruntime

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

type paseoCleanupFixture struct {
	t       *testing.T
	root    string
	program string
}

type paseoCleanupResult struct {
	Fingerprint string   `json:"fingerprint"`
	Changed     bool     `json:"changed"`
	Steps       []string `json:"steps"`
	Code        string   `json:"code"`
}

func newPaseoCleanupFixture(t *testing.T) paseoCleanupFixture {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Fatal("Paseo cleanup tests require python3")
	}
	root := t.TempDir()
	source, err := os.ReadFile("../../../config/agents/paseo/cleanup.sh")
	if err != nil {
		t.Fatal(err)
	}
	program := strings.NewReplacer(
		"'/etc/systemd/system'", fmt.Sprintf("%q", root+"/etc/systemd/system"),
		"'/var/lib/subyard/paseo-ownership'", fmt.Sprintf("%q", root+"/var/lib/subyard/paseo-ownership"),
		"SAFE_ROOT = '/'", fmt.Sprintf("SAFE_ROOT = %q", root),
		"OWNER_UID = 0", fmt.Sprintf("OWNER_UID = %d", os.Getuid()),
	).Replace(string(source))
	f := paseoCleanupFixture{t: t, root: root, program: program}
	for _, dir := range []string{"etc/systemd/system", "var/lib/subyard", "bin", "srv/agents/paseo", "opt/subyard/paseo"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	f.write("active", "active")
	f.write("enabled", "enabled")
	f.write("reload", "no")
	f.write("etc/systemd/system/paseo.service", "[Service]\nExecStart=/opt/subyard/paseo/current/bin/paseo\n")
	f.write("srv/agents/paseo/auth.json", "synthetic-auth")
	f.write("srv/agents/paseo/history", "synthetic-history")
	f.write("opt/subyard/paseo/binary", "synthetic-binary")
	f.write("bin/systemctl", `#!/bin/sh
set -eu
case "$1" in
show)
  printf 'ActiveState=%s\nUnitFileState=%s\nFragmentPath=%s\nNeedDaemonReload=%s\nLoadState=loaded\n' "$(cat "$FIXTURE_ROOT/active")" "$(cat "$FIXTURE_ROOT/enabled")" "$FIXTURE_ROOT/etc/systemd/system/paseo.service" "$(cat "$FIXTURE_ROOT/reload")"
  ;;
*)
  printf '%s\n' "$1" >>"$FIXTURE_ROOT/calls"
  if [ -f "$FIXTURE_ROOT/fail-$1" ]; then
    printf 'synthetic-private-output\n' >&2
    exit 1
  fi
  case "$1" in
    stop) printf inactive >"$FIXTURE_ROOT/active" ;;
    disable) printf disabled >"$FIXTURE_ROOT/enabled" ;;
    daemon-reload) printf no >"$FIXTURE_ROOT/reload" ;;
    *) exit 1 ;;
  esac
  ;;
esac
`)
	if err := os.Chmod(root+"/bin/systemctl", 0o700); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f paseoCleanupFixture) write(path, content string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.root, path), []byte(content), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f paseoCleanupFixture) read(path string) string {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.root, path))
	if err != nil {
		f.t.Fatal(err)
	}
	return string(data)
}

func (f paseoCleanupFixture) run(mode, fingerprint, wantCode string) paseoCleanupResult {
	f.t.Helper()
	command := exec.Command("sh", "-eu", "-s", "--", mode, "dev", "1000", fingerprint)
	command.Stdin = strings.NewReader(f.program)
	command.Env = append(os.Environ(), "PATH="+f.root+"/bin:"+os.Getenv("PATH"), "FIXTURE_ROOT="+f.root)
	output, err := command.CombinedOutput()
	if (err != nil) != (wantCode != "") {
		f.t.Fatalf("cleanup %s: %v: %s", mode, err, output)
	}
	var result paseoCleanupResult
	if err := json.Unmarshal(output, &result); err != nil {
		f.t.Fatalf("invalid hook output: %v: %s", err, output)
	}
	if result.Code != wantCode {
		f.t.Fatalf("cleanup code %q, want %q", result.Code, wantCode)
	}
	if wantCode == "" && !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(result.Fingerprint) {
		f.t.Fatalf("invalid fingerprint %q", result.Fingerprint)
	}
	if strings.Contains(string(output), "synthetic-") {
		f.t.Fatalf("hook exposed fixture contents: %s", output)
	}
	return result
}

func TestPaseoCleanupPreservesDataAndRetiresUnit(t *testing.T) {
	f := newPaseoCleanupFixture(t)
	unit := f.read("etc/systemd/system/paseo.service")
	f.write("var/lib/subyard/paseo-ownership", "unknown legacy ownership")
	before := f.run("observe", "", "")
	if !before.Changed || len(before.Steps) < 2 {
		t.Fatal("missing explicit cleanup plan")
	}
	if after := f.run("observe", "", ""); after.Fingerprint != before.Fingerprint {
		t.Fatal("observation changed state")
	}
	if _, err := os.Stat(f.root + "/calls"); !os.IsNotExist(err) {
		t.Fatal("observation invoked a service mutation")
	}
	if after := f.run("apply", before.Fingerprint, ""); after.Changed {
		t.Fatal("cleanup did not converge")
	}
	if _, err := os.Lstat(f.root + "/etc/systemd/system/paseo.service"); !os.IsNotExist(err) {
		t.Fatal("live unit remained")
	}
	for path, want := range map[string]string{
		"etc/systemd/system/paseo.service.subyard-retired": unit,
		"var/lib/subyard/paseo-ownership":                  "unknown legacy ownership",
		"srv/agents/paseo/auth.json":                       "synthetic-auth",
		"srv/agents/paseo/history":                         "synthetic-history",
		"opt/subyard/paseo/binary":                         "synthetic-binary",
	} {
		if got := f.read(path); got != want {
			t.Fatalf("cleanup modified %s", path)
		}
	}
	wantCalls := "stop\ndisable\ndaemon-reload\n"
	if got := f.read("calls"); got != wantCalls {
		t.Fatalf("service operations %q", got)
	}
	after := f.run("observe", "", "")
	f.run("apply", after.Fingerprint, "")
	if got := f.read("calls"); got != wantCalls {
		t.Fatalf("converged retry mutated service: %q", got)
	}
}

func TestPaseoCleanupRejectsChangedStateBeforeMutation(t *testing.T) {
	for _, change := range []string{"unit-content", "unit-mode", "unit-inode", "receipt", "active", "enabled"} {
		t.Run(change, func(t *testing.T) {
			f := newPaseoCleanupFixture(t)
			before := f.run("observe", "", "")
			path := f.root + "/etc/systemd/system/paseo.service"
			switch change {
			case "unit-content":
				f.write("etc/systemd/system/paseo.service", "changed unit")
			case "unit-mode":
				if err := os.Chmod(path, 0o640); err != nil {
					t.Fatal(err)
				}
			case "unit-inode":
				f.write("etc/systemd/system/replacement", f.read("etc/systemd/system/paseo.service"))
				if err := os.Rename(f.root+"/etc/systemd/system/replacement", path); err != nil {
					t.Fatal(err)
				}
			case "receipt":
				f.write("var/lib/subyard/paseo-ownership", "new receipt")
			case "active":
				f.write("active", "inactive")
			case "enabled":
				f.write("enabled", "disabled")
			}
			f.run("apply", before.Fingerprint, "cleanup_stale")
			if _, err := os.Stat(f.root + "/calls"); !os.IsNotExist(err) {
				t.Fatal("stale cleanup mutated service")
			}
		})
	}
}

func TestPaseoCleanupRefusesUnsafeFiles(t *testing.T) {
	for _, unsafe := range []string{"symlink", "directory", "parent-symlink", "backup"} {
		t.Run(unsafe, func(t *testing.T) {
			f := newPaseoCleanupFixture(t)
			path := f.root + "/etc/systemd/system/paseo.service"
			code := "cleanup_unsafe_file"
			switch unsafe {
			case "symlink", "directory":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				var err error
				if unsafe == "symlink" {
					err = os.Symlink(f.root+"/srv/agents/paseo/auth.json", path)
				} else {
					err = os.Mkdir(path, 0o700)
				}
				if err != nil {
					t.Fatal(err)
				}
			case "parent-symlink":
				parent := filepath.Dir(path)
				if err := os.Rename(parent, parent+"-real"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(parent+"-real", parent); err != nil {
					t.Fatal(err)
				}
				code = "cleanup_unsafe_path"
			case "backup":
				f.write("etc/systemd/system/paseo.service.subyard-retired", "existing backup")
				code = "cleanup_backup_exists"
			}
			f.run("observe", "", code)
			f.run("apply", strings.Repeat("0", 64), code)
			if _, err := os.Stat(f.root + "/calls"); !os.IsNotExist(err) {
				t.Fatal("unsafe cleanup mutated service")
			}
		})
	}
}

func TestPaseoCleanupRecoversServiceFailures(t *testing.T) {
	for _, operation := range []string{"stop", "disable", "daemon-reload"} {
		t.Run(operation, func(t *testing.T) {
			f := newPaseoCleanupFixture(t)
			unit := f.read("etc/systemd/system/paseo.service")
			f.write("fail-"+operation, "")
			f.write("reload", "yes")
			before := f.run("observe", "", "")
			f.run("apply", before.Fingerprint, "cleanup_service_failed")
			path := "etc/systemd/system/paseo.service"
			if operation == "daemon-reload" {
				path += ".subyard-retired"
			}
			if f.read(path) != unit {
				t.Fatal("failed operation lost original unit")
			}
			if err := os.Remove(f.root + "/fail-" + operation); err != nil {
				t.Fatal(err)
			}
			retry := f.run("observe", "", "")
			if !retry.Changed {
				t.Fatal("interrupted cleanup reported converged")
			}
			if after := f.run("apply", retry.Fingerprint, ""); after.Changed {
				t.Fatal("retry did not converge")
			}
			if operation == "daemon-reload" && f.read("calls") != "stop\ndisable\ndaemon-reload\ndaemon-reload\n" {
				t.Fatal("backup-only recovery stopped a service")
			}
		})
	}
}

func TestPaseoCleanupAbsentUnitIsNoopAndModeFailsClosed(t *testing.T) {
	f := newPaseoCleanupFixture(t)
	if err := os.Remove(f.root + "/etc/systemd/system/paseo.service"); err != nil {
		t.Fatal(err)
	}
	before := f.run("observe", "", "")
	if before.Changed || len(before.Steps) != 0 {
		t.Fatal("absent unit needs cleanup")
	}
	f.run("apply", before.Fingerprint, "")
	f.run("unexpected", "", "cleanup_arguments_invalid")
	if _, err := os.Stat(f.root + "/calls"); !os.IsNotExist(err) {
		t.Fatal("absent local unit touched a vendor service")
	}
	if _, err := os.Stat(f.root + "/var/lib/subyard/paseo-ownership"); !os.IsNotExist(err) {
		t.Fatal("cleanup fabricated ownership")
	}
}
