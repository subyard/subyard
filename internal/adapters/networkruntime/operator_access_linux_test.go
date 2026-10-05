package networkruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestOperatorHostLockAccessPreservesIdentityACLAndAcquisition(t *testing.T) {
	directory := filepath.Join(testkit.TempDir(t), "subyard-network")
	owner, group := os.Geteuid(), os.Getegid()
	if err := ensureHostLockAt(directory, owner, group); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, hostLockName)
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	unrelated, actor := strconv.Itoa(owner+1), strconv.Itoa(owner+2)
	if _, err := testkit.KernelAccessACL(context.Background(), file, unrelated); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := grantHostLockAccessAt(context.Background(), directory, owner, group, actor, testkit.KernelAccessACL); err != nil {
			t.Fatal(err)
		}
	}
	acl, err := testkit.KernelAccessACL(context.Background(), file, "")
	if err != nil || !strings.Contains(acl, "user:"+unrelated+":rw-") || !strings.Contains(acl, "user:"+actor+":rw-") {
		t.Fatalf("named access changed: %v", err)
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
		t.Fatalf("lock identity changed: %v", err)
	}
	if err := checkHostLockAt(directory, owner, group); err != nil {
		t.Fatal(err)
	}
	release, err := acquireHostLockAt(context.Background(), directory, owner, group)
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestOperatorHostLockGrantRefusesDriftBeforeWrite(t *testing.T) {
	for _, scenario := range []string{"replacement", "ancestor", "hardlink", "mode", "ACL drift", "mask", "symlink"} {
		t.Run(scenario, func(t *testing.T) {
			directory := filepath.Join(testkit.TempDir(t), "subyard-network")
			owner, group := os.Geteuid(), os.Getegid()
			if err := ensureHostLockAt(directory, owner, group); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(directory, hostLockName)
			if scenario == "hardlink" {
				if err := os.Link(path, path+".link"); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "mode" {
				if err := os.Chmod(path, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "symlink" {
				if err := os.Rename(path, path+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".original", path); err != nil {
					t.Fatal(err)
				}
			}
			writes, reads := 0, 0
			tools := func(ctx context.Context, file *os.File, uid string) (string, error) {
				if uid != "" {
					writes++
					return testkit.KernelAccessACL(ctx, file, uid)
				}
				observed, err := testkit.KernelAccessACL(ctx, file, "")
				reads++
				if reads == 1 {
					switch scenario {
					case "replacement":
						if err := os.Rename(path, path+".original"); err != nil {
							t.Fatal(err)
						}
						testkit.WriteFile(t, path, []byte("foreign"), 0660)
					case "ancestor":
						if err := os.Chmod(directory, 0777); err != nil {
							t.Fatal(err)
						}
					case "ACL drift":
						if _, err := testkit.KernelAccessACL(ctx, file, strconv.Itoa(owner+3)); err != nil {
							t.Fatal(err)
						}
					case "mask":
						observed = strings.ReplaceAll(observed, "group::rw-", "group::r--")
					}
				}
				return observed, err
			}
			if err := grantHostLockAccessAt(context.Background(), directory, owner, group, strconv.Itoa(owner+2), tools); err == nil || writes != 0 {
				t.Fatalf("unsafe grant: error=%v writes=%d", err, writes)
			}
		})
	}
}

func TestOperatorHostLockGrantUsesExistingBarrier(t *testing.T) {
	directory := filepath.Join(testkit.TempDir(t), "subyard-network")
	owner, group := os.Geteuid(), os.Getegid()
	if err := ensureHostLockAt(directory, owner, group); err != nil {
		t.Fatal(err)
	}
	release, err := acquireHostLockAt(context.Background(), directory, owner, group)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	called := false
	tools := func(context.Context, *os.File, string) (string, error) { called = true; return "", nil }
	err = grantHostLockAccessAt(ctx, directory, owner, group, strconv.Itoa(owner+2), tools)
	if !errors.Is(err, context.DeadlineExceeded) || called {
		t.Fatalf("grant escaped existing barrier: %v tool=%v", err, called)
	}
}
