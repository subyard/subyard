package configmaterial

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

// casWorld is one guest directory with a destination the writer replaces.
type casWorld struct {
	t                 *testing.T
	home, destination string
}

func newCASWorld(t *testing.T) casWorld {
	home := filepath.Join(testkit.TempDir(t), "home")
	if err := os.MkdirAll(filepath.Join(home, ".config"), 0o755); err != nil {
		t.Fatal(err)
	}
	return casWorld{t: t, home: home, destination: filepath.Join(home, ".config", "agent.md")}
}

// hook is Python that defines CAS_HOOK(point); GUEST(content) rename-saves the
// destination, as an editor does, and EDIT(content) writes the displaced inode
// in place.
func (world casWorld) run(hook, expect string, payload []byte, arguments ...string) (int, string) {
	world.t.Helper()
	prelude := fmt.Sprintf(`import os as _t
_DEST = %q
def GUEST(content):
    staging = _DEST + ".editor"
    with open(staging, "w") as output:
        output.write(content)
    _t.rename(staging, _DEST)
def EDIT(content):
    directory = _t.path.dirname(_DEST)
    for entry in _t.listdir(directory):
        if ".subyard-cas." in entry:
            with open(_t.path.join(directory, entry), "a") as output:
                output.write(content)
CALLS = {}
def ONCE(point):
    CALLS[point] = CALLS.get(point, 0) + 1
    return CALLS[point] == 1
`, world.destination)
	if arguments == nil {
		arguments = []string{"write", world.home, world.destination, fmt.Sprint(os.Getuid()), expect, fmt.Sprintf("%x", sha256.Sum256(payload))}
	}
	command := exec.Command("python3", append([]string{"-B", "-c", prelude + hook + "\n" + casProgram()}, arguments...)...)
	command.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		world.t.Fatal(err)
	}
	if code := command.ProcessState.ExitCode(); code != 0 && code != 3 && code != 4 && code != 5 && code != 97 {
		world.t.Fatalf("writer failed: %d %s", code, stderr.String())
	}
	return command.ProcessState.ExitCode(), stdout.String()
}

// contents maps every file in the destination directory to its bytes.
func (world casWorld) contents() map[string]string {
	world.t.Helper()
	entries, err := os.ReadDir(filepath.Dir(world.destination))
	if err != nil {
		world.t.Fatal(err)
	}
	result := map[string]string{}
	for _, entry := range entries {
		payload, err := os.ReadFile(filepath.Join(filepath.Dir(world.destination), entry.Name()))
		if err != nil {
			world.t.Fatal(err)
		}
		name := entry.Name()
		if strings.Contains(name, ".subyard-cas.") {
			name = "leftover"
		}
		result[name] = string(payload)
	}
	return result
}

func (world casWorld) observe() FileToken {
	world.t.Helper()
	_, stdout := world.run("", "", nil, "observe", world.home, world.destination)
	tokens, err := ParseTokens([]byte(stdout), []string{world.destination})
	if err != nil {
		world.t.Fatalf("%v: %s", err, stdout)
	}
	return tokens[0]
}

