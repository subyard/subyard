package uninstall

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestOwnerDonorCloseKeepsBorrowedChildBarrierUntilChildDeath(t *testing.T) {
	root := testkit.TempDir(t)
	home := filepath.Join(root, "data")
	_, release, err := OwnerLock(context.Background(), home, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	borrowed, err := BorrowOwnerLock(home)
	if err != nil || borrowed == nil {
		t.Fatal("borrow original OFD", err)
	}
	defer borrowed.Close()
	child := exec.Command(os.Args[0], "-test.run=^TestInheritedOwnerDescriptorHelper$")
	child.Env = append(os.Environ(), "SUBYARD_TEST_OWNER_ROLE=child", "SUBYARD_TEST_OWNER_ROOT="+root, "SUBYARD_OWNER_LOCK_FD=3")
	child.ExtraFiles = []*os.File{borrowed}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { child.Process.Kill(); child.Wait() }()
	end := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(root, "ready")); err == nil {
			break
		}
		if time.Now().After(end) {
			t.Fatal("borrowed child did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	borrowed.Close()
	release()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	_, unexpectedRelease, err := OwnerLock(ctx, home, nil)
	cancel()
	if err == nil {
		unexpectedRelease()
		t.Fatal("normal donor Close unlocked the child's original open file description")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, finalRelease, err := OwnerLock(ctx, home, nil)
	if err != nil {
		t.Fatal("last borrower death did not release the lock", err)
	}
	finalRelease()
}

func TestQueuedOwnerLockRejectsReplacedInodeAfterAcquisition(t *testing.T) {
	home := filepath.Join(testkit.TempDir(t), "data")
	_, release, err := OwnerLock(context.Background(), home, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	path := OwnerLockPath(home)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	count := func() int {
		names, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, name := range names {
			if actual, err := os.Stat(filepath.Join("/proc/self/fd", name.Name())); err == nil && os.SameFile(info, actual) {
				n++
			}
		}
		return n
	}
	before := count()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, unlock, err := OwnerLock(ctx, home, nil)
		if err == nil {
			unlock()
		}
		result <- err
	}()
	end := time.Now().Add(time.Second)
	for count() == before {
		if time.Now().After(end) {
			t.Fatal("waiter never opened the original coordination inode")
		}
		time.Sleep(time.Millisecond)
	}
	replaced := path + ".replaced-" + strconv.Itoa(os.Getpid())
	if err := os.Rename(path, replaced); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(replaced); os.Remove(path) })
	testkit.WriteFile(t, path, nil, 0o600)
	release()
	if err := <-result; err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiter did not reject replaced coordination inode: %v", err)
	}
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal("refused waiter retained the replacement inode", err)
	}
}

func TestNativeOwnerRegistrationForwardsAndClosesOnlyItsOwnOFD(t *testing.T) {
	home := filepath.Join(testkit.TempDir(t), "data")
	_, donorClose, err := OwnerLock(context.Background(), home, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer donorClose()
	held, err := BorrowOwnerLock(home)
	if err != nil || held == nil {
		t.Fatal(err)
	}
	defer held.Close()
	if release, err := RegisterBorrowedOwnerLock(home+"-foreign", held); err == nil {
		release()
		t.Fatal("registered a native barrier for another installation")
	}
	release, err := RegisterBorrowedOwnerLock(home, held)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	donorClose()
	held.Close()
	forwarded, err := BorrowOwnerLock(home)
	if err != nil || forwarded == nil {
		t.Fatal("registered native custody cannot forward the actual OFD", err)
	}
	defer forwarded.Close()
	release()
	release()
	if absent, err := BorrowOwnerLock(home); err != nil || absent != nil {
		t.Fatalf("registration restored a closed donor: %v %v", absent, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	_, unexpected, err := OwnerLock(ctx, home, nil)
	cancel()
	if err == nil {
		unexpected()
		t.Fatal("native registration Close unlocked its forwarded borrower")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	forwarded.Close()
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, finalClose, err := OwnerLock(ctx, home, nil)
	if err != nil {
		t.Fatal("final forwarded borrower did not release barrier", err)
	}
	finalClose()
}
