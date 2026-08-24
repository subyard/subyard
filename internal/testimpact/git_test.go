package testimpact

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

const emptyTreeSHA1 = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

func TestDecodeRawDiffAcceptsCanonicalStatusesAndLiteralPaths(t *testing.T) {
	pathWithControls := "dir/line\nwith\ttab\x01"
	raw := rawRecord("000000", "100644", "A", "added") +
		rawRecord("100644", "100755", "M", pathWithControls) +
		rawRecord("100644", "000000", "D", "deleted") +
		rawRecord("100644", "120000", "T", "typed") +
		rawRecord("100644", "100644", "R075", "rename-old", "rename-new") +
		rawRecord("100755", "100755", "C100", "copy-old", "copy-new")

	got, err := decodeRawDiff([]byte(raw))
	if err != nil {
		t.Fatalf("decodeRawDiff() error = %v", err)
	}
	want := ChangeSet{SchemaVersion: 1, Changes: []Change{
		{Status: "A", OldMode: "000000", NewMode: "100644", NewPath: stringPointer("added")},
		{Status: "C", Similarity: intPointer(100), OldMode: "100755", NewMode: "100755", OldPath: stringPointer("copy-old"), NewPath: stringPointer("copy-new")},
		{Status: "D", OldMode: "100644", NewMode: "000000", OldPath: stringPointer("deleted")},
		{Status: "M", OldMode: "100644", NewMode: "100755", OldPath: &pathWithControls, NewPath: &pathWithControls},
		{Status: "R", Similarity: intPointer(75), OldMode: "100644", NewMode: "100644", OldPath: stringPointer("rename-old"), NewPath: stringPointer("rename-new")},
		{Status: "T", OldMode: "100644", NewMode: "120000", OldPath: stringPointer("typed"), NewPath: stringPointer("typed")},
	}}
	assertChangeSet(t, got, want)
}

func TestDecodeRawDiffRejectsMalformedAndUnmergedRecords(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
	}{
		{"partial rename", []byte(rawRecord("100644", "100644", "R100", "old"))},
		{"missing colon", []byte("100644 100644 abc def M\x00file\x00")},
		{"missing header field", []byte(":100644 100644 abc M\x00file\x00")},
		{"missing path terminator", []byte(":100644 100644 abc def M\x00file")},
		{"unknown status", []byte(rawRecord("100644", "100644", "X", "file"))},
		{"invalid similarity", []byte(rawRecord("100644", "100644", "Rabc", "old", "new"))},
		{"invalid utf8 path", append([]byte(":100644 100644 abc def M\x00"), []byte{'f', 0xff, 0}...)},
		{"unmerged status", []byte(rawRecord("000000", "000000", "U", "conflict"))},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodeRawDiff(test.raw); err == nil {
				t.Fatal("decodeRawDiff() accepted invalid raw diff")
			}
		})
	}
}

func TestReadCommitChangesReportsCommittedAddDeleteModifyModeAndRename(t *testing.T) {
	repo := newGitRepository(t)
	writeFile(t, repo, "modify.txt", "before\n", 0o644)
	writeFile(t, repo, "delete.txt", "delete\n", 0o644)
	writeFile(t, repo, "mode.txt", "mode\n", 0o644)
	writeFile(t, repo, "rename-old.txt", "rename content\n", 0o644)
	gitRun(t, repo, "add", "--all")
	gitRun(t, repo, "commit", "-m", "base")
	base := gitOutput(t, repo, "rev-parse", "HEAD")

	writeFile(t, repo, "modify.txt", "after\n", 0o644)
	removePath(t, filepath.Join(repo, "delete.txt"))
	if err := os.Chmod(filepath.Join(repo, "mode.txt"), 0o755); err != nil {
		t.Fatalf("chmod mode.txt: %v", err)
	}
	if err := os.Rename(filepath.Join(repo, "rename-old.txt"), filepath.Join(repo, "rename-new.txt")); err != nil {
		t.Fatalf("rename fixture: %v", err)
	}
	writeFile(t, repo, "added.txt", "added\n", 0o644)
	gitRun(t, repo, "add", "--all")
	gitRun(t, repo, "commit", "-m", "head")
	head := gitOutput(t, repo, "rev-parse", "HEAD")

	got, err := ReadCommitChanges(context.Background(), repo, base, head)
	if err != nil {
		t.Fatalf("ReadCommitChanges() error = %v", err)
	}
	want := ChangeSet{SchemaVersion: 1, Changes: []Change{
		{Status: "A", OldMode: "000000", NewMode: "100644", NewPath: stringPointer("added.txt")},
		{Status: "D", OldMode: "100644", NewMode: "000000", OldPath: stringPointer("delete.txt")},
		{Status: "M", OldMode: "100644", NewMode: "100755", OldPath: stringPointer("mode.txt"), NewPath: stringPointer("mode.txt")},
		{Status: "M", OldMode: "100644", NewMode: "100644", OldPath: stringPointer("modify.txt"), NewPath: stringPointer("modify.txt")},
		{Status: "R", Similarity: intPointer(100), OldMode: "100644", NewMode: "100644", OldPath: stringPointer("rename-old.txt"), NewPath: stringPointer("rename-new.txt")},
	}}
	assertChangeSet(t, got, want)

	empty, err := ReadCommitChanges(context.Background(), repo, head, head)
	if err != nil {
		t.Fatalf("ReadCommitChanges() empty diff error = %v", err)
	}
	assertChangeSet(t, empty, ChangeSet{SchemaVersion: 1, Changes: []Change{}})

	gitRun(t, repo, "checkout", "--detach", head)
	detached, err := ReadCommitChanges(context.Background(), repo, base, "HEAD")
	if err != nil {
		t.Fatalf("ReadCommitChanges() detached HEAD error = %v", err)
	}
	assertChangeSet(t, detached, want)

	if _, err := ReadCommitChanges(context.Background(), repo, "refs/heads/missing", head); err == nil {
		t.Fatal("ReadCommitChanges() accepted missing base ref")
	}
}

