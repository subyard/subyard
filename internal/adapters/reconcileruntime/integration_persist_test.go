package reconcileruntime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
)

// persistFixture is one source directory and its host-backed target on the
// same test filesystem. The program runs as the current developer account.
type persistFixture struct {
	t                    *testing.T
	root, source, target string
	mappings             []PersistMapping
	start                bool
	injection            string
}

func newPersistFixture(t *testing.T) *persistFixture {
	t.Helper()
	root := testkit.TempDir(t)
	f := &persistFixture{t: t, root: root, source: root + "/home/.claude/projects", target: root + "/sessions/claude/projects", start: true}
	for _, path := range []string{f.source + "/p1/empty", root + "/sessions/claude"} {
		mkdirs(t, path)
	}
	testkit.WriteFile(t, f.source+"/p1/s.jsonl", []byte("0\n1\n"), 0o600)
	testkit.WriteFile(t, f.source+"/p1/MEMORY.md", []byte("memory\n"), 0o644)
	if err := os.Symlink("../elsewhere", f.source+"/p1/link"); err != nil {
		t.Fatal(err)
	}
	f.mappings = []PersistMapping{{Integration: "claude", Source: f.source, Target: f.target, Selected: true}}
	return f
}

func mkdirs(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func (f *persistFixture) run(mode string, extra map[string]any) (map[string]any, string, error) {
	f.t.Helper()
	developer, _ := inventoryDeveloper(f.t)
	request := map[string]any{"yard": "default", "uid": os.Getuid(), "developer": developer, "mappings": f.mappings, "start": f.start}
	for name, value := range extra {
		request[name] = value
	}
	payload, _ := json.Marshal(request)
	program := strings.Replace(integrationPersistProgram, "\nUID = GID = -1\n", "\n"+f.injection+"\nUID = GID = -1\n", 1)
	command := exec.Command("python3", "-B", "-c", program, mode)
	command.Stdin = strings.NewReader(string(payload))
	var stdout, stderr strings.Builder
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	var value map[string]any
	_ = json.Unmarshal([]byte(stdout.String()), &value)
	return value, stderr.String(), err
}

func (f *persistFixture) observe() map[string]any {
	f.t.Helper()
	value, stderr, err := f.run("observe", nil)
	if err != nil {
		f.t.Fatalf("observe: %v %s", err, stderr)
	}
	return value
}

func (f *persistFixture) entry() map[string]any {
	f.t.Helper()
	return f.observe()["entries"].([]any)[0].(map[string]any)
}

func (f *persistFixture) ops() string {
	f.t.Helper()
	var result []string
	for _, entry := range f.observe()["entries"].([]any) {
		for _, op := range entry.(map[string]any)["ops"].([]any) {
			result = append(result, op.(string))
		}
	}
	return strings.Join(result, "+")
}

func (f *persistFixture) apply() (map[string]any, string, error) {
	f.t.Helper()
	value, stderr, err := f.run("observe", nil)
	if err != nil {
		return nil, stderr, err
	}
	return f.run("apply", map[string]any{"expected": value["fingerprint"]})
}

func (f *persistFixture) mustApply() map[string]any {
	f.t.Helper()
	f.injection = ""
	value, stderr, err := f.apply()
	if err != nil {
		f.t.Fatalf("apply: %v %s", err, stderr)
	}
	return value
}

func (f *persistFixture) interrupt(injection string) {
	f.t.Helper()
	f.injection = injection
	_, stderr, err := f.apply()
	f.injection = ""
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 77 {
		f.t.Fatalf("cut did not interrupt: %v %s", err, stderr)
	}
}

func (f *persistFixture) retained() []map[string]any {
	f.t.Helper()
	value, stderr, err := f.run("retained", nil)
	if err != nil {
		f.t.Fatalf("retained: %v %s", err, stderr)
	}
	var result []map[string]any
	for _, item := range value["retained"].([]any) {
		result = append(result, item.(map[string]any))
	}
	return result
}

func (f *persistFixture) retainedPaths() []string {
	f.t.Helper()
	var result []string
	for _, item := range f.retained() {
		result = append(result, item["path"].(string))
	}
	return result
}

func (f *persistFixture) backups() []string {
	matches, _ := filepath.Glob(f.source + ".subyard-backup-*")
	return matches
}

func (f *persistFixture) backup() string {
	f.t.Helper()
	if backups := f.backups(); len(backups) == 1 {
		return backups[0]
	}
	f.t.Fatalf("expected one backup, got %v", f.backups())
	return ""
}

func (f *persistFixture) records() []string {
	matches, _ := filepath.Glob(f.source + ".subyard-transfer*")
	return matches
}

func (f *persistFixture) stages() []string {
	matches, _ := filepath.Glob(filepath.Dir(f.target) + "/.subyard-stage-*")
	return matches
}

func read(t *testing.T, path string) string {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

func inode(t *testing.T, path string) uint64 {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Sys().(*syscall.Stat_t).Ino
}

// assertDone checks the final state: S is the exact link, T holds the data,
// and no backup, stage or record of the transfer remains.
func (f *persistFixture) assertDone() {
	f.t.Helper()
	if link, err := os.Readlink(f.source); err != nil || link != f.target {
		f.t.Fatalf("source is not the exact target link: %q %v", link, err)
	}
	if read(f.t, f.target+"/p1/s.jsonl") != "0\n1\n" || read(f.t, f.target+"/p1/MEMORY.md") != "memory\n" {
		f.t.Fatal("target lacks source bytes")
	}
	if link, err := os.Readlink(f.target + "/p1/link"); err != nil || link != "../elsewhere" {
		f.t.Fatal("symlink not copied literally", link, err)
	}
	if info, err := os.Stat(f.target + "/p1/MEMORY.md"); err != nil || info.Mode().Perm() != 0o644 {
		f.t.Fatal("file mode not preserved", err)
	}
	if info, err := os.Stat(f.target + "/p1/empty"); err != nil || !info.IsDir() {
		f.t.Fatal("empty directory missing", err)
	}
	if len(f.backups())+len(f.stages())+len(f.records()) != 0 {
		f.t.Fatal("transfer state left after success", f.backups(), f.stages(), f.records())
	}
	if ops := f.ops(); ops != "" {
		f.t.Fatal("repeat observation is not a no-op", ops)
	}
}

func TestPersistAdoptionDeletesTheBackupAfterTheSwitch(t *testing.T) {
	f := newPersistFixture(t)
	if err := os.Chmod(f.source, 0o750); err != nil {
		t.Fatal(err)
	}
	if entry := f.entry(); entry["persist"] != "projected" || fmt.Sprint(entry["ops"]) != "[adopt]" || entry["files"] != 3.0 {
		t.Fatal("adoption plan", entry)
	}
	if result := f.mustApply(); len(result["results"].([]any)) != 0 {
		t.Fatal("clean adoption reported remnants", result["results"])
	}
	f.assertDone()
	if info, err := os.Stat(f.target); err != nil || info.Mode().Perm() != 0o750 {
		t.Fatal("a target root this run created lacks the captured mode", err)
	}
	if len(f.retained()) != 0 {
		t.Fatal("nothing should be retained")
	}
}

func TestPersistCatchUpPassRecopiesChangedFiles(t *testing.T) {
	f := newPersistFixture(t)
	// Lines written during the first pass reach the target through the catch-up pass.
	f.injection = fmt.Sprintf(`native_pass, passes = stage_pass, [0]
def stage_pass(*a):
    tree = native_pass(*a)
    passes[0] += 1
    if passes[0] == 1:
        with open(%q, 'a') as stream:
            stream.write('2\n')
    return tree`, f.source+"/p1/s.jsonl")
	if _, stderr, err := f.apply(); err != nil {
		t.Fatalf("apply: %v %s", err, stderr)
	}
	if read(t, f.target+"/p1/s.jsonl") != "0\n1\n2\n" {
		t.Fatal("catch-up pass missed a change")
	}
}

// Interrupt points, from before the descriptor to the middle of finish.
var persistCuts = map[string]string{
	"temporary":  "native_renameat2 = renameat2\ndef renameat2(a, b, c, d, flags):\n    if b.endswith('.tmp'): os._exit(77)\n    native_renameat2(a, b, c, d, flags)",
	"descriptor": "native_renameat2 = renameat2\ndef renameat2(a, b, c, d, flags):\n    native_renameat2(a, b, c, d, flags)\n    if d.endswith('.subyard-transfer.json'): os._exit(77)",
	"copying":    "native_copy = copy_file\ndef copy_file(*a):\n    value = native_copy(*a)\n    os._exit(77)",
	"publishing": "native_renameat2 = renameat2\ncount = [0]\ndef renameat2(a, b, c, d, flags):\n    native_renameat2(a, b, c, d, flags)\n    if flags == NOREPLACE and b.startswith('.subyard-copy-'):\n        count[0] += 1\n        if count[0] == 2: os._exit(77)",
	"prepared":   "native_symlink = os.symlink\ndef link(a, b, **k):\n    native_symlink(a, b, **k)\n    if '.subyard-backup-' in b: os._exit(77)\nos.symlink = link",
	"exchanged":  "native_renameat2 = renameat2\ndef renameat2(a, b, c, d, flags):\n    native_renameat2(a, b, c, d, flags)\n    if flags == EXCHANGE and not b.startswith('.subyard-probe'): os._exit(77)",
	"finishing":  "native_discard = discard_stage\ndef discard_stage(*a):\n    native_discard(*a)\n    os._exit(77)",
}

func TestPersistInterruptAtEachStepConvergesToDone(t *testing.T) {
	for _, cut := range slices.Sorted(maps.Keys(persistCuts)) {
		t.Run(cut, func(t *testing.T) {
			f := newPersistFixture(t)
			f.interrupt(persistCuts[cut])
			want := map[string]string{"temporary": "adopt", "exchanged": "finish", "finishing": "finish"}[cut]
			if want == "" {
				want = "cancel+adopt"
			}
			if ops := f.ops(); ops != want {
				t.Fatalf("after %s: ops %s, want %s", cut, ops, want)
			}
			if cut != "exchanged" && cut != "finishing" && !isDirectory(f.source) {
				t.Fatal("the source changed before the commit point")
			}
			f.mustApply()
			f.assertDone()
		})
	}
}

func isDirectory(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.IsDir()
}

func TestPersistStatesByEvent(t *testing.T) {
	type event struct {
		name            string
		selected, start bool
	}
	events := []event{{"init", true, true}, {"deselect", false, true}, {"activation", true, false}}
	states := map[string]func(*persistFixture){
		"N": func(*persistFixture) {},
		"A": func(f *persistFixture) { f.interrupt(persistCuts["publishing"]) },
		"X": func(f *persistFixture) { f.interrupt(persistCuts["exchanged"]) },
		"Done": func(f *persistFixture) {
			f.mustApply()
		},
		"R": func(f *persistFixture) {
			f.mustApply()
			mkdirs(t, f.source+".subyard-backup-0123456789abcdef/p1")
			mkdirs(t, filepath.Dir(f.target)+"/.subyard-stage-default-0123456789abcdef")
		},
	}
	// Expected ops, or "pending" for a pending-configuration refusal.
	want := map[string]map[string]string{
		"N":    {"init": "adopt", "deselect": "", "activation": "pending"},
		"A":    {"init": "cancel+adopt", "deselect": "cancel", "activation": "pending"},
		"X":    {"init": "finish", "deselect": "finish", "activation": "finish"},
		"Done": {"init": "", "deselect": "", "activation": ""},
		"R":    {"init": "", "deselect": "", "activation": ""},
	}
	for _, state := range slices.Sorted(maps.Keys(states)) {
		for _, ev := range events {
			t.Run(state+"/"+ev.name, func(t *testing.T) {
				f := newPersistFixture(t)
				states[state](f)
				f.mappings[0].Selected, f.start = ev.selected, ev.start
				expected := want[state][ev.name]
				if expected == "pending" {
					_, stderr, err := f.run("observe", nil)
					if err == nil || !strings.Contains(stderr, "persist adoption pending") || !strings.Contains(stderr, `"persist": "deferred"`) {
						t.Fatalf("activation started an adoption: %v %s", err, stderr)
					}
					return
				}
				if ops := f.ops(); ops != expected {
					t.Fatalf("ops %q, want %q", ops, expected)
				}
				before := inode(t, f.source)
				f.mustApply()
				if f.ops() != "" {
					t.Fatal("apply did not converge", f.ops())
				}
				switch {
				case ev.selected || state == "Done" || state == "R":
					if link, _ := os.Readlink(f.source); link != f.target {
						t.Fatal("selected source is not the link")
					}
				case state == "X":
					// The descriptor still names our own link, so finish removes it.
					if _, err := os.Lstat(f.source); !os.IsNotExist(err) || len(f.backups())+len(f.records()) != 0 {
						t.Fatal("finish under deselect", err, f.backups(), f.records())
					}
				default:
					if !isDirectory(f.source) || inode(t, f.source) != before || len(f.records())+len(f.stages()) != 0 {
						t.Fatal("deselect changed the source or kept transfer state", f.records(), f.stages())
					}
				}
				if state == "A" && ev.name == "deselect" && read(t, f.target+"/p1/MEMORY.md") != "memory\n" {
					t.Fatal("cancel removed a copy already published to the target")
				}
				if state == "R" && len(f.retained()) != 2 {
					t.Fatal("retained items not listed", f.retained())
				}
			})
		}
	}
}

func TestPersistDeselectAfterTheExchangeRemovesOnlyOwnLink(t *testing.T) {
	t.Run("own-link", func(t *testing.T) {
		f := newPersistFixture(t)
		f.interrupt(persistCuts["exchanged"])
		f.mappings[0].Selected = false
		facts := (persistObservation{Entries: []persistEntry{{Source: f.source, Target: f.target, Ops: []string{"finish"}}}}).facts()
		if _, text, _ := persistStepText(facts[0]); !strings.Contains(text, "then remove the link; the data remains in "+f.target) {
			t.Fatal("deselect finish step does not disclose the link removal", text)
		}
		f.mustApply()
		if _, err := os.Lstat(f.source); !os.IsNotExist(err) {
			t.Fatal("own link left after deselect", err)
		}
		if len(f.backups())+len(f.records())+len(f.stages()) != 0 || read(t, f.target+"/p1/s.jsonl") != "0\n1\n" {
			t.Fatal("deselect did not finish or lost target data", f.backups(), f.records(), f.stages())
		}
		// Reselecting finds no source directory: the inventory creates the link normally.
		f.mappings[0].Selected = true
		if entry := f.entry(); fmt.Sprint(entry["ops"]) != "[]" || entry["persist"] != "" {
			t.Fatal("reselect after deselect", entry)
		}
	})
	t.Run("foreign-link", func(t *testing.T) {
		f := newPersistFixture(t)
		f.interrupt(persistCuts["exchanged"])
		if err := os.Remove(f.source); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(f.root+"/elsewhere", f.source); err != nil {
			t.Fatal(err)
		}
		f.mappings[0].Selected = false
		f.mustApply()
		if link, _ := os.Readlink(f.source); link != f.root+"/elsewhere" || len(f.records()) != 0 {
			t.Fatal("finish removed a link that does not name the recorded target", link, f.records())
		}
	})
	t.Run("selected-keeps-link", func(t *testing.T) {
		f := newPersistFixture(t)
		f.interrupt(persistCuts["exchanged"])
		f.mustApply()
		f.assertDone()
	})
}

func TestPersistRemnantFinishDiscardsOnlyOwnState(t *testing.T) {
	// Interrupted before the exchange, then the source directory moved away:
	// neither S nor B holds the recorded identity.
	f := newPersistFixture(t)
	f.interrupt(persistCuts["copying"])
	aside := f.root + "/aside"
	if err := os.Rename(f.source, aside); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Dir(f.target)+"/unrelated", []byte("keep\n"), 0o600)
	if ops := f.ops(); ops != "finish" {
		t.Fatal("remnant ops", ops)
	}
	f.mustApply()
	if len(f.records())+len(f.stages())+len(f.backups()) != 0 || len(f.retained()) != 0 {
		t.Fatal("remnant finish left own state", f.records(), f.stages(), f.retained())
	}
	if read(t, aside+"/p1/s.jsonl") != "0\n1\n" || read(t, filepath.Dir(f.target)+"/unrelated") != "keep\n" {
		t.Fatal("remnant finish deleted data that is not its own")
	}
	if _, err := os.Lstat(f.source); !os.IsNotExist(err) {
		t.Fatal("remnant finish created or kept a source", err)
	}
}

func TestPersistRecreatedSourceAfterTheExchangeIsANewAdoption(t *testing.T) {
	f := newPersistFixture(t)
	f.interrupt(persistCuts["exchanged"])
	if err := os.Remove(f.source); err != nil {
		t.Fatal(err)
	}
	mkdirs(t, f.source+"/p2")
	testkit.WriteFile(t, f.source+"/p2/s.jsonl", []byte("new\n"), 0o600)
	if ops := f.ops(); ops != "finish+adopt" {
		t.Fatal("recreated source", ops)
	}
	f.mustApply()
	f.assertDone()
	if read(t, f.target+"/p2/s.jsonl") != "new\n" {
		t.Fatal("recreated source data not transferred")
	}
}

func TestPersistTargetWinsAndThePlanDisclosesIt(t *testing.T) {
	f := newPersistFixture(t)
	mkdirs(t, f.target+"/p1")
	testkit.WriteFile(t, f.target+"/p1/s.jsonl", []byte("target\n"), 0o600)
	// A file where the source has a directory excludes that whole subtree.
	testkit.WriteFile(t, f.target+"/p1/empty", []byte("file\n"), 0o600)
	if err := os.Chmod(f.target+"/p1", 0o750); err != nil {
		t.Fatal(err)
	}
	entry := f.entry()
	if fmt.Sprint(entry["existing"]) != "[p1/empty p1/s.jsonl]" || entry["existing_count"] != 2.0 {
		t.Fatal("intersection not disclosed", entry["existing"], entry["existing_count"])
	}
	result := f.mustApply()
	outcome := result["results"].([]any)[0].(map[string]any)
	if fmt.Sprint(outcome["not_transferred"]) != "[p1/empty p1/s.jsonl]" {
		t.Fatal("not transferred entries", outcome)
	}
	if read(t, f.target+"/p1/s.jsonl") != "target\n" || read(t, f.target+"/p1/empty") != "file\n" || read(t, f.target+"/p1/MEMORY.md") != "memory\n" {
		t.Fatal("the target did not win")
	}
	if info, _ := os.Stat(f.target + "/p1"); info.Mode().Perm() != 0o750 {
		t.Fatal("an existing target directory was chmod'ed", info.Mode())
	}
	if len(f.backups()) != 0 {
		t.Fatal("backup kept after success")
	}
	// The disclosure is bounded and counts the rest.
	existing := make([]string, 20)
	for i := range existing {
		existing[i] = fmt.Sprintf("e%02d", i)
	}
	desired, _ := json.Marshal(persistStep{Ops: []string{"adopt"}, Source: f.source, Target: f.target, Existing: existing, ExistingCount: 25})
	source, text, ok := persistStepText(integrationFact{Key: "persist:adopt:" + f.source, Desired: string(desired)})
	if !ok || source != f.source || !strings.Contains(text, "25 entries already exist in the target and will not be transferred; their source copies are deleted with the backup") ||
		!strings.Contains(text, "e19 and 5 more") {
		t.Fatal("plan disclosure", text)
	}
	lines := persistOutcomeLines([]persistOutcome{{Source: f.source, NotTransferred: append(existing, "x1", "x2"), Retained: []string{"/b"}}})
	if len(lines) != 2 || !strings.Contains(lines[0], "22 entries not transferred") || !strings.Contains(lines[0], "and 2 more") || !strings.Contains(lines[1], "retained /b") {
		t.Fatal("outcome lines", lines)
	}
}

func TestPersistPublishRefusesUnwritableTargetEntries(t *testing.T) {
	// Bytes the target refuses would be lost with the backup: refuse, stay
	// before the exchange, and list every refused path.
	f := newPersistFixture(t)
	mkdirs(t, f.target+"/p1")
	if err := os.Chmod(f.target+"/p1", 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(f.target+"/p1", 0o700) })
	_, stderr, err := f.apply()
	for _, want := range []string{"persist target entry refused", f.target + "/p1/MEMORY.md", f.target + "/p1/s.jsonl", f.target + "/p1/empty", "Permission denied"} {
		if err == nil || !strings.Contains(stderr, want) {
			t.Fatalf("refusal omitted %q: %v %s", want, err, stderr)
		}
	}
	if !isDirectory(f.source) || len(f.backups()) != 0 || len(f.records()) != 1 {
		t.Fatal("refused publication exchanged or lost the transfer record", f.backups(), f.records())
	}
	if err := os.Chmod(f.target+"/p1", 0o700); err != nil {
		t.Fatal(err)
	}
	f.mustApply()
	f.assertDone()
}