func TestCompareAndWriteReplacesOnlyConsentedBytes(t *testing.T) {
	desired := []byte("desired\n")
	for _, test := range []struct {
		name, initial, hook string
		consented           string
		absent              bool
		code                int
		want                map[string]string
	}{
		{name: "matching file", initial: "old", want: map[string]string{"agent.md": "desired\n"}},
		{name: "absent file", absent: true, want: map[string]string{"agent.md": "desired\n"}},
		{name: "changed before compare", initial: "guest", consented: "old", code: 4, want: map[string]string{"agent.md": "guest"}},
		{name: "created before no-replace", absent: true, hook: `def CAS_HOOK(point):
    if point == "staged": GUEST("guest")`, code: 4, want: map[string]string{"agent.md": "guest"}},
		{name: "rename-save before exchange", initial: "old", hook: `def CAS_HOOK(point):
    if point == "staged": GUEST("guest")`, code: 4, want: map[string]string{"agent.md": "guest"}},
		{name: "in-place write before verification", initial: "old", hook: `def CAS_HOOK(point):
    if point == "exchanged": EDIT("+edit")`, code: 4, want: map[string]string{"agent.md": "old+edit"}},
		// The guest saves between the exchange and the exchange-back: the newest
		// save stays at the destination and the older one is preserved.
		{name: "save between exchanges", initial: "old", hook: `def CAS_HOOK(point):
    if point == "staged": GUEST("first")
    if point == "exchanged": GUEST("second")`, code: 4, want: map[string]string{"agent.md": "second", "leftover": "first"}},
		{name: "rename-save after replacement", initial: "old", hook: `def CAS_HOOK(point):
    if point == "verified": GUEST("guest")`, code: 4, want: map[string]string{"agent.md": "guest"}},
		// Without renameat2 the compare repeats right before a plain rename.
		{name: "unsupported matching", initial: "old", hook: `def CAS_HOOK(point):
    if point == "renameat2": raise OSError(38, "unsupported")`, want: map[string]string{"agent.md": "desired\n"}},
		{name: "unsupported absent", absent: true, hook: `def CAS_HOOK(point):
    if point == "renameat2": raise OSError(22, "unsupported")`, want: map[string]string{"agent.md": "desired\n"}},
		{name: "unsupported created", absent: true, hook: `def CAS_HOOK(point):
    if point == "renameat2": raise OSError(38, "unsupported")
    if point == "fallback": GUEST("guest")`, code: 4, want: map[string]string{"agent.md": "guest"}},
		{name: "unsupported changed", initial: "old", hook: `def CAS_HOOK(point):
    if point == "renameat2": raise OSError(38, "unsupported")
    if point == "fallback": GUEST("guest")`, code: 4, want: map[string]string{"agent.md": "guest"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			world := newCASWorld(t)
			expect := TokenAbsent
			if !test.absent {
				testkit.WriteFile(t, world.destination, []byte(test.initial), 0o644)
				expect = PayloadToken([]byte(cmp.Or(test.consented, test.initial)))
			}
			code, stdout := world.run(test.hook, expect, desired)
			if code != test.code {
				t.Fatalf("exit %d, want %d: %s", code, test.code, stdout)
			}
			if got := world.contents(); !mapsEqual(got, test.want) {
				t.Fatalf("guest directory %v, want %v", got, test.want)
			}
			refusal := ParseWriteResult(world.destination, code, []byte(stdout))
			var typed WriteRefusal
			if (refusal != nil) != (code != 0) || code != 0 && (!errors.As(refusal, &typed) || typed.Outcome != OutcomeChanged ||
				len(typed.Leftovers) != len(test.want)-1) {
				t.Fatalf("write result %v from %q", refusal, stdout)
			}
			if code == 0 {
				info, err := os.Stat(world.destination)
				if err != nil || info.Mode().Perm() != 0o644 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
					t.Fatal("written file mode or owner", err)
				}
			}
		})
	}
}

func TestCompareAndWriteConvergedAndUnreadable(t *testing.T) {
	world := newCASWorld(t)
	testkit.WriteFile(t, world.destination, []byte("desired\n"), 0o600)
	before, _ := os.Stat(world.destination)
	if code, _ := world.run("", PayloadToken([]byte("other")), []byte("desired\n")); code != 0 {
		t.Fatal("converged file refused", code)
	}
	// Like a write, convergence leaves the developer's owner and mode.
	if after, _ := os.Stat(world.destination); !os.SameFile(before, after) || after.Mode().Perm() != 0o644 {
		t.Fatal("converged file was rewritten or kept its mode")
	}
	for _, kind := range []string{"symlink", "directory", "fifo", "symlinked parent"} {
		t.Run(kind, func(t *testing.T) {
			world := newCASWorld(t)
			target := filepath.Join(filepath.Dir(world.home), "outside")
			testkit.WriteFile(t, target, []byte("outside"), 0o644)
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(target, world.destination)
			case "directory":
				err = os.Mkdir(world.destination, 0o755)
			case "fifo":
				err = syscall.Mkfifo(world.destination, 0o644)
			case "symlinked parent":
				if err = os.RemoveAll(filepath.Dir(world.destination)); err == nil {
					err = os.Symlink(filepath.Dir(target), filepath.Dir(world.destination))
				}
				world.destination = filepath.Join(filepath.Dir(world.destination), "outside")
			}
			if err != nil {
				t.Fatal(err)
			}
			if token := world.observe().Token; token != TokenUnreadable {
				t.Fatalf("observed %s", token)
			}
			if code, _ := world.run("", PayloadToken([]byte("outside")), []byte("desired\n")); code != 3 {
				t.Fatalf("unreadable destination written: %d", code)
			}
			if payload, _ := os.ReadFile(target); string(payload) != "outside" {
				t.Fatal("writer followed a link")
			}
		})
	}
}

