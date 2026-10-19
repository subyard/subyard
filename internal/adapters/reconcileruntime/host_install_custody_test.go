package reconcileruntime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
)

func TestHostInstallerRequiresOriginalOwnerDescriptorBeforeEffects(t *testing.T) {
	home := testkit.TempDir(t)
	_, release, err := uninstall.OwnerLock(context.Background(), home, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	owner, err := uninstall.BorrowOwnerLock(home)
	if err != nil || owner == nil {
		t.Fatal("missing original owner", err)
	}
	defer owner.Close()
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	source, err := filepath.Abs("../../../scripts/lib/native-install-custody.py")
	if err != nil {
		t.Fatal(err)
	}
	resource := filepath.Join(home, "must-remain-absent", "engine")
	command := func(input *os.File) *exec.Cmd {
		cmd := exec.Command("python3", "-B", source, "owner-barrier", "host-power", resource)
		cmd.Env = append(os.Environ(), "SUBYARD_NATIVE_OWNER_STDIN=1", "SUBYARD_USER="+account.Username, "SUBYARD_HOME="+home)
		cmd.Stdin = input
		return cmd
	}
	if output, err := command(owner).CombinedOutput(); err != nil {
		t.Fatal(err, string(output))
	}
	library := exec.Command("bash", "-c", `set -euo pipefail
SCRIPT_DIR=$1
die() { exit 2; }
. "$SCRIPT_DIR/lib/native-install-custody.sh"
subyard_install_custody host-power "$2" "$2/engine" "$2/unit"
subyard_install_custody_verify host-power "$2"`, "native-host-test", filepath.Dir(filepath.Dir(source)), home)
	library.Env = command(owner).Env
	library.Stdin = owner
	if output, err := library.CombinedOutput(); err != nil {
		t.Fatal("actual installer lock loop replaced original owner stdin", err, string(output))
	}
	wrong, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer wrong.Close()
	if err := command(wrong).Run(); err == nil {
		t.Fatal("replaced stdin admitted native installation")
	}
	if _, err := os.Stat(filepath.Dir(resource)); !os.IsNotExist(err) {
		t.Fatal("barrier validation created a resource directory", err)
	}
}

func TestHostInstallerLockChecksExactPreparedFiles(t *testing.T) {
	root := testkit.TempDir(t)
	paths := []string{filepath.Join(root, "unit<&-é"), filepath.Join(root, "absent")}
	testkit.WriteFile(t, paths[0], []byte("approved payload\n"), 0o644)
	expected, err := nativeInstalledFiles(paths)
	if err != nil {
		t.Fatal(err)
	}
	source, err := filepath.Abs("../../../scripts/lib/native-install-custody.py")
	if err != nil {
		t.Fatal(err)
	}
	arguments, _ := json.Marshal(paths)
	observe := func() string {
		t.Helper()
		command := exec.Command("python3", "-B", "-c", `import importlib.util,json,sys
spec=importlib.util.spec_from_file_location('custody',sys.argv[1])
module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module)
print(module.installed_files(json.loads(sys.argv[2])))`, source, string(arguments))
		raw, err := command.CombinedOutput()
		if err != nil {
			t.Fatal(err, string(raw))
		}
		return strings.TrimSpace(string(raw))
	}
	if actual := observe(); actual != expected {
		t.Fatalf("Go preparation and actual locked installer disagree: %s != %s", actual, expected)
	}
	if err := os.WriteFile(paths[0], []byte("changed after Go preflight\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if observe() == expected {
		t.Fatal("actual installer failed to detect source/resource drift under its lock")
	}
}

func TestHostInstallerCannotSplitBarrierWithDifferentLibraryDirectory(t *testing.T) {
	root := testkit.TempDir(t)
	resources := []string{filepath.Join(root, "engine"), filepath.Join(root, "unit")}
	source, err := filepath.Abs("../../../scripts")
	if err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(root, "ready")
	release := filepath.Join(root, "release")
	script := `set -euo pipefail
die() { exit 2; }
SCRIPT_DIR=$1
. "$SCRIPT_DIR/lib/native-install-custody.sh"
subyard_install_custody host-power "$2" "$3" "$4"
if [ "$5" = hold ]; then
  touch "$6"
  while [ ! -f "$7" ]; do sleep .01; done
fi
subyard_install_custody_verify host-power "$2"
`
	command := func(library, mode, expected, generation, successor string) *exec.Cmd {
		cmd := exec.Command("bash", "-c", script, "fixture", source, library, resources[0], resources[1], mode, ready, release)
		cmd.Env = append(os.Environ(), "SUBYARD_NATIVE_EXPECTED_VECTOR="+hostTestVector(expected), "SUBYARD_NATIVE_GENERATION="+generation, "SUBYARD_NATIVE_SUCCESSOR="+successor)
		return cmd
	}
	first := command(filepath.Join(root, "first-libexec"), "hold", "0", strings.Repeat("a", 64), "")
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { first.Process.Kill(); first.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("actual installer did not retain its resource lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// A different configured LIBEXEC_DIR with the identical actual files must
	// share those same locks, including when only the secondary unit overlaps.
	if err := command(filepath.Join(root, "second-libexec"), "run", "0", strings.Repeat("a", 64), strings.Repeat("b", 64)).Run(); err == nil {
		t.Fatal("library directory split the same actual resource barrier")
	}
	overlap := command(filepath.Join(root, "other-engine-libexec"), "run", "0", strings.Repeat("d", 64), "")
	overlap.Args[6] = filepath.Join(root, "different-engine")
	if err := overlap.Run(); err == nil {
		t.Fatal("secondary resource overlap bypassed actual shared unit lock")
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := first.Wait(); err != nil {
		t.Fatal(err)
	}
	if output, err := command(filepath.Join(root, "second-libexec"), "run", "0", strings.Repeat("a", 64), strings.Repeat("b", 64)).CombinedOutput(); err != nil {
		t.Fatal(err, string(output))
	}
	if err := command(filepath.Join(root, "first-libexec"), "run", "0", strings.Repeat("a", 64), "").Run(); err == nil {
		t.Fatal("delayed first claim restored authority after fence")
	}
	if output, err := command(filepath.Join(root, "third-libexec"), "run", strings.Repeat("b", 64), strings.Repeat("c", 64), "").CombinedOutput(); err != nil {
		t.Fatal(err, string(output))
	}
	if output, err := command(filepath.Join(root, "ordinary-libexec"), "run", "", "", "").CombinedOutput(); err != nil {
		t.Fatal(err, string(output))
	}
	if err := command(filepath.Join(root, "third-libexec"), "run", strings.Repeat("c", 64), strings.Repeat("d", 64), "").Run(); err == nil {
		t.Fatal("ordinary installation left delayed native authority valid")
	}
}

func hostTestVector(value string) string {
	if value == "" {
		return ""
	}
	raw, _ := json.Marshal([]map[string]string{{"role": "engine", "value": value}, {"role": "service", "value": value}})
	return string(raw)
}

func TestHostResourceVectorRejectsSharedSecondaryABAAndFinishesExactPartialPublication(t *testing.T) {
	root := testkit.TempDir(t)
	helper, err := filepath.Abs("../../../scripts/lib/native-install-custody.py")
	if err != nil {
		t.Fatal(err)
	}
	command := func(mode string) *exec.Cmd {
		return exec.Command("python3", "-B", "-c", `import importlib.util,sys,os,fcntl,json
from pathlib import Path
spec=importlib.util.spec_from_file_location('custody',sys.argv[1]);m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
root=Path(sys.argv[2]);mode=sys.argv[3]
resources=[root/'engine-a',root/'service']
if mode=='ordinary-other-primary': resources[0]=root/'engine-b'
fds=[]
for resource in resources:
 fd=os.open(resource.parent/('.'+resource.name+'.native-install.lock'),os.O_RDWR|os.O_CREAT|os.O_NOFOLLOW,0o600);os.fchmod(fd,0o600);fcntl.flock(fd,fcntl.LOCK_EX|fcntl.LOCK_NB);fds.append(fd)
expected=json.dumps([{'role':'engine','value':'0'},{'role':'service','value':'0'}],separators=(',',':'))
if mode.startswith('partial-'):
 original=m.claim
 def cut(*args):
  original(*args)
  os._exit(77)
 m.claim=cut
if mode=='ordinary-other-primary':m.claim_vector('host-power',resources,fds,'','','',True)
else:m.claim_vector('host-power',resources,fds,expected,'a'*64,'b'*64 if 'fence' in mode else '')
`, helper, root, mode)
	}
	for _, mode := range []string{"partial-claim", "claim", "partial-fence", "fence"} {
		err := command(mode).Run()
		if strings.HasPrefix(mode, "partial-") {
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 77 {
				t.Fatal("partial publication cut missing", mode, err)
			}
		} else if err != nil {
			t.Fatal(mode, err)
		}
	}
	if err := command("claim").Run(); err == nil {
		t.Fatal("delayed original claim survived vector fence")
	}
	// An invocation whose original primary remains absent must still lose
	// authority after another supported primary mutates the shared unit.
	for _, path := range []string{".engine-a.native-custody.json", ".service.native-custody.json"} {
		if err := os.Remove(filepath.Join(root, path)); err != nil {
			t.Fatal(err)
		}
	}
	if err := command("ordinary-other-primary").Run(); err != nil {
		t.Fatal(err)
	}
	if err := command("claim").Run(); err == nil {
		t.Fatal("changed primary split shared-secondary generation")
	}
	if _, err := os.Stat(filepath.Join(root, ".engine-a.native-custody.json")); !os.IsNotExist(err) {
		t.Fatal("vector refusal partially claimed original primary", err)
	}
}
