package testimpact

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ReadCurrentChanges returns the tracked index/worktree changes from base plus
// non-ignored untracked files in repo.
func ReadCurrentChanges(ctx context.Context, repo, base string) (ChangeSet, error) {
	repository, err := openGitRepository(repo)
	if err != nil {
		return ChangeSet{}, err
	}
	baseTree, err := repository.resolve(ctx, base, "^{tree}")
	if err != nil {
		return ChangeSet{}, fmt.Errorf("resolve current base: %w", err)
	}
	unmerged, err := repository.run(ctx, "ls-files", "--unmerged", "-z")
	if err != nil {
		return ChangeSet{}, fmt.Errorf("inspect unmerged files: %w", err)
	}
	if len(unmerged) != 0 {
		return ChangeSet{}, errors.New("current index contains unmerged files")
	}

	raw, err := repository.run(ctx,
		"diff", "--raw", "-z", "--no-ext-diff",
		"--find-renames=50%", "--find-copies=50%", baseTree, "--",
	)
	if err != nil {
		return ChangeSet{}, fmt.Errorf("diff current changes: %w", err)
	}
	set, err := decodeRawDiff(raw)
	if err != nil {
		return ChangeSet{}, fmt.Errorf("decode current changes: %w", err)
	}

	untracked, err := repository.run(ctx, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return ChangeSet{}, fmt.Errorf("list untracked files: %w", err)
	}
	untrackedChanges, err := repository.decodeUntracked(untracked)
	if err != nil {
		return ChangeSet{}, err
	}
	return canonicalizeChanges(append(set.Changes, untrackedChanges...))
}

// ReadCommitChanges returns changes from the base commit to the head commit.
func ReadCommitChanges(ctx context.Context, repo, base, head string) (ChangeSet, error) {
	repository, err := openGitRepository(repo)
	if err != nil {
		return ChangeSet{}, err
	}
	baseCommit, err := repository.resolve(ctx, base, "^{commit}")
	if err != nil {
		return ChangeSet{}, fmt.Errorf("resolve base commit: %w", err)
	}
	headCommit, err := repository.resolve(ctx, head, "^{commit}")
	if err != nil {
		return ChangeSet{}, fmt.Errorf("resolve head commit: %w", err)
	}

	raw, err := repository.run(ctx,
		"diff", "--raw", "-z", "--no-ext-diff",
		"--find-renames=50%", "--find-copies=50%", baseCommit, headCommit, "--",
	)
	if err != nil {
		return ChangeSet{}, fmt.Errorf("diff commits: %w", err)
	}
	set, err := decodeRawDiff(raw)
	if err != nil {
		return ChangeSet{}, fmt.Errorf("decode commit changes: %w", err)
	}
	return set, nil
}

type gitRepository struct {
	root string
}

func openGitRepository(repo string) (gitRepository, error) {
	if repo == "" {
		return gitRepository{}, errors.New("repository path is empty")
	}
	absolute, err := filepath.Abs(repo)
	if err != nil {
		return gitRepository{}, fmt.Errorf("make repository path absolute: %w", err)
	}
	root, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return gitRepository{}, fmt.Errorf("canonicalize repository path: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return gitRepository{}, fmt.Errorf("stat repository path: %w", err)
	}
	if !info.IsDir() {
		return gitRepository{}, errors.New("repository path is not a directory")
	}
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		return gitRepository{}, fmt.Errorf("repository is not a supported worktree: %w", err)
	}
	return gitRepository{root: root}, nil
}

func (repository gitRepository) resolve(ctx context.Context, ref, suffix string) (string, error) {
	if ref == "" {
		return "", errors.New("ref is empty")
	}
	output, err := repository.run(ctx, "rev-parse", "--verify", "--end-of-options", ref+suffix)
	if err != nil {
		return "", err
	}
	resolved := strings.TrimSpace(string(output))
	if resolved == "" || strings.ContainsAny(resolved, "\x00\r\n \t") {
		return "", errors.New("Git returned an invalid object ID")
	}
	return resolved, nil
}

func (repository gitRepository) run(ctx context.Context, args ...string) ([]byte, error) {
	commandArgs := make([]string, 0, len(args)+4)
	commandArgs = append(commandArgs, "-c", "safe.directory="+repository.root, "-C", repository.root)
	commandArgs = append(commandArgs, args...)
	command := exec.CommandContext(ctx, "git", commandArgs...)
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_OPTIONAL_LOCKS=0",
	}
	output, err := command.CombinedOutput()
	if err != nil {
		if contextError := ctx.Err(); contextError != nil {
			return nil, fmt.Errorf("git %s canceled: %w", args[0], contextError)
		}
		message := strings.TrimSpace(string(output))
		if message == "" {
			return nil, fmt.Errorf("git %s: %w", args[0], err)
		}
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, message)
	}
	return output, nil
}