func TestPersistFinishRetainsWhatItCannotAccess(t *testing.T) {
	f := newPersistFixture(t)
	f.interrupt(persistCuts["exchanged"])
	stage, backup := f.stages()[0], f.backup()
	testkit.WriteFile(t, stage+"/foreign", []byte("x"), 0o600)
	for _, path := range []string{stage, backup} {
		if err := os.Chmod(path, 0o000); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { os.Chmod(stage, 0o700); os.Chmod(backup, 0o700) })
	result, stderr, err := f.apply()
	if err != nil {
		t.Fatalf("finish failed instead of retaining: %v %s", err, stderr)
	}
	reported := fmt.Sprint(result["results"])
	if !strings.Contains(reported, stage) || !strings.Contains(reported, backup) || len(f.records()) != 0 {
		t.Fatal("finish did not report both retained items and remove the descriptor", reported, f.records())
	}
	if ops := f.ops(); ops != "" {
		t.Fatal("retained state is not terminal", ops)
	}
	if paths := f.retainedPaths(); !slices.Contains(paths, stage) || !slices.Contains(paths, backup) {
		t.Fatal("status omitted retained items", paths)
	}
}

func TestPersistRetainedListingExcludesEveryActiveTransfer(t *testing.T) {
	// Memory and projects share the target parent; the projects transfer is
	// interrupted while memory is adopted.
	f := newPersistFixture(t)
	memory := f.root + "/home/.claude-memory"
	mkdirs(t, memory)
	testkit.WriteFile(t, memory+"/MEMORY.md", []byte("m\n"), 0o600)
	f.mappings = append(f.mappings, PersistMapping{Integration: "claude", Source: memory, Target: f.root + "/sessions/claude/memory", Selected: true})
	f.interrupt("native_pass = stage_pass\ndef stage_pass(*a):\n    value = native_pass(*a)\n    if a[1].endswith('/projects'): os._exit(77)\n    return value")
	if link, _ := os.Readlink(memory); link != f.root+"/sessions/claude/memory" || len(f.stages()) != 1 {
		t.Fatal("fixture: memory adopted and projects pending", link, f.stages())
	}
	if paths := f.retainedPaths(); len(paths) != 0 {
		t.Fatal("an active transfer stage was listed as retained", paths)
	}
}

