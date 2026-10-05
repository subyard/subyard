package incusclient

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func operatorSocketFixture(t *testing.T) (string, string) {
	t.Helper()
	root := testkit.TempDir(t)
	path := filepath.Join(root, "unix.socket")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if err := os.Chmod(path, 0660); err != nil {
		t.Fatal(err)
	}
	return root, path
}

var kernelSocketACL = testkit.KernelAccessACL

func TestOperatorSocketAccessPreservesNativeIdentityAndACL(t *testing.T) {
	root, path := operatorSocketFixture(t)
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	uid := strconv.Itoa(os.Getuid() + 1)
	if err := grantSocketAccess(context.Background(), root, "unix.socket", uint32(os.Getgid()), uid, kernelSocketACL); err != nil {
		t.Fatal(err)
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
		t.Fatalf("native socket identity or mode changed: %v", err)
	}
	if err := grantSocketAccess(context.Background(), root, "unix.socket", uint32(os.Getgid()), uid, kernelSocketACL); err != nil {
		t.Fatalf("idempotent grant: %v", err)
	}
}

func TestOperatorSocketAccessRejectsReplacementBeforeWriting(t *testing.T) {
	root, path := operatorSocketFixture(t)
	writes := 0
	tools := func(ctx context.Context, file *os.File, uid string) (string, error) {
		if uid != "" {
			writes++
		}
		output, err := kernelSocketACL(ctx, file, uid)
		if uid == "" {
			if err := os.Rename(path, path+".retained"); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, path, []byte("foreign"), 0600)
		}
		return output, err
	}
	err := grantSocketAccess(context.Background(), root, "unix.socket", uint32(os.Getgid()), "12345", tools)
	if err == nil || writes != 0 {
		t.Fatalf("replaced socket accepted or written: %v writes=%d", err, writes)
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "foreign" {
		t.Fatalf("replacement changed: %v", err)
	}
}

func TestOperatorSocketAccessRejectsUnsafePathAndACL(t *testing.T) {
	for _, scenario := range []string{"regular file", "symlink", "mode", "group", "ancestor", "mask", "unrelated ACL drift"} {
		t.Run(scenario, func(t *testing.T) {
			root, path := operatorSocketFixture(t)
			gid := uint32(os.Getgid())
			tools := kernelSocketACL
			switch scenario {
			case "regular file", "symlink":
				if err := os.Rename(path, path+".retained"); err != nil {
					t.Fatal(err)
				}
				if scenario == "regular file" {
					testkit.WriteFile(t, path, []byte("foreign"), 0660)
				} else if err := os.Symlink(path+".retained", path); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.Chmod(path, 0666); err != nil {
					t.Fatal(err)
				}
			case "group":
				gid++
			case "ancestor":
				if err := os.Chmod(root, 0770); err != nil {
					t.Fatal(err)
				}
			case "mask":
				tools = func(context.Context, *os.File, string) (string, error) {
					return "user::rw-\ngroup::r--\nmask::r--\nother::---", nil
				}
			case "unrelated ACL drift":
				calls := 0
				tools = func(ctx context.Context, file *os.File, uid string) (string, error) {
					calls++
					output, err := kernelSocketACL(ctx, file, uid)
					if calls == 3 {
						output += "\nuser:23456:r--"
					}
					return output, err
				}
			}
			if err := grantSocketAccess(context.Background(), root, "unix.socket", gid, "12345", tools); err == nil {
				t.Fatal("unsafe socket access accepted")
			}
		})
	}
}

func TestDefaultLocalEndpointRejectsOverrides(t *testing.T) {
	t.Setenv("INCUS_SOCKET", "")
	t.Setenv("INCUS_DIR", "")
	if !New("").DefaultLocalEndpoint() || New("/custom.socket").DefaultLocalEndpoint() {
		t.Fatal("default endpoint classification differs")
	}
	for _, name := range []string{"INCUS_SOCKET", "INCUS_DIR"} {
		t.Setenv(name, "/custom")
		if New("").DefaultLocalEndpoint() {
			t.Fatal("custom endpoint accepted")
		}
		t.Setenv(name, "")
	}
}

func TestOperatorSocketAccessRejectsNativeACLDriftBeforeGrant(t *testing.T) {
	root, _ := operatorSocketFixture(t)
	writes := 0
	tools := func(ctx context.Context, file *os.File, uid string) (string, error) {
		if uid != "" {
			writes++
		}
		output, err := kernelSocketACL(ctx, file, uid)
		if uid == "" {
			_, err = kernelSocketACL(ctx, file, "23456")
		}
		return output, err
	}
	if err := grantSocketAccess(context.Background(), root, "unix.socket", uint32(os.Getgid()), "12345", tools); err == nil || writes != 0 {
		t.Fatalf("native ACL changed before write: err=%v writes=%d", err, writes)
	}
}