func TestReadCurrentChangesCombinesStagedUnstagedUntrackedAndExcludesIgnored(t *testing.T) {
	repo := newGitRepository(t)
	writeFile(t, repo, ".gitignore", "ignored.tmp\n", 0o644)
	writeFile(t, repo, "staged.txt", "before\n", 0o644)
	writeFile(t, repo, "unstaged.txt", "before\n", 0o644)
	writeFile(t, repo, "deleted.txt", "before\n", 0o644)
	writeFile(t, repo, "mode.txt", "before\n", 0o644)
	writeFile(t, repo, "rename-old.txt", "rename\n", 0o644)
	gitRun(t, repo, "add", "--all")
	gitRun(t, repo, "commit", "-m", "base")
	base := gitOutput(t, repo, "rev-parse", "HEAD")

	writeFile(t, repo, "staged.txt", "staged\n", 0o644)
	gitRun(t, repo, "add", "staged.txt")
	writeFile(t, repo, "unstaged.txt", "unstaged\n", 0o644)
	removePath(t, filepath.Join(repo, "deleted.txt"))
	if err := os.Chmod(filepath.Join(repo, "mode.txt"), 0o755); err != nil {
		t.Fatalf("chmod mode.txt: %v", err)
	}
	gitRun(t, repo, "mv", "rename-old.txt", "rename-new.txt")
	writeFile(t, repo, "untracked.txt", "new\n", 0o644)
	writeFile(t, repo, "ignored.tmp", "ignored\n", 0o644)

	got, err := ReadCurrentChanges(context.Background(), repo, base)
	if err != nil {
		t.Fatalf("ReadCurrentChanges() error = %v", err)
	}
	want := ChangeSet{SchemaVersion: 1, Changes: []Change{
		{Status: "A", OldMode: "000000", NewMode: "100644", NewPath: stringPointer("untracked.txt")},
		{Status: "D", OldMode: "100644", NewMode: "000000", OldPath: stringPointer("deleted.txt")},
		{Status: "M", OldMode: "100644", NewMode: "100755", OldPath: stringPointer("mode.txt"), NewPath: stringPointer("mode.txt")},
		{Status: "M", OldMode: "100644", NewMode: "100644", OldPath: stringPointer("staged.txt"), NewPath: stringPointer("staged.txt")},
		{Status: "M", OldMode: "100644", NewMode: "100644", OldPath: stringPointer("unstaged.txt"), NewPath: stringPointer("unstaged.txt")},
		{Status: "R", Similarity: intPointer(100), OldMode: "100644", NewMode: "100644", OldPath: stringPointer("rename-old.txt"), NewPath: stringPointer("rename-new.txt")},
	}}
	assertChangeSet(t, got, want)
}