func TestPersistRefusesEveryProblemAtOnceBeforeWriting(t *testing.T) {
	f := newPersistFixture(t)
	mkdirs(t, f.source+"/p1/mnt")
	testkit.WriteFile(t, f.source+"/p1/shared", []byte("x"), 0o666)
	testkit.WriteFile(t, f.source+"/p1/sealed", []byte("x"), 0o000)
	if err := syscall.Mkfifo(f.source+"/p1/fifo", 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f.source+"/p1/shared", 0o666); err != nil {
		t.Fatal(err)
	}
	f.injection = mountedAt("mnt") + "\nclass FakeVolume:\n    f_bavail = 1\n    f_frsize = 1\nos.statvfs = lambda path: FakeVolume"
	_, stderr, err := f.run("observe", nil)
	for _, want := range []string{"persist mount boundary", f.source + "/p1/mnt", "unsafe persist data", f.source + "/p1/shared",
		"unsupported persist entry", f.source + "/p1/fifo", "not readable by the developer", f.source + "/p1/sealed", "persist space unavailable"} {
		if err == nil || !strings.Contains(stderr, want) {
			t.Fatalf("observe omitted %q: %v %s", want, err, stderr)
		}
	}
	if _, _, err := f.apply(); err == nil {
		t.Fatal("refused adoption applied")
	}
	if len(f.records())+len(f.stages()) != 0 || !isDirectory(f.source) {
		t.Fatal("refusal wrote state")
	}
}

