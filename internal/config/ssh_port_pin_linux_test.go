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
