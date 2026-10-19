package releaseowner

import (
	"context"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/sys/unix"
)

type interruptOwnerFacts struct{ noEffects }

func TestNativeOwnerMonitorRetriesInterruptUntilActualLoss(t *testing.T) {
	root, err := os.Open(testkit.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	calls := 0
	watchNativeOwner(root, done, cancel, func(fds []unix.PollFd, timeout int) (int, error) {
		calls++
		if len(fds) != 1 || fds[0].Fd != int32(root.Fd()) || fds[0].Events != unix.POLLRDHUP|unix.POLLHUP|unix.POLLERR || timeout != 100 {
			t.Fatal("owner monitor changed fixed socket contract")
		}
		if ctx.Err() != nil {
			t.Fatal("interrupt or non-loss event canceled original owner")
		}
		switch calls {
		case 1:
			return 0, unix.EINTR
		case 2:
			return 0, nil
		case 3:
			fds[0].Revents = unix.POLLRDHUP
			return 1, nil
		default:
			t.Fatal("owner loss did not stop monitor")
			return 0, nil
		}
	})
	if calls != 3 || ctx.Err() != context.Canceled {
		t.Fatal("actual socket loss did not cancel owner")
	}
}

func (interruptOwnerFacts) CaptureInventory(ctx context.Context) ([]string, []string, error) {
	return []string{}, []string{}, ctx.Err()
}

func TestNativeOwnerInterruptChild(t *testing.T) {
	if os.Getenv("SUBYARD_NATIVE_INTERRUPT_CHILD") != "1" {
		return
	}
	if err := Serve(context.Background(), os.Stdin, os.Stdout, func(context.Context, Context, *os.File) (Components, error) { return interruptOwnerFacts{}, nil }); err != nil {
		os.Exit(3)
	}
	os.Exit(0)
}

func TestNativeOwnerSignalsDoNotMeanOwnerEOF(t *testing.T) {
	root := testkit.TempDir(t)
	fd, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer fd.Close()
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	parent, child := os.NewFile(uintptr(pair[0]), "interrupted native owner"), os.NewFile(uintptr(pair[1]), "interrupt child")
	defer parent.Close()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^TestNativeOwnerInterruptChild$")
	command.Env = append(os.Environ(), "SUBYARD_NATIVE_INTERRUPT_CHILD=1")
	command.Stdin = child
	command.Stdout = child
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		child.Close()
		t.Fatal(err)
	}
	child.Close()
	defer func() { command.Process.Kill(); command.Wait() }()
	peer := nativeTestPeer{t: t, socket: parent}
	id, err := IdentityOf(fd)
	if err != nil {
		t.Fatal(err)
	}
	hash := digest(nil)
	c := Context{Session: hash, Artifact: hash, DataHome: root, ConfigHome: root, RuntimeRoot: root, RepositoryRoot: root, Yard: "default", Target: "new", Root: id, Repository: id}
	peer.request(Request{Op: "initialize", Context: &c}, fd, fd)
	for index := 0; index < 128; index++ {
		if err := unix.Kill(command.Process.Pid, unix.SIGURG); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
		peer.request(Request{Op: "capture-inventory", Session: hash})
	}
	parent.Close()
	if err := command.Wait(); err != nil {
		t.Fatal("actual peer EOF did not close readonly helper", err)
	}
}