func TestPersistRootsMustBeSafe(t *testing.T) {
	for _, scenario := range []string{"source", "target"} {
		t.Run(scenario, func(t *testing.T) {
			f := newPersistFixture(t)
			path, reason := f.source, "unsafe persist data"
			if scenario == "target" {
				path, reason = f.target, "unsafe persist target"
				mkdirs(t, f.target)
			}
			if err := os.Chmod(path, 0o777); err != nil {
				t.Fatal(err)
			}
			_, stderr, err := f.run("observe", nil)
			if err == nil || !strings.Contains(stderr, reason) || !strings.Contains(stderr, `"path": "`+path+`"`) {
				t.Fatalf("writable %s root accepted: %v %s", scenario, err, stderr)
			}
		})
	}
}

func TestPersistUnsupportedRenameRefusesBeforeWriting(t *testing.T) {
	f := newPersistFixture(t)
	f.injection = "native_renameat2 = renameat2\ndef renameat2(a, b, c, d, flags):\n    if flags == EXCHANGE: raise OSError(errno.EINVAL, 'unsupported')\n    native_renameat2(a, b, c, d, flags)"
	_, stderr, err := f.apply()
	if err == nil || !strings.Contains(stderr, "atomic exchange unsupported") {
		t.Fatalf("missing exchange support accepted: %v %s", err, stderr)
	}
	if len(f.records())+len(f.stages()) != 0 || !isDirectory(f.source) {
		t.Fatal("refusal wrote state", f.records(), f.stages())
	}
	if entries, _ := os.ReadDir(filepath.Dir(f.source)); len(entries) != 1 {
		t.Fatal("probe names left", entries)
	}
}

func TestPersistAliasesAreFoundByIdentity(t *testing.T) {
	for name, target := range map[string]func(*persistFixture) string{
		"same":          func(f *persistFixture) string { return f.source },
		"target-inside": func(f *persistFixture) string { return f.source + "/p1/target" },
		"source-inside": func(f *persistFixture) string { return filepath.Dir(f.source) },
	} {
		t.Run(name, func(t *testing.T) {
			f := newPersistFixture(t)
			f.mappings[0].Target = target(f)
			_, stderr, err := f.run("observe", nil)
			if err == nil || !strings.Contains(stderr, "persist paths overlap") {
				t.Fatalf("alias accepted: %v %s", err, stderr)
			}
		})
	}
	t.Run("injected-ancestry", func(t *testing.T) {
		// The target is an alias of the source's parent under another path,
		// as a bind mount would be: only identities reveal S inside T.
		f := newPersistFixture(t)
		mkdirs(t, f.target)
		f.injection = fmt.Sprintf(`native_chain = chain
def chain(fd):
    if os.readlink('/proc/self/fd/%%d' %% fd) == %q:
        alias = open_path(%q)
        try:
            return native_chain(alias)
        finally:
            os.close(alias)
    return native_chain(fd)`, f.target, filepath.Dir(f.source))
		_, stderr, err := f.run("observe", nil)
		if err == nil || !strings.Contains(stderr, "persist paths overlap") {
			t.Fatalf("aliased ancestry accepted: %v %s", err, stderr)
		}
	})
}

func TestPersistBackupIdentityMismatchIsRetained(t *testing.T) {
	f := newPersistFixture(t)
	f.interrupt(persistCuts["exchanged"])
	backup := f.backup()
	if err := os.Rename(backup, f.root+"/original"); err != nil {
		t.Fatal(err)
	}
	mkdirs(t, backup)
	testkit.WriteFile(t, backup+"/foreign", []byte("foreign\n"), 0o600)
	if ops := f.ops(); ops != "finish" {
		t.Fatal("remnant ops", ops)
	}
	result := f.mustApply()
	if read(t, backup+"/foreign") != "foreign\n" || read(t, f.root+"/original/p1/s.jsonl") != "0\n1\n" {
		t.Fatal("a backup without the recorded identity was deleted")
	}
	if len(f.records()) != 0 || !strings.Contains(fmt.Sprint(result["results"]), backup) {
		t.Fatal("finish did not remove the descriptor or report the retained backup", f.records(), result)
	}
	if paths := f.retainedPaths(); !slices.Equal(paths, []string{backup}) {
		t.Fatal("retained listing", paths)
	}
}

// Reported as a different filesystem for this entry name: a mount point.
func mountedAt(name string) string {
	return fmt.Sprintf(`native_lstat = lstat
class Mounted:
    def __init__(self, st):
        self.st = st
    def __getattr__(self, field):
        return self.st.st_dev + 1 if field == 'st_dev' else getattr(self.st, field)
def lstat(fd, name):
    st = native_lstat(fd, name)
    return Mounted(st) if st is not None and name == %[1]q else st
native_mount_id = mount_id
def mount_id(fd):
    path = os.readlink('/proc/self/fd/%%d' %% fd)
    return native_mount_id(fd) + (1 if path.endswith('/' + %[1]q) or '/' + %[1]q + '/' in path else 0)`, name)
}

// A bind mount of the same filesystem: equal st_dev, different mount identity.
const boundAtBind = `native_mount_id = mount_id
def mount_id(fd):
    path = os.readlink('/proc/self/fd/%d' % fd)
    return native_mount_id(fd) + (1 if path.endswith('/bind') or '/bind/' in path else 0)`

func TestPersistDeleteBackupNeverEntersAMountOrFollowsALink(t *testing.T) {
	for name, injection := range map[string]string{"mount": mountedAt("mnt"), "bind": boundAtBind} {
		t.Run(name, func(t *testing.T) {
			f := newPersistFixture(t)
			f.interrupt(persistCuts["exchanged"])
			mount := map[string]string{"mount": "mnt", "bind": "bind"}[name]
			mounted := f.backup() + "/p1/" + mount
			mkdirs(t, mounted)
			testkit.WriteFile(t, mounted+"/data", []byte("other filesystem\n"), 0o600)
			// A mounted file too: its backing file is never unlinked.
			mkdirs(t, f.backup()+"/q")
			testkit.WriteFile(t, f.backup()+"/q/"+mount, []byte("mounted file\n"), 0o600)
			outside := f.root + "/outside"
			mkdirs(t, outside)
			testkit.WriteFile(t, outside+"/keep", []byte("keep\n"), 0o600)
			if err := os.Symlink(outside, f.backup()+"/p1/out"); err != nil {
				t.Fatal(err)
			}
			f.injection = injection
			if _, stderr, err := f.apply(); err != nil {
				t.Fatalf("finish: %v %s", err, stderr)
			}
			if read(t, mounted+"/data") != "other filesystem\n" || read(t, outside+"/keep") != "keep\n" || read(t, f.backup()+"/q/"+mount) != "mounted file\n" {
				t.Fatal("delete_backup entered a mount or followed a link")
			}
			f.injection = ""
			if paths := f.retainedPaths(); !slices.Contains(paths, f.backup()) || len(f.records()) != 0 {
				t.Fatal("the backup holding a mount is not reported as retained", paths, f.records())
			}
		})
	}
	t.Run("read-only-directories", func(t *testing.T) {
		f := newPersistFixture(t)
		f.interrupt(persistCuts["exchanged"])
		if err := os.Chmod(f.backup()+"/p1", 0o500); err != nil {
			t.Fatal(err)
		}
		f.mustApply()
		f.assertDone()
	})
}

