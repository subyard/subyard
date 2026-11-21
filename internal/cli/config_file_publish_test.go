package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestWriteExportedConfigFileKeepsFinalSymlink(t *testing.T) {
	root := testkit.TempDir(t)
	target := filepath.Join(root, "dotfiles", "config.env")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, target, []byte("OLD=1\n"), 0o644)
	link := filepath.Join(root, "checkout", "config.env")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := writeExportedConfigFile(link, []byte("NEW=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("final symlink replaced: %v %v", info, err)
	}
	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("target metadata: %v %v", info, err)
	}
	if content, _ := os.ReadFile(target); string(content) != "NEW=1\n" {
		t.Fatalf("target content %q", content)
	}

	dangling := filepath.Join(root, "checkout", "missing.env")
	if err := os.Symlink(filepath.Join(root, "absent", "missing.env"), dangling); err != nil {
		t.Fatal(err)
	}
	err = writeExportedConfigFile(dangling, []byte("X=1\n"), 0o600)
	if err == nil || err.Error() != "open "+dangling+": no such file or directory" {
		t.Fatalf("dangling symlink into a missing directory: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "absent")); !os.IsNotExist(err) {
		t.Fatalf("directory behind dangling symlink: %v", err)
	}

	// The kernel fails "missing/.." with ENOENT; a lexical clean would name config.env.
	sibling := filepath.Join(root, "dotfiles", "sibling.env")
	if err := os.Symlink("missing/../config.env", sibling); err != nil {
		t.Fatal(err)
	}
	err = writeExportedConfigFile(sibling, []byte("WRONG=1\n"), 0o600)
	if err == nil || err.Error() != "open "+sibling+": no such file or directory" {
		t.Fatalf("dot-dot through a missing directory: %v", err)
	}
	if content, _ := os.ReadFile(target); string(content) != "NEW=1\n" {
		t.Fatalf("unrelated file overwritten: %q", content)
	}

	if os.Geteuid() != 0 {
		if err := os.Chmod(target, 0o400); err != nil {
			t.Fatal(err)
		}
		err = writeExportedConfigFile(link, []byte("RO=1\n"), 0o600)
		if err == nil || err.Error() != "open "+link+": permission denied" {
			t.Fatalf("read-only target: %v", err)
		}
		if content, _ := os.ReadFile(target); string(content) != "NEW=1\n" {
			t.Fatalf("read-only target replaced: %q", content)
		}
	}

	nested := filepath.Join(root, "checkout", "yards", "named", "config.env")
	if err := writeExportedConfigFile(nested, []byte("Y=1\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(nested); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("new file metadata: %v %v", info, err)
	}
}

func TestHardenConfigCandidateReplacesHardLinkedFile(t *testing.T) {
	root := testkit.TempDir(t)
	path := filepath.Join(root, "config.env")
	testkit.WriteFile(t, path, []byte("A=1\n"), 0o664)
	outside := filepath.Join(testkit.TempDir(t), "linked")
	if err := os.Link(path, outside); err != nil {
		t.Fatal(err)
	}
	if err := hardenConfigCandidate(root); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o644 {
		t.Fatalf("candidate metadata: %v %v", info, err)
	}
	other, err := os.Stat(outside)
	if err != nil || os.SameFile(info, other) {
		t.Fatalf("hard link not broken: %v", err)
	}
	if content, _ := os.ReadFile(path); string(content) != "A=1\n" {
		t.Fatalf("candidate content %q", content)
	}
}
