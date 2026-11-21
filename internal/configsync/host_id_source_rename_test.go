package configsync

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/testkit/interrupt"
)

func TestHostRenameRefusesHistoricalUnmigratedSSHPortsWithoutMutation(t *testing.T) {
	for _, variant := range []string{"host-only", "host-and-yard", "yard-pin-only"} {
		t.Run(variant, func(t *testing.T) {
			fixture := newSyncFixture(t, "owner-b")
			fixture.writeSource("hosts/owner-a/config.env", "SSH_PORT=2233\n")
			if variant != "host-only" {
				fixture.writeSource("hosts/owner-a/yards/demo/config.env", "SSH_PORT=2234\n")
			}
			fixture.commit("historical raw SSH settings")
			runSyncGit(t, fixture.source, "config", "user.name", "Subyard Test")
			runSyncGit(t, fixture.source, "config", "user.email", "test@invalid")
			options := fixture.options(false)
			catalog, err := config.LoadCatalog(options.RepositoryRoot)
			if err != nil {
				t.Fatal(err)
			}
			// Construct the old on-disk manifest directly, without running the new
			// BuildPlan/Apply migration that would normalize its managed digests.
			source, err := readSource(options, "owner-a", catalog)
			if err != nil {
				t.Fatal(err)
			}
			manifest := Manifest{SchemaVersion: 2, Generation: 1, HostID: "owner-b", SourceID: source.id, SourceCommit: source.commit, SourceDigest: source.digest, SourceSchema: 1}
			for path, file := range source.files {
				manifest.Files = append(manifest.Files, ManagedFile{Path: path, Digest: file.Digest, Mode: file.Mode, Generation: 1})
			}
			payload, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			writeSyncTestFile(t, HostIDPath(fixture.configHome), "owner-b\n", 0o600)
			writeSyncTestFile(t, ManifestPath(fixture.configHome), string(payload), 0o600)
			if variant == "yard-pin-only" {
				writeSyncTestFile(t, filepath.Join(fixture.configHome, "yards/demo/config.env"), "SSH_PORT=2234\n", 0o600)
			}
			if err := RegisterSource(fixture.configHome, fixture.source); err != nil {
				t.Fatal(err)
			}
			index, err := os.ReadFile(filepath.Join(fixture.source, ".git/index"))
			if err != nil {
				t.Fatal(err)
			}
			objects, _ := gitOutput(fixture.source, "count-objects", "-v")
			if _, err := PrepareHostIDRenameWithSource(options, "owner-b", "owner-a"); err == nil || !strings.Contains(err.Error(), "local SSH port migration") {
				t.Fatalf("unmigrated historical recovery = %v", err)
			}
			assertSyncTestFile(t, HostIDPath(fixture.configHome), "owner-b\n", 0o600)
			assertSyncTestFile(t, ManifestPath(fixture.configHome), string(payload), 0o600)
			if head, _ := gitOutput(fixture.source, "rev-parse", "HEAD"); strings.TrimSpace(head) != source.commit {
				t.Fatal("refusal changed source HEAD")
			}
			if current, _ := os.ReadFile(filepath.Join(fixture.source, ".git/index")); !bytes.Equal(current, index) {
				t.Fatal("refusal changed source index")
			}
			if current, _ := gitOutput(fixture.source, "count-objects", "-v"); current != objects {
				t.Fatal("refusal wrote Git objects")
			}
			if status, _ := gitOutput(fixture.source, "status", "--porcelain"); status != "" {
				t.Fatal("refusal changed source worktree")
			}
			for _, path := range []string{config.GitSettingsRelativePath, HostIDRenameTransactionPath(fixture.configHome), filepath.Join(fixture.configHome, "yards/default/config.env")} {
				if path == config.GitSettingsRelativePath {
					path = filepath.Join(fixture.configHome, path)
				}
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("refusal published %s: %v", path, err)
				}
			}
			if variant == "yard-pin-only" {
				assertSyncTestFile(t, filepath.Join(fixture.configHome, "yards/demo/config.env"), "SSH_PORT=2234\n", 0o600)
			} else if _, err := os.Stat(filepath.Join(fixture.configHome, "yards")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("refusal created local yard settings")
			}
		})
	}
}

