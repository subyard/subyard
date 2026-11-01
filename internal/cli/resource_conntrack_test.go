package cli

import (
	"context"
	"errors"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/hostruntime"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/shellquote"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
)

type conntrackCustodyObserver struct {
	startupProcessObserver
	uncertain bool
	terminals []uint64
}

func (o *conntrackCustodyObserver) Uncertain() { o.uncertain = true }
func (o *conntrackCustodyObserver) CommandTerminal(n uint64) error {
	o.terminals = append(o.terminals, n)
	return o.startupProcessObserver.CommandTerminal(n)
}

func resourceConntrackFixture(t *testing.T, root, body string) (hostruntime.ConntrackCleaner, *conntrackCustodyObserver) {
	t.Helper()
	source, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "conntrack")
	testkit.WriteFile(t, binary, []byte("#!/bin/sh\nset -eu\n"+body), 0700)
	observer := &conntrackCustodyObserver{startupProcessObserver: startupProcessObserver{root: root}}
	cleaner := hostruntime.ConntrackCleaner{Lookup: func(string) (string, error) { return binary, nil }, Run: func(ctx context.Context, binary string, args ...string) ([]byte, error) {
		return runResourceConntrack(ctx, source, binary, false, args...)
	}}
	cleaner.Delete = func(ctx context.Context, binary string, args []string, settle func(error) error) error {
		_, err := runResourceConntrackPostcondition(ctx, source, binary, false, settle, args...)
		return err
	}
	return cleaner, observer
}

