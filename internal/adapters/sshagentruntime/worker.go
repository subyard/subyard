package sshagentruntime

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/shellquote"
)

// guestSession publishes only the socket of this standard OpenSSH agent-forwarding
// session. Its stdin is a lifetime pipe, never a command or credential channel.
// Replacing the link is atomic; an older session must not remove its successor.
const guestSession = `set -eu
umask 077
test -n "${SSH_AUTH_SOCK:-}"
test -S "$SSH_AUTH_SOCK"
test -f /etc/ssh/ssh_config.d/50-subyard-agent.conf
for directory in "$HOME/.subyard" "$HOME/.subyard/run"; do
    test ! -L "$directory"
    if test -e "$directory"; then
        test -d "$directory"
        test -O "$directory"
        test "$(stat -c %a "$directory")" = 700
    else
        mkdir -m 700 "$directory"
    fi
done
socket="$HOME/.subyard/run/ssh-agent.sock"
if test -e "$socket" || test -L "$socket"; then test -L "$socket"; fi
temporary="$HOME/.subyard/run/.ssh-agent-$1"
cleanup() {
    rm -f -- "$temporary"
    if test "$(readlink "$socket" 2>/dev/null || true)" = "$SSH_AUTH_SOCK"; then
        rm -f -- "$socket"
    fi
}
trap cleanup EXIT
trap 'exit 0' HUP INT TERM
ln -s -- "$SSH_AUTH_SOCK" "$temporary"
mv -Tf -- "$temporary" "$socket"
printf 'subyard-ssh-agent-ready\n'
cat >/dev/null
`

// RunWorker is entered only by the owner user's transient service. Its state has
// no private-key path or passphrase, so it cannot reload a key after a restart.
func RunWorker(parent context.Context, statePath string) error {
	if !filepath.IsAbs(statePath) {
		return errors.New("invalid SSH-agent worker state path")
	}
	if err := privateDirectory(filepath.Dir(statePath), false); err != nil {
		return err
	}
	var s session
	if err := readJSON(statePath, &s); err != nil {
		return err
	}
	if _, err := hex.DecodeString(s.Token); err != nil {
		return errors.New("invalid SSH-agent session token")
	}
	if s.Stopped || !time.Now().Before(s.ExpiresAt) {
		return nil
	}
	if !domain.SafeName(s.Yard) || !domain.SafeName(s.DevUser) || s.SSHPort < 1 || s.SSHPort > 65535 || len(s.Token) != 32 || !filepath.IsAbs(s.RuntimeDir) || !filepath.IsAbs(s.DataHome) || !filepath.IsAbs(s.OperatorHome) {
		return errors.New("invalid SSH-agent worker state")
	}
	// Validate all fields again: an internal entry point is not an authorization
	// shortcut around ownership checks. All children run as the operator, not root.
	if err := privateDirectory(s.RuntimeDir, false); err != nil {
		return err
	}
	for _, name := range []string{"id_ed25519", "known_hosts"} {
		if _, err := keyMetadata(filepath.Join(s.DataHome, "ssh", name)); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(parent, time.Until(s.ExpiresAt))
	defer cancel()
	socket := filepath.Join(s.RuntimeDir, "agent.sock")
	cmd := exec.CommandContext(ctx, "ssh-agent", "-D", "-a", socket, "-t", strconv.FormatInt(int64(time.Until(s.ExpiresAt)/time.Second), 10))
	cmd.Env = []string{"HOME=" + s.OperatorHome, "PATH=/usr/local/bin:/usr/bin:/bin"}
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		return errors.New("could not start host SSH agent")
	}
	agentDone := make(chan struct{})
	go func() { _ = cmd.Wait(); cancel(); close(agentDone) }()
	defer func() {
		cancel()
		_ = cmd.Process.Kill()
		<-agentDone
		_ = removeOwned(socket)
		_ = removeOwned(filepath.Join(s.RuntimeDir, "connection"))
	}()
	if err := waitUntil(ctx, func() bool {
		var token string
		return readJSON(filepath.Join(s.RuntimeDir, "ready"), &token) == nil && token == s.Token
	}); err != nil {
		return nil
	}
	for ctx.Err() == nil {
		if err := forwardSession(ctx, s); err != nil && ctx.Err() == nil {
			// A dead owner agent is never restarted or reloaded. Connection failures may
			// reconnect using only the still-live original agent and original deadline.
			if !ownedSocket(socket) {
				return errors.New("host SSH agent stopped")
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
	return nil
}

func sshArguments(s session) []string {
	// OpenSSH's session setup still probes SSH_AUTH_SOCK even with an explicit
	// ForwardAgent path. IdentityAgent=none would silently disable forwarding.
	// IdentitiesOnly and the dedicated -i file restrict the owner login identity.
	return []string{
		"-T", "-S", "none", "-F", "/dev/null",
		"-o", "BatchMode=yes", "-o", "ConnectTimeout=5",
		"-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=2",
		"-o", "StrictHostKeyChecking=yes", "-o", "IdentitiesOnly=yes",
		"-o", "ForwardAgent=" + sshOptionPath(filepath.Join(s.RuntimeDir, "agent.sock")),
		"-o", "UserKnownHostsFile=" + sshOptionPath(filepath.Join(s.DataHome, "ssh", "known_hosts")),
		"-i", strings.ReplaceAll(filepath.Join(s.DataHome, "ssh", "id_ed25519"), "%", "%%"),
		"-p", strconv.Itoa(s.SSHPort), "-l", s.DevUser, "127.0.0.1",
		shellquote.Command([]string{"sh", "-c", guestSession, "subyard-ssh-agent", s.Token}),
	}
}

func forwardSession(ctx context.Context, s session) error {
	command := exec.CommandContext(ctx, "ssh", sshArguments(s)...)
	command.Env = []string{"HOME=" + s.OperatorHome, "PATH=/usr/local/bin:/usr/bin:/bin", "SSH_AUTH_SOCK=" + filepath.Join(s.RuntimeDir, "agent.sock")}
	input, keepOpen, err := os.Pipe()
	if err != nil {
		return err
	}
	defer input.Close()
	defer keepOpen.Close()
	command.Stdin = input
	output, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	command.Stderr = io.Discard
	if err = command.Start(); err != nil {
		return err
	}
	ready := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(io.LimitReader(output, 256))
		ready <- scanner.Scan() && scanner.Text() == "subyard-ssh-agent-ready"
	}()
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case ok := <-ready:
		if !ok {
			_ = command.Process.Kill()
			<-done
			return errors.New("yard rejected shared SSH-agent session")
		}
	case <-ctx.Done():
		_ = command.Process.Kill()
		<-done
		return ctx.Err()
	case <-time.After(15 * time.Second):
		_ = command.Process.Kill()
		<-done
		return errors.New("yard SSH-agent session did not become ready")
	}
	path := filepath.Join(s.RuntimeDir, "connection")
	if err = writeJSON(path, s.Token); err != nil {
		_ = command.Process.Kill()
		<-done
		return err
	}
	err = <-done
	_ = removeOwned(path)
	if err != nil {
		return fmt.Errorf("yard SSH-agent connection ended: %w", err)
	}
	return nil
}

func sshOptionPath(path string) string { return strconv.Quote(strings.ReplaceAll(path, "%", "%%")) }
