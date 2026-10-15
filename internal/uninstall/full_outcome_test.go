package uninstall

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestScopedCustomHostRootPreservesForeignContent(t *testing.T) {
	root := testkit.TempDir(t)
	owned := filepath.Join(root, "host-secrets")
	if err := os.Mkdir(owned, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(owned, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "foreign-project"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "foreign-project"), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(owned, "credential"), []byte("synthetic owned data"), 0600)
	testkit.WriteFile(t, filepath.Join(root, "foreign-project", "README"), []byte("preserve"), 0600)
	testkit.WriteFile(t, filepath.Join(root, "foreign.control"), []byte("foreign"), 0600)
	tree, err := CaptureScoped(root, []string{"host-secrets"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = tree.Remove(); err != nil {
		t.Fatal(err)
	}
	if err = tree.VerifyRemoved(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"foreign.control", "foreign-project/README"} {
		if _, err = os.Stat(filepath.Join(root, path)); err != nil {
			t.Fatal("foreign content disappeared", err)
		}
	}
	if _, err = os.Stat(owned); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("owned data remains", err)
	}
}

func TestStartupFreshRetryAfterPreparedRename(t *testing.T) {
	root := testkit.TempDir(t)
	path := filepath.Join(root, "profile")
	foreign := "export FOREIGN=value\n"
	testkit.WriteFile(t, path, []byte(foreign+"# Subyard CLI interactive PATH\nexport PATH=\"/owned/bin:$PATH\"\n"), 0644)
	edit, err := PrepareStartup(path, "/owned/runtime", "/owned/bin", false)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := edit.PrepareApply()
	if err != nil {
		t.Fatal(err)
	}
	// Simulate loss after native preparation but before Pending is checkpointed.
	// Reload only the original plan; retry must reuse its exact future sidecar.
	originalBody, _ := json.Marshal(edit)
	var original StartupEdit
	if err = json.Unmarshal(originalBody, &original); err != nil {
		t.Fatal(err)
	}
	retried, err := original.PrepareApply()
	if err != nil {
		t.Fatal("fresh retry lost original shell sidecar intent", err)
	}
	if retried.Pending.Path != prepared.Pending.Path || retried.Pending.Entries[0].Inode != prepared.Pending.Entries[0].Inode {
		t.Fatal("retry derived another sidecar identity")
	}
	prepared = retried
	sidecars, err := filepath.Glob(filepath.Join(root, ".subyard-uninstall-*"))
	if err != nil || len(sidecars) != 1 {
		t.Fatal("interrupted shell preparation leaked sidecars", sidecars, err)
	}
	// Persist/reload through JSON exactly as a fresh recovery process does.
	body, _ := json.Marshal(prepared)
	var saved StartupEdit
	if err = json.Unmarshal(body, &saved); err != nil {
		t.Fatal(err)
	}
	if _, err = prepared.CommitApply(); err != nil {
		t.Fatal(err)
	}
	if _, err = saved.CommitApply(); err != nil {
		t.Fatal("fresh retry rejected own completed rename", err)
	}
	result, err := os.ReadFile(path)
	if err != nil || string(result) != foreign {
		t.Fatal("foreign shell content changed", err)
	}
	testkit.WriteFile(t, path, []byte("replacement"), 0644)
	if _, err = saved.CommitApply(); err == nil {
		t.Fatal("fresh retry adopted a replaced shell file")
	}
}

func TestQueuedGenerationCannotRecreateRemovedInstallation(t *testing.T) {
	base := testkit.TempDir(t)
	root := filepath.Join(base, "installation")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	generation, err := CaptureRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	_, release, err := OwnerLock(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		_, unlock, err := OwnerLock(context.Background(), root, nil)
		if err == nil {
			defer unlock()
			err = CheckGeneration([]Tree{generation})
		}
		done <- err
	}()
	<-started
	if err = os.Remove(root); err != nil {
		t.Fatal(err)
	}
	release()
	select {
	case err := <-done:
		if !errors.Is(err, domain.ErrPlanStale) {
			t.Fatal("queued plan accepted removed installation", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued owner lock did not drain")
	}
	if _, err = os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("queued plan recreated installation", err)
	}
}

func TestPrivateRefinementPersistsBeforeUnlinkAndFreshRetry(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	store, plan := recoveryFixture(t)
	root := filepath.Join(store.Directory, "private-root")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "owned"), []byte("owned"), 0600)
	testkit.WriteFile(t, filepath.Join(root, "foreign.control"), []byte("foreign"), 0600)
	tree, err := CaptureRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	tree.Opaque = true
	tree.Scoped = true
	tree.OwnedPaths = []string{"owned"}
	plan.Trees = []Tree{tree}
	plan.Stage = RecoveryYardsRemoved
	ownerLock, releaseOwner, err := OwnerLock(context.Background(), plan.Installation.DataHome, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan.OwnerLock = &ownerLock
	if _, err = store.Save(plan, nil); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join("..", "..", "scripts", "lib", "teardown-files.py")
	run := func(code string) error {
		body, err := os.ReadFile(filepath.Join(store.Directory, "plan.json"))
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		command := exec.Command(python, "-B", helper)
		if code != "" {
			command = exec.Command(python, "-B", "-c", code, helper)
		}
		command.Env = append(os.Environ(), "SUBYARD_TEARDOWN_PLAN="+filepath.Join(store.Directory, "plan.json"), "SUBYARD_TEARDOWN_PLAN_SHA256="+hex.EncodeToString(sum[:]), "SUBYARD_TEARDOWN_PARENT_ID=0:0")
		output, err := command.CombinedOutput()
		if err != nil && !strings.Contains(string(output), "intentional interruption") {
			t.Log(string(output))
		}
		return err
	}
	if err = run(""); err == nil {
		releaseOwner()
		t.Fatal("orphaned physical leaf bypassed live owner barrier")
	}
	if _, err = os.Stat(filepath.Join(root, "owned")); err != nil {
		releaseOwner()
		t.Fatal("blocked orphan removed data", err)
	}
	releaseOwner()
	interruption := `import importlib.util,sys
s=importlib.util.spec_from_file_location("fixture",sys.argv[1]);m=importlib.util.module_from_spec(s);s.loader.exec_module(m)
def stop(*args,**kwargs): raise RuntimeError("intentional interruption")
m.guard.remove_entries=stop
m.main()`
	if err = run(interruption); err == nil {
		t.Fatal("fixture interruption did not happen")
	}
	saved, _, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if saved.Trees[0].Opaque || len(saved.Trees[0].Entries) != 2 {
		t.Fatal("private metadata was not persisted before unlink")
	}
	if _, err = os.Stat(filepath.Join(root, "owned")); err != nil {
		t.Fatal("unlink happened before refinement checkpoint")
	}
	if err = run(""); err != nil {
		t.Fatal("fresh process could not resume exact refined plan", err)
	}
	if _, err = os.Stat(filepath.Join(root, "owned")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("owned file remains", err)
	}
	if _, err = os.Stat(filepath.Join(root, "foreign.control")); err != nil {
		t.Fatal("foreign control removed", err)
	}
}

func TestInterruptedEpochAdvanceRetainsLockAndExactRetry(t *testing.T) {
	base := testkit.TempDir(t)
	data := filepath.Join(base, "data")
	lock, release, err := OwnerLock(context.Background(), data, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	before, err := ReadOwnerEpoch(data)
	if err != nil {
		t.Fatal(err)
	}
	if err = AdvanceOwnerEpoch(data, before, before+1); err != nil {
		t.Fatal(err)
	}
	// Fresh retry after the syscall completed but before terminal publication.
	if err = AdvanceOwnerEpoch(data, before, before+1); err != nil {
		t.Fatal("own interrupted advance was not recognized", err)
	}
	if err = CheckOwnerLockIdentity(lock); err != nil {
		t.Fatal("epoch advance replaced live lock inode", err)
	}
	if err = AdvanceOwnerEpoch(data, before+5, before+6); err == nil {
		t.Fatal("foreign generation was adopted")
	}
}

func TestPartialEpochPublicationResumesWithoutReplacingLiveLock(t *testing.T) {
	data := filepath.Join(testkit.TempDir(t), "data")
	lock, release, err := OwnerLock(context.Background(), data, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	testkit.WriteFile(t, OwnerEpochPath(data), []byte("9\n"), 0600)
	testkit.WriteFile(t, OwnerEpochPath(data)+"-next", []byte("1"), 0600)
	if current, err := ReadOwnerEpoch(data); err != nil || current != 9 {
		t.Fatal("reader adopted partial publication", current, err)
	}
	if err := AdvanceOwnerEpoch(data, 9, 10); err != nil {
		t.Fatal(err)
	}
	if current, err := ReadOwnerEpoch(data); err != nil || current != 10 {
		t.Fatal(current, err)
	}
	if err := CheckOwnerLockIdentity(lock); err != nil {
		t.Fatal("live lock changed", err)
	}
	if _, err := os.Lstat(OwnerEpochPath(data) + "-next"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("completed publication intent remains", err)
	}
	if err := AdvanceOwnerEpoch(data, 9, 10); err != nil {
		t.Fatal("fresh completed retry", err)
	}
	testkit.WriteFile(t, OwnerEpochPath(data)+"-next", []byte("foreign"), 0600)
	if err := AdvanceOwnerEpoch(data, 10, 11); err == nil {
		t.Fatal("foreign pending publication adopted")
	}
}
