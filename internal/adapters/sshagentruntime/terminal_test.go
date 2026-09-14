package sshagentruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	manager := Manager{Config: cfg.Config, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Environment: []string{"PATH=/usr/bin:/bin", "SSH_ASKPASS_REQUIRE=never"}}
	_, err = manager.Unlock(ctx, cfg.Key, time.Minute)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func TestEncryptedKeyUnlockWithRealTerminalAndAskpassDisabled(t *testing.T) {
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
	prompted := make(chan struct{}, 1)
	captured := make(chan string, 1)
	go func() {
		var text bytes.Buffer
		buffer := make([]byte, 1024)
		sent := false
		for {
			n, err := outputReader.Read(buffer)
			text.Write(buffer[:n])
			if !sent && strings.Contains(text.String(), "Enter passphrase") {
				sent = true
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
	select {
	case <-prompted:
		if _, err := io.WriteString(inputWriter, "synthetic-passphrase\n"); err != nil {
			t.Fatal(err)
		}
	case err := <-done:
		t.Fatalf("terminal unlock exited before prompting: %v; %s", err, <-captured)
	case <-ctx.Done():
		t.Fatal("terminal unlock did not prompt")
	}
	// Keep terminal input open so script does not send EOF while ssh-add prompts.
	select {
	case err := <-done:
		output := <-captured
		if err != nil {
			t.Fatalf("terminal unlock failed: %v; %s", err, strings.ReplaceAll(output, "synthetic-passphrase", "[redacted synthetic input]"))
		}
	case <-ctx.Done():
		t.Fatal("terminal unlock did not complete")
	}
	status, err := manager.Status(context.Background())
	if err != nil || status.State != "unlocked" {
		t.Fatalf("terminal grant: %+v %v", status, err)
	}
}
