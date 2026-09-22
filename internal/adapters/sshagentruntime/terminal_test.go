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

	"golang.org/x/term"
)

type ttyUnlockConfig struct {
	Config Config
	Key    string
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
	_, unlockErr := manager.Unlock(ctx, cfg.Key, time.Minute)
	after, err := term.GetState(int(os.Stdin.Fd()))
	if err != nil || !reflect.DeepEqual(before, after) {
		fmt.Fprintln(os.Stderr, "terminal settings were not restored")
		return 3
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
	for _, scenario := range []string{"success", "retry", "cancel"} {
		t.Run(scenario, func(t *testing.T) { testTerminalUnlock(t, scenario) })
	}
}

func testTerminalUnlock(t *testing.T, scenario string) {
	cfg, _ := syntheticYard(t)
	manager, key := prepareSyntheticUnlock(t, cfg)
	raw, err := json.Marshal(ttyUnlockConfig{Config: manager.Config, Key: key})
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
	captured := make(chan string, 1)
	go func() {
		var text bytes.Buffer
		buffer := make([]byte, 1024)
		sent := 0
		for {
			n, err := outputReader.Read(buffer)
			text.Write(buffer[:n])
			count := strings.Count(text.String(), "Enter passphrase for ") + strings.Count(text.String(), "Bad passphrase, try again for ")
			if count > sent {
				sent = count
				prompted <- struct{}{}
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
	if scenario == "retry" {
		responses = append([]string{"wrong-synthetic-input\n"}, responses...)
	}
	for _, response := range responses {
		select {
		case <-prompted:
			if scenario == "cancel" {
				raw, err := os.ReadFile(path + ".pid")
				if err != nil {
					t.Fatal(err)
				}
				pid, err := strconv.Atoi(string(raw))
				if err != nil {
					t.Fatal(err)
				}
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
		if strings.Contains(output, "synthetic-passphrase") || strings.Contains(output, "wrong-synthetic-input") {
			t.Fatal("terminal echoed the key passphrase")
		}
		if !strings.Contains(output, "terminal settings restored") {
			t.Fatal("terminal settings were not restored after unlock")
		}
		if (err != nil) != (scenario == "cancel") {
			t.Fatalf("unexpected terminal unlock result: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("terminal unlock did not complete")
	}
	status, err := manager.Status(context.Background())
	wantState := "unlocked"
	if scenario == "cancel" {
		wantState = "locked"
	}
	if err != nil || status.State != wantState {
		t.Fatalf("terminal grant: %+v %v", status, err)
	}
}
