package reconcileruntime

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Subyard/Subyard/internal/testkit"
)

// `incus exec` changes terminal modes when its stdin is a terminal. The script
// does the same, so a child that keeps the terminal stops on SIGTTOU.
func TestReadOnlyProbeDoesNotStopOnOperatorTerminal(t *testing.T) {
	if root := os.Getenv("SUBYARD_TEST_PROBE_TERMINAL_ROOT"); root != "" {
		runtime := Runtime{RepositoryRoot: root, Stdin: os.Stdin, Environment: []string{"PATH=/usr/bin:/bin"}}
		if err := runtime.runScript(readOnlyProbeContext(context.Background()), io.Discard, "probe.sh"); err != nil {
			t.Fatal(err)
		}
		return
	}
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skip("no pseudo-terminal:", err)
	}
	master := os.NewFile(uintptr(fd), "fixture-terminal-master")
	defer master.Close()
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	number, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := os.OpenFile("/dev/pts/"+strconv.Itoa(number), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	go io.Copy(io.Discard, master)

	root := testkit.TempDir(t)
	if err := os.Mkdir(filepath.Join(root, "scripts"), 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "finished")
	testkit.WriteFile(t, filepath.Join(root, "scripts", "probe.sh"), []byte("#!/bin/sh\n"+
		"if [ -t 0 ]; then stty raw; fi\n"+
		": > '"+marker+"'\n"), 0o700)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// The helper is a session leader that owns the terminal, like the operator's CLI.
	helper := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestReadOnlyProbeDoesNotStopOnOperatorTerminal$")
	helper.Env = append(os.Environ(), "SUBYARD_TEST_PROBE_TERMINAL_ROOT="+root)
	helper.Stdin = terminal
	helper.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	helper.WaitDelay = time.Second
	output, err := helper.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatal("probe stopped on the operator terminal")
	}
	if err != nil {
		t.Fatal(err, string(output))
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("probe did not finish", err, string(output))
	}
}