func decodeRawDiff(raw []byte) (ChangeSet, error) {
	changes := make([]Change, 0)
	for len(raw) > 0 {
		header, remaining, found := bytes.Cut(raw, []byte{0})
		if !found {
			return ChangeSet{}, errors.New("raw diff header is not NUL-terminated")
		}
		if len(header) == 0 || header[0] != ':' {
			return ChangeSet{}, errors.New("raw diff record does not start with a colon")
		}
		fields := strings.Split(string(header[1:]), " ")
		if len(fields) != 5 || slices.Contains(fields, "") {
			return ChangeSet{}, fmt.Errorf("raw diff header has %d fields, want 5", len(fields))
		}

		oldMode, newMode, statusField := fields[0], fields[1], fields[4]
		status, similarity, pathCount, err := decodeRawStatus(statusField)
		if err != nil {
			return ChangeSet{}, err
		}
		paths := make([]string, 0, pathCount)
		for range pathCount {
			var path []byte
			path, remaining, found = bytes.Cut(remaining, []byte{0})
			if !found {
				return ChangeSet{}, fmt.Errorf("raw diff %s path is not NUL-terminated", status)
			}
			if !utf8.Valid(path) {
				return ChangeSet{}, fmt.Errorf("raw diff %s path is not valid UTF-8", status)
			}
			paths = append(paths, string(path))
		}

		change := Change{Status: status, Similarity: similarity, OldMode: oldMode, NewMode: newMode}
		switch status {
		case "A":
			change.NewPath = &paths[0]
		case "D":
			change.OldPath = &paths[0]
		case "M", "T":
			change.OldPath = &paths[0]
			change.NewPath = &paths[0]
		case "R", "C":
			change.OldPath = &paths[0]
			change.NewPath = &paths[1]
		}
		changes = append(changes, change)
		raw = remaining
	}
	return canonicalizeChanges(changes)
}

func decodeRawStatus(value string) (status string, similarity *int, pathCount int, err error) {
	if len(value) == 1 {
		switch value {
		case "A", "D", "M", "T":
			return value, nil, 1, nil
		case "U":
			return "", nil, 0, errors.New("raw diff contains an unmerged status")
		default:
			return "", nil, 0, fmt.Errorf("raw diff contains unsupported status %q", value)
		}
	}
	if len(value) != 4 || (value[0] != 'R' && value[0] != 'C') {
		return "", nil, 0, fmt.Errorf("raw diff contains invalid status %q", value)
	}
	score, parseErr := strconv.Atoi(value[1:])
	if parseErr != nil || score < 0 || score > 100 {
		return "", nil, 0, fmt.Errorf("raw diff contains invalid similarity %q", value[1:])
	}
	status = value[:1]
	return status, &score, 2, nil
}

func (repository gitRepository) decodeUntracked(raw []byte) ([]Change, error) {
	changes := make([]Change, 0)
	for len(raw) > 0 {
		pathBytes, remaining, found := bytes.Cut(raw, []byte{0})
		if !found {
			return nil, errors.New("untracked path is not NUL-terminated")
		}
		if !utf8.Valid(pathBytes) {
			return nil, errors.New("untracked path is not valid UTF-8")
		}
		path := string(pathBytes)
		if err := validatePath(path); err != nil {
			return nil, fmt.Errorf("invalid untracked path: %w", err)
		}
		mode, err := untrackedMode(filepath.Join(repository.root, filepath.FromSlash(path)))
		if err != nil {
			return nil, fmt.Errorf("read untracked path %q: %w", path, err)
		}
		changes = append(changes, Change{
			Status:  "A",
			NewPath: &path,
			OldMode: "000000",
			NewMode: mode,
		})
		raw = remaining
	}
	return changes, nil
}

func untrackedMode(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "120000", nil
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("unsupported file type")
	}
	if info.Mode().Perm()&0o111 != 0 {
		return "100755", nil
	}
	return "100644", nil
}

func canonicalizeChanges(changes []Change) (ChangeSet, error) {
	canonical := slices.Clone(changes)
	exact := make(map[changeKey]struct{}, len(canonical))
	newPaths := make(map[string]struct{}, len(canonical))
	oldPaths := make(map[string]oldPathUse, len(canonical))
	for index, change := range canonical {
		if err := ValidateChange(change); err != nil {
			return ChangeSet{}, fmt.Errorf("validate raw change %d: %w", index, err)
		}
		key := makeChangeKey(change)
		if _, exists := exact[key]; exists {
			return ChangeSet{}, fmt.Errorf("raw change %d duplicates another change", index)
		}
		exact[key] = struct{}{}
		if err := registerIdentities(change, newPaths, oldPaths); err != nil {
			return ChangeSet{}, fmt.Errorf("raw change %d conflicts with another change: %w", index, err)
		}
	}
	slices.SortFunc(canonical, compareChange)
	return ChangeSet{SchemaVersion: 1, Changes: canonical}, nil
}
