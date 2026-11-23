package config

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/sys/unix"
)

func TestPersistentCASPreservesOwnerAndACL(t *testing.T) {
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	gid := -1
	for _, group := range groups {
		if group != os.Getgid() {
			gid = group
			break
		}
	}
	if gid < 0 && os.Geteuid() == 0 && os.Getenv("SUBYARD_METADATA_GROUP_TEST") == "" {
		// Isolate supplementary-group setup from the Go test process and its peers.
		child := exec.Command(os.Args[0], "-test.run=^TestPersistentCASPreservesOwnerAndACL$", "-test.v")
		child.Env = append(os.Environ(), "SUBYARD_METADATA_GROUP_TEST=1")
		child.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 0, Gid: uint32(os.Getgid()), Groups: []uint32{uint32(os.Getgid() + 1)}}}
		output, err := child.CombinedOutput()
		t.Log(string(output))
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	if gid < 0 {
		t.Fatal("metadata fixture requires a supplementary group distinct from the primary gid")
	}
	for _, writer := range []string{"cleanup", "shared CAS"} {
		for _, withACL := range []bool{false, true} {
			t.Run(writer+"/acl="+strconv.FormatBool(withACL), func(t *testing.T) {
				root, home, catalog, path := deadAssignmentFixture(t)
				testkit.WriteFile(t, path, []byte("# keep\nAGENT_sample_CONFIG=\"$SUBYARD_CONFIG_DIR/../private/agents/sample/settings.json\"\n"), 0640)
				if err := os.Chown(path, os.Getuid(), gid); err != nil {
					t.Fatal(err)
				}
				if withACL {
					file, err := os.Open(path)
					if err != nil {
						t.Fatal(err)
					}
					_, aclErr := testkit.KernelAccessACL(context.Background(), file, strconv.Itoa(os.Getuid()+77))
					file.Close()
					if errors.Is(aclErr, unix.ENOTSUP) {
						t.Skip("filesystem does not support POSIX access ACLs")
					}
					if aclErr != nil {
						t.Fatal(aclErr)
					}
					// The ACL helper grants rw; keep the source within settings' writable mask.
					if err := os.Chmod(path, 0640); err != nil {
						t.Fatal(err)
					}
				}
				before, err := ReadPersistentFileSnapshot(home, path)
				if err != nil {
					t.Fatal(err)
				}
				acl := persistentTestACL(t, path)
				if withACL && len(acl) == 0 {
					t.Fatal("ACL fixture did not create an access ACL")
				}
				if writer == "cleanup" {
					plan, err := catalog.PlanDeadAssignments(home, filepath.Join(root, "config"), nil)
					if err != nil || len(plan.Removed) != 1 {
						t.Fatalf("plan=%+v err=%v", plan, err)
					}
					if err := plan.Apply(home); err != nil {
						t.Fatal(err)
					}
				} else if err := CompareAndSwapPersistentFile(home, path, before, []byte("# keep\n")); err != nil {
					t.Fatal(err)
				}
				after, err := ReadPersistentFileSnapshot(home, path)
				if err != nil {
					t.Fatal(err)
				}
				if string(after.Content) != "# keep\n" || after.Identity.Inode == before.Identity.Inode || after.Identity.UID != before.Identity.UID || after.Identity.GID != before.Identity.GID || after.Identity.Mode != before.Identity.Mode {
					t.Fatalf("atomic replacement changed metadata: before=%+v after=%+v", before.Identity, after.Identity)
				}
				if !bytes.Equal(acl, persistentTestACL(t, path)) {
					t.Fatal("atomic replacement changed the access ACL")
				}
			})
		}
	}
}

func persistentTestACL(t *testing.T, path string) []byte {
	t.Helper()
	data := make([]byte, 64<<10)
	n, err := unix.Getxattr(path, "system.posix_acl_access", data)
	if errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return data[:n]
}
