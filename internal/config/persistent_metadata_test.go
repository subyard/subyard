package config

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/sys/unix"
)

func TestPersistentCASPreservesOwnerAndACL(t *testing.T) {
	testkit.UseHostTempDir(t)
	t.Run("original group", func(t *testing.T) {
		testPersistentCASMetadata(t, -1)
	})
	t.Run("supplementary group", func(t *testing.T) {
		groups, err := os.Getgroups()
		if err != nil {
			t.Skipf("cannot enumerate supplementary groups: %v", err)
		}
		probe := filepath.Join(testkit.TempDir(t), "chown-probe")
		testkit.WriteFile(t, probe, nil, 0600)
		for _, gid := range groups {
			if gid == os.Getgid() {
				continue
			}
			if err := os.Chown(probe, os.Getuid(), gid); err != nil {
				t.Logf("supplementary gid %d is unusable: %v", gid, err)
				continue
			}
			testPersistentCASMetadata(t, gid)
			return
		}
		t.Skip("no usable supplementary group distinct from the primary gid; no candidate passed the scratch-file chown probe")
	})
}

func testPersistentCASMetadata(t *testing.T, gid int) {
	t.Helper()
	for _, writer := range []string{"cleanup", "shared CAS"} {
		for _, withACL := range []bool{false, true} {
			t.Run(writer+"/acl="+strconv.FormatBool(withACL), func(t *testing.T) {
				root, home, catalog, path := deadAssignmentFixture(t)
				testkit.WriteFile(t, path, []byte("# keep\nAGENT_sample_CONFIG=\"$SUBYARD_CONFIG_DIR/../private/agents/sample/settings.json\"\n"), 0640)
				if gid >= 0 {
					if err := os.Chown(path, os.Getuid(), gid); err != nil {
						t.Fatal(err)
					}
				}
				if withACL {
					file, err := os.Open(path)
					if err != nil {
						t.Fatal(err)
					}
					// The current uid is mapped even inside a restricted user namespace.
					_, aclErr := testkit.KernelAccessACL(context.Background(), file, strconv.Itoa(os.Getuid()))
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
				if gid >= 0 && before.Identity.GID != uint32(gid) {
					t.Fatalf("fixture gid=%d, want supplementary gid=%d", before.Identity.GID, gid)
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