func TestReadCurrentChangesSupportsCleanDetachedLinkedAndUnbornRepositories(t *testing.T) {
	repo := newGitRepository(t)
	writeFile(t, repo, "tracked.txt", "tracked\n", 0o644)
	gitRun(t, repo, "add", "tracked.txt")
	gitRun(t, repo, "commit", "-m", "base")
	base := gitOutput(t, repo, "rev-parse", "HEAD")
	gitRun(t, repo, "checkout", "--detach", base)

	clean, err := ReadCurrentChanges(context.Background(), repo, base)
	if err != nil {
		t.Fatalf("ReadCurrentChanges() clean detached error = %v", err)
	}
	assertChangeSet(t, clean, ChangeSet{SchemaVersion: 1, Changes: []Change{}})

	linked := filepath.Join(t.TempDir(), "linked")
	gitRun(t, repo, "worktree", "add", "--detach", linked, base)
	t.Cleanup(func() { gitRun(t, repo, "worktree", "remove", "--force", linked) })
	linkedClean, err := ReadCurrentChanges(context.Background(), linked, base)
	if err != nil {
		t.Fatalf("ReadCurrentChanges() linked worktree error = %v", err)
	}
	assertChangeSet(t, linkedClean, ChangeSet{SchemaVersion: 1, Changes: []Change{}})

	clone := filepath.Join(t.TempDir(), "ordinary-clone")
	command := exec.Command("git", "clone", "--quiet", repo, clone)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v\n%s", err, output)
	}
	cloneClean, err := ReadCurrentChanges(context.Background(), clone, base)
	if err != nil {
		t.Fatalf("ReadCurrentChanges() ordinary clone error = %v", err)
	}
	assertChangeSet(t, cloneClean, ChangeSet{SchemaVersion: 1, Changes: []Change{}})

	unborn := newGitRepository(t)
	writeFile(t, unborn, "first.txt", "first\n", 0o644)
	gitRun(t, unborn, "add", "first.txt")
	writeFile(t, unborn, "second.txt", "second\n", 0o644)
	unbornChanges, err := ReadCurrentChanges(context.Background(), unborn, emptyTreeSHA1)
	if err != nil {
		t.Fatalf("ReadCurrentChanges() unborn error = %v", err)
	}
	assertChangeSet(t, unbornChanges, ChangeSet{SchemaVersion: 1, Changes: []Change{
		{Status: "A", OldMode: "000000", NewMode: "100644", NewPath: stringPointer("first.txt")},
		{Status: "A", OldMode: "000000", NewMode: "100644", NewPath: stringPointer("second.txt")},
	}})

	if _, err := ReadCurrentChanges(context.Background(), repo, "refs/heads/missing"); err == nil {
		t.Fatal("ReadCurrentChanges() accepted missing base ref")
	}
}

func TestReadCurrentChangesRejectsUnmergedIndex(t *testing.T) {
	repo := newGitRepository(t)
	writeFile(t, repo, "conflict.txt", "base\n", 0o644)
	gitRun(t, repo, "add", "conflict.txt")
	gitRun(t, repo, "commit", "-m", "base")
	base := gitOutput(t, repo, "rev-parse", "HEAD")
	gitRun(t, repo, "checkout", "-b", "left")
	writeFile(t, repo, "conflict.txt", "left\n", 0o644)
	gitRun(t, repo, "commit", "-am", "left")
	gitRun(t, repo, "checkout", "-b", "right", base)
	writeFile(t, repo, "conflict.txt", "right\n", 0o644)
	gitRun(t, repo, "commit", "-am", "right")
	command := exec.Command("git", "-C", repo, "merge", "left")
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("git merge unexpectedly succeeded: %s", output)
	}

	if _, err := ReadCurrentChanges(context.Background(), repo, base); err == nil {
		t.Fatal("ReadCurrentChanges() accepted unmerged index")
	}
}

func TestReadCommitChangesRejectsUnavailableShallowBase(t *testing.T) {
	source := newGitRepository(t)
	writeFile(t, source, "file.txt", "one\n", 0o644)
	gitRun(t, source, "add", "file.txt")
	gitRun(t, source, "commit", "-m", "one")
	oldCommit := gitOutput(t, source, "rev-parse", "HEAD")
	writeFile(t, source, "file.txt", "two\n", 0o644)
	gitRun(t, source, "commit", "-am", "two")

	clone := filepath.Join(t.TempDir(), "shallow")
	command := exec.Command("git", "clone", "--quiet", "--depth=1", "file://"+source, clone)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v\n%s", err, output)
	}
	if _, err := ReadCommitChanges(context.Background(), clone, oldCommit, "HEAD"); err == nil {
		t.Fatal("ReadCommitChanges() accepted unavailable shallow base")
	}
}

