package releaseruntime

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/sys/unix"
)

func TestApprovedDraftPublishesSameDirectoryWithoutOverwrite(t *testing.T) {
	parent := testkit.TempDir(t)
	root := filepath.Join(parent, "runtime")
	draft := filepath.Join(parent, "draft")
	for _, path := range []string{filepath.Join(root, "releases"), filepath.Join(draft, "releases", "release-b")} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	candidate := publishedCandidate{release: "release-b", root: filepath.Join(draft, "releases", "release-b")}
	testkit.WriteFile(t, filepath.Join(candidate.root, "marker"), []byte("approved"), 0600)
	before, _ := os.Lstat(candidate.root)
	rootInfo, _ := os.Lstat(root)
	releasesInfo, _ := os.Lstat(filepath.Join(root, "releases"))
	target := filepath.Join(root, "releases", "release-b")
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("draft already published: %v", err)
	}
	if err := publishApprovedDraft(candidate, root, rootInfo, releasesInfo); err != nil {
		t.Fatal(err)
	}
	after, err := os.Lstat(target)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("publication did not retain verified inode: %v", err)
	}
	if err := os.Mkdir(candidate.root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := publishApprovedDraft(candidate, root, rootInfo, releasesInfo); !errors.Is(err, unix.EEXIST) {
		t.Fatalf("publication overwrote existing target: %v", err)
	}
}

func TestApprovedDraftRejectsReplacedRuntimeBeforePublication(t *testing.T) {
	parent := testkit.TempDir(t)
	root := filepath.Join(parent, "runtime")
	candidate := publishedCandidate{release: "release-b", root: filepath.Join(parent, "draft", "releases", "release-b")}
	for _, path := range []string{filepath.Join(root, "releases"), candidate.root} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	rootInfo, _ := os.Lstat(root)
	releasesInfo, _ := os.Lstat(filepath.Join(root, "releases"))
	if err := os.Rename(root, root+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "releases"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := publishApprovedDraft(candidate, root, rootInfo, releasesInfo); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("replaced runtime accepted: %v", err)
	}
	if _, err := os.Lstat(candidate.root); err != nil {
		t.Fatalf("rejected draft changed: %v", err)
	}
}
