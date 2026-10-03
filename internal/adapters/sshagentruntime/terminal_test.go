package sshagentruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

type ttyUnlockConfig struct {
	Config           Config
	Key              string
	CheckQueuedInput bool
}

func runTTYUnlockFixture(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 2
	}
	var cfg ttyUnlockConfig
	if json.Unmarshal(raw, &cfg) != nil {
		return 2
	}
	signalCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(signalCtx, 15*time.Second)
	defer cancel()
	before, err := term.GetState(int(os.Stdin.Fd()))
	if err != nil {
		return 2
	}
	if err := os.WriteFile(path+".pid", []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		return 2
	}
	manager := Manager{Config: cfg.Config, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Environment: []string{"PATH=/usr/bin:/bin", "SSH_ASKPASS_REQUIRE=never"}}
	if cfg.CheckQueuedInput {
		go func() {
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				queued, err := unix.IoctlGetInt(int(os.Stdin.Fd()), unix.TIOCINQ)
				if err != nil {
					return
				}
				if queued > 0 {
					fmt.Fprintln(os.Stderr, "synthetic input queued")
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	_, unlockErr := manager.Unlock(ctx, cfg.Key, time.Minute)
	after, err := term.GetState(int(os.Stdin.Fd()))
	if err != nil || !reflect.DeepEqual(before, after) {
		fmt.Fprintln(os.Stderr, "terminal settings were not restored")
		return 3
	}
	if cfg.CheckQueuedInput {
		queued, err := unix.IoctlGetInt(int(os.Stdin.Fd()), unix.TIOCINQ)
		if err != nil || queued != 0 {
			fmt.Fprintln(os.Stderr, "terminal input was not cleared")
			return 3
		}
	}
	fmt.Fprintln(os.Stderr, "terminal settings restored")
	err = unlockErr
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func TestEncryptedKeyUnlockWithRealTerminalAndAskpassDisabled(t *testing.T) {
	for _, scenario := range []string{"success", "retry", "cancel", "cancel-forced"} {
		t.Run(scenario, func(t *testing.T) { testTerminalUnlock(t, scenario) })
	}
}

func testTerminalUnlock(t *testing.T, scenario string) {
	cancelled := scenario == "cancel" || scenario == "cancel-forced"
	cfg, _ := syntheticYard(t)
	manager, key := prepareSyntheticUnlock(t, cfg)
	raw, err := json.Marshal(ttyUnlockConfig{Config: manager.Config, Key: key,
		CheckQueuedInput: scenario == "cancel-forced"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(cfg.Directory), "terminal.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	command := quote(manager.Config.Executable) + " _ssh-agent-tty-test " + quote(path)
	process := exec.CommandContext(ctx, "script", "-qefc", command, "/dev/null")
	process.Env = []string{"PATH=/usr/bin:/bin", "TERM=dumb", "SSH_ASKPASS_REQUIRE=never"}
	if scenario == "cancel-forced" {
		tools := testkit.TempDir(t)
		testkit.WriteFile(t, filepath.Join(tools, "ssh-add"), []byte("#!/bin/sh\n"+
			"set -e\n"+
			"trap '' TERM\n"+
			"stty -echo < /dev/tty\n"+
			"printf 'Enter passphrase for synthetic-fixture: ' > /dev/tty\n"+
			"exec /bin/sleep 30\n"), 0o700)
		process.Env[0] = "PATH=" + tools + ":/usr/bin:/bin"
	}
	inputWriter, err := process.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer inputWriter.Close()
	outputReader, outputWriter := io.Pipe()
	defer outputReader.Close()
	defer outputWriter.Close()
	process.Stdout = outputWriter
	process.Stderr = outputWriter
	process.WaitDelay = time.Second
	prompted := make(chan struct{}, 4)
	inputQueued := make(chan struct{}, 1)
	captured := make(chan string, 1)
	go func() {
		var text bytes.Buffer
		buffer := make([]byte, 1024)
		sent := 0
		queued := false
		for {
			n, err := outputReader.Read(buffer)
			text.Write(buffer[:n])
			count := strings.Count(text.String(), "Enter passphrase for ") + strings.Count(text.String(), "Bad passphrase, try again for ")
			if count > sent {
				sent = count
				prompted <- struct{}{}
			}
			if !queued && strings.Contains(text.String(), "synthetic input queued") {
				queued = true
				inputQueued <- struct{}{}
			}
			if err != nil {
				captured <- text.String()
				return
			}
		}
	}()
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { err := process.Wait(); outputWriter.Close(); done <- err }()
	responses := []string{"synthetic-passphrase\n"}
	var cancelledAt time.Time
	if scenario == "retry" {
		responses = append([]string{"wrong-synthetic-input\n"}, responses...)
	}
	for _, response := range responses {
		select {
		case <-prompted:
			if cancelled {
				if scenario == "cancel-forced" {
					if _, err := io.WriteString(inputWriter, "synthetic-queued-input\n"); err != nil {
						t.Fatal(err)
					}
					select {
					case <-inputQueued:
					case err := <-done:
						t.Fatalf("terminal unlock exited before input queued: %v", err)
					case <-ctx.Done():
						t.Fatal("terminal input did not queue")
					}
				}
				raw, err := os.ReadFile(path + ".pid")
				if err != nil {
					t.Fatal(err)
				}
				pid, err := strconv.Atoi(string(raw))
				if err != nil {
					t.Fatal(err)
				}
				cancelledAt = time.Now()
				if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
			} else if _, err := io.WriteString(inputWriter, response); err != nil {
				t.Fatal(err)
			}
		case err := <-done:
			t.Fatalf("terminal unlock exited before prompting: %v", err)
		case <-ctx.Done():
			t.Fatal("terminal unlock did not prompt")
		}
	}
	// Keep terminal input open so script does not send EOF while ssh-add prompts.
	select {
	case err := <-done:
		output := <-captured
		if strings.Contains(output, "synthetic-passphrase") || strings.Contains(output, "wrong-synthetic-input") ||
			strings.Contains(output, "synthetic-queued-input") {
			t.Fatal("terminal echoed the key passphrase")
		}
		if strings.Contains(output, "terminal input was not cleared") {
			t.Fatal("terminal retained queued input after cancelled unlock")
		}
		if !strings.Contains(output, "terminal settings restored") {
			t.Fatal("terminal settings were not restored after unlock")
		}
		if (err != nil) != cancelled {
			t.Fatalf("unexpected terminal unlock result: %v", err)
		}
		if scenario == "cancel-forced" && time.Since(cancelledAt) < 2*time.Second {
			t.Fatal("cancellation did not exercise the forced child exit")
		}
	case <-ctx.Done():
		t.Fatal("terminal unlock did not complete")
	}
	status, err := manager.Status(context.Background())
	wantState := "unlocked"
	if cancelled {
		wantState = "locked"
	}
	if err != nil || status.State != wantState {
		t.Fatalf("terminal grant: %+v %v", status, err)
	}
	if cancelled {
		for _, name := range []string{"private.sock", "control.sock"} {
			if _, err := os.Lstat(filepath.Join(cfg.Directory, name)); !os.IsNotExist(err) {
				t.Fatalf("cancelled unlock retained %s: %v", name, err)
			}
		}
	}
}