// A crash at any point of a write over a file the guest changed keeps every
// guest byte, and the next observation reports what was preserved.
func TestCompareAndWriteCrashKeepsGuestBytes(t *testing.T) {
	for _, point := range []string{"staged", "exchanged", "exchanged-back"} {
		t.Run(point, func(t *testing.T) {
			world := newCASWorld(t)
			testkit.WriteFile(t, world.destination, []byte("old"), 0o644)
			hook := fmt.Sprintf(`def CAS_HOOK(point):
    if point == "staged": GUEST("guest")
    if point == %q and ONCE(point): _t._exit(97)`, point)
			if code, _ := world.run(hook, PayloadToken([]byte("old")), []byte("desired\n")); code != 97 {
				t.Fatalf("crash did not occur: %d", code)
			}
			got := world.contents()
			values := []string{got["agent.md"], got["leftover"]}
			if len(got) != 2 || !slices.Contains(values, "guest") || !slices.Contains(values, "desired\n") {
				t.Fatalf("guest directory after crash: %v", got)
			}
			if observed := world.observe(); len(observed.Leftovers) != 1 {
				t.Fatalf("preserved file not reported: %#v", observed)
			}
		})
	}
}

func TestCompareAndWriteRequests(t *testing.T) {
	if _, err := RawRequest("dev", "/home/dev/../etc/passwd", 1000, nil, TokenAbsent); err == nil {
		t.Fatal("destination outside the home accepted")
	}
	if _, err := RawRequest("dev", "/home/dev/a", 1000, nil, TokenUnreadable); err == nil {
		t.Fatal("unreadable token accepted as consent")
	}
	if _, err := ApplyRequest("json", ModeApply, "dev", "/home/dev/a.json", 1000, []byte(`{}`), ""); err == nil {
		t.Fatal("structured apply without a token accepted")
	}
	if _, err := Request("json", ModeApply, "dev", "/home/dev/a.json", 1000, []byte(`{}`)); err == nil {
		t.Fatal("structured apply through the read-only request accepted")
	}
	if _, err := ParseTokens([]byte(`{"files":[{"destination":"/home/dev/a","token":"absent","leftovers":["/etc/x"]}]}`), []string{"/home/dev/a"}); err == nil {
		t.Fatal("unsafe leftover accepted")
	}
	if err := ParseWriteResult("/home/dev/a", 4, []byte(`{"outcome":"unreadable","leftovers":[]}`)); err == nil || errors.As(err, new(WriteRefusal)) {
		t.Fatal("mismatched outcome accepted")
	}
}

func mapsEqual(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if other, ok := right[key]; !ok || other != value {
			return false
		}
	}
	return true
}

// Guest bytes that a crash right after the exchange preserved block the next
// write and its convergence until the operator resolves them.
func TestCompareAndWriteLeftoverBlocksConvergence(t *testing.T) {
	world := newCASWorld(t)
	testkit.WriteFile(t, world.destination, []byte("old"), 0o644)
	hook := `def CAS_HOOK(point):
    if point == "staged": GUEST("guest")
    if point == "exchanged": _t._exit(97)`
	if code, _ := world.run(hook, PayloadToken([]byte("old")), []byte("desired\n")); code != 97 {
		t.Fatalf("crash did not occur: %d", code)
	}
	if got := world.contents(); got["agent.md"] != "desired\n" || got["leftover"] != "guest" {
		t.Fatalf("after crash: %v", got)
	}
	leftovers := world.observe().Leftovers
	code, stdout := world.run("", PayloadToken([]byte("old")), []byte("desired\n"))
	refusal := ParseWriteResult(world.destination, code, []byte(stdout))
	var typed WriteRefusal
	if code != 5 || !errors.As(refusal, &typed) || typed.Outcome != OutcomeLeftover || !slices.Equal(typed.Leftovers, leftovers) ||
		!strings.Contains(refusal.Error(), leftovers[0]) {
		t.Fatalf("rerun over preserved bytes: %d %v", code, refusal)
	}
	if got := world.contents(); got["agent.md"] != "desired\n" || got["leftover"] != "guest" {
		t.Fatalf("rerun changed the directory: %v", got)
	}
}

// A file that grows past the read bound after it was hashed is not verified:
// the writer gives it back instead of unlinking or stranding it.
func TestCompareAndWriteUnverifiableDisplacedFileIsGivenBack(t *testing.T) {
	world := newCASWorld(t)
	testkit.WriteFile(t, world.destination, []byte("old"), 0o644)
	hook := fmt.Sprintf(`def CAS_HOOK(point):
    if point == "staged":
        with open(%q, "ab") as output: output.write(b"x" * (33 << 20))`, world.destination)
	code, stdout := world.run(hook, PayloadToken([]byte("old")), []byte("desired\n"))
	var typed WriteRefusal
	if code != 4 || !errors.As(ParseWriteResult(world.destination, code, []byte(stdout)), &typed) || len(typed.Leftovers) != 0 {
		t.Fatalf("grown file: %d %s", code, stdout)
	}
	entries, _ := os.ReadDir(filepath.Dir(world.destination))
	info, err := os.Stat(world.destination)
	if len(entries) != 1 || err != nil || info.Size() != 3+33<<20 {
		t.Fatalf("grown guest file not at its destination: %d entries, %v", len(entries), err)
	}
}