func TestHostRenameValidatesNormalizedSSHCacheProjection(t *testing.T) {
	fixture := renameSourceFixture(t)
	writeSyncTestFile(t, filepath.Join(fixture.configHome, "yards/demo/config.env"), "SSH_PORT=2222\n", 0o600)
	if _, err := PrepareHostIDRenameWithSource(fixture.options(false), "owner-b", ""); err == nil || !strings.Contains(err.Error(), "SSH port") {
		t.Fatalf("normalized projected port collision = %v", err)
	}
	assertSyncTestFile(t, HostIDPath(fixture.configHome), "owner-a\n", 0o600)
}

func renameSourceFixture(t *testing.T) *syncFixture {
	t.Helper()
	fixture := newSyncFixture(t, "owner-a")
	fixture.writeSource("shared/config.env", "YARD_IMAGE=images:debian/13\n")
	fixture.writeSource("hosts/owner-a/config.env", "SSH_PORT=2233\n")
	fixture.writeSource("hosts/owner-a/yards/demo/config.env", "SSH_PORT=2234\n")
	fixture.writeSource("hosts/owner-a/yards/demo/overrides/agents/codex/rules/repo.rules", "allow\n")
	fixture.commit("initial")
	runSyncGit(t, fixture.source, "config", "user.name", "Subyard Test")
	runSyncGit(t, fixture.source, "config", "user.email", "test@invalid")
	plan, err := BuildPlan(fixture.options(false))
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(plan); err != nil {
		t.Fatal(err)
	}
	if err := RegisterSource(fixture.configHome, fixture.source); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestRegisteredHostRenamePreservesSettingsAndCommitsOnlyMove(t *testing.T) {
	fixture := renameSourceFixture(t)
	writeSyncTestFile(t, filepath.Join(fixture.configHome, "config.env"), "YARD_IMAGE=images:ubuntu/24.04\n", 0o600)
	before, err := gitOutput(fixture.source, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	objects, err := gitOutput(fixture.source, "count-objects", "-v")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PrepareHostIDRenameWithSource(fixture.options(false), "owner-b", "")
	if err != nil {
		t.Fatal(err)
	}
	afterObjects, _ := gitOutput(fixture.source, "count-objects", "-v")
	if objects != afterObjects {
		t.Fatal("preview wrote source Git objects")
	}
	if err := ApplyHostIDRename(plan); err != nil {
		t.Fatal(err)
	}
	assertSyncTestFile(t, HostIDPath(fixture.configHome), "owner-b\n", 0o600)
	assertSyncTestFile(t, filepath.Join(fixture.configHome, "config.env"), "YARD_IMAGE=images:ubuntu/24.04\n", 0o600)
	assertSyncTestFile(t, filepath.Join(fixture.source, "hosts/owner-b/yards/demo/config.env"), "SSH_PORT=2234\n", 0o600)
	assertSyncTestFile(t, filepath.Join(fixture.configHome, "yards/demo/config.env"), "SSH_PORT='2234'\n", 0o600)
	assertSyncTestFile(t, filepath.Join(fixture.configHome, config.GitSettingsRelativePath, "config.env"), "", 0o600)
	assertSyncTestFile(t, filepath.Join(fixture.configHome, config.GitSettingsRelativePath, "yards/demo/config.env"), "", 0o600)
	assertSyncTestFile(t, filepath.Join(fixture.configHome, config.GitSettingsRelativePath, "yards/demo/overrides/agents/codex/rules/repo.rules"), "allow\n", 0o644)
	diff, err := gitOutput(fixture.source, "diff", "--name-status", "-M100%", strings.TrimSpace(before), "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(diff), "\n") {
		if !strings.HasPrefix(line, "R100\t") {
			t.Fatalf("non-content-preserving commit: %s", diff)
		}
	}
	next, err := BuildPlan(fixture.options(false))
	if err != nil {
		t.Fatal(err)
	}
	if next.NeedsApply() {
		t.Fatalf("rename did not converge: %+v", next)
	}
	if err := ApplyHostIDRename(plan); !errors.Is(err, ErrPlanStale) {
		t.Fatalf("replay = %v", err)
	}
}

func TestRegisteredHostRenameExplicitRecoveryRestoresDeletedCache(t *testing.T) {
	fixture := renameSourceFixture(t)
	before, err := readManifest(fixture.configHome)
	if err != nil {
		t.Fatal(err)
	}
	before.HostID = "owner-b"
	payload, _ := json.Marshal(before)
	writeSyncTestFile(t, HostIDPath(fixture.configHome), "owner-b\n", 0o600)
	writeSyncTestFile(t, ManifestPath(fixture.configHome), string(payload), 0o600)
	for _, file := range before.Files {
		if err := os.Remove(filepath.Join(fixture.configHome, file.Path)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := BuildPlan(fixture.options(false)); err == nil || !strings.Contains(err.Error(), "preserved owner") {
		t.Fatalf("mismatched empty sync admitted: %v", err)
	}
	if _, err := PrepareHostIDRenameWithSource(fixture.options(false), "owner-c", ""); err == nil {
		t.Fatal("plain rename admitted defective provenance with a missing current subtree")
	}
	plan, err := PrepareHostIDRenameWithSource(fixture.options(false), "owner-b", "owner-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyHostIDRename(plan); err != nil {
		t.Fatal(err)
	}
	assertSyncTestFile(t, filepath.Join(fixture.configHome, "yards/demo/config.env"), "SSH_PORT='2234'\n", 0o600)
	assertSyncTestFile(t, filepath.Join(fixture.configHome, config.GitSettingsRelativePath, "config.env"), "", 0o600)
	assertSyncTestFile(t, filepath.Join(fixture.configHome, config.GitSettingsRelativePath, "yards/demo/config.env"), "", 0o600)
	next, err := BuildPlan(fixture.options(false))
	if err != nil {
		t.Fatal(err)
	}
	if next.NeedsApply() {
		t.Fatalf("recovery did not converge: %+v", next)
	}
}

func TestRegisteredHostRenameRefusesDirtyCollisionAndStaleLocal(t *testing.T) {
	for _, kind := range []string{"dirty", "collision", "local"} {
		t.Run(kind, func(t *testing.T) {
			fixture := renameSourceFixture(t)
			plan, err := PrepareHostIDRenameWithSource(fixture.options(false), "owner-b", "")
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "dirty":
				fixture.writeSource("notes.txt", "unrelated\n")
			case "collision":
				fixture.writeSource("hosts/owner-b/config.env", "SSH_PORT=2244\n")
			case "local":
				writeSyncTestFile(t, filepath.Join(fixture.configHome, "config.env"), "SSH_PORT=2299\n", 0o600)
			}
			if err := ApplyHostIDRename(plan); err == nil {
				t.Fatal("changed baseline admitted")
			}
			assertSyncTestFile(t, HostIDPath(fixture.configHome), "owner-a\n", 0o600)
		})
	}
}

// sourceRenameRecovery is a source rename whose journal is durable and whose recovery has
// not started yet, the state a crash right after the journal write leaves.
type sourceRenameRecovery struct {
	fixture   *syncFixture
	source    *hostSourceRename
	normalize *strings.Replacer
}

func newSourceRenameRecovery(t *testing.T) sourceRenameRecovery {
	t.Helper()
	fixture := renameSourceFixture(t)
	plan, err := PrepareHostIDRenameWithSource(fixture.options(false), "owner-b", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := finalizeSourceRename(&plan); err != nil {
		t.Fatal(err)
	}
	journal := hostIDRenameJournal{SchemaVersion: 2, OldHostID: plan.OldHostID, NewHostID: plan.NewHostID, HostIDDigest: plan.hostIDDigest, ManifestDigest: plan.manifestDigest, ManifestWasPresent: plan.manifestWasPresent, ManifestBefore: plan.manifestBefore, ManifestAfter: plan.manifestAfter, Source: plan.source}
	if err := writeHostIDRenameJournal(fixture.configHome, journal); err != nil {
		t.Fatal(err)
	}
	manifest, err := readManifest(fixture.configHome)
	if err != nil {
		t.Fatal(err)
	}
	// Fixture paths, the source identity derived from them and commit IDs differ between
	// fixtures; everything else in the signature must match byte for byte.
	normalize := strings.NewReplacer(
		filepath.Dir(fixture.source), "$BASE", plan.source.BeforeCommit, "$BEFORE-COMMIT",
		plan.source.AfterCommit, "$AFTER-COMMIT", plan.source.RecordDigest, "$RECORD-DIGEST",
		manifest.SourceID, "$SOURCE-ID",
	)
	return sourceRenameRecovery{fixture: fixture, source: plan.source, normalize: normalize}
}

type sourceRenameSignature struct {
	Config, Worktree, Git map[string]string
	Head, Status          string
}

func (s sourceRenameRecovery) signature(t *testing.T) sourceRenameSignature {
	t.Helper()
	tree := func(root string, ignore ...string) map[string]string {
		result := map[string]string{}
		for path, entry := range interrupt.Tree(t, root, ignore...) {
			result[s.normalize.Replace(path)] = entry
		}
		return result
	}
	// These records name fixture paths or commits; their normalized content replaces the digest.
	home := tree(s.fixture.configHome)
	for _, path := range []string{".sync/manifest.json", ".sync/source.json", ".sync/host-id-rename-complete.json"} {
		if entry, ok := home[path]; ok {
			content, err := os.ReadFile(filepath.Join(s.fixture.configHome, path))
			if err != nil {
				t.Fatal(err)
			}
			home[path] = strings.Join(strings.Fields(entry)[:2], " ") + " " + s.normalize.Replace(string(content))
		}
	}
	git := tree(s.source.GitDir, "objects", "logs", "refs")
	head, err := gitOutput(s.fixture.source, "rev-parse", "HEAD", "HEAD^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	status, err := gitOutput(s.fixture.source, "status", "--porcelain=v1", "--untracked-files=all", "--ignored")
	if err != nil {
		t.Fatal(err)
	}
	return sourceRenameSignature{
		Config: home, Worktree: tree(s.fixture.source, ".git"), Git: git,
		Head: s.normalize.Replace(head), Status: status,
	}
}

// recover runs recovery of the pending journal under the configuration lock, as
// RecoverHostIDRename does, with a checkpoint.
func (s sourceRenameRecovery) recover(t *testing.T, checkpoint func(string) error) error {
	t.Helper()
	unlock, err := config.LockRoot(s.fixture.configHome, true)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	journal, pending, err := readHostIDRenameJournal(s.fixture.configHome)
	if err != nil || !pending {
		t.Fatalf("journal pending=%v: %v", pending, err)
	}
	return recoverHostSourceRenameAtCheckpoint(s.fixture.configHome, journal, checkpoint)
}

// recoverUntil fails recovery at the named checkpoint.
func (s sourceRenameRecovery) recoverUntil(t *testing.T, name string) error {
	t.Helper()
	return s.recover(t, func(checkpoint string) error {
		if checkpoint == name {
			return interrupt.ErrInjected
		}
		return nil
	})
}

// assertConverged checks what the operator relies on after recovery: the new owner, the
// source commit that only moves the subtree, no pending journal or Git lock, and a sync
// that needs no further change.
func (s sourceRenameRecovery) assertConverged(t *testing.T) {
	t.Helper()
	assertSyncTestFile(t, HostIDPath(s.fixture.configHome), "owner-b\n", 0o600)
	assertSyncTestFile(t, filepath.Join(s.fixture.source, "hosts/owner-b/yards/demo/config.env"), "SSH_PORT=2234\n", 0o600)
	if _, err := os.Lstat(filepath.Join(s.fixture.source, "hosts/owner-a")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old owner subtree survived recovery: %v", err)
	}
	if head, _ := gitOutput(s.fixture.source, "rev-parse", "HEAD"); strings.TrimSpace(head) != s.source.AfterCommit {
		t.Fatalf("HEAD = %s, want the prepared rename commit", head)
	}
	for _, file := range s.source.Cache {
		assertSyncTestFile(t, filepath.Join(s.fixture.configHome, file.Path), string(file.Content), os.FileMode(file.Mode))
	}
	if _, pending, err := AssessHostIDRenameRecovery(s.fixture.configHome); err != nil || pending {
		t.Fatalf("recovery still pending=%v: %v", pending, err)
	}
	for _, path := range []string{filepath.Join(s.source.GitDir, "index.lock"), renameIndexImage(s.source)} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("recovery left %s: %v", path, err)
		}
	}
	next, err := BuildPlan(s.fixture.options(false))
	if err != nil {
		t.Fatal(err)
	}
	if next.NeedsApply() {
		t.Fatalf("recovered configuration is not converged: %+v", next)
	}
}

// The sweep cuts explicit recovery from a durable journal, not ApplyHostIDRename. Apply runs
// recoverHostIDRenameLocked again in a defer on any error, so an injected error there tests
// failure handling rather than a crash and resume; that limitation remains.
//
// Recovery's error path only releases its own index.lock, which a real crash would leave as
// a hard link to the index image. Every checkpoint except index-lock runs while the lock is
// held, so Interrupted restores that link for those cuts and resume must adopt it as after
// a crash.
//
// Known gap, left out of the sweep: recovery removes the journal before the index image, so
// a crash between the two leaves .git/subyard-host-rename-<commit>.index behind with nothing
// left to remove it. Recovery has no checkpoint there, so the sweep ends at journal-cleanup.
func TestRegisteredHostRenameRecoveryResumesAtEveryInterruption(t *testing.T) {
	interrupt.Sweep(t, interrupt.Case[sourceRenameRecovery]{
		Setup: newSourceRenameRecovery,
		Run: func(t *testing.T, s sourceRenameRecovery, in *interrupt.Injector) interrupt.Outcome {
			return interrupt.Outcome{Err: s.recover(t, in.Hook("source-rename"))}
		},
		Resume: func(t *testing.T, s sourceRenameRecovery) interrupt.Outcome {
			return interrupt.Outcome{Err: RecoverHostIDRename(s.fixture.configHome)}
		},
		Signature: func(t *testing.T, s sourceRenameRecovery) any { return s.signature(t) },
		Completed: func(t *testing.T, s sourceRenameRecovery, o interrupt.Outcome) {
			if o.Err != nil {
				t.Fatal(o.Err)
			}
			s.assertConverged(t)
		},
		Interrupted: func(t *testing.T, s sourceRenameRecovery, p interrupt.Point, o interrupt.Outcome) {
			if !errors.Is(o.Err, interrupt.ErrInjected) {
				t.Fatalf("recovery error = %v, want the injected interruption at %s", o.Err, p)
			}
			assessed, pending, err := AssessHostIDRenameRecovery(s.fixture.configHome)
			if err != nil || !pending || assessed.Target != "owner-b" || !assessed.Source {
				t.Fatalf("after %s recovery assessment=%+v pending=%v: %v", p, assessed, pending, err)
			}
			if p.Name != "index-lock" {
				if err := os.Link(renameIndexImage(s.source), filepath.Join(s.source.GitDir, "index.lock")); err != nil {
					t.Fatal(err)
				}
			}
		},
		Resumed: func(t *testing.T, s sourceRenameRecovery, p interrupt.Point, o interrupt.Outcome) {
			if o.Err != nil {
				t.Fatalf("recovery after %s: %v", p, o.Err)
			}
			s.assertConverged(t)
		},
		Repeated: func(t *testing.T, _ sourceRenameRecovery, p interrupt.Point, o interrupt.Outcome) {
			if o.Err != nil {
				t.Fatalf("second recovery after %s: %v", p, o.Err)
			}
		},
		MinPoints: 12,
		Require: []string{"index-lock", "subtree-move", "head-update", "index-publication", "cache-write",
			"manifest-publication", "identity-publication", "receipt-publication", "journal-cleanup"},
	})
}

// A local override edited after the source commit point stays as edited, and recovery
// still completes the rename around it.
func TestRegisteredHostRenameRecoveryKeepsLocalEditAfterSourceCommit(t *testing.T) {
	s := newSourceRenameRecovery(t)
	if err := s.recoverUntil(t, "index-publication"); !errors.Is(err, interrupt.ErrInjected) {
		t.Fatalf("recovery=%v", err)
	}
	if head, _ := gitOutput(s.fixture.source, "rev-parse", "HEAD"); strings.TrimSpace(head) != s.source.AfterCommit {
		t.Fatal("the cut did not land after the source commit point")
	}
	writeSyncTestFile(t, filepath.Join(s.fixture.configHome, "config.env"), "YARD_IMAGE=images:ubuntu/24.04\n", 0o600)
	for range 2 {
		if err := RecoverHostIDRename(s.fixture.configHome); err != nil {
			t.Fatal(err)
		}
	}
	assertSyncTestFile(t, filepath.Join(s.fixture.configHome, "config.env"), "YARD_IMAGE=images:ubuntu/24.04\n", 0o600)
	s.assertConverged(t)
}

// Recovery releases index.lock before it removes the journal. A Git index refresh in that
// window rewrites the stat cache but keeps the published tree, and recovery must accept it.
func TestRegisteredHostRenameRecoveryAcceptsRefreshedPublishedIndex(t *testing.T) {
	s := newSourceRenameRecovery(t)
	if err := s.recoverUntil(t, "journal-cleanup"); !errors.Is(err, interrupt.ErrInjected) {
		t.Fatalf("recovery=%v", err)
	}
	if _, err := os.Lstat(filepath.Join(s.source.GitDir, "index.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("index.lock is still held: %v", err)
	}
	runSyncGit(t, s.fixture.source, "update-index", "-q", "--really-refresh")
	index, err := os.ReadFile(filepath.Join(s.source.GitDir, "index"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(index, s.source.AfterIndex) {
		t.Fatal("the index refresh did not change the published index bytes")
	}
	if tree, err := inspectRenameIndexTree(s.source); err != nil || tree != s.source.AfterTree {
		t.Fatalf("refreshed index tree=%s: %v, want the published tree", tree, err)
	}
	if _, pending, err := AssessHostIDRenameRecovery(s.fixture.configHome); err != nil || !pending {
		t.Fatalf("recovery pending=%v: %v", pending, err)
	}
	if err := RecoverHostIDRename(s.fixture.configHome); err != nil {
		t.Fatal(err)
	}
	s.assertConverged(t)
}

func TestHostRenameIndexLockExcludesGitAndPreservesForeignReplacement(t *testing.T) {
	fixture := renameSourceFixture(t)
	plan, err := PrepareHostIDRenameWithSource(fixture.options(false), "owner-b", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := finalizeSourceRename(&plan); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(plan.source.GitDir, "index.lock")
	writeSyncTestFile(t, lockPath, "foreign lock\n", 0o600)
	if _, err := lockRenameIndex(plan.source); err == nil {
		t.Fatal("foreign index lock was adopted")
	}
	assertSyncTestFile(t, lockPath, "foreign lock\n", 0o600)
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	release, err := lockRenameIndex(plan.source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gitOutput(fixture.source, "add", "--", "subyard-config.json"); err == nil {
		t.Fatal("Git staging was admitted while rename held index.lock")
	}
	// Recovery can reclaim exactly its own hard-linked lock.
	recoveryRelease, err := lockRenameIndex(plan.source)
	if err != nil {
		t.Fatal(err)
	}
	recoveryRelease()
	writeSyncTestFile(t, lockPath, "replacement foreign lock\n", 0o600)
	release()
	assertSyncTestFile(t, lockPath, "replacement foreign lock\n", 0o600)
}

func TestRegisteredHostRenameAllowsIntentionalSubtreeRemoval(t *testing.T) {
	fixture := renameSourceFixture(t)
	runSyncGit(t, fixture.source, "rm", "-qr", "--", "hosts/owner-a")
	fixture.commit("Remove this owner overlay intentionally")
	plan, err := BuildPlan(fixture.options(false))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) == 0 {
		t.Fatal("intentional subtree removal was ignored")
	}
}

func TestExplicitHostSourceRecoveryRetryIsIdempotent(t *testing.T) {
	fixture := renameSourceFixture(t)
	plan, err := PrepareHostIDRenameWithSource(fixture.options(false), "owner-b", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyHostIDRename(plan); err != nil {
		t.Fatal(err)
	}
	before, _ := gitOutput(fixture.source, "rev-parse", "HEAD")
	repeat, err := PrepareHostIDRenameWithSource(fixture.options(false), "owner-b", "owner-a")
	if err != nil {
		t.Fatal(err)
	}
	if !repeat.AlreadyMigrated {
		t.Fatal("completed recovery was not recognized")
	}
	if err := ApplyHostIDRename(repeat); err != nil {
		t.Fatal(err)
	}
	after, _ := gitOutput(fixture.source, "rev-parse", "HEAD")
	if before != after {
		t.Fatal("idempotent recovery made another commit")
	}
}

func TestRegisteredHostRenameMissingGitIdentityChangesNothing(t *testing.T) {
	fixture := renameSourceFixture(t)
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	runSyncGit(t, fixture.source, "config", "--unset", "user.name")
	runSyncGit(t, fixture.source, "config", "--unset", "user.email")
	runSyncGit(t, fixture.source, "config", "user.useConfigOnly", "true")
	head, _ := gitOutput(fixture.source, "rev-parse", "HEAD")
	if _, err := PrepareHostIDRenameWithSource(fixture.options(false), "owner-b", ""); err == nil {
		t.Fatal("missing Git identity admitted")
	}
	after, _ := gitOutput(fixture.source, "rev-parse", "HEAD")
	if head != after {
		t.Fatal("failed identity preflight changed HEAD")
	}
	assertSyncTestFile(t, HostIDPath(fixture.configHome), "owner-a\n", 0o600)
}

func TestHostRenameRecoveryPreservesLocalEditBeforeSourceCommit(t *testing.T) {
	s := newSourceRenameRecovery(t)
	// Cut after the subtree move and before HEAD, the source commit point. A crash there
	// leaves the owned index.lock, which rollback must remove.
	if err := s.recoverUntil(t, "head-update"); !errors.Is(err, interrupt.ErrInjected) {
		t.Fatalf("recovery=%v", err)
	}
	if err := os.Link(renameIndexImage(s.source), filepath.Join(s.source.GitDir, "index.lock")); err != nil {
		t.Fatal(err)
	}
	writeSyncTestFile(t, filepath.Join(s.fixture.configHome, "config.env"), "YARD_IMAGE=images:ubuntu/24.04\n", 0o600)
	if err := RecoverHostIDRename(s.fixture.configHome); !errors.Is(err, ErrPlanStale) {
		t.Fatalf("recovery=%v", err)
	}
	assertSyncTestFile(t, HostIDPath(s.fixture.configHome), "owner-a\n", 0o600)
	assertSyncTestFile(t, filepath.Join(s.fixture.configHome, "config.env"), "YARD_IMAGE=images:ubuntu/24.04\n", 0o600)
	assertSyncTestFile(t, filepath.Join(s.fixture.source, "hosts/owner-a/config.env"), "SSH_PORT=2233\n", 0o600)
	if head, _ := gitOutput(s.fixture.source, "rev-parse", "HEAD"); strings.TrimSpace(head) != s.source.BeforeCommit {
		t.Fatal("rollback moved HEAD")
	}
	for _, path := range []string{HostIDRenameTransactionPath(s.fixture.configHome), filepath.Join(s.source.GitDir, "index.lock"), renameIndexImage(s.source)} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("rollback left %s: %v", path, err)
		}
	}
	if _, err := BuildPlan(s.fixture.options(false)); err != nil {
		t.Fatal(err)
	}
}

func TestHostRenameIndexPublicationRefusesForeignReplacement(t *testing.T) {
	s := newSourceRenameRecovery(t)
	indexPath := filepath.Join(s.source.GitDir, "index")
	before, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(s.source.GitDir, "index.lock")
	err = s.recover(t, func(checkpoint string) error {
		if checkpoint != "index-publication" {
			return nil
		}
		if err := os.Remove(lockPath); err != nil {
			return err
		}
		writeSyncTestFile(t, lockPath, "foreign replacement\n", 0o600)
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "lock changed before publication") {
		t.Fatalf("recovery=%v", err)
	}
	assertSyncTestFile(t, lockPath, "foreign replacement\n", 0o600)
	after, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("foreign replacement was published into index")
	}
	assertSyncTestFile(t, HostIDPath(s.fixture.configHome), "owner-a\n", 0o600)
	if err := RecoverHostIDRename(s.fixture.configHome); err == nil {
		t.Fatal("foreign replacement did not block native recovery")
	}
	assertSyncTestFile(t, lockPath, "foreign replacement\n", 0o600)
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	if err := RecoverHostIDRename(s.fixture.configHome); err != nil {
		t.Fatal(err)
	}
	s.assertConverged(t)
}

func TestHostRenameExactRecoveryRefusesJournalSubstitution(t *testing.T) {
	for _, kind := range []string{"local-to-source", "source-target", "source-payload"} {
		t.Run(kind, func(t *testing.T) {
			fixture := renameSourceFixture(t)
			original, err := PrepareHostIDRenameWithSource(fixture.options(false), "owner-b", "")
			if err != nil {
				t.Fatal(err)
			}
			replacement, err := PrepareHostIDRenameWithSource(fixture.options(false), "owner-c", "")
			if err != nil {
				t.Fatal(err)
			}
			if err := finalizeSourceRename(&original); err != nil {
				t.Fatal(err)
			}
			if err := finalizeSourceRename(&replacement); err != nil {
				t.Fatal(err)
			}
			asJournal := func(plan HostIDRenamePlan) hostIDRenameJournal {
				return hostIDRenameJournal{SchemaVersion: 2, OldHostID: plan.OldHostID, NewHostID: plan.NewHostID, HostIDDigest: plan.hostIDDigest, ManifestDigest: plan.manifestDigest, ManifestWasPresent: plan.manifestWasPresent, ManifestBefore: plan.manifestBefore, ManifestAfter: plan.manifestAfter, Source: plan.source}
			}
			first := asJournal(original)
			next := asJournal(replacement)
			if kind == "local-to-source" {
				first.SchemaVersion = 1
				first.Source = nil
			}
			if kind == "source-payload" {
				next = asJournal(original)
				copySource := *next.Source
				copySource.Paths = append(append([]string(nil), copySource.Paths...), "different approved preview")
				next.Source = &copySource
			}
			if err := writeHostIDRenameJournal(fixture.configHome, first); err != nil {
				t.Fatal(err)
			}
			assessed, pending, err := AssessHostIDRenameRecovery(fixture.configHome)
			if err != nil || !pending {
				t.Fatalf("assessment=%v pending=%v", err, pending)
			}
			if err := writeHostIDRenameJournal(fixture.configHome, next); err != nil {
				t.Fatal(err)
			}
			journalBefore, err := os.ReadFile(HostIDRenameTransactionPath(fixture.configHome))
			if err != nil {
				t.Fatal(err)
			}
			manifestBefore, err := os.ReadFile(ManifestPath(fixture.configHome))
			if err != nil {
				t.Fatal(err)
			}
			indexBefore, err := os.ReadFile(filepath.Join(next.Source.GitDir, "index"))
			if err != nil {
				t.Fatal(err)
			}
			headBefore, err := gitOutput(fixture.source, "rev-parse", "HEAD")
			if err != nil {
				t.Fatal(err)
			}
			if err := RecoverHostIDRenameExact(assessed); !errors.Is(err, ErrPlanStale) {
				t.Fatalf("substitution recovery=%v", err)
			}
			journalAfter, _ := os.ReadFile(HostIDRenameTransactionPath(fixture.configHome))
			manifestAfter, _ := os.ReadFile(ManifestPath(fixture.configHome))
			indexAfter, _ := os.ReadFile(filepath.Join(next.Source.GitDir, "index"))
			headAfter, _ := gitOutput(fixture.source, "rev-parse", "HEAD")
			if string(journalBefore) != string(journalAfter) || string(manifestBefore) != string(manifestAfter) || string(indexBefore) != string(indexAfter) || headBefore != headAfter {
				t.Fatal("stale recovery mutated replacement journal, manifest, index or HEAD")
			}
			assertSyncTestFile(t, HostIDPath(fixture.configHome), "owner-a\n", 0o600)
			for _, file := range next.Source.Cache {
				assertSyncTestFile(t, filepath.Join(fixture.configHome, file.Path), string(file.Content), os.FileMode(file.Mode))
			}
			if _, err := os.Lstat(filepath.Join(fixture.source, "hosts/owner-a/config.env")); err != nil {
				t.Fatal("stale recovery moved owner subtree")
			}
		})
	}
}

func TestPreparedHostRenameRefusesPendingLocalJournal(t *testing.T) {
	fixture := renameSourceFixture(t)
	plan, err := PrepareHostIDRenameWithSource(fixture.options(false), "owner-b", "")
	if err != nil {
		t.Fatal(err)
	}
	journal := hostIDRenameJournal{SchemaVersion: 1, OldHostID: plan.OldHostID, NewHostID: "owner-c", HostIDDigest: plan.hostIDDigest, ManifestDigest: plan.manifestDigest, ManifestWasPresent: true, ManifestBefore: plan.manifestBefore, ManifestAfter: plan.manifestAfter}
	if err := writeHostIDRenameJournal(fixture.configHome, journal); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(HostIDRenameTransactionPath(fixture.configHome))
	if err := ApplyHostIDRename(plan); !errors.Is(err, ErrRecoveryPending) {
		t.Fatalf("apply=%v", err)
	}
	after, _ := os.ReadFile(HostIDRenameTransactionPath(fixture.configHome))
	if string(before) != string(after) {
		t.Fatal("apply consumed unassessed local journal")
	}
	assertSyncTestFile(t, HostIDPath(fixture.configHome), "owner-a\n", 0o600)
}
