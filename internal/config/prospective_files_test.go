package config

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestProspectiveFilesRejectsInvalidSnapshotsAndScope(t *testing.T) {
	root := testkit.TempDir(t)
	path := filepath.Join(root, "config.env")
	valid := PersistentFileSnapshot{Exists: true, Content: []byte("DEV_SUDO=1\n")}
	testkit.WriteFile(t, path, valid.Content, 0o600)
	existing, err := ReadPersistentFileSnapshot(root, path)
	if err != nil {
		t.Fatal(err)
	}
	for name, snapshot := range map[string]PersistentFileSnapshot{
		"virtual":        valid,
		"existing":       existing,
		"unsafe-write":   {Exists: true, Identity: PersistentFileIdentity{Mode: syscall.S_IFREG | 0o666, UID: uint32(os.Geteuid()), Links: 1}},
		"symlink":        {Exists: true, Identity: PersistentFileIdentity{Mode: syscall.S_IFLNK | 0o600, UID: uint32(os.Geteuid()), Links: 1}},
		"foreign-owner":  {Exists: true, Identity: PersistentFileIdentity{Mode: syscall.S_IFREG | 0o600, UID: uint32(os.Geteuid()) + 1, Links: 1}},
		"hardlink":       {Exists: true, Identity: PersistentFileIdentity{Mode: syscall.S_IFREG | 0o600, UID: uint32(os.Geteuid()), Links: 2}},
		"absent-content": {Content: valid.Content},
		"overlong":       {Exists: true, Content: make([]byte, (32<<20)+1)},
	} {
		t.Run(name, func(t *testing.T) {
			files := &ProspectiveFiles{Root: root, ReadSnapshot: func(string) (PersistentFileSnapshot, error) { return snapshot, nil }}
			_, err := files.readFile(path)
			wantValid := name == "virtual" || name == "existing"
			if (err == nil) != wantValid {
				t.Fatalf("snapshot validity = %v, err=%v", wantValid, err)
			}
			files.ReadPreparedInput = func(string) (PersistentFileSnapshot, bool, error) { return snapshot, true, nil }
			_, err = files.readFile(path)
			if (err == nil) != wantValid {
				t.Fatalf("prepared snapshot validity = %v, err=%v", wantValid, err)
			}
		})
	}
	calls := 0
	files := &ProspectiveFiles{Root: root, ReadSnapshot: func(string) (PersistentFileSnapshot, error) { calls++; return valid, nil }}
	if _, err := files.readFile(root + "/../outside"); err == nil || calls != 0 {
		t.Fatal("escaping prospective path reached reader")
	}
	files.Root = "/"
	if _, err := files.readFile(path); err == nil {
		t.Fatal("filesystem root accepted")
	}
}

func TestProspectiveFilesPreservesNormalAssetPolicies(t *testing.T) {
	root, outside := testkit.TempDir(t), testkit.TempDir(t)
	path := filepath.Join(outside, "rules")
	testkit.WriteFile(t, path, []byte("external\n"), 0o644)
	link := filepath.Join(outside, "instructions")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	files := &ProspectiveFiles{Root: root, ReadSnapshot: func(string) (PersistentFileSnapshot, error) {
		t.Fatal("external path reached prospective reader")
		return PersistentFileSnapshot{}, nil
	}}
	if _, err := files.ReadAsset(MaterializedAsset{Source: link}); err == nil {
		t.Fatal("ordinary non-symlink policy lost")
	}
	if data, err := files.ReadAsset(MaterializedAsset{Source: link, FollowSymlinks: true}); err != nil || string(data) != "external\n" {
		t.Fatalf("external instruction policy: %q %v", data, err)
	}
	var ordinary *ProspectiveFiles
	if data, err := ordinary.ReadAsset(MaterializedAsset{Source: path}); err != nil || string(data) != "external\n" {
		t.Fatalf("nil prospective reader: %q %v", data, err)
	}
}

func TestProspectiveFilesPreservesRetainedReadableModes(t *testing.T) {
	for _, mode := range []os.FileMode{0o600, 0o640, 0o644} {
		t.Run(mode.String(), func(t *testing.T) {
			root := testkit.TempDir(t)
			path := filepath.Join(root, "config.env")
			content := []byte("DEV_SUDO=1\n")
			testkit.WriteFile(t, path, content, mode)
			files := &ProspectiveFiles{Root: root, ReadSnapshot: func(candidate string) (PersistentFileSnapshot, error) {
				return ReadPersistentFileSnapshot(root, candidate)
			}}
			data, err := files.readFile(path)
			if err != nil || string(data) != string(content) {
				t.Fatalf("retained readable file rejected: %v", err)
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != mode {
				t.Fatal("retained mode changed")
			}
		})
	}
}