func TestReadCommitChangesRejectsBareRepository(t *testing.T) {
	bare := filepath.Join(t.TempDir(), "bare.git")
	command := exec.Command("git", "init", "--quiet", "--bare", bare)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, output)
	}
	if _, err := ReadCommitChanges(context.Background(), bare, "HEAD", "HEAD"); err == nil {
		t.Fatal("ReadCommitChanges() accepted bare repository")
	}
}

func TestReadCommitChangesPreservesDeadlineCancellation(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatalf("create fake worktree metadata: %v", err)
	}
	fakeBin := t.TempDir()
	writeExecutable(t, filepath.Join(fakeBin, "git"), `#!/bin/sh
printf 'unsafe child output' >&2
exec /bin/sleep 5
`)
	t.Setenv("PATH", fakeBin)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := ReadCommitChanges(ctx, repo, "base", "head")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ReadCommitChanges() error = %v, want context deadline identity", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("ReadCommitChanges() cancellation took %v, want at most 2s", elapsed)
	}
	if !strings.Contains(err.Error(), "resolve base commit") || !strings.Contains(err.Error(), "git rev-parse") {
		t.Fatalf("ReadCommitChanges() error = %q, want sanitized operation context", err)
	}
	if strings.Contains(err.Error(), "unsafe child output") {
		t.Fatalf("ReadCommitChanges() exposed canceled child output: %q", err)
	}
}

func TestReadCommitChangesIsolatesGitEnvironmentAndUsesCanonicalRepository(t *testing.T) {
	realRepo := t.TempDir()
	if err := os.Mkdir(filepath.Join(realRepo, ".git"), 0o755); err != nil {
		t.Fatalf("create fake worktree metadata: %v", err)
	}
	symlinkParent := t.TempDir()
	repoLink := filepath.Join(symlinkParent, "repository-link")
	if err := os.Symlink(realRepo, repoLink); err != nil {
		t.Fatalf("symlink repository: %v", err)
	}
	fakeBin := t.TempDir()
	script := filepath.Join(fakeBin, "git")
	scriptBody := fmt.Sprintf(`#!/bin/sh
if [ "$LC_ALL" != C ] || [ "$GIT_CONFIG_NOSYSTEM" != 1 ] || [ "$GIT_CONFIG_GLOBAL" != /dev/null ] || [ "$GIT_OPTIONAL_LOCKS" != 0 ]; then
  exit 90
fi
if [ "${GIT_DIR+x}" = x ] || [ "${GIT_WORK_TREE+x}" = x ] || [ "${GIT_INDEX_FILE+x}" = x ] || [ "${GIT_CONFIG_COUNT+x}" = x ] || [ "${GIT_CONFIG_KEY_0+x}" = x ] || [ "${GIT_CONFIG_VALUE_0+x}" = x ] || [ "${GIT_EXTERNAL_DIFF+x}" = x ] || [ "${GIT_DIFF_OPTS+x}" = x ] || [ "${GIT_CONFIG_PARAMETERS+x}" = x ] || [ "${HOME+x}" = x ]; then
  exit 91
fi
if [ "$1" != -c ] || [ "$2" != %s ] || [ "$3" != -C ] || [ "$4" != %s ]; then
  exit 92
fi
shift 4
case "$1" in
  rev-parse)
    [ "$2" = --verify ] && [ "$3" = --end-of-options ] || exit 93
    printf '1111111111111111111111111111111111111111\n'
    ;;
  diff)
    [ "$2" = --raw ] && [ "$3" = -z ] && [ "$4" = --no-ext-diff ] && [ "$5" = --find-renames=50%% ] && [ "$6" = --find-copies=50%% ] || exit 94
    printf ':000000 100644 0000000 1111111 A\0selected.txt\0'
    ;;
  *) exit 95 ;;
esac
`, "safe.directory="+realRepo, realRepo)
	writeExecutable(t, script, scriptBody)
	t.Setenv("PATH", fakeBin)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "hostile.git"))
	t.Setenv("GIT_WORK_TREE", t.TempDir())
	t.Setenv("GIT_INDEX_FILE", filepath.Join(t.TempDir(), "index"))
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "diff.hostile.textconv")
	t.Setenv("GIT_CONFIG_VALUE_0", "false")
	t.Setenv("GIT_CONFIG_PARAMETERS", "'safe.directory'='*'")
	t.Setenv("GIT_EXTERNAL_DIFF", "false")
	t.Setenv("GIT_DIFF_OPTS", "--stat")

	got, err := ReadCommitChanges(context.Background(), repoLink, "base", "head")
	if err != nil {
		t.Fatalf("ReadCommitChanges() error = %v", err)
	}
	assertChangeSet(t, got, ChangeSet{SchemaVersion: 1, Changes: []Change{
		{Status: "A", OldMode: "000000", NewMode: "100644", NewPath: stringPointer("selected.txt")},
	}})
}