func TestPersistDiscardStageLeavesForeignEntries(t *testing.T) {
	f := newPersistFixture(t)
	f.interrupt(persistCuts["publishing"])
	stage := f.stages()[0]
	testkit.WriteFile(t, stage+"/foreign", []byte("foreign\n"), 0o600)
	testkit.WriteFile(t, stage+"/.subyard-copy-ffffffffffffffff-1", []byte("other transfer\n"), 0o600)
	mkdirs(t, stage+"/sub")
	f.mappings[0].Selected = false
	f.mustApply()
	if read(t, stage+"/foreign") != "foreign\n" || read(t, stage+"/.subyard-copy-ffffffffffffffff-1") != "other transfer\n" || !isDirectory(stage+"/sub") {
		t.Fatal("discard_stage removed a foreign entry")
	}
	entries, _ := os.ReadDir(stage)
	if len(entries) != 3 || len(f.records()) != 0 {
		t.Fatal("own copies or the descriptor left", entries, f.records())
	}
	if paths := f.retainedPaths(); !slices.Equal(paths, []string{stage}) {
		t.Fatal("retained stage not listed", paths)
	}
	// Retained items are terminal: a new adoption is not blocked by them.
	f.mappings[0].Selected = true
	f.mustApply()
	if link, _ := os.Readlink(f.source); link != f.target || !slices.Equal(f.retainedPaths(), []string{stage}) {
		t.Fatal("retained stage blocked adoption or was removed")
	}
}

func TestPersistFinishIsIdempotent(t *testing.T) {
	f := newPersistFixture(t)
	f.interrupt(persistCuts["exchanged"])
	// A missing stage is a no-op.
	for _, stage := range f.stages() {
		if err := os.RemoveAll(stage); err != nil {
			t.Fatal(err)
		}
	}
	f.interrupt(persistCuts["finishing"])
	f.interrupt(persistCuts["finishing"])
	f.mustApply()
	f.assertDone()
}

func TestPersistRetainedListingIsReadOnlyWithSizes(t *testing.T) {
	f := newPersistFixture(t)
	f.mustApply()
	backup := f.source + ".subyard-backup-0123456789abcdef"
	mkdirs(t, backup)
	testkit.WriteFile(t, backup+"/a", []byte("12345"), 0o600)
	stage := filepath.Dir(f.target) + "/.subyard-stage-default-0123456789abcdef"
	mkdirs(t, stage+"/locked")
	if err := os.Chmod(stage+"/locked", 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(stage+"/locked", 0o700) })
	f.mappings[0].Selected, f.start = false, false
	retained := f.retained()
	if len(retained) != 2 || retained[0]["path"] != backup || retained[0]["kind"] != "backup" || retained[0]["bytes"] == nil ||
		retained[1]["path"] != stage || retained[1]["bytes"] != nil {
		t.Fatal("retained listing", retained)
	}
	if read(t, backup+"/a") != "12345" || f.ops() != "" {
		t.Fatal("listing changed data or retained items became work")
	}
	size := int64(5)
	lines := (Runtime{Yard: domain.Context{YardInstanceName: "yard-default", IncusProject: "subyard-default"}}).PersistRetainedLines(
		[]PersistRetained{{Kind: "backup", Path: backup, Bytes: &size}, {Kind: "stage", Path: stage}})
	if len(lines) != 2 || !strings.Contains(lines[0], "(5 bytes)") || !strings.Contains(lines[1], "size unknown") || !strings.Contains(lines[1], "'ls' '-la'") {
		t.Fatal("retained lines", lines)
	}
}

func TestPersistMountPlacedAfterLookupIsRefused(t *testing.T) {
	t.Run("scan", func(t *testing.T) {
		f := newPersistFixture(t)
		mounted := f.source + "/p1"
		f.injection = racedMountAt(mounted)
		_, stderr, err := f.run("observe", nil)
		if err == nil || !strings.Contains(stderr, "persist mount boundary") || !strings.Contains(stderr, mounted) {
			t.Fatalf("scan descended into a directory mounted after lookup: %v %s", err, stderr)
		}
	})
	t.Run("publish", func(t *testing.T) {
		f := newPersistFixture(t)
		mkdirs(t, f.target+"/p1")
		mounted := f.target + "/p1"
		f.injection = racedMountAt(mounted, "publish")
		value, stderr, err := f.apply()
		if err != nil {
			t.Fatalf("apply: %v %s", err, stderr)
		}
		if entries, _ := os.ReadDir(mounted); len(entries) != 0 || !strings.Contains(fmt.Sprint(value["results"]), "p1") {
			t.Fatal("publish wrote below a directory mounted after lookup", entries, value)
		}
	})
}

func racedMountAt(path string, during ...string) string {
	quoted := make([]string, len(during))
	for i, value := range during {
		quoted[i] = fmt.Sprintf("%q", value)
	}
	return fmt.Sprintf(`DURING = [%s]
def placing():
    names, frame = set(), sys._getframe()
    while frame:
        names.add(frame.f_code.co_name)
        frame = frame.f_back
    return all(name in names for name in DURING)
native_mount_id = mount_id
native_open, raced, RACED = os.open, [], %q
def inside(path):
    return path == RACED or path.startswith(RACED + '/')
def raced_open(name, flags, *args, dir_fd=None, **kwargs):
    full = name if dir_fd is None else os.readlink('/proc/self/fd/%%d' %% dir_fd) + '/' + name
    if inside(full) and not flags & os.O_PATH and not raced and placing():
        raced.append(full)
    return native_open(name, flags, *args, dir_fd=dir_fd, **kwargs)
os.open = raced_open
def mount_id(fd):
    return native_mount_id(fd) + (1 if raced and inside(os.readlink('/proc/self/fd/%%d' %% fd)) else 0)`, strings.Join(quoted, ", "), path)
}

// The safety argument is a source-level contract: destructive calls appear
// only in the three bounded functions, and every rename is no-replace or the
// exchange.
func TestPersistDestructiveSyscallsOnlyInBoundedFunctions(t *testing.T) {
	checker := `import ast, sys
tree = ast.parse(sys.stdin.read())
DESTRUCTIVE = {'unlink', 'remove', 'rmdir', 'removedirs', 'rename', 'replace', 'truncate', 'ftruncate', 'rmtree', 'unlinkat'}
ALLOWED = {'discard_stage', 'delete_backup', 'remove_own_records'}
found = []
for top in tree.body:
    for node in ast.walk(top):
        if not isinstance(node, ast.Call):
            continue
        name = node.func.attr if isinstance(node.func, ast.Attribute) else getattr(node.func, 'id', '')
        owner = getattr(top, 'name', '<module>')
        if name in DESTRUCTIVE:
            found.append(('destructive', name, owner))
        if name == 'renameat2' and owner != 'renameat2':
            flags = node.args[4] if len(node.args) > 4 else None
            if not (isinstance(flags, ast.Name) and flags.id in ('NOREPLACE', 'EXCHANGE')):
                found.append(('rename', ast.dump(flags) if flags else '', owner))
        if name == 'open' and any(isinstance(n, ast.Attribute) and n.attr == 'O_TRUNC' for n in ast.walk(node)):
            found.append(('truncate', 'O_TRUNC', owner))
        if name in ('open', 'fdopen') and any(isinstance(a, ast.Constant) and isinstance(a.value, str) and ('w' in a.value or 'a' in a.value) and owner != 'adopt' for a in node.args):
            found.append(('write', name, owner))
print('\n'.join('%s %s %s' % f for f in found))`
	command := exec.Command("python3", "-B", "-c", checker)
	command.Stdin = strings.NewReader(integrationPersistProgram)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("contract checker: %v %s", err, output)
	}
	sites := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[0] != "destructive" || !slices.Contains([]string{"discard_stage", "delete_backup", "remove_own_records"}, fields[2]) {
			t.Fatalf("destructive call outside the bounded functions: %q", line)
		}
		sites[fields[2]] = true
	}
	if len(sites) != 3 {
		t.Fatal("contract checker found no destructive sites; it is not inspecting the program", sites)
	}
}

