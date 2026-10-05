package projectruntime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/transport"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

// VSCode checks the controller listener before opening the preview SSH session.
type VSCode struct {
	transport.Process
	Home, SSHHost, CodeSSHHost string
}

func (code VSCode) Run(ctx context.Context, arguments ...string) ([]byte, error) {
	if err := previewPortAvailable("127.0.0.1:8765"); err != nil {
		return nil, err
	}
	if err := code.ensureSSH(ctx); err != nil {
		return nil, err
	}
	return code.Process.Run(ctx, arguments...)
}

func (code VSCode) ensureSSH(ctx context.Context) error {
	if !domain.SafeSSHTarget(code.SSHHost) || code.CodeSSHHost != domain.CodeSSHHost(code.SSHHost) || !filepath.IsAbs(code.Home) {
		return errors.New("resolved controller SSH context is required for VS Code")
	}
	ready, err := code.sshReady(ctx)
	if err != nil || ready {
		return err
	}
	unavailable := fmt.Errorf("VS Code SSH alias %q is not configured; run yard init for a local yard or refresh its registration with yard remote add", code.CodeSSHHost)
	normal, err := code.sshOptions(ctx, code.SSHHost)
	if err != nil {
		return err
	}
	if !strings.Contains(normal, "hostname 127.0.0.1\n") {
		return unavailable
	}
	// Older managed snippets predate the dedicated preview alias. Retain their
	// transport, identity and host-key pins while extending the shared Host block.
	directory := filepath.Join(code.Home, ".ssh")
	paths, err := filepath.Glob(filepath.Join(directory, "subyard*.config"))
	if err != nil {
		return err
	}
	legacyHost := "\nHost " + code.SSHHost + "\n"
	var target string
	var original []byte
	for _, path := range paths {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() > 8<<20 {
			continue
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(string(contents), "# Managed by Subyard") || strings.Count(string(contents), legacyHost) != 1 {
			continue
		}
		if target != "" {
			return errors.New("ambiguous managed SSH configuration for VS Code")
		}
		target, original = path, contents
	}
	if target != "" {
		header, body, _ := strings.Cut(string(original), "\n")
		preview := "Host " + code.CodeSSHHost + "\n" +
			"    LocalForward 127.0.0.1:8765 127.0.0.1:8765\n" +
			"    ExitOnForwardFailure yes\n" +
			"    ControlMaster auto\n" +
			"    ControlPath ~/.ssh/subyard-code-cm-%C\n" +
			"    ControlPersist no\n\n"
		body = strings.Replace("\n"+body, legacyHost, "\nHost "+code.SSHHost+" "+code.CodeSSHHost+"\n", 1)
		updated := []byte(header + "\n" + preview + strings.TrimPrefix(body, "\n"))
		if err := config.WritePersistentFileIfUnchanged(directory, target, config.PersistentFileSnapshot{Exists: true, Content: original}, updated); err != nil {
			return fmt.Errorf("upgrade managed VS Code SSH alias: %w", err)
		}
		ready, err = code.sshReady(ctx)
		if err != nil || ready {
			return err
		}
	}
	return unavailable
}

func (code VSCode) sshReady(ctx context.Context) (bool, error) {
	options, err := code.sshOptions(ctx, code.CodeSSHHost)
	if err != nil {
		return false, err
	}
	return strings.Contains(options, "hostname 127.0.0.1\n") &&
		strings.Contains(options, "localforward 127.0.0.1:8765 127.0.0.1:8765\n") &&
		strings.Contains(options, "exitonforwardfailure yes\n") &&
		strings.Contains(options, "/subyard-code-cm-") &&
		strings.Contains(options, "controlpersist no\n"), nil
}

func (code VSCode) sshOptions(ctx context.Context, host string) (string, error) {
	ssh := transport.Process{Program: "ssh", Env: code.Env, Timeout: 5 * time.Second, MaxBytes: 64 << 10}
	output, err := ssh.Run(ctx, "-G", host)
	if err != nil {
		return "", fmt.Errorf("check VS Code SSH configuration: %w", err)
	}
	return strings.NewReplacer("[", "", "]", "").Replace(string(output)), nil
}

func previewPortAvailable(address string) error {
	listener, err := net.Listen("tcp4", address)
	if err != nil {
		return fmt.Errorf("preview port %s is unavailable; close its current listener before opening yard code", address)
	}
	if err := listener.Close(); err != nil {
		return errors.New("could not release the preview port before opening VS Code")
	}
	return nil
}
