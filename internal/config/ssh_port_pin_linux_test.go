package config

import (
	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"testing"
)

func TestSSHPortPinNativeComponent(t *testing.T) {
	root := testkit.TempDir(t)
	if err := os.Mkdir(filepath.Join(root, ".owner"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "repository"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(filepath.Join(root, "repository/config"), os.DirFS("../../config")); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"owner.marker": SSHPortPinMarker, ".sync/settings/config.env": "SSH_PORT=2240\n", ".sync/settings/yards/named/config.env": "SSH_PORT=2241\n", "yards/named/config.env": "# retained\nDEV_SUDO=0\n"} {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, p, []byte(content), 0o600)
	}
	f, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	plan, err := PrepareSSHPortPin(f, "named")
	if err != nil {
		t.Fatal(err)
	}
	if status, err := ApplySSHPortPin(f, plan); err != nil || status != "desired" {
		t.Fatalf("status %s: %v", status, err)
	}
	before, _ := ReadPersistentSnapshotAt(f, "yards/named/config.env")
	if status, err := ApplySSHPortPin(f, plan); err != nil || status != "desired" {
		t.Fatal(status, err)
	}
	after, _ := ReadPersistentSnapshotAt(f, "yards/named/config.env")
	if before.Identity != after.Identity {
		t.Fatal("desired state was replayed")
	}
}

func TestPinnedRootCASRejectsSourceDriftAndRetainsCustody(t *testing.T) {
	root := testkit.TempDir(t)
	target := filepath.Join(root, "config.env")
	source := filepath.Join(root, "source.env")
	testkit.WriteFile(t, target, []byte("OLD=1\n"), 0o640)
	testkit.WriteFile(t, source, []byte("SSH_PORT=2222\n"), 0o600)
	f, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	before, err := ReadPersistentSnapshotAt(f, "config.env")
	if err != nil {
		t.Fatal(err)
	}
	input, err := ReadPersistentSnapshotAt(f, "source.env")
	if err != nil {
		t.Fatal(err)
	}
	preconditions := []PersistentPrecondition{{Relative: "source.env", Binding: BindPersistentSnapshot(input)}}
	testkit.WriteFile(t, source, []byte("SSH_PORT=3333\n"), 0o600)
	if err = CompareAndSwapLocalSettingsAt(f, "config.env", BindPersistentSnapshot(before), []byte("OLD=2\n"), preconditions, nil, nil); err != ErrPersistentTargetStale {
		t.Fatal(err)
	}
	after, err := ReadPersistentSnapshotAt(f, "config.env")
	if err != nil || BindPersistentSnapshot(after) != BindPersistentSnapshot(before) {
		t.Fatal("stale source changed target", err)
	}
	other, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err = unix.Flock(int(other.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != unix.EWOULDBLOCK {
		t.Fatal("borrowed CAS released caller lock", err)
	}
}
