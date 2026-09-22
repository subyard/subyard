package sshtrust

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/shellquote"
	"golang.org/x/crypto/ssh"
)

// Real OpenSSH consumes the generated nested ProxyCommand, while synthetic
// loopback servers keep this check independent of sshd, root, and external hosts.
func TestOpenSSHCombinedTrustThroughProxyChain(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "::1"} {
		t.Run(host, func(t *testing.T) { testOpenSSHCombinedTrustThroughProxyChain(t, host) })
	}
}

func testOpenSSHCombinedTrustThroughProxyChain(t *testing.T, host string) {
	program, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("OpenSSH client is unavailable")
	}
	first, _ := trustProbeServer(t, net.JoinHostPort(host, "0"), nil)
	second, _ := trustProbeServer(t, net.JoinHostPort(host, "0"), nil)
	var denyFinal atomic.Bool
	final, finalKey := trustProbeServer(t, "127.0.0.1:0", &denyFinal)
	root := t.TempDir()
	identity := filepath.Join(root, "identity")
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "synthetic test identity")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(identity, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	known, config, wrapper := filepath.Join(root, "known_hosts"), filepath.Join(root, "config"), filepath.Join(root, "ssh")
	configuration := fmt.Sprintf(`Host final
    HostName 127.0.0.1
    Port %d
    ProxyJump dev@%s,dev@%s
Host *
    User dev
    IdentityFile %q
    IdentitiesOnly yes
    UserKnownHostsFile %q
    GlobalKnownHostsFile /dev/null
    StrictHostKeyChecking yes
`, final.Addr().(*net.TCPAddr).Port, first.Addr().String(),
		second.Addr().String(), identity, known)
	if err := os.WriteFile(config, []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec "+shellquote.Command([]string{program, "-F", config})+" \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	manager := &Manager{Environment: os.Environ()}
	defer manager.Close()
	prompts := 0
	manager.Confirm = func(context.Context, []Proposal) error {
		prompts++
		return domain.ErrOperationDeclined
	}
	// A known final endpoint failing authentication must not persist unknown
	// proxy keys or even offer a trust prompt.
	finalLine := fmt.Sprintf("[127.0.0.1]:%d %s", final.Addr().(*net.TCPAddr).Port, ssh.MarshalAuthorizedKey(finalKey))
	if err := os.WriteFile(known, []byte(finalLine), 0o600); err != nil {
		t.Fatal(err)
	}
	denyFinal.Store(true)
	if _, err := manager.Options(ctx, wrapper, "final"); err == nil || prompts != 0 {
		t.Fatalf("authentication failure: err=%v prompts=%d", err, prompts)
	}
	data, err := os.ReadFile(known)
	if err != nil || string(data) != finalLine {
		t.Fatalf("authentication failure changed trust: %v", err)
	}
	denyFinal.Store(false)
	if err := os.Remove(known); err != nil {
		t.Fatal(err)
	}
	manager.Confirm = func(_ context.Context, proposals []Proposal) error {
		prompts++
		if len(proposals) != 3 {
			t.Fatalf("expected one combined proposal for three keys, got %d", len(proposals))
		}
		if _, err := os.Stat(known); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("trust persisted before consent: %v", err)
		}
		return domain.ErrOperationDeclined
	}
	if _, err := manager.Options(ctx, wrapper, "final"); !errors.Is(err, domain.ErrOperationDeclined) {
		t.Fatalf("decline: %v", err)
	}
	if _, err := os.Stat(known); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("decline persisted trust: %v", err)
	}
	manager.Confirm = func(_ context.Context, proposals []Proposal) error {
		prompts++
		if len(proposals) != 3 {
			t.Fatalf("expected three approved keys, got %d", len(proposals))
		}
		return nil
	}
	options, err := manager.Options(ctx, wrapper, "final")
	if err != nil {
		t.Fatal(err)
	}
	output, err := exec.CommandContext(ctx, wrapper, append(options, "final", "--", "true")...).CombinedOutput()
	if err != nil {
		t.Fatalf("strict payload through approved chain: %v: %s", err, output)
	}
	data, err = os.ReadFile(known)
	if err != nil || strings.Count(string(data), "\n") != 3 {
		t.Fatalf("expected exactly three persisted keys: %v", err)
	}
	if _, err := manager.Options(ctx, wrapper, "final"); err != nil || prompts != 2 {
		t.Fatalf("approved route prompted again: err=%v prompts=%d", err, prompts)
	}
}

func trustProbeServer(t *testing.T, address string, reject *atomic.Bool) (net.Listener, ssh.PublicKey) {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
		if reject != nil && reject.Load() {
			return nil, errors.New("synthetic authentication rejection")
		}
		return nil, nil
	}}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", address)
	if err != nil {
		if strings.HasPrefix(address, "[::1]") {
			t.Skipf("IPv6 loopback unavailable: %v", err)
		}
		t.Fatal(err)
	}
	var connections sync.Map
	t.Cleanup(func() {
		_ = listener.Close()
		connections.Range(func(key, _ any) bool { _ = key.(net.Conn).Close(); return true })
	})
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Store(connection, true)
			go func() {
				defer connections.Delete(connection)
				defer connection.Close()
				server, channels, requests, err := ssh.NewServerConn(connection, config)
				if err != nil {
					return
				}
				defer server.Close()
				go ssh.DiscardRequests(requests)
				for incoming := range channels {
					go trustProbeChannel(incoming)
				}
			}()
		}
	}()
	return listener, signer.PublicKey()
}

func trustProbeChannel(incoming ssh.NewChannel) {
	switch incoming.ChannelType() {
	case "session":
		channel, requests, err := incoming.Accept()
		if err != nil {
			return
		}
		defer channel.Close()
		for request := range requests {
			if request.Type == "exec" {
				_ = request.Reply(true, nil)
				_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
				return
			}
			_ = request.Reply(false, nil)
		}
	case "direct-tcpip":
		var destination struct {
			Host       string
			Port       uint32
			Origin     string
			OriginPort uint32
		}
		if err := ssh.Unmarshal(incoming.ExtraData(), &destination); err != nil {
			_ = incoming.Reject(ssh.ConnectionFailed, "invalid destination")
			return
		}
		ip := net.ParseIP(destination.Host)
		if ip == nil || !ip.IsLoopback() {
			_ = incoming.Reject(ssh.Prohibited, "loopback destinations only")
			return
		}
		target, err := net.DialTimeout("tcp", net.JoinHostPort(destination.Host, fmt.Sprint(destination.Port)), time.Second)
		if err != nil {
			_ = incoming.Reject(ssh.ConnectionFailed, "loopback connection failed")
			return
		}
		defer target.Close()
		channel, requests, err := incoming.Accept()
		if err != nil {
			return
		}
		defer channel.Close()
		go ssh.DiscardRequests(requests)
		go func() { _, _ = io.Copy(channel, target); _ = channel.CloseWrite() }()
		_, _ = io.Copy(target, channel)
	default:
		_ = incoming.Reject(ssh.UnknownChannelType, "unsupported channel")
	}
}