func TestResourceConntrackNativeCustodyPreservesExactCleanerOutcomes(t *testing.T) {
	for _, mode := range []string{"empty", "list-exit1", "delete-race", "delete-error", "remaining-then-empty"} {
		t.Run(mode, func(t *testing.T) {
			home := testkit.TempDir(t)
			_, release, err := uninstall.OwnerLock(context.Background(), home, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			marker := shellquote.Word(filepath.Join(home, "deleted"))
			body := ""
			switch mode {
			case "empty":
				body = "exit 0\n"
			case "list-exit1":
				body = "echo 'conntrack v1.4.7 (conntrack-tools): 0 flow entries have been shown.' >&2\nexit 1\n"
			case "delete-race":
				body = "if [ \"$1\" = -D ]; then touch " + marker + "; exit 1; fi\nif [ ! -f " + marker + " ]; then echo entry; fi\n"
			case "remaining-then-empty":
				body = "if [ \"$1\" = -D ]; then exit 1; fi\nif [ ! -f " + marker + " ]; then echo first > " + marker + "; echo entry; elif [ \"$(cat " + marker + ")\" = first ]; then echo second > " + marker + "; echo entry; fi\n"
			case "delete-error":
				body = "if [ \"$1\" = -D ]; then exit 1; fi\necho entry\n"
			}
			cleaner, observer := resourceConntrackFixture(t, home, body)
			ctx := ports.WithNativeOwnerHome(ports.WithNativeOperationObserver(context.Background(), observer), home)
			err = cleaner.Clear(ctx, netip.MustParseAddrPort("10.20.30.40:51820"))
			wantErr := mode == "list-exit1" || mode == "delete-error" || mode == "remaining-then-empty"
			if (err != nil) != wantErr || observer.uncertain != wantErr {
				t.Fatalf("err=%v unknown=%v", err, observer.uncertain)
			}
			if !wantErr && len(observer.terminals) != int(observer.commands) {
				t.Fatalf("commands=%d terminals=%v", observer.commands, observer.terminals)
			}
			if (mode == "delete-race" || mode == "remaining-then-empty") && observer.commands != 3 {
				t.Fatalf("expected list/delete/recheck, got %d", observer.commands)
			}
		})
	}
}

func TestResourceConntrackOriginalOFDOutlivesLostCleanupHelper(t *testing.T) {
	if home := os.Getenv("SUBYARD_TEST_CONNTRACK_LOSS"); home != "" {
		_, release, err := uninstall.OwnerLock(context.Background(), home, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		ready := shellquote.Word(filepath.Join(home, "cleanup-pid"))
		finish := shellquote.Word(filepath.Join(home, "release"))
		cleaner, observer := resourceConntrackFixture(t, home, "if [ \"$1\" = -L ]; then echo entry; exit 0; fi\necho $$ > "+ready+"\nwhile [ ! -f "+finish+" ]; do sleep .01; done\n")
		ctx := ports.WithNativeOwnerHome(ports.WithNativeOperationObserver(context.Background(), observer), home)
		if err := cleaner.Clear(ctx, netip.MustParseAddrPort("10.20.30.40:51820")); err != nil {
			t.Fatal(err)
		}
		return
	}
	home := testkit.TempDir(t)
	child := exec.Command(os.Args[0], "-test.run=^TestResourceConntrackOriginalOFDOutlivesLostCleanupHelper$")
	child.Env = append(os.Environ(), "SUBYARD_TEST_CONNTRACK_LOSS="+home)
	child.Stdout, child.Stderr = io.Discard, io.Discard
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	pid := 0
	defer func() {
		os.WriteFile(filepath.Join(home, "release"), nil, 0600)
		if pid > 0 {
			syscall.Kill(-pid, syscall.SIGKILL)
		}
		child.Process.Kill()
		child.Wait()
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		raw, _ := os.ReadFile(filepath.Join(home, "cleanup-pid"))
		pid, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
		if pid > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("cleanup child not dispatched")
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err == nil {
		t.Fatal("lost helper succeeded")
	}
	contender, err := os.OpenFile(uninstall.OwnerLockPath(home), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer contender.Close()
	if err := syscall.Flock(int(contender.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		t.Fatal("cleanup child lost original OFD")
	}
	if raw, err := os.ReadFile(filepath.Join(home, "dispatch-terminal")); err != nil || string(raw) != "1" {
		t.Fatalf("lost deletion minted terminal: %q %v", raw, err)
	}
	if _, err := os.Stat(filepath.Join(home, "dispatch-intent")); err != nil {
		t.Fatal("missing pre-dispatch intent", err)
	}
	testkit.WriteFile(t, filepath.Join(home, "release"), nil, 0600)
	for time.Now().Before(deadline) {
		if err := syscall.Flock(int(contender.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("cleanup child retained OFD after drain")
}

func TestResourceConntrackRejectsForeignStdinBeforeExec(t *testing.T) {
	home := testkit.TempDir(t)
	_, release, err := uninstall.OwnerLock(context.Background(), home, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	source, err := filepath.Abs("../../scripts/lib/native-owner-barrier.py")
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(home, "effect")
	binary := filepath.Join(home, "conntrack")
	testkit.WriteFile(t, binary, []byte("#!/bin/sh\ntouch "+shellquote.Word(marker)+"\n"), 0700)
	command := exec.Command("/usr/bin/python3", "-B", source, "conntrack", strconv.Itoa(os.Getuid()), home, binary, "-L", "-f", "ipv4", "-p", "udp", "--orig-dst", "10.20.30.40", "--orig-port-dst", "51820", "--reply-src", "10.20.30.40")
	foreign, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	command.Stdin = foreign
	if err := command.Run(); err == nil {
		t.Fatal("foreign stdin admitted")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("foreign stdin reached conntrack")
	}
}

// This transport fixture closes fd>=3 like sudo; only original stdin reaches
// the production post-entry validator and the actual cleanup executable.
func TestResourceConntrackSudoTransportKeepsOriginalStdin(t *testing.T) {
	home := testkit.TempDir(t)
	_, release, err := uninstall.OwnerLock(context.Background(), home, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	bin := testkit.TempDir(t)
	testkit.WriteFile(t, filepath.Join(bin, "sudo"), []byte("#!/bin/sh\nset -eu\n[ \"$1\" = -n ]\n[ \"$2\" = -- ]\nshift 2\nexec 3>&-\nexec \"$@\"\n"), 0700)
	cleaner, observer := resourceConntrackFixture(t, home, "exit 0\n")
	source, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	cleaner.Run = func(ctx context.Context, binary string, args ...string) ([]byte, error) {
		return runResourceConntrackWithSudo(ctx, source, binary, true, filepath.Join(bin, "sudo"), nil, args...)
	}
	ctx := ports.WithNativeOwnerHome(ports.WithNativeOperationObserver(context.Background(), observer), home)
	if err := cleaner.Clear(ctx, netip.MustParseAddrPort("10.20.30.40:51820")); err != nil {
		t.Fatal(err)
	}
	if observer.uncertain || observer.commands != 1 || len(observer.terminals) != 1 {
		t.Fatalf("sudo transport custody: commands=%d terminal=%v unknown=%v", observer.commands, observer.terminals, observer.uncertain)
	}
}
