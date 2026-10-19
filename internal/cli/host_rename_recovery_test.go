package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"errors"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerinventory"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
)

func TestQueuedHostRenameCannotRecreateRetiredConfiguration(t *testing.T) {
	root, home, configHome, environment := configCommandFixture(t)
	program := configExactProgram(t, root, environment, nil, nil)
	authority, err := program.observeHostRenameAuthority(context.Background(), configHome)
	if err != nil {
		t.Fatal(err)
	}
	defer authority.close()
	dataHome := filepath.Join(home, ".subyard")
	_, release, err := uninstall.OwnerLock(context.Background(), dataHome, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if release != nil {
			release()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- authority.withLock(ctx, program, func() error {
			return os.WriteFile(filepath.Join(configHome, "late-rename"), []byte("unexpected"), 0o600)
		})
	}()
	select {
	case err := <-done:
		t.Fatalf("rename did not wait for the owner barrier: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := uninstall.AdvanceOwnerEpoch(dataHome, 0, 1); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(configHome); err != nil {
		t.Fatal(err)
	}
	release()
	release = nil
	if err := <-done; !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("retired rename outcome = %v", err)
	}
	if _, err := os.Lstat(configHome); !os.IsNotExist(err) {
		t.Fatal("queued rename recreated configuration or migration lock state")
	}
}

func TestEarlyOwnerMutationRequiresActualHeldBarrier(t *testing.T) {
	home := testkit.TempDir(t)
	admission, err := captureOwnerWriteAdmission(config.Loaded{Context: domain.Context{Paths: domain.RuntimePaths{DataHome: home}}}, repositoryRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	program := &CLI{ownerMutationLock: &uninstall.Tree{Path: uninstall.OwnerLockPath(home)}}
	if release, err := program.acquireOwnerMutation(context.Background(), admission, false, true); err == nil {
		release()
		t.Fatal("a pointer to an unheld barrier admitted an early writer")
	}
	lock, release, err := uninstall.OwnerLock(context.Background(), home, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	program.ownerMutationLock = &lock
	nested, err := program.acquireOwnerMutation(context.Background(), admission, false, true)
	if err != nil {
		t.Fatal(err)
	}
	nested()
	borrowed, err := uninstall.BorrowOwnerLock(home)
	if err != nil || borrowed == nil {
		t.Fatal("nested source mutation released its parent's barrier")
	}
	borrowed.Close()
}

// Native fixture at the source-HEAD commit point: owner identity and cache
// manifest still describe the old pair, while the prepared Git move is committed.
func pendingSourceRenameFixture(t *testing.T, sealed bool) (*CLI, config.Loaded, string, string, []string) {
	t.Helper()
	t.Setenv("TMPDIR", "/tmp")
	root, home, configHome, environment := configCommandFixture(t)
	if sealed {
		runtimeRoot := aliasRepairRuntimeFixture(t, home)
		root = sourceAdmissionToolingFixture(t, runtimeRoot)
		environment = append(environment, "YARD_RUNTIME_ROOT="+runtimeRoot, "SUBYARD_CONFIG_DIR="+filepath.Join(root, "config"))
	}
	source := configSourceGitRepository(t, "owner-a", "YARD_IMAGE=images:debian/13\n")
	runConfigSyncGit(t, source, "config", "user.name", "Subyard Test")
	runConfigSyncGit(t, source, "config", "user.email", "test@invalid")
	writeConfigCommandFile(t, configsync.HostIDPath(configHome), "owner-a\n")
	loaded := loadConfigCommandContext(t, root, environment, "default")
	plan, err := configsync.BuildPlan(configsync.Options{SourceRoot: source, ConfigHome: configHome, RepositoryRoot: root, OperatorHome: home, Environment: environmentMap(environment), FileSettings: config.SyncableFileMappings(loaded)})
	if err != nil {
		t.Fatal(err)
	}
	if err := configsync.Apply(plan); err != nil {
		t.Fatal(err)
	}
	if err := configsync.RegisterSource(configHome, source); err != nil {
		t.Fatal(err)
	}
	oldCommit := strings.TrimSpace(configSourceGitOutput(t, source, "rev-parse", "HEAD"))
	oldTree := strings.TrimSpace(configSourceGitOutput(t, source, "rev-parse", "HEAD^{tree}"))
	before, err := os.ReadFile(configsync.ManifestPath(configHome))
	if err != nil {
		t.Fatal(err)
	}
	runConfigSyncGit(t, source, "mv", "hosts/owner-a", "hosts/owner-b")
	commitConfigSource(t, source, "Rename owner settings")
	afterCommit := strings.TrimSpace(configSourceGitOutput(t, source, "rev-parse", "HEAD"))
	afterTree := strings.TrimSpace(configSourceGitOutput(t, source, "rev-parse", "HEAD^{tree}"))
	gitDir := strings.TrimSpace(configSourceGitOutput(t, source, "rev-parse", "--absolute-git-dir"))
	headRef := strings.TrimSpace(configSourceGitOutput(t, source, "symbolic-ref", "HEAD"))
	index, err := os.ReadFile(filepath.Join(gitDir, "index"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest configsync.Manifest
	if err := json.Unmarshal(before, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.HostID = "owner-b"
	manifest.SourceCommit = afterCommit
	after, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	record, err := os.ReadFile(configsync.SourceRecordPath(configHome))
	if err != nil {
		t.Fatal(err)
	}
	journal := map[string]any{"schemaVersion": 2, "oldHostId": "owner-a", "newHostId": "owner-b", "hostIdDigest": fmt.Sprintf("%x", sha256.Sum256([]byte("owner-a\n"))), "manifestDigest": fmt.Sprintf("%x", sha256.Sum256(before)), "manifestWasPresent": true, "manifestBefore": before, "manifestAfter": after, "source": map[string]any{"Root": source, "SourceHostID": "owner-a", "BeforeCommit": oldCommit, "AfterCommit": afterCommit, "BeforeTree": oldTree, "AfterTree": afterTree, "RecordDigest": fmt.Sprintf("%x", sha256.Sum256(record)), "GitDir": gitDir, "HeadRef": headRef, "AfterIndex": index}}
	payload, err := json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	writeConfigCommandFile(t, configsync.HostIDRenameTransactionPath(configHome), string(payload))
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
	if err != nil {
		t.Fatal(err)
	}
	return program, loaded, configHome, source, environment
}

func TestOrdinaryHostRenameRetryReportsRecoveredSuccess(t *testing.T) {
	program, _, home, source, environment := pendingSourceRenameFixture(t, false)
	head := configSourceGitOutput(t, source, "rev-parse", "HEAD")
	var stdout, stderr bytes.Buffer
	prompt := &testkit.Prompt{}
	retry, err := New(Options{RepositoryRoot: program.options.RepositoryRoot, Program: "yard", Arguments: []string{"host", "rename", "owner-b"}, Environment: environment, WorkingDir: program.options.RepositoryRoot, Stdout: &stdout, Stderr: &stderr, Prompt: prompt})
	if err != nil {
		t.Fatal(err)
	}
	if code := retry.Run(context.Background()); code != 0 || !strings.Contains(stdout.String(), "rename recovered") || len(prompt.Requests) != 0 {
		t.Fatalf("retry code=%d stdout=%s stderr=%s prompts=%d", code, stdout.String(), stderr.String(), len(prompt.Requests))
	}
	if current := configSourceGitOutput(t, source, "rev-parse", "HEAD"); current != head {
		t.Fatal("retry created a commit")
	}
	identity, err := os.ReadFile(configsync.HostIDPath(home))
	if err != nil || string(identity) != "owner-b\n" {
		t.Fatal("retry did not complete identity")
	}
}

func TestPendingSourceRenameRecoveryRechecksAuthorityUnderReleaseLock(t *testing.T) {
	for _, change := range []string{"none", "tooling", "admission"} {
		t.Run("authority-change="+change, func(t *testing.T) {
			program, _, home, _, _ := pendingSourceRenameFixture(t, true)
			authority, err := program.observeHostRenameAuthority(context.Background(), home)
			if err != nil {
				t.Fatal(err)
			}
			defer authority.close()
			store, err := releasetransition.NewPOSIXV2Store(home)
			if err != nil {
				t.Fatal(err)
			}
			unlock, err := store.Lock()
			if err != nil {
				t.Fatal(err)
			}
			held := true
			defer func() {
				if held {
					unlock()
				}
			}()
			done := make(chan error, 1)
			entered := make(chan struct{})
			go func() {
				close(entered)
				done <- authority.withLock(context.Background(), program, func() error { return configsync.RecoverHostIDRename(home) })
			}()
			<-entered
			select {
			case err := <-done:
				t.Fatalf("pending recovery crossed held release lock: %v", err)
			case <-time.After(30 * time.Millisecond):
			}
			identity, err := os.ReadFile(configsync.HostIDPath(home))
			if err != nil || string(identity) != "owner-a\n" {
				t.Fatal("identity published while release lock held")
			}
			if change == "tooling" {
				writeConfigCommandFile(t, filepath.Join(program.options.RepositoryRoot, "config/subyard.env"), "# replacement unsealed tooling\n")
			}
			if change == "admission" {
				writeConfigCommandFile(t, filepath.Join(home, "release-transition/v2/journal.json"), "{invalid-changed-admission\n")
			}
			unlock()
			held = false
			select {
			case err := <-done:
				if change != "none" && err == nil {
					t.Fatal("changed sealed admission was not refused")
				}
				if change == "none" && err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("recovery did not settle after release lock")
			}
			identity, err = os.ReadFile(configsync.HostIDPath(home))
			expected := "owner-b\n"
			if change != "none" {
				expected = "owner-a\n"
			}
			if err != nil || string(identity) != expected {
				t.Fatalf("identity=%q err=%v", identity, err)
			}
		})
	}
}

func TestOrdinaryHostRenameRetryDoesNotReportRolledBackPairAsRecovered(t *testing.T) {
	program, _, home, source, environment := pendingSourceRenameFixture(t, false)
	path := configsync.HostIDRenameTransactionPath(home)
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var journal map[string]any
	if err := json.Unmarshal(payload, &journal); err != nil {
		t.Fatal(err)
	}
	sourceState := journal["source"].(map[string]any)
	runConfigSyncGit(t, source, "update-ref", "HEAD", sourceState["BeforeCommit"].(string), sourceState["AfterCommit"].(string))
	runConfigSyncGit(t, source, "read-tree", sourceState["BeforeTree"].(string))
	snapshot, err := config.ReadPersistentFileSnapshot(home, filepath.Join(home, "config.env"))
	if err != nil {
		t.Fatal(err)
	}
	sourceState["Local"] = []any{map[string]any{"Path": "config.env", "Exists": true, "Digest": fmt.Sprintf("%x", sha256.Sum256(snapshot.Content)), "Mode": uint32(snapshot.Identity.Mode & 0o777), "Identity": snapshot.Identity}}
	payload, err = json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	writeConfigCommandFile(t, path, string(payload))
	writeConfigCommandFile(t, filepath.Join(home, "config.env"), "YARD_IMAGE=images:ubuntu/24.04\n")
	var stdout, stderr bytes.Buffer
	prompt := &testkit.Prompt{}
	retry, err := New(Options{RepositoryRoot: program.options.RepositoryRoot, Program: "yard", Arguments: []string{"host", "rename", "owner-b"}, Environment: environment, WorkingDir: program.options.RepositoryRoot, Stdout: &stdout, Stderr: &stderr, Prompt: prompt})
	if err != nil {
		t.Fatal(err)
	}
	if code := retry.Run(context.Background()); code != 1 || strings.Contains(stdout.String(), "rename recovered") || len(prompt.Requests) != 0 {
		t.Fatalf("retry code=%d stdout=%s stderr=%s prompts=%d", code, stdout.String(), stderr.String(), len(prompt.Requests))
	}
	identity, err := os.ReadFile(configsync.HostIDPath(home))
	if err != nil || string(identity) != "owner-a\n" {
		t.Fatal("rollback did not retain old owner")
	}
	if _, err := os.Lstat(filepath.Join(source, "hosts/owner-a/config.env")); err != nil {
		t.Fatal("rollback did not restore old source")
	}
}

func TestAssessedHostRenameCallbacksRefuseReplacementJournal(t *testing.T) {
	for _, entry := range []string{"generic-local-to-source", "early-source-target"} {
		t.Run(entry, func(t *testing.T) {
			program, loaded, home, source, _ := pendingSourceRenameFixture(t, false)
			path := configsync.HostIDRenameTransactionPath(home)
			sourcePayload, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var journal map[string]any
			if err := json.Unmarshal(sourcePayload, &journal); err != nil {
				t.Fatal(err)
			}
			if entry == "generic-local-to-source" {
				journal["schemaVersion"] = 1
				delete(journal, "source")
				payload, err := json.Marshal(journal)
				if err != nil {
					t.Fatal(err)
				}
				writeConfigCommandFile(t, path, string(payload))
			}
			assessed, pending, err := configsync.AssessHostIDRenameRecovery(home)
			if err != nil || !pending {
				t.Fatalf("assessment err=%v pending=%v", err, pending)
			}
			var authority *hostRenameAuthority
			if entry == "early-source-target" {
				authority, err = program.observeHostRenameAuthority(context.Background(), home)
				if err != nil {
					t.Fatal(err)
				}
				defer authority.close()
				journal["newHostId"] = "owner-c"
				sourcePayload, err = json.Marshal(journal)
				if err != nil {
					t.Fatal(err)
				}
			}
			writeConfigCommandFile(t, path, string(sourcePayload))
			head := configSourceGitOutput(t, source, "rev-parse", "HEAD")
			gitDir := strings.TrimSpace(configSourceGitOutput(t, source, "rev-parse", "--absolute-git-dir"))
			index, err := os.ReadFile(filepath.Join(gitDir, "index"))
			if err != nil {
				t.Fatal(err)
			}
			manifest, err := os.ReadFile(configsync.ManifestPath(home))
			if err != nil {
				t.Fatal(err)
			}
			cachePath := filepath.Join(home, config.GitSettingsRelativePath, "config.env")
			cache, err := os.ReadFile(cachePath)
			if err != nil {
				t.Fatal(err)
			}
			store := ownerinventory.Connections{Root: filepath.Join(loaded.Context.Paths.DataHome, "owner-inventory")}
			if err := program.recoverAssessedHostRename(context.Background(), assessed, store, authority); !errors.Is(err, configsync.ErrPlanStale) {
				t.Fatalf("replacement callback=%v", err)
			}
			afterJournal, _ := os.ReadFile(path)
			afterIndex, _ := os.ReadFile(filepath.Join(gitDir, "index"))
			afterManifest, _ := os.ReadFile(configsync.ManifestPath(home))
			afterCache, _ := os.ReadFile(cachePath)
			if !bytes.Equal(afterJournal, sourcePayload) || !bytes.Equal(index, afterIndex) || !bytes.Equal(manifest, afterManifest) || !bytes.Equal(cache, afterCache) || head != configSourceGitOutput(t, source, "rev-parse", "HEAD") {
				t.Fatal("replacement callback mutated journal, Git, manifest, index or cache")
			}
			identity, _ := os.ReadFile(configsync.HostIDPath(home))
			if string(identity) != "owner-a\n" {
				t.Fatal("replacement callback published identity")
			}
		})
	}
}
