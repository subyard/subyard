package uninstall

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// StartupEdit carries only installer-generated strings and native metadata.
// Unrelated user startup content is neither part of the plan nor persisted.
type StartupEdit struct {
	Before    Tree
	Blocks    []string
	Pending   *Tree
	Temporary string
}

func PrepareStartup(path, runtimeRoot, binDir string, keepBinPath bool) (StartupEdit, error) {
	edit := StartupEdit{}
	tree, err := Capture(path)
	if err != nil {
		return edit, err
	}
	edit.Before = tree
	if len(tree.Entries) == 0 {
		return edit, nil
	}
	entry := tree.Entries[0]
	if len(tree.Entries) != 1 || entry.Mode&syscall.S_IFMT != syscall.S_IFREG || entry.UID != uint32(os.Getuid()) || entry.Mode&0022 != 0 {
		return edit, errors.New("shell startup file must be an operator-owned non-writable regular file")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return edit, err
	}
	if len(content) > 1<<20 {
		return edit, errors.New("shell startup file exceeds the bounded edit limit")
	}
	for _, kind := range []string{"interactive", "login"} {
		marker := "# Subyard CLI " + kind + " PATH\n"
		block := marker + fmt.Sprintf("export PATH=\"%s:$PATH\"\n", binDir)
		if bytes.Contains(content, []byte(block)) {
			if keepBinPath {
				edit.Blocks = append(edit.Blocks, marker)
			} else {
				edit.Blocks = append(edit.Blocks, block)
			}
		}
	}
	for _, shell := range []string{"bash", "zsh"} {
		completion := filepath.Join(runtimeRoot, "current", "completions", "yard."+shell)
		edit.Blocks = appendIfPresent(edit.Blocks, content, fmt.Sprintf("# Subyard CLI completion\n[ -f \"%s\" ] && source \"%s\"\n", completion, completion))
	}
	completion := filepath.Join(runtimeRoot, "current", "completions", "yard.bash")
	edit.Blocks = appendIfPresent(edit.Blocks, content, fmt.Sprintf("# Subyard CLI login completion\nif [ -n \"${BASH_VERSION:-}\" ]; then\n  case $- in\n    *i*) [ -f \"%s\" ] && . \"%s\" ;;\n  esac\nfi\n", completion, completion))
	for _, block := range edit.Blocks {
		if bytes.Count(content, []byte(block)) != 1 {
			return edit, errors.New("duplicate managed shell startup block")
		}
	}
	if err := tree.Check(); err != nil {
		return edit, err
	}
	if len(edit.Blocks) > 0 {
		nonce := make([]byte, 12)
		if _, err := rand.Read(nonce); err != nil {
			return edit, err
		}
		edit.Temporary = filepath.Join(filepath.Dir(path), ".subyard-uninstall-"+hex.EncodeToString(nonce))
		planned, err := CaptureRoot(edit.Temporary)
		if err != nil {
			return edit, err
		}
		if len(planned.Entries) > 0 {
			return edit, errors.New("planned shell sidecar already exists")
		}
	}
	return edit, nil
}

func appendIfPresent(blocks []string, content []byte, block string) []string {
	if bytes.Contains(content, []byte(block)) {
		return append(blocks, block)
	}
	return blocks
}

// The physical guard prepares and commits the edit against the same recorded
// inode, including a fresh retry after its atomic rename.
func (edit StartupEdit) PrepareApply() (StartupEdit, error) {
	if edit.Pending != nil || len(edit.Blocks) == 0 || len(edit.Before.Entries) == 0 {
		return edit, nil
	}
	err := nativeArtifact("prepare-startup", edit, &edit)
	return edit, err
}
func (edit StartupEdit) CommitApply() (Tree, error) {
	if edit.Pending == nil {
		return edit.Before, nil
	}
	var updated Tree
	err := nativeArtifact("commit-startup", edit, &updated)
	return updated, err
}

// KeepBinPath preserves a PATH entry when any neighboring executable or
// directory remains. Only exact captured yard/sy launchers are removable.
func KeepBinPath(directory string) (bool, error) {
	if directory == "" {
		return true, nil
	}
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry.Name() != "yard" && entry.Name() != "sy" {
			return true, nil
		}
	}
	return false, nil
}