func TestPersistProjectionKeepsInitialInventoryPlanBound(t *testing.T) {
	root := testkit.TempDir(t)
	home, state, target := root+"/home", root+"/state", root+"/sessions/claude/projects"
	source := home + "/.claude/projects"
	for _, path := range []string{source, target} {
		mkdirs(t, path)
	}
	developer, gid := inventoryDeveloper(t)
	entries := []integrationArtifact{{ID: "claude", Kind: "link", Path: source, Target: target, Digest: fmt.Sprintf("%x", sha256.Sum256([]byte(target)))}}
	program := strings.NewReplacer("@STATE_ROOT@", state, "@STATE_UID@", fmt.Sprint(os.Getuid())).Replace(integrationInventoryProgram)
	run := func(mode string, persist map[string]string, expected string) (string, error) {
		payload, _ := json.Marshal(map[string]any{"home": home, "uid": os.Getuid(), "developer": developer, "entries": entries, "persist": persist, "expected": expected})
		command := exec.Command("python3", "-B", "-c", program, mode)
		command.Stdin = strings.NewReader(string(payload))
		output, err := command.CombinedOutput()
		return string(output), err
	}
	if output, err := run("observe", nil, ""); err == nil || !strings.Contains(output, "unowned selected artifact") {
		t.Fatalf("foreign directory accepted without projection: %v %s", err, output)
	}
	if output, err := run("observe", map[string]string{source: "deferred"}, ""); err != nil {
		t.Fatalf("deferred path reported as artifact conflict: %v %s", err, output)
	}
	output, err := run("observe", map[string]string{source: "projected"}, "")
	var observed struct{ Fingerprint string }
	if err != nil || json.Unmarshal([]byte(output), &observed) != nil {
		t.Fatalf("projection refused: %v %s", err, output)
	}
	if output, err := run("apply", map[string]string{source: "projected"}, observed.Fingerprint); err == nil {
		t.Fatalf("projection accepted before the transfer published its link: %s", output)
	}
	// The persist transfer exchanges the directory for the exact link.
	if err := os.Rename(source, source+".aside"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, source); err != nil {
		t.Fatal(err)
	}
	if err := os.Lchown(source, os.Getuid(), gid); err != nil {
		t.Fatal(err)
	}
	if output, err := run("apply", map[string]string{source: "projected"}, observed.Fingerprint); err != nil {
		t.Fatalf("initial plan stale after the exchange: %v %s", err, output)
	}
	if output, err := run("commit", map[string]string{source: "projected"}, ""); err != nil {
		t.Fatalf("commit: %v %s", err, output)
	}
	if output, err := run("observe", nil, ""); err != nil || !strings.Contains(output, `"changed": false`) {
		t.Fatalf("recorded link not converged: %v %s", err, output)
	}
}

func TestPersistRefusalsArePendingConfigurationWithNextSteps(t *testing.T) {
	runtime := Runtime{Yard: domain.Context{YardName: "default", YardInstanceName: "yard-default", IncusProject: "subyard-default"}}
	stderr, _ := json.Marshal(map[string]any{"conflicts": []map[string]string{
		{"reason": "persist transfer identity unavailable", "path": "/home/dev/.claude/projects.subyard-transfer.json", "integration": "claude"},
		{"reason": "atomic exchange unsupported", "path": "", "detail": "Invalid argument", "integration": "claude"},
		{"reason": "persist adoption pending", "path": "/home/dev/.claude-memory", "integration": "claude"},
		{"reason": "persist filesystem error", "path": "/home/dev/.claude/projects/p", "detail": "Permission denied", "integration": "claude"},
		{"reason": "persist mount boundary", "path": "/home/dev/.claude/projects/mnt", "integration": "claude"},
		{"reason": "persist paths overlap", "path": "/home/dev/.claude/projects", "detail": "/home/dev/.claude/projects/x", "integration": "claude"},
		{"reason": "a future reason", "path": "/home/dev/.claude/projects/x", "integration": "claude"},
		{"reason": "persist target entry refused", "path": "/mnt/host/agent-sessions/claude/projects/p1/x", "detail": "Permission denied", "integration": "claude"},
	}, "entries": []map[string]any{{"source": "/home/dev/.claude-memory", "persist": "deferred"}}})
	err := runtime.persistRefusal(stderr)
	text := err.Error()
	for _, want := range []string{"projects.subyard-transfer.json", "not a complete Subyard record", "'disable' 'claude'", "release activation does not start", "init", "Permission denied",
		"'namei' '-l' '--' '/home/dev/.claude/projects/p'", "'findmnt'", "AGENT_claude_PERSIST", "a future reason", "'integration' 'status'",
		"the target refused \"/mnt/host/agent-sessions/claude/projects/p1/x\"", "'namei' '-l' '--' '/mnt/host/agent-sessions/claude/projects/p1/'"} {
		if !strings.Contains(text, want) {
			t.Fatalf("refusal omitted %q: %s", want, text)
		}
	}
	var refused persistRefused
	if !errors.As(err, &refused) || len(refused.entries) != 1 || refused.entries[0].Persist != "deferred" {
		t.Fatal("refused entries not carried for the inventory projection")
	}
	var refusals *domain.Refusals
	if !errors.As(err, &refusals) || len(refusals.Unwrap()) != 8 {
		t.Fatal("refusals were not all reported", err)
	}
	for _, cause := range refusals.Unwrap() {
		var pending interface{ PendingIntegrationConfiguration() bool }
		if !errors.As(cause, &pending) || !pending.PendingIntegrationConfiguration() {
			t.Fatalf("refusal is not pending configuration: %v", cause)
		}
	}
	for _, raw := range []string{"not json", `{"error": "MemoryError"}`} {
		var pending interface{ PendingIntegrationConfiguration() bool }
		if err := runtime.persistRefusal([]byte(raw)); !errors.As(err, &pending) || !pending.PendingIntegrationConfiguration() ||
			!strings.Contains(err.Error(), "data was preserved") {
			t.Fatalf("unexplained failure is not pending configuration: %v", err)
		}
	}
	desired, _ := json.Marshal(persistStep{Ops: []string{"finish"}, Source: "/home/dev/.claude/projects"})
	if source, text, ok := persistStepText(integrationFact{Key: "persist:finish:/home/dev/.claude/projects", Desired: string(desired)}); !ok ||
		source != "/home/dev/.claude/projects" || !strings.Contains(text, "removing its stage copies and the original directory") {
		t.Fatal("finish step text", text)
	}
	// The Orca adoption lane greps a deselect (integration disable) plan for this text.
	desired, _ = json.Marshal(persistStep{Ops: []string{"cancel"}, Source: "/home/dev/.claude/projects"})
	if _, text, ok := persistStepText(integrationFact{Key: "persist:cancel:/home/dev/.claude/projects", Desired: string(desired)}); !ok ||
		!strings.Contains(text, "cancel the interrupted adoption of /home/dev/.claude/projects") {
		t.Fatal("cancel step text", text)
	}
}

