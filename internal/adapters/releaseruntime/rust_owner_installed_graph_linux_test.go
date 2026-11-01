//go:build linux

package releaseruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releaseowner"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/uninstall"
)

// Close alone may force-kill the coordinator and does not report its Wait error.
// Only a successful coordinator exit follows the ordinary native-child drain path.
func closeInstalledGraphOwner(session *rustOwnerSession) error {
	if err := session.Close(); err != nil {
		return err
	}
	select {
	case <-session.done:
	default:
		return fmt.Errorf("installed owner coordinator Wait is incomplete")
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.exitErr != nil {
		return fmt.Errorf("installed owner coordinator did not exit successfully: %w", session.exitErr)
	}
	return nil
}

func TestInstalledGraphOwnerCloseRequiresSuccessfulWait(t *testing.T) {
	for _, test := range []struct {
		name, script string
		wantError    bool
	}{
		{"normal", "exit 0", false},
		{"nonzero", "exit 7", true},
		{"forced-kill", "exec sleep 60", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := exec.Command("sh", "-c", test.script)
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			session := &rustOwnerSession{command: command, done: make(chan struct{})}
			go func() {
				err := command.Wait()
				session.mu.Lock()
				session.exitErr = err
				session.mu.Unlock()
				close(session.done)
			}()
			t.Cleanup(func() { _ = session.Close() })
			if err := closeInstalledGraphOwner(session); (err != nil) != test.wantError {
				t.Fatalf("successful Wait requirement: got %v, want error %t", err, test.wantError)
			}
		})
	}
}