func TestReadCommitChangesTrustsCurrentMixedOwnershipCheckoutWithoutGlobalConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mixed checkout ownership fixture is Unix-specific")
	}
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("absolute repository path: %v", err)
	}
	gitDirInfo, err := os.Stat(filepath.Join(repo, ".git"))
	if err != nil {
		t.Fatalf("stat .git: %v", err)
	}
	repoInfo, err := os.Stat(repo)
	if err != nil {
		t.Fatalf("stat repository: %v", err)
	}
	gitDirStat, gitDirOK := gitDirInfo.Sys().(*syscall.Stat_t)
	repoStat, repoOK := repoInfo.Sys().(*syscall.Stat_t)
	if !gitDirOK || !repoOK || gitDirStat.Uid == repoStat.Uid {
		t.Skip("checkout does not expose the expected mixed-ownership fixture")
	}
	t.Setenv("PATH", "/usr/bin:/bin")

	withoutTrust, err := runRealGitWithBoundaryTimeout(t,
		"-C", repo, "rev-parse", "--verify", "HEAD",
	)
	assertDubiousOwnershipRejection(t, withoutTrust, err)

	unrelated := t.TempDir()
	wrongExactTrust, err := runRealGitWithBoundaryTimeout(t,
		"-c", "safe.directory="+unrelated,
		"-C", repo, "rev-parse", "--verify", "HEAD",
	)
	assertDubiousOwnershipRejection(t, wrongExactTrust, err)

	got, err := ReadCommitChanges(context.Background(), repo, "HEAD", "HEAD")
	if err != nil {
		t.Fatalf("ReadCommitChanges() mixed-ownership checkout error = %v", err)
	}
	assertChangeSet(t, got, ChangeSet{SchemaVersion: 1, Changes: []Change{}})

	afterAdapter, err := runRealGitWithBoundaryTimeout(t,
		"-C", repo, "rev-parse", "--verify", "HEAD",
	)
	assertDubiousOwnershipRejection(t, afterAdapter, err)
}

func rawRecord(oldMode, newMode, status string, paths ...string) string {
	record := ":" + oldMode + " " + newMode + " abcdef0 1234567 " + status + "\x00"
	for _, path := range paths {
		record += path + "\x00"
	}
	return record
}

func newGitRepository(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	gitRun(t, repo, "init", "--quiet")
	gitRun(t, repo, "config", "user.name", "Test User")
	gitRun(t, repo, "config", "user.email", "test@example.com")
	gitRun(t, repo, "config", "core.filemode", "true")
	return repo
}

func gitRun(t *testing.T, repo string, args ...string) {
	t.Helper()
	commandArgs := append([]string{"-C", repo}, args...)
	command := exec.Command("git", commandArgs...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func gitOutput(t *testing.T, repo string, args ...string) string {
	t.Helper()
	commandArgs := append([]string{"-C", repo}, args...)
	command := exec.Command("git", commandArgs...)
	output, err := command.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(output))
}

func runRealGitWithBoundaryTimeout(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "git", args...)
	command.Env = []string{
		"PATH=/usr/bin:/bin",
		"LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_OPTIONAL_LOCKS=0",
	}
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("real Git boundary diagnostic timed out: %v", ctx.Err())
	}
	return output, err
}

func assertDubiousOwnershipRejection(t *testing.T, output []byte, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("real Git accepted mixed-owned checkout without its exact safe.directory: %s", output)
	}
	message := string(output)
	if !strings.Contains(message, "detected dubious ownership") || !strings.Contains(message, "safe.directory") {
		t.Fatalf("real Git rejection = %q, want dubious-ownership safe.directory error", message)
	}
}

func writeFile(t *testing.T, repo, name, contents string, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(repo, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir fixture parent: %v", err)
	}
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatalf("write fixture %s: %v", name, err)
	}
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatalf("write executable fixture: %v", err)
	}
}

func removePath(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove fixture %s: %v", path, err)
	}
}

func stringPointer(value string) *string { return &value }

func intPointer(value int) *int { return &value }

func assertChangeSet(t *testing.T, got, want ChangeSet) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("change set mismatch\n got: %#v\nwant: %#v", got, want)
	}
}