func TestInventoryReportsAllIndependentForeignFiles(t *testing.T) {
	root := testkit.TempDir(t)
	home := root + "/home"
	mkdirs(t, home)
	entries := []integrationArtifact{}
	for _, name := range []string{"second", "first"} {
		path := home + "/" + name
		testkit.WriteFile(t, path, []byte("foreign"), 0o644)
		entries = append(entries, integrationArtifact{ID: "sample", Kind: "file", Path: path, Digest: strings.Repeat("a", 64)})
	}
	program := strings.NewReplacer("@STATE_ROOT@", root+"/state", "@STATE_UID@", fmt.Sprint(os.Getuid())).Replace(integrationInventoryProgram)
	payload, _ := json.Marshal(map[string]any{"home": home, "uid": os.Getuid(), "entries": entries})
	command := exec.Command("python3", "-B", "-c", program, "observe")
	command.Stdin = strings.NewReader(string(payload))
	output, err := command.CombinedOutput()
	var report struct {
		Conflicts []struct{ Reason, Path string }
	}
	if err == nil || json.Unmarshal(output, &report) != nil || len(report.Conflicts) != 2 {
		t.Fatalf("incomplete refusals: %v %s", err, output)
	}
	for _, name := range []string{"first", "second"} {
		if read(t, home+"/"+name) != "foreign" {
			t.Fatal("foreign file changed")
		}
	}
}

func (f *persistFixture) observation() persistObservation {
	f.t.Helper()
	payload, _ := json.Marshal(f.observe())
	var observation persistObservation
	if err := json.Unmarshal(payload, &observation); err != nil {
		f.t.Fatal(err)
	}
	return observation
}

// Init confirms a persist plan before provisioning recomputes it. Execution
// refuses a fresh transfer that differs from the confirmed one.
func TestPersistExecutionEnforcesTheConfirmedPlan(t *testing.T) {
	f := newPersistFixture(t)
	confirmed := f.observation().Fingerprint
	runtime := Runtime{Yard: domain.Context{YardName: "default"}, ApprovedPersist: &confirmed}
	if err := runtime.checkApprovedPersist(f.observation()); err != nil {
		t.Fatal("unchanged plan refused", err)
	}
	// S is replaced by another safe directory while confirmation waits.
	if err := os.Rename(f.source, f.root+"/original"); err != nil {
		t.Fatal(err)
	}
	mkdirs(t, f.source+"/other")
	testkit.WriteFile(t, f.source+"/other/s.jsonl", []byte("other\n"), 0o600)
	err := runtime.checkApprovedPersist(f.observation())
	var pending interface{ PendingIntegrationConfiguration() bool }
	if !errors.Is(err, domain.ErrPlanStale) || !errors.As(err, &pending) || !pending.PendingIntegrationConfiguration() ||
		!strings.Contains(err.Error(), "'yard' '-Y' 'default' 'init'") {
		t.Fatalf("replaced source not refused as a stale pending plan: %v", err)
	}
	if len(f.records())+len(f.stages()) != 0 || !isDirectory(f.source) {
		t.Fatal("refused execution wrote state")
	}
	// A transfer appearing where none was confirmed is refused too.
	none := ""
	if err := (Runtime{Yard: domain.Context{YardName: "default"}, ApprovedPersist: &none}).checkApprovedPersist(f.observation()); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("unconfirmed transfer accepted", err)
	}
	// Without a confirmed binding (other commands bind their own plan) nothing changes.
	if err := (Runtime{}).checkApprovedPersist(f.observation()); err != nil {
		t.Fatal(err)
	}
}

// A fresh host runs init before Incus exists; inspecting persist transfers must
// not fail on the missing server.
func TestPersistInspectionWithoutIncusInspectsNothing(t *testing.T) {
	runtime := Runtime{Incus: &testkit.Incus{Err: errors.New("dial unix /var/lib/incus/unix.socket: connect: no such file or directory")}}
	plan, inspected, err := runtime.InspectPersistTransfers(context.Background())
	if err != nil || inspected || plan.Changed {
		t.Fatalf("plan=%#v inspected=%v err=%v", plan, inspected, err)
	}
}

// An adoption of two mappings is cut after the first finished but before the
// inventory recorded its link. Disabling the integration cancels the second and
// removes the first one's exact link, disclosed in the plan; the data stays in T.
func TestPersistDisableRemovesTheUnrecordedLinkOfAnInterruptedAdoption(t *testing.T) {
	f := newPersistFixture(t)
	memory, memoryTarget := f.root+"/home/.claude-memory", f.root+"/sessions/claude/memory"
	mkdirs(t, memory)
	testkit.WriteFile(t, memory+"/MEMORY.md", []byte("remember\n"), 0o600)
	f.mappings = append([]PersistMapping{{Integration: "claude", Source: memory, Target: memoryTarget, Selected: true}}, f.mappings...)
	// The first published copy is memory's only file; the second cuts projects.
	f.interrupt(persistCuts["publishing"])
	if link, _ := os.Readlink(memory); link != memoryTarget || !isDirectory(f.source) || len(f.records()) != 1 {
		t.Fatal("cut did not land between the two transfers", link, f.records())
	}
	for index := range f.mappings {
		f.mappings[index].Selected = false
	}
	observation := f.observation()
	// The inventory records no link for memory, so the transfer removes it.
	planned := observation.retireUnrecordedLinks(nil)
	ops := map[string]string{}
	for _, entry := range planned.Entries {
		ops[entry.Source] = strings.Join(entry.Ops, "+")
	}
	if ops[memory] != "retire" || ops[f.source] != "cancel" || !planned.changed() {
		t.Fatal("disable plan", ops)
	}
	disclosed := false
	for _, fact := range planned.facts() {
		if source, text, _ := persistStepText(fact); source == memory &&
			strings.Contains(text, "remove the link "+memory+" to "+memoryTarget) && strings.Contains(text, "the data remains in "+memoryTarget) {
			disclosed = true
		}
	}
	if !disclosed {
		t.Fatal("disable plan does not disclose the link removal", planned.facts())
	}
	// A link the inventory records is retired by the inventory, not here.
	recorded := observation.retireUnrecordedLinks([]integrationArtifact{{Kind: "link", Path: memory}})
	for _, entry := range recorded.Entries {
		if entry.Source == memory && len(entry.Ops) != 0 {
			t.Fatal("recorded link planned twice", entry.Ops)
		}
	}
	if _, stderr, err := f.run("apply", map[string]any{"expected": observation.Fingerprint, "retire": planned.retireSources()}); err != nil {
		t.Fatal("disable apply", err, stderr)
	}
	if _, err := os.Lstat(memory); !os.IsNotExist(err) {
		t.Fatal("memory link left after disable", err)
	}
	if read(t, memoryTarget+"/MEMORY.md") != "remember\n" || !isDirectory(f.source) || len(f.records())+len(f.stages()) != 0 {
		t.Fatal("disable lost data or kept transfer state", f.records(), f.stages())
	}
	if after := f.observation().retireUnrecordedLinks(nil); after.changed() {
		t.Fatal("disable did not converge", after.Entries)
	}
	// Re-enabling finds no source: the inventory creates the link normally.
	for index := range f.mappings {
		f.mappings[index].Selected = true
	}
	for _, entry := range f.observation().Entries {
		if entry.Source == memory && (len(entry.Ops) != 0 || entry.Persist != "") {
			t.Fatal("re-enable planned a transfer for the absent memory source", entry)
		}
	}
	developer, _ := inventoryDeveloper(t)
	entries := []integrationArtifact{{ID: "claude", Kind: "link", Path: memory, Target: memoryTarget, Digest: fmt.Sprintf("%x", sha256.Sum256([]byte(memoryTarget)))}}
	program := strings.NewReplacer("@STATE_ROOT@", f.root+"/state", "@STATE_UID@", fmt.Sprint(os.Getuid())).Replace(integrationInventoryProgram)
	inventory := func(mode, expected string) string {
		payload, _ := json.Marshal(map[string]any{"home": f.root + "/home", "uid": os.Getuid(), "developer": developer, "entries": entries, "expected": expected})
		command := exec.Command("python3", "-B", "-c", program, mode)
		command.Stdin = strings.NewReader(string(payload))
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("inventory %s: %v %s", mode, err, output)
		}
		return string(output)
	}
	var observed struct{ Fingerprint string }
	if json.Unmarshal([]byte(inventory("observe", "")), &observed) != nil {
		t.Fatal("inventory observation")
	}
	inventory("apply", observed.Fingerprint)
	inventory("commit", "")
	if link, _ := os.Readlink(memory); link != memoryTarget || read(t, memory+"/MEMORY.md") != "remember\n" {
		t.Fatal("re-enable did not recreate the memory link", link)
	}
}

