package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/sys/unix"
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

func TestCleanupJournalDirectoryFinishesNarrowAndRefusesBroad(t *testing.T) {
	root := testkit.TempDir(t)
	parent := filepath.Join(root, "cleanup", "yard")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "journal.json")
	journal := &hardCleanupJournal{Schema: 1, Token: "0123456789abcdef0123456789abcdef", Phase: "recreate"}
	if err := saveHardCleanup(path, journal); err != nil {
		t.Fatalf("narrow owned journal directory: %v", err)
	}
	if info, err := os.Stat(parent); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("narrow journal directory not finished: %v %v", info, err)
	}
	if err := os.Chmod(parent, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := saveHardCleanup(path, journal); err == nil || err.Error() != "unsafe hard cleanup recovery directory" {
		t.Fatalf("broad journal directory: %v", err)
	}
	if info, err := os.Stat(parent); err != nil || info.Mode().Perm() != 0o750 {
		t.Fatalf("broad journal directory changed: %v %v", info, err)
	}
}

func TestCleanupJournalFinishesOuterDirectoryWithoutSearchPermission(t *testing.T) {
	root := testkit.TempDir(t)
	outer := filepath.Join(root, "cleanup")
	parent := filepath.Join(outer, "yard")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(outer, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(outer, 0o700) })
	path := filepath.Join(parent, "journal.json")
	journal := &hardCleanupJournal{Schema: 1, Token: "0123456789abcdef0123456789abcdef", Phase: "recreate"}
	if err := saveHardCleanup(path, journal); err != nil {
		t.Fatalf("outer journal directory without search permission: %v", err)
	}
	for _, directory := range []string{outer, parent} {
		if info, err := os.Stat(directory); err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("journal directory %s not finished: %v %v", directory, info, err)
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestWriteExportedConfigFileNeverOpensSpecialFiles(t *testing.T) {
	root := testkit.TempDir(t)
	fifo := filepath.Join(root, "config.env")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := unix.Open(fifo, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(reader)
	if err := writeExportedConfigFile(fifo, []byte("X=1\n"), 0o600); err == nil {
		t.Fatal("published over a FIFO")
	}
	// A writer that opened and closed the FIFO would leave POLLHUP on the reader.
	events := []unix.PollFd{{Fd: int32(reader), Events: unix.POLLIN}}
	if _, err := unix.Poll(events, 0); err != nil {
		t.Fatal(err)
	}
	if events[0].Revents&unix.POLLHUP != 0 {
		t.Fatal("export opened the FIFO before refusing it")
	}
}

func TestWriteExportedConfigFileReportsDirectoryTextErrors(t *testing.T) {
	root := testkit.TempDir(t)
	if err := os.Mkdir(filepath.Join(root, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "file"), []byte("keep"), 0o600)
	for text, want := range map[string]string{"dir/.": "is a directory", "file/.": "not a directory"} {
		link := filepath.Join(root, "link-"+filepath.Dir(text))
		if err := os.Symlink(text, link); err != nil {
			t.Fatal(err)
		}
		err := writeExportedConfigFile(link, []byte("X=1\n"), 0o600)
		if err == nil || err.Error() != "open "+link+": "+want {
			t.Fatalf("link to %s: %v", text, err)
		}
	}
	if content, _ := os.ReadFile(filepath.Join(root, "file")); string(content) != "keep" {
		t.Fatalf("file changed: %q", content)
	}
}
