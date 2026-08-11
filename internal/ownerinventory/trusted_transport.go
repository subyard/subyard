package ownerinventory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	transportadapter "github.com/Subyard/Subyard/internal/adapters/transport"
)

func AssessSSHHostKey(
	ctx context.Context, root, program, target string, timeout time.Duration,
) (SSHHostTrust, error) {
	temporaryRoot := filepath.Join(root, "tmp")
	if err := os.MkdirAll(temporaryRoot, 0o700); err != nil {
		return SSHHostTrust{}, err
	}
	temporary, err := os.MkdirTemp(temporaryRoot, ".ssh-assessment-*")
	if err != nil {
		return SSHHostTrust{}, err
	}
	defer os.RemoveAll(temporary)
	if err := os.Chmod(temporary, 0o700); err != nil {
		return SSHHostTrust{}, err
	}
	knownHostsPath := filepath.Join(temporary, "known_hosts")
	knownHosts, err := os.OpenFile(knownHostsPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return SSHHostTrust{}, err
	}
	if err := knownHosts.Close(); err != nil {
		return SSHHostTrust{}, err
	}
	process, err := transportadapter.SSHHostKeyAssessment(program, target, knownHostsPath, timeout)
	if err != nil {
		return SSHHostTrust{}, err
	}
	_, processErr := process.Call(ctx, "", nil)
	trust, trustErr := ReadSSHHostTrust(knownHostsPath)
	if trustErr != nil {
		if processErr != nil {
			return SSHHostTrust{}, fmt.Errorf("SSH host-key assessment failed: %w", processErr)
		}
		return SSHHostTrust{}, trustErr
	}
	return trust, nil
}

type managedTrustTransport struct {
	root           string
	program        string
	target         string
	knownHostsLine string
	timeout        time.Duration
	environment    []string
}

func (transport *managedTrustTransport) Call(
	ctx context.Context, _ string, request []byte,
) ([]byte, error) {
	if err := os.MkdirAll(transport.root, 0o700); err != nil {
		return nil, err
	}
	temporary, err := os.MkdirTemp(transport.root, ".ssh-trust-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(temporary)
	if err := os.Chmod(temporary, 0o700); err != nil {
		return nil, err
	}
	knownHostsPath := filepath.Join(temporary, "known_hosts")
	if err := os.WriteFile(knownHostsPath, []byte(transport.knownHostsLine+"\n"), 0o600); err != nil {
		return nil, err
	}
	process, err := transportadapter.SSHPinned(
		transport.program, transport.target, knownHostsPath, transport.timeout,
	)
	if err != nil {
		return nil, err
	}
	process.Env = transport.environment
	response, err := process.Call(ctx, "", request)
	if err != nil && sshHostKeyChanged(err.Error()) {
		return response, fmt.Errorf(
			"%w: OwnerHost SSH server key changed (expected %s); run yard host repair",
			ErrIntegrity, fingerprintFromKnownHostsLine(transport.knownHostsLine),
		)
	}
	return response, err
}

func fingerprintFromKnownHostsLine(line string) string {
	trust, err := NewSSHHostTrust(line)
	if err != nil {
		return "unknown"
	}
	return trust.Fingerprint
}

func sshHostKeyChanged(message string) bool {
	message = strings.ToLower(message)
	return strings.Contains(message, "remote host identification has changed") ||
		strings.Contains(message, "host key verification failed") ||
		strings.Contains(message, "offending ") && strings.Contains(message, "key")
}

func (store Connections) TrustedSSHClient(
	connection Connection, program string, timeout time.Duration,
) (Client, error) {
	trust, err := connection.RequireTrust()
	if err != nil {
		return Client{}, err
	}
	registered, err := store.List()
	if err != nil {
		return Client{}, err
	}
	matched := false
	for _, current := range registered {
		if current.HostID != connection.HostID {
			continue
		}
		currentTrust, trustErr := current.RequireTrust()
		if trustErr != nil || current.Destination != connection.Destination ||
			currentTrust.Fingerprint != trust.Fingerprint ||
			currentTrust.KnownHostsLine != trust.KnownHostsLine {
			return Client{}, errors.New("owner connection changed before trusted SSH client creation")
		}
		matched = true
		break
	}
	if !matched {
		return Client{}, errors.New("owner connection is not registered")
	}
	return Client{
		Transport: &managedTrustTransport{
			root: filepath.Join(store.Root, "tmp"), program: program,
			target: connection.Destination, knownHostsLine: trust.KnownHostsLine,
			timeout: timeout,
		},
		verifiedFingerprint: trust.Fingerprint,
	}, nil
}

func CandidateSSHClient(
	root, target string, trust SSHHostTrust, program string, timeout time.Duration,
) (Client, error) {
	if err := trust.Validate(); err != nil {
		return Client{}, err
	}
	return Client{
		Transport: &managedTrustTransport{
			root: filepath.Join(root, "tmp"), program: program, target: target,
			knownHostsLine: trust.KnownHostsLine, timeout: timeout,
		},
		verifiedFingerprint: trust.Fingerprint,
	}, nil
}
