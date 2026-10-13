package configsync

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/Subyard/Subyard/internal/config"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestRegisteredHostRenameResumesAfterEachPublicationBoundary(t *testing.T) {
	for _, boundary := range []string{"journal", "move", "head", "index", "manifest", "identity", "local-edit"} {
		t.Run(boundary, func(t *testing.T) {
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
			stages := map[string]int{"journal": 0, "move": 1, "head": 2, "index": 3, "manifest": 4, "identity": 5, "local-edit": 2}
			stage := stages[boundary]
			if stage >= 1 {
				if err := os.Rename(filepath.Join(fixture.source, "hosts/owner-a"), filepath.Join(fixture.source, "hosts/owner-b")); err != nil {
					t.Fatal(err)
				}
			}
			if stage >= 2 {
				if _, err := gitOutput(fixture.source, "update-ref", "HEAD", plan.source.AfterCommit, plan.source.BeforeCommit); err != nil {
					t.Fatal(err)
				}
			}
			if stage >= 3 {
				if _, err := gitOutput(fixture.source, "read-tree", plan.source.AfterTree); err != nil {
					t.Fatal(err)
				}
			}
			if stage >= 4 {
				writeSyncTestFile(t, ManifestPath(fixture.configHome), string(plan.manifestAfter), 0o600)
			}
			if stage >= 5 {
				writeSyncTestFile(t, HostIDPath(fixture.configHome), "owner-b\n", 0o600)
			}
			if boundary == "local-edit" {
				writeSyncTestFile(t, filepath.Join(fixture.configHome, "config.env"), "YARD_IMAGE=images:ubuntu/24.04\n", 0o600)
			}
			if err := RecoverHostIDRename(fixture.configHome); err != nil {
				t.Fatal(err)
			}
			if err := RecoverHostIDRename(fixture.configHome); err != nil {
				t.Fatal(err)
			}
			next, err := BuildPlan(fixture.options(false))
			if err != nil {
				t.Fatal(err)
			}
			if next.NeedsApply() {
				t.Fatal("recovered configuration is not converged")
			}
		})
	}
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
	if err := os.Rename(filepath.Join(fixture.source, "hosts/owner-a"), filepath.Join(fixture.source, "hosts/owner-b")); err != nil {
		t.Fatal(err)
	}
	writeSyncTestFile(t, filepath.Join(fixture.configHome, "config.env"), "YARD_IMAGE=images:ubuntu/24.04\n", 0o600)
	if err := RecoverHostIDRename(fixture.configHome); !errors.Is(err, ErrPlanStale) {
		t.Fatalf("recovery=%v", err)
	}
	assertSyncTestFile(t, HostIDPath(fixture.configHome), "owner-a\n", 0o600)
	assertSyncTestFile(t, filepath.Join(fixture.configHome, "config.env"), "YARD_IMAGE=images:ubuntu/24.04\n", 0o600)
	if _, err := BuildPlan(fixture.options(false)); err != nil {
		t.Fatal(err)
	}
}

func TestHostRenameIndexPublicationRefusesForeignReplacement(t *testing.T) {
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
	indexPath := filepath.Join(plan.source.GitDir, "index")
	before, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(plan.source.GitDir, "index.lock")
	err = recoverHostSourceRenameAtCheckpoint(fixture.configHome, journal, func(checkpoint string) error {
		if checkpoint != "index-publication" {
			t.Fatal("unexpected checkpoint")
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
	assertSyncTestFile(t, HostIDPath(fixture.configHome), "owner-a\n", 0o600)
	if err := RecoverHostIDRename(fixture.configHome); err == nil {
		t.Fatal("foreign replacement did not block native recovery")
	}
	assertSyncTestFile(t, lockPath, "foreign replacement\n", 0o600)
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	if err := RecoverHostIDRename(fixture.configHome); err != nil {
		t.Fatal(err)
	}
	assertSyncTestFile(t, HostIDPath(fixture.configHome), "owner-b\n", 0o600)
	if _, err := os.Lstat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned index lock survived complete recovery: %v", err)
	}
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