// Only an exact own link of a deselected mapping is removable, and only by init
// or disable; activation and a request naming anything else change nothing.
func TestPersistRetireIsBoundedToTheExactLinkOfADeselectedMapping(t *testing.T) {
	retirable := func(f *persistFixture) bool {
		entry := f.entry()
		return entry["retirable"] == true
	}
	f := newPersistFixture(t)
	f.mustApply()
	f.mappings[0].Selected = false
	if !retirable(f) {
		t.Fatal("exact own link of a deselected mapping is not removable")
	}
	f.start = false
	if retirable(f) {
		t.Fatal("activation may remove a link")
	}
	f.start, f.mappings[0].Selected = true, true
	if retirable(f) {
		t.Fatal("selected link is removable")
	}
	value, _, _ := f.run("observe", nil)
	if _, stderr, err := f.run("apply", map[string]any{"expected": value["fingerprint"], "retire": []string{f.source}}); err == nil || !strings.Contains(stderr, "integration plan is stale") {
		t.Fatal("retire of a selected link accepted", err, stderr)
	}
	if link, _ := os.Readlink(f.source); link != f.target {
		t.Fatal("refused retire changed the link")
	}
	if err := os.Remove(f.source); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.root+"/elsewhere", f.source); err != nil {
		t.Fatal(err)
	}
	f.mappings[0].Selected = false
	if retirable(f) {
		t.Fatal("a link to another target is removable")
	}
}

// A deselected exact link without transfer records is retire-only work. Init
// must disclose and bind it, or provisioning refuses the unconfirmed retire.
func TestPersistInitInspectionBindsARetireOnlyPlan(t *testing.T) {
	f := newPersistFixture(t)
	f.mustApply()
	f.mappings[0].Selected = false
	full := func() (IntegrationPlan, error) {
		// The full plan applies the inventory narrowing: nothing records the link.
		persist := f.observation().retireUnrecordedLinks(nil)
		return IntegrationPlan{Changed: persist.changed(), persist: persist, facts: persist.facts()}, nil
	}
	plan, inspected, err := persistInspectionPlan(f.observation(), nil, full)
	if err != nil || !inspected {
		t.Fatal(err)
	}
	disclosed := false
	for _, step := range plan.PersistOperationSteps("yard") {
		disclosed = disclosed || strings.Contains(step.Desired, "remove the link "+f.source)
	}
	if !disclosed {
		t.Fatal("init inspection does not disclose the retire", plan.PersistOperationSteps("yard"))
	}
	approved := plan.PersistFingerprint()
	fresh, _ := full()
	if err := (Runtime{Yard: domain.Context{YardName: "default"}, ApprovedPersist: &approved}).checkApprovedPersist(fresh.persist); err != nil {
		t.Fatal("provisioning refused the confirmed retire", err)
	}
	// With the link recorded the full plan has no persist work and nothing is bound.
	f.mappings[0].Selected = true
	if plan, inspected, err := persistInspectionPlan(f.observation(), nil, func() (IntegrationPlan, error) { t.Fatal("full plan without work"); return IntegrationPlan{}, nil }); err != nil || !inspected || plan.persist.changed() {
		t.Fatal("converged inspection", plan, err)
	}
}

// The bounded removal re-checks the exact link at the unlink: a regular file or
// a link to another target placed under the name after validation stays.
func TestPersistRemoveOwnRecordsKeepsAReplacedLink(t *testing.T) {
	root := testkit.TempDir(t)
	testkit.WriteFile(t, root+"/file", []byte("data\n"), 0o600)
	for _, link := range []struct{ name, target string }{{"other", root + "/elsewhere"}, {"exact", root + "/target"}} {
		if err := os.Symlink(link.target, root+"/"+link.name); err != nil {
			t.Fatal(err)
		}
	}
	developer, _ := inventoryDeveloper(t)
	script := `
import json, sys
request = json.load(sys.stdin)
drop_privileges(request)
parent = open_path(request['root'])
for name in ('file', 'other', 'exact'):
    remove_own_records(parent, [name], link=request['root'] + '/target')
`
	program := strings.Replace(integrationPersistProgram, "\nif __name__ == '__main__':", "\n"+script+"\nif False:", 1)
	payload, _ := json.Marshal(map[string]any{"root": root, "developer": developer, "uid": os.Getuid()})
	command := exec.Command("python3", "-B", "-c", program)
	command.Stdin = strings.NewReader(string(payload))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("helper: %v %s", err, output)
	}
	if read(t, root+"/file") != "data\n" {
		t.Fatal("regular file removed by an exact-link removal")
	}
	if link, _ := os.Readlink(root + "/other"); link != root+"/elsewhere" {
		t.Fatal("link to another target removed")
	}
	if _, err := os.Lstat(root + "/exact"); !os.IsNotExist(err) {
		t.Fatal("exact link kept", err)
	}
}

// Provisioning recreates the integration substrate. When only that
// precondition blocks the full plan, init proceeds uninspected (as for a
// stopped guest) instead of refusing on every retry; other errors propagate.
func TestPersistInitInspectionWithoutTheSubstrateDefersToProvisioning(t *testing.T) {
	retire := newPersistFixture(t)
	retire.mustApply()
	retire.mappings[0].Selected = false
	pending := newPersistFixture(t)
	pending.interrupt(persistCuts["publishing"])
	pending.mappings[0].Selected = false
	for name, f := range map[string]*persistFixture{"retirable": retire, "cancel": pending} {
		t.Run(name, func(t *testing.T) {
			observation := f.observation()
			unavailable := func() (IntegrationPlan, error) { return IntegrationPlan{}, errIntegrationSubstrateUnavailable }
			if plan, inspected, err := persistInspectionPlan(observation, nil, unavailable); err != nil || inspected || plan.persist.changed() {
				t.Fatalf("missing substrate is a dead end: inspected=%v err=%v", inspected, err)
			}
			other := errors.New("integration reconcile requires a running yard; start it explicitly first")
			if _, inspected, err := persistInspectionPlan(observation, nil, func() (IntegrationPlan, error) { return IntegrationPlan{}, other }); !errors.Is(err, other) || inspected {
				t.Fatalf("other full-plan error swallowed: inspected=%v err=%v", inspected, err)
			}
		})
	}
}

// The provision stage recreates the integration substrate, so its missing
// precondition means "not converged", never an init refusal; other errors propagate.
func TestProvisionConvergenceTreatsAMissingSubstrateAsPending(t *testing.T) {
	if ok, err := integrationPlanConverged(IntegrationPlan{}, errIntegrationSubstrateUnavailable); ok || err != nil {
		t.Fatalf("missing substrate aborts init before provisioning: ok=%v err=%v", ok, err)
	}
	other := errors.New("integration reconcile requires a running yard; start it explicitly first")
	if ok, err := integrationPlanConverged(IntegrationPlan{}, other); ok || !errors.Is(err, other) {
		t.Fatalf("other plan error swallowed: ok=%v err=%v", ok, err)
	}
	if ok, err := integrationPlanConverged(IntegrationPlan{Changed: true}, nil); ok || err != nil {
		t.Fatal("changed plan reported converged")
	}
	if ok, err := integrationPlanConverged(IntegrationPlan{}, nil); !ok || err != nil {
		t.Fatal("converged plan reported pending")
	}
}