// This opt-in check consumes an authentic completed Go0.22 source. It neither
// authors protected transition state nor replaces native providers or dispatch.
func TestRustOwnerInstalledNormalGraph(t *testing.T) {
	if os.Getenv("SUBYARD_RUST_INSTALLED_GRAPH_FIRST_ADMISSION") == "1" || os.Getenv("SUBYARD_RUST_INSTALLED_GRAPH_COMPENSATION") == "1" {
		t.Fatal("old-source first admission is retired; complete released Go v0.22.0 before a new Rust goal")
	}
	token := os.Getenv("SUBYARD_RUST_INSTALLED_GRAPH_TOKEN")
	if token == "" {
		t.Skip("requires the allocated installed-owner guest fixture")
	}
	if strings.Trim(token, "0123456789") != "" || os.Getenv("SUBYARD_E2E_VM") != "1" {
		t.Fatal("installed graph requires a numeric token and allocated VM1")
	}
	root := "/var/tmp/subyard-rust-owner-" + token
	fixture := filepath.Join(root, "success")
	for path, want := range map[string]string{
		filepath.Join(root, ".subyard-p0-marker"):    "subyard-rust-owner-" + token,
		filepath.Join(fixture, ".subyard-p0-marker"): "subyard-rust-owner-" + token + ":success",
	} {
		info, err := os.Lstat(filepath.Dir(path))
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatal("installed graph root is not a private fixture directory")
		}
		body, err := os.ReadFile(path)
		if err != nil || strings.TrimSpace(string(body)) != want {
			t.Fatal("installed graph marker differs")
		}
	}
	dataHome, configHome := filepath.Join(fixture, "data"), filepath.Join(fixture, "config")
	runtimeRoot := filepath.Join(dataHome, "runtime")
	if os.Getenv("SUBYARD_HOME") != dataHome || os.Getenv("SUBYARD_CONFIG_HOME") != configHome || os.Getenv("YARD_RUNTIME_ROOT") != runtimeRoot || os.Getenv("SUBYARD_INCUS_SOCKET") != "" {
		t.Fatal("installed graph environment differs from the real local fixture")
	}
	targetID := releasetransition.ReleaseID(os.Getenv("SUBYARD_RUST_INSTALLED_GRAPH_TARGET"))
	if targetID == "" || strings.ContainsAny(string(targetID), "/\\") {
		t.Fatal("an exact published target release is required")
	}
	repository := filepath.Join(runtimeRoot, "releases", string(targetID))
	catalog, err := config.LoadCatalog(repository)
	if err != nil {
		t.Fatal("load installed candidate catalog:", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	environment := os.Environ()
	inputs := CaptureCommandInputs(environment, func(name string) bool { _, ok := catalog.LookupSetting(name); return ok })
	runtime := New(Config{Inputs: inputs, Context: RuntimeContext{OperatorHome: filepath.Join(fixture, "home"), DataHome: dataHome, ConfigHome: configHome, RuntimeRoot: runtimeRoot}, RepositoryRoot: repository, WorkingDir: repository, Stderr: os.Stderr})
	defer runtime.Close()
	target, err := runtime.verifyPublishedCandidate(ctx, publishedCandidate{release: targetID, root: repository}, runtimeRoot, nil)
	if err != nil {
		t.Fatal("verify exact published normal candidate:", err)
	}
	defer target.Close()
	if target.releaseOwner == nil {
		t.Fatal("candidate has no verified normal Rust owner declaration")
	}
	links, err := releasetransition.NewRuntimeLinkStore(runtimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	sourceID := releasetransition.ReleaseID(os.Getenv("SUBYARD_RUST_INSTALLED_GRAPH_SOURCE"))
	beforeLinks, err := links.Observe()
	if err != nil || sourceID != "0.22.0-11e9174b7cac" || beforeLinks.Active != sourceID || beforeLinks.Previous != nil || beforeLinks.Active == targetID {
		t.Fatal("clean authentic Go0.22 source links differ", err)
	}
	store, err := releasetransition.NewPOSIXV2Store(configHome)
	if err != nil {
		t.Fatal(err)
	}
	readJournal := func() ([]byte, releasetransition.JournalRecord) {
		t.Helper()
		snapshot, err := store.ReadCurrentJournal()
		if err != nil || !snapshot.Exists {
			t.Fatal("protected current journal is missing", err)
		}
		journal, err := releasetransition.ParseJournal(snapshot.Payload)
		if err != nil {
			t.Fatal("protected current journal is invalid", err)
		}
		return snapshot.Payload, journal
	}
	original, sourceJournal := readJournal()
	if sourceJournal.Checkpoint != releasetransition.JournalComplete || sourceJournal.Goal.Target != sourceID || sourceJournal.Releases.Target != sourceID || sourceJournal.Releases.From != sourceID || sourceJournal.Releases.Previous != nil {
		t.Fatal("source is not a genuine completed Go0.22 self-activation")
	}
	for _, step := range sourceJournal.Steps {
		if step.Checkpoint != releasetransition.StepVerified {
			t.Fatal("Go0.22 source has an unverified migration step")
		}
	}
	sourceRoot := filepath.Join(runtimeRoot, "releases", string(sourceID))
	source, err := runtime.verifyPublishedCandidate(ctx, publishedCandidate{release: sourceID, root: sourceRoot}, runtimeRoot, &sourceJournal.ArtifactDigest)
	if err != nil {
		t.Fatal("verify authentic completed Go0.22 source:", err)
	}
	defer source.Close()
	if source.version != "0.22.0" || source.releaseOwner != nil || source.registryDigest != "e4d70df389a0eaac35bd309f864f69c5609a9e3b395e69334441187a96d49d20" {
		t.Fatal("completed source does not have the authenticated Go0.22 registry")
	}
	raw, err := os.ReadFile(filepath.Join(sourceRoot, "config/release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	registry, digest, err := releasetransition.ParseRegistryV2(raw, releasetransition.BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := store.InspectSourceMetadata(registry, digest)
	if err != nil {
		t.Fatal("Go0.22 source metadata is not closed:", err)
	}
	for domain, epoch := range registry.CurrentEpochs {
		if metadata.Ledger.Ledger.Domains[domain].Epoch != epoch {
			t.Fatal("Go0.22 source has not completed its own registry frontier")
		}
	}
	if _, err := os.Stat(filepath.Join(configHome, "yards/test-yard/config.env")); err != nil {
		t.Fatal("ordinary canonical registration is missing", err)
	}
	nativeUUID := func() string {
		t.Helper()
		raw, err := exec.CommandContext(ctx, "incus", "config", "get", "yard-test-yard", "volatile.uuid", "--project", "subyard-test-yard").Output()
		if err != nil || strings.TrimSpace(string(raw)) == "" {
			t.Fatal("real canonical instance UUID is unavailable", err)
		}
		return strings.TrimSpace(string(raw))
	}
	uuid, err := os.ReadFile(filepath.Join(fixture, "source-instance.uuid"))
	if err != nil || strings.TrimSpace(string(uuid)) != nativeUUID() {
		t.Fatal("source native identity differs from ordinary Go init", err)
	}
	request := releasetransition.ProcessRequest{SchemaVersion: releasetransition.ProcessProtocolSchemaV1, Mode: releasetransition.ProcessInspect, RuntimeRoot: runtimeRoot, ConfigHome: configHome, Yard: "default", Target: targetID, Direction: releasetransition.DirectionActivateTarget, ArtifactDigest: target.manifestDigest, RegistryDigest: target.registryDigest, InheritedSettingIDs: inputs.InheritedSettingIDs()}
	open := func() *rustOwnerSession {
		t.Helper()
		session, err := runtime.startRustOwner(ctx, target, target, target, request, "")
		if err != nil {
			t.Fatal("open normal retained owner:", err)
		}
		t.Cleanup(func() { _ = session.Close() })
		return session
	}
	first := open()
	inspection, err := first.Inspect(ctx, nil)
	if err != nil || len(inspection.Blockers) != 0 || !inspection.Assessment.Changed || inspection.Resume != nil {
		t.Fatalf("fresh original inspection did not admit the real owner graph: changed=%t blockers=%+v resume=%t outcome=%+v callError=%v", inspection.Assessment.Changed, inspection.Blockers, inspection.Resume != nil, inspection.Outcome, err)
	}
	if current, err := store.ReadCurrentJournal(); err != nil || !current.Exists || !bytes.Equal(current.Payload, original) {
		t.Fatal("read-only original capture changed protected source history")
	}
	_, release, err := uninstall.OwnerLock(ctx, dataHome, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	barrier, err := uninstall.BorrowOwnerLock(dataHome)
	if err != nil || barrier == nil {
		t.Fatal("original held owner OFD is unavailable", err)
	}
	defer barrier.Close()
	defer first.Close() // Close the retained child before releasing parent OFDs on failure.
	if err := uninstall.ValidateOwnerLockDescriptor(dataHome, uint32(os.Getuid()), barrier); err != nil {
		t.Fatal(err)
	}
	outcome, err := first.Execute(ctx, inspection.Plan, strconv.Itoa(int(barrier.Fd())))
	if err != nil || outcome.Status != releasetransition.StatusReady || !outcome.ReachedGoal || outcome.Transaction == nil {
		t.Logf("original inherited setting IDs: %q", inputs.InheritedSettingIDs())
		t.Fatalf("normal owner graph did not reach Ready: status=%s code=%s transaction=%t reached_goal=%t message=%.512s error=%.512v", outcome.Status, outcome.Code, outcome.Transaction != nil, outcome.ReachedGoal, outcome.Message, err)
	}
	if err := closeInstalledGraphOwner(first); err != nil {
		t.Fatal("drain original retained owner:", err)
	}
	completed, journal := readJournal()
	if journal.Checkpoint != releasetransition.JournalComplete || journal.Transaction != *outcome.Transaction || journal.Transaction == sourceJournal.Transaction {
		t.Fatal("normal Rust activation did not close its genuine new transaction")
	}
	completedLinks, err := links.Observe()
	if err != nil || completedLinks.Active != targetID || completedLinks.Previous == nil || *completedLinks.Previous != beforeLinks.Active {
		t.Fatal("normal target activation links differ", err)
	}
	second := open() // Rebuild normal facts from the new links and protected history.
	fresh, err := second.Inspect(ctx, nil)
	if err != nil || len(fresh.Blockers) != 0 || fresh.Assessment.Changed || fresh.Resume != nil || fresh.Outcome == nil || fresh.Outcome.Status != releasetransition.StatusReady {
		t.Fatal("fresh completed inspection is not a Ready fixed point", err)
	}
	noop, err := second.Execute(ctx, fresh.Plan, "")
	if err != nil || noop.Status != releasetransition.StatusReady || !noop.ReachedGoal || noop.Transaction != nil && *noop.Transaction != journal.Transaction {
		t.Fatal("fresh checked completed no-op changed the transaction", err)
	}
	if err := closeInstalledGraphOwner(second); err != nil {
		t.Fatal("drain fresh completed owner:", err)
	}
	after, afterJournal := readJournal()
	afterLinks, err := links.Observe()
	if err != nil || !bytes.Equal(completed, after) || afterJournal.Transaction != journal.Transaction || afterLinks.Active != completedLinks.Active || afterLinks.Previous == nil || *afterLinks.Previous != *completedLinks.Previous {
		t.Fatal("fresh completed inspection/no-op changed protected history or links", err)
	}
	if nativeUUID() != strings.TrimSpace(string(uuid)) {
		t.Fatal("normal target replaced the original Go0.22 native instance")
	}
	retainedSource, err := runtime.verifyPublishedCandidate(ctx, publishedCandidate{release: sourceID, root: sourceRoot}, runtimeRoot, &sourceJournal.ArtifactDigest)
	if err != nil {
		t.Fatal("original Go0.22 source artifacts changed:", err)
	}
	defer retainedSource.Close()
	result, err := json.Marshal(struct {
		Transaction releasetransition.TransactionID `json:"transaction"`
		Status      string                          `json:"status"`
		Fresh       string                          `json:"fresh"`
	}{journal.Transaction, "ready", "completed-inspection-noop-ready"})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(string(result))
}

// The shell fixture authors ordinary stopped-yard imports before this process
// starts. This test uses only the normal owner transport and original capsule.
func TestRustOwnerInstalledCurrentPartialRecovery(t *testing.T) {
	token := os.Getenv("SUBYARD_RUST_INSTALLED_GRAPH_TOKEN")
	if token == "" {
		t.Skip("requires allocated installed-owner guest fixture")
	}
	if strings.Trim(token, "0123456789") != "" || os.Getenv("SUBYARD_E2E_VM") != "1" {
		t.Fatal("allocated numeric fixture required")
	}
	fixture := filepath.Join("/var/tmp/subyard-rust-owner-"+token, "success")
	for path, want := range map[string]string{filepath.Join(filepath.Dir(fixture), ".subyard-p0-marker"): "subyard-rust-owner-" + token, filepath.Join(fixture, ".subyard-p0-marker"): "subyard-rust-owner-" + token + ":success"} {
		info, e := os.Lstat(filepath.Dir(path))
		if e != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
			t.Fatal("unsafe fixture")
		}
		body, e := os.ReadFile(path)
		if e != nil || strings.TrimSpace(string(body)) != want {
			t.Fatal("fixture marker differs")
		}
	}
	data, configHome := filepath.Join(fixture, "data"), filepath.Join(fixture, "config")
	runtimeRoot := filepath.Join(data, "runtime")
	if os.Getenv("SUBYARD_HOME") != data || os.Getenv("SUBYARD_CONFIG_HOME") != configHome || os.Getenv("YARD_RUNTIME_ROOT") != runtimeRoot || os.Getenv("SUBYARD_INCUS_SOCKET") != "" {
		t.Fatal("fixture environment differs")
	}
	targetID := releasetransition.ReleaseID(os.Getenv("SUBYARD_RUST_INSTALLED_GRAPH_TARGET"))
	if targetID == "" || strings.ContainsAny(string(targetID), "/\\") {
		t.Fatal("exact target required")
	}
	repo := filepath.Join(runtimeRoot, "releases", string(targetID))
	catalog, e := config.LoadCatalog(repo)
	if e != nil {
		t.Fatal(e)
	}
	inputs := CaptureCommandInputs(os.Environ(), func(name string) bool { _, ok := catalog.LookupSetting(name); return ok })
	runtime := New(Config{Inputs: inputs, Context: RuntimeContext{OperatorHome: filepath.Join(fixture, "home"), DataHome: data, ConfigHome: configHome, RuntimeRoot: runtimeRoot}, RepositoryRoot: repo, WorkingDir: repo, Stderr: os.Stderr})
	defer runtime.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	target, e := runtime.verifyPublishedCandidate(ctx, publishedCandidate{release: targetID, root: repo}, runtimeRoot, nil)
	if e != nil || target.releaseOwner == nil {
		t.Fatal("normal target verification failed", e)
	}
	defer target.Close()
	links, e := releasetransition.NewRuntimeLinkStore(runtimeRoot)
	if e != nil {
		t.Fatal(e)
	}
	beforeLinks, e := links.Observe()
	if e != nil || beforeLinks.Active != targetID {
		t.Fatal("nominal target must already be active", e)
	}
	store, e := releasetransition.NewPOSIXV2Store(configHome)
	if e != nil {
		t.Fatal(e)
	}
	journal := func() ([]byte, releasetransition.JournalRecord) {
		t.Helper()
		s, e := store.ReadCurrentJournal()
		if e != nil || !s.Exists {
			t.Fatal("missing journal", e)
		}
		j, e := releasetransition.ParseJournal(s.Payload)
		if e != nil {
			t.Fatal(e)
		}
		return s.Payload, j
	}
	_, nominal := journal()
	if nominal.Checkpoint != releasetransition.JournalComplete {
		t.Fatal("nominal graph must complete first")
	}
	save := func(name string, body []byte) {
		t.Helper()
		f, e := os.OpenFile(filepath.Join(fixture, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			t.Fatal(e)
		}
		if e = f.Chmod(0600); e != nil {
			f.Close()
			t.Fatal(e)
		}
		if _, e = f.Write(body); e == nil {
			e = f.Sync()
		}
		ce := f.Close()
		if e != nil || ce != nil {
			t.Fatal("save original evidence", e, ce)
		}
		d, e := os.Open(fixture)
		if e != nil {
			t.Fatal(e)
		}
		e = d.Sync()
		d.Close()
		if e != nil {
			t.Fatal(e)
		}
	}
	command := func(label string, args ...string) []byte {
		t.Helper()
		c := exec.CommandContext(ctx, args[0], args[1:]...)
		out, e := c.Output()
		if e != nil {
			t.Fatalf("%s: %v", label, e)
		}
		return out
	}
	incus := func(label string, args ...string) []byte {
		t.Helper()
		return command(label, append([]string{"incus"}, args...)...)
	}
	uuid := strings.TrimSpace(string(incus("original UUID", "config", "get", "yard-partial-yard", "volatile.uuid", "--project", "subyard-partial-yard")))
	pin, e := os.ReadFile(filepath.Join(fixture, "partial-instance.uuid"))
	if e != nil || uuid == "" || strings.TrimSpace(string(pin)) != uuid {
		t.Fatal("original partial instance differs")
	}
	a := func() []byte {
		return incus("A identity", "exec", "yard-partial-yard", "--project", "subyard-partial-yard", "--", "sh", "-eu", "-c", "stat -c '%d:%i:%f:%u:%g:%s:%y' /home/dev/.codex/config.toml; sha256sum /home/dev/.codex/config.toml")
	}
	bPath := filepath.Join(fixture, "partial-host/host-codex-rules/repo.rules")
	beforeA := a()
	beforeB, e := os.ReadFile(bPath)
	if e != nil {
		t.Fatal(e)
	}
	bIdentity := func() string {
		t.Helper()
		i, e := os.Stat(bPath)
		if e != nil {
			t.Fatal(e)
		}
		st := i.Sys().(*syscall.Stat_t)
		return fmt.Sprintf("%d:%d:%d:%d:%d:%d:%d:%d", st.Dev, st.Ino, st.Mode, st.Uid, st.Gid, st.Size, st.Mtim.Sec, st.Mtim.Nsec)
	}
	beforeBIdentity := bIdentity()
	desiredB, e := os.ReadFile(filepath.Join(fixture, "partial-rules-source.rules"))
	if e != nil || len(desiredB) != 256<<10 || bytes.Equal(beforeB, desiredB) {
		t.Fatal("declared B source differs", e)
	}
	mount := filepath.Join(fixture, "partial-host/host-codex-rules")
	mountIdentity := func() string {
		t.Helper()
		info, e := os.Lstat(mount)
		if e != nil || !info.IsDir() || info.Mode().Perm() != 0755 {
			t.Fatal("unsafe original mount", e)
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != 1001 || st.Gid != 1001 {
			t.Fatal("mount ownership differs")
		}
		raw, e := os.ReadFile("/proc/self/mountinfo")
		if e != nil {
			t.Fatal(e)
		}
		optionsWithoutSize := func(value string) string {
			var options []string
			for _, option := range strings.Split(value, ",") {
				if !strings.HasPrefix(option, "size=") {
					options = append(options, option)
				}
			}
			sort.Strings(options)
			return strings.Join(options, ",")
		}
		found := ""
		for _, line := range strings.Split(string(raw), "\n") {
			parts := strings.Split(line, " - ")
			if len(parts) != 2 {
				continue
			}
			left, right := strings.Fields(parts[0]), strings.Fields(parts[1])
			if len(left) < 6 || len(right) < 3 || left[4] != mount {
				continue
			}
			if found != "" || right[0] != "tmpfs" || right[1] != "subyard-installed-partial-"+token || left[3] != "/" {
				t.Fatal("foreign or stacked mount")
			}
			found = fmt.Sprintf("%s:%s:%s:%d:%d:%d:%d:%s:%s", left[0], left[2], right[1], st.Dev, st.Ino, st.Uid, st.Mode, optionsWithoutSize(left[5]), optionsWithoutSize(right[2]))
		}
		if found == "" {
			t.Fatal("original mount missing")
		}
		marker := command("mount marker", "sudo", "-n", "python3", "-c", "import os,stat,sys; f=os.open(sys.argv[1],os.O_RDONLY|os.O_NOFOLLOW); s=os.fstat(f); assert stat.S_ISREG(s.st_mode) and s.st_uid==0 and stat.S_IMODE(s.st_mode)==0o600 and s.st_nlink==1; sys.stdout.buffer.write(os.read(f,256)); os.close(f)", filepath.Join(mount, ".subyard-partial-marker"))
		if string(marker) != "subyard-rust-owner-"+token+":partial\n" {
			t.Fatal("mount marker differs")
		}
		return found
	}
	mountBefore := mountIdentity()
	var filesystem syscall.Statfs_t
	if e := syscall.Statfs(mount, &filesystem); e != nil || filesystem.Blocks*uint64(filesystem.Bsize) != 64<<10 {
		t.Fatal("original mount is not the declared64KiB constraint", e)
	}
	var declared map[string]any
	rawMount, e := os.ReadFile(filepath.Join(fixture, "partial-mount.json"))
	if e != nil || json.Unmarshal(rawMount, &declared) != nil {
		t.Fatal("missing original mount record", e)
	}
	info, e := os.Stat(mount)
	if e != nil {
		t.Fatal(e)
	}
	st := info.Sys().(*syscall.Stat_t)
	if !strings.HasPrefix(mountBefore, fmt.Sprintf("%v:%v:%v:", declared["id"], declared["majorMinor"], declared["source"])) || declared["device"] != float64(st.Dev) || declared["inode"] != float64(st.Ino) || declared["source"] != "subyard-installed-partial-"+token || declared["uid"] != float64(1001) {
		t.Fatal("mount record does not bind original physical target")
	}

	request := releasetransition.ProcessRequest{SchemaVersion: releasetransition.ProcessProtocolSchemaV1, Mode: releasetransition.ProcessInspect, RuntimeRoot: runtimeRoot, ConfigHome: configHome, Yard: "default", Target: targetID, Direction: releasetransition.DirectionActivateTarget, ArtifactDigest: target.manifestDigest, RegistryDigest: target.registryDigest, InheritedSettingIDs: inputs.InheritedSettingIDs()}
	open := func() *rustOwnerSession {
		t.Helper()
		s, e := runtime.startRustOwner(ctx, target, target, target, request, "")
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	}
	first := open()
	inspection, e := first.Inspect(ctx, nil)
	if e != nil || len(inspection.Blockers) != 0 || !inspection.Assessment.Changed || inspection.Resume != nil {
		t.Fatal("fresh partial capture did not admit original changed scope", e)
	}
	_, release, e := uninstall.OwnerLock(ctx, data, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer release()
	barrier, e := uninstall.BorrowOwnerLock(data)
	if e != nil || barrier == nil {
		t.Fatal(e)
	}
	defer barrier.Close()
	defer first.Close()
	if e = uninstall.ValidateOwnerLockDescriptor(data, uint32(os.Getuid()), barrier); e != nil {
		t.Fatal(e)
	}
	// The authentic TOML writer uses this existing guest lock for both reads and
	// writes. A shared holder permits observation but blocks A's exclusive apply.
	const holdA = `import fcntl,hashlib,os,stat,sys
root='/var/lib/subyard/config-materialization'
name=hashlib.sha256(b'dev\0/home/dev/.codex/config.toml').hexdigest()+'.lock'
r=os.lstat(root);assert stat.S_ISDIR(r.st_mode) and r.st_uid==0 and stat.S_IMODE(r.st_mode)==0o700
fd=os.open(root+'/'+name,os.O_RDONLY|os.O_NOFOLLOW);s=os.fstat(fd)
assert stat.S_ISREG(s.st_mode) and s.st_uid==0 and stat.S_IMODE(s.st_mode)==0o600 and s.st_nlink==1
fcntl.flock(fd,fcntl.LOCK_SH)
print('held',s.st_dev,s.st_ino,os.getpid(),flush=True)
assert sys.stdin.buffer.read(1)==b'x'
`
	holder := exec.CommandContext(ctx, "incus", "exec", "yard-partial-yard", "--project", "subyard-partial-yard", "--", "python3", "-u", "-c", holdA)
	input, e := holder.StdinPipe()
	if e != nil {
		t.Fatal(e)
	}
	output, e := holder.StdoutPipe()
	if e != nil {
		t.Fatal(e)
	}
	if e = holder.Start(); e != nil {
		t.Fatal("existing A lock holder could not start", e)
	}
	holderClosed := false
	defer func() {
		if !holderClosed {
			_, _ = input.Write([]byte{'x'})
			_ = input.Close()
			_ = holder.Wait()
		}
	}()
	var held string
	var lockDev, lockIno, holderPID uint64
	if _, e = fmt.Fscan(output, &held, &lockDev, &lockIno, &holderPID); e != nil || held != "held" || lockIno == 0 || holderPID <= 1 {
		t.Fatal("existing A lock was not held shared", e)
	}
	type execution struct {
		outcome releasetransition.Outcome
		err     error
	}
	finished := make(chan execution, 1)
	go func() {
		result, err := first.Execute(ctx, inspection.Plan, strconv.Itoa(int(barrier.Fd())))
		finished <- execution{result, err}
	}()
	const blockedA = `import os,sys,time
p='/var/lib/subyard/config-materialization/'+sys.argv[1];s=os.stat(p,follow_symlinks=False)
assert (s.st_dev,s.st_ino)==tuple(map(int,sys.argv[2:4]))
key=f'{os.major(s.st_dev):02x}:{os.minor(s.st_dev):02x}:{s.st_ino}'
deadline=time.monotonic()+90
while True:
 rows=[line.split() for line in open('/proc/locks')]
 held=[v for v in rows if len(v)>6 and v[1:4]==['FLOCK','ADVISORY','READ'] and v[4]==sys.argv[4] and v[5]==key]
 wait=[v for v in rows if len(v)>7 and v[1:5]==['->','FLOCK','ADVISORY','WRITE'] and v[6]==key]
 if len(held)==len(wait)==1:
  pid=int(wait[0][5]);assert pid>1
  try: matching=sorted(name for name in os.listdir('/proc/'+str(pid)+'/fd') if (q:=os.stat('/proc/'+str(pid)+'/fd/'+name)) and (q.st_dev,q.st_ino)==(s.st_dev,s.st_ino))
  except (FileNotFoundError,ProcessLookupError):matching=[]
  if matching:print('blocked',pid,','.join(matching));break
 if sys.argv[5]=='once' or time.monotonic()>deadline:raise AssertionError('exact blocked writer unavailable')
 time.sleep(.05)
`
	lockName := fmt.Sprintf("%x.lock", sha256.Sum256([]byte("dev\x00/home/dev/.codex/config.toml")))
	var pending []byte
	var pj releasetransition.JournalRecord
	var nativeAtCut map[string][]byte
	acceptedName := ""
	waiter := func(mode string) []byte {
		return incus("exact A lock waiter", "exec", "yard-partial-yard", "--project", "subyard-partial-yard", "--", "python3", "-c", blockedA, lockName, strconv.FormatUint(lockDev, 10), strconv.FormatUint(lockIno, 10), strconv.FormatUint(holderPID, 10), mode)
	}
	blocked := waiter("wait")
	if !strings.HasPrefix(string(blocked), "blocked ") {
		t.Fatal("real A writer did not wait on exact existing lock")
	}
	pending, pj = journal()
	for {
		_, native, factErr := installedGraphFacts(configHome)
		if factErr == nil {
			for name, raw := range native {
				if !strings.Contains(name, ".operation-") || !strings.HasSuffix(name, "-accepted.json") || native[strings.TrimSuffix(name, "accepted.json")+"terminal.json"] != nil {
					continue
				}
				var op ports.NativeOperation
				var intent releaseowner.Intent
				if json.Unmarshal(raw, &op) == nil && json.Unmarshal(native[name[:4]+"intent.json"], &intent) == nil && op.Kind == "instance-exec" && op.Project == "subyard-partial-yard" && op.Resource == "yard-partial-yard" && op.ID != "" && intent.Owner == "materialized-config" && intent.Resource == "partial-yard" && intent.Binding.Transaction == string(pj.Transaction) && native[name[:4]+"terminal.json"] == nil {
					if acceptedName != "" {
						t.Fatal("multiple unfinished native execs at original cut")
					}
					acceptedName = name
				}
			}
		}
		if acceptedName != "" {
			nativeAtCut = native
			break
		}
		select {
		case result := <-finished:
			t.Fatalf("original writer closed before the held cut: call_error=%t reached_goal=%t", result.err != nil, result.outcome.ReachedGoal)
		case <-ctx.Done():
			t.Fatal("original writer never reached the held cut", ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !bytes.Equal(waiter("once"), blocked) {
		t.Fatal("accepted SDK exec lost its exact blocked PID/FD before SIGKILL")
	}
	cutJournal, cutFacts, e := installedGraphFacts(configHome)
	if e != nil || !reflect.DeepEqual(cutJournal, pj) || !reflect.DeepEqual(cutFacts, nativeAtCut) {
		t.Fatal("original accepted-without-terminal cut changed before SIGKILL", e)
	}
	if e = first.command.Process.Kill(); e != nil {
		t.Fatal("original Rust coordinator was unavailable at in-flight cut", e)
	}
	select {
	case <-first.done:
	case <-ctx.Done():
		t.Fatal("killed original Rust coordinator did not exit", ctx.Err())
	}
	first.mu.Lock()
	exit, ok := first.exitErr.(*exec.ExitError)
	first.mu.Unlock()
	if !ok {
		t.Fatal("original Rust coordinator did not report a failed Wait")
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatal("original Rust coordinator did not exit by SIGKILL")
	}
	select {
	case result := <-finished:
		if result.err == nil && result.outcome.ReachedGoal {
			t.Fatal("killed original coordinator reported Ready")
		}
	case <-ctx.Done():
		t.Fatal("killed original Execute did not return", ctx.Err())
	}
	if e = first.Close(); e != nil {
		t.Fatal("original Rust session descriptor cleanup failed", e)
	}
	update, e := os.OpenFile(filepath.Join(configHome, "migrations/update.lock"), os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if e != nil {
		t.Fatal("original update lease unavailable", e)
	}
	defer update.Close()
	if e = syscall.Flock(int(update.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != syscall.EWOULDBLOCK {
		t.Fatal("surviving helper did not retain the original update lease", e)
	}
	if !bytes.Equal(waiter("once"), blocked) {
		t.Fatal("original SDK writer did not survive Rust SIGKILL")
	}
	contender := open()
	probeCtx, stopProbe := context.WithTimeout(ctx, 10*time.Second)
	defer stopProbe()
	probe, probeErr := contender.Inspect(probeCtx, nil)
	if probeErr != nil || probe.Outcome != nil && (probe.Outcome.Status == releasetransition.StatusReady || probe.Outcome.ReachedGoal) || probe.Resume == nil && probe.Outcome == nil && len(probe.Blockers) == 0 {
		t.Fatal("fresh helper did not explicitly refuse Ready while original writer was blocked", probeErr)
	}
	if probe.Resume != nil {
		if *probe.Resume != pj.Transaction {
			t.Fatal("fresh helper selected a different pending transaction")
		}
		outcome, attemptErr := contender.Execute(probeCtx, probe.Plan, strconv.Itoa(int(barrier.Fd())))
		if attemptErr == nil && outcome.ReachedGoal {
			t.Fatal("fresh helper completed while original writer was blocked")
		}
	}
	stopProbe()
	_ = contender.Close()
	_, nativeAfterProbe, factErr := installedGraphFacts(configHome)
	unchanged, _ := journal()
	whileB, e := os.ReadFile(bPath)
	if factErr != nil || !reflect.DeepEqual(nativeAfterProbe, nativeAtCut) || !bytes.Equal(waiter("once"), blocked) || !bytes.Equal(unchanged, pending) || !bytes.Equal(a(), beforeA) || e != nil || !bytes.Equal(whileB, beforeB) || bIdentity() != beforeBIdentity {
		t.Fatal("fresh helper changed original state while SDK writer was blocked", e)
	}
	if _, e = input.Write([]byte{'x'}); e != nil {
		t.Fatal("could not release exact guest lock", e)
	}
	_ = input.Close()
	if e = holder.Wait(); e != nil {
		t.Fatal("guest lock holder did not release", e)
	}
	holderClosed = true
	for {
		e = syscall.Flock(int(update.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if e == nil {
			_ = syscall.Flock(int(update.Fd()), syscall.LOCK_UN)
			break
		}
		if e != syscall.EWOULDBLOCK {
			t.Fatal("original update lease query failed", e)
		}
		select {
		case <-ctx.Done():
			t.Fatal("original native helper did not release its update lease", ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	if mountIdentity() != mountBefore {
		t.Fatal("original mount authority changed during failure")
	}
	if pj.Checkpoint == releasetransition.JournalComplete || pj.Transaction == nominal.Transaction || pj.Releases.From != targetID || pj.Releases.Target != targetID {
		t.Fatal("missing original pending same-target transaction")
	}
	afterA := a()
	afterB, e := os.ReadFile(bPath)
	if e != nil || bytes.Equal(beforeA, afterA) || !bytes.Equal(beforeB, afterB) || bIdentity() != beforeBIdentity {
		t.Fatal("failure did not leave actual A-applied/B-original state", e)
	}
	// A's meaningful managed value must have applied, not merely an inode rewrite.
	incus("A desired", "exec", "yard-partial-yard", "--project", "subyard-partial-yard", "--", "python3", "-c", "import tomllib; assert tomllib.load(open('/home/dev/.codex/config.toml','rb'))['tui']['fullscreen_transcript'] is True")
	custody := filepath.Join(configHome, "migrations/native-custody/v1", string(pj.Transaction))
	observedJournal, custodyFacts, e := installedGraphFacts(configHome)
	if e != nil || !reflect.DeepEqual(observedJournal, pj) {
		t.Fatal("original custody is unavailable", e)
	}
	closedFailure := false
	originalBundle := map[string][]byte{}
	for name, raw := range custodyFacts {
		piece := false
		if len(name) == 15 && strings.HasPrefix(name, "bundle.") && strings.HasSuffix(name, ".json") {
			n, e := strconv.Atoi(name[7:10])
			piece = e == nil && n >= 0 && n < 128
		}
		if name == "bundle.json" || piece {
			originalBundle[name] = raw
		}
		if !strings.HasSuffix(name, ".terminal.json") {
			continue
		}
		var terminal releaseowner.Terminal
		if json.Unmarshal(raw, &terminal) != nil {
			t.Fatal("invalid terminal")
		}
		if name == acceptedName[:4]+"terminal.json" && terminal.State == "failed" && terminal.Owner == "materialized-config" && terminal.Resource == "partial-yard" && terminal.Binding.Transaction == string(pj.Transaction) {
			if len(terminal.Operations) == 0 {
				t.Fatal("no SDK operation census")
			}
			execSeen := false
			for _, op := range terminal.Operations {
				if op.Status != 200 || op.Project != "subyard-partial-yard" || op.Resource != "yard-partial-yard" {
					t.Fatal("native invocation was not exact closed SDK success-status work")
				}
				execSeen = execSeen || op.Kind == "instance-exec"
			}
			closedFailure = execSeen
		}
	}
	if len(originalBundle) == 0 {
		t.Fatal("missing original capsule bundle")
	}
	if !closedFailure {
		t.Fatal("missing genuine whole FAILED terminal with original completed SDK exec")
	}
	var originalExec ports.NativeOperation
	var acceptedExec ports.NativeOperation
	if json.Unmarshal(nativeAtCut[acceptedName], &acceptedExec) != nil || json.Unmarshal(custodyFacts[strings.TrimSuffix(acceptedName, "accepted.json")+"terminal.json"], &originalExec) != nil || originalExec.Kind != "instance-exec" || originalExec.Status != 200 || originalExec.ID != acceptedExec.ID || originalExec.Server != acceptedExec.Server || originalExec.Project != acceptedExec.Project || originalExec.Resource != acceptedExec.Resource {
		t.Fatal("original A command did not reach exact SDK success after Rust death")
	}
	save("partial-pending-journal.json", pending)
	// Reuse the existing bounded three-directory custody grammar before Resume can
	// replace the failed terminal. It selects the TX from the validated journal.
	helper := os.Getenv("SUBYARD_RUST_INSTALLED_GRAPH_CAPTURE_HELPER")
	if helper == "" {
		t.Fatal("exact transported capture helper is required")
	}
	command("snapshot original custody", "python3", "-c", installedGraphCapsuleSnapshot, helper, configHome, filepath.Join(fixture, "partial-pending-capsule.tar"))

	facts, _ := json.Marshal(map[string]any{"transaction": pj.Transaction, "uuid": uuid, "capacityBytes": filesystem.Blocks * uint64(filesystem.Bsize), "a": string(afterA), "bOriginalIdentity": beforeBIdentity, "bOriginalSHA256": fmt.Sprintf("%x", sha256.Sum256(beforeB)), "bDesiredSHA256": fmt.Sprintf("%x", sha256.Sum256(desiredB)), "mount": mountBefore, "native": "whole-failed-sdk-status200-helper-drained"})
	save("partial-pending-facts.json", append(facts, '\n'))
	command("repair capacity", "sudo", "-n", "mount", "-o", "remount,size=1m", mount)
	if e := syscall.Statfs(mount, &filesystem); e != nil || filesystem.Blocks*uint64(filesystem.Bsize) != 1<<20 {
		t.Fatal("capacity repair did not establish1MiB", e)
	}
	if mountIdentity() != mountBefore {
		t.Fatal("capacity repair replaced original mount authority")
	}
	second := open()
	defer second.Close()
	fresh, e := second.Inspect(ctx, nil)
	if e != nil || len(fresh.Blockers) != 0 || fresh.Resume == nil || *fresh.Resume != pj.Transaction {
		status, code := "", ""
		if fresh.Outcome != nil {
			status, code = string(fresh.Outcome.Status), string(fresh.Outcome.Code)
		}
		var blockers []string
		for _, blocker := range fresh.Blockers {
			blockers = append(blockers, string(blocker.Code))
		}
		t.Fatalf("fresh helper did not restore original pending Resume: call_error=%t status=%s code=%s blockers=%v resume_match=%t special_admission=%t", e != nil, status, code, blockers, fresh.Resume != nil && *fresh.Resume == pj.Transaction, second.admission != nil)
	}
	resumed, e := second.Execute(ctx, fresh.Plan, strconv.Itoa(int(barrier.Fd())))
	if e != nil || resumed.Status != releasetransition.StatusReady || !resumed.ReachedGoal || resumed.Transaction == nil || *resumed.Transaction != pj.Transaction {
		t.Fatal("original Resume did not reach Ready", e)
	}
	if e = closeInstalledGraphOwner(second); e != nil {
		t.Fatal("resume helper did not drain", e)
	}
	for name, want := range originalBundle {
		got, e := os.ReadFile(filepath.Join(custody, name))
		if e != nil || !bytes.Equal(got, want) {
			t.Fatal("Resume changed the original capsule", e)
		}
	}
	if !bytes.Equal(afterA, a()) {
		t.Fatal("Resume rewrote already-desired A identity/mtime")
	}
	gotB, e := os.ReadFile(bPath)
	if e != nil || !bytes.Equal(gotB, desiredB) {
		t.Fatal("remaining B did not converge", e)
	}
	completed, cj := journal()
	afterLinks, e := links.Observe()
	if e != nil || cj.Checkpoint != releasetransition.JournalComplete || cj.Transaction != pj.Transaction || !reflect.DeepEqual(afterLinks, beforeLinks) {
		t.Fatalf("Resume changed original transaction/runtime links: call_error=%t complete=%t transaction_match=%t links_match=%t", e != nil, cj.Checkpoint == releasetransition.JournalComplete, cj.Transaction == pj.Transaction, reflect.DeepEqual(afterLinks, beforeLinks))
	}
	third := open()
	defer third.Close()
	noop, e := third.Inspect(ctx, nil)
	if e != nil || noop.Resume != nil || len(noop.Blockers) != 0 || noop.Assessment.Changed || noop.Outcome == nil || noop.Outcome.Status != releasetransition.StatusReady {
		t.Fatal("fresh final inspection is not Ready", e)
	}
	ready, e := third.Execute(ctx, noop.Plan, "")
	if e != nil || ready.Status != releasetransition.StatusReady || !ready.ReachedGoal {
		t.Fatal("fresh final no-op failed", e)
	}
	if e = closeInstalledGraphOwner(third); e != nil {
		t.Fatal(e)
	}
	last, _ := journal()
	if !bytes.Equal(last, completed) || !bytes.Equal(afterA, a()) {
		t.Fatal("final no-op changed protected state or A")
	}
	t.Log("original current config A-applied/B-original, completed SDK failure, fresh same-transaction Resume B-only, Ready and fresh no-op Ready")
}
