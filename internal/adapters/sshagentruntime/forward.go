package sshagentruntime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func forwardGrant(ctx context.Context, cfg Config, privateSocket string, state *workerState, ready chan<- error) {
	first := true
	for {
		err := forwardOnce(ctx, cfg, privateSocket, state, func() {
			if first {
				first = false
				ready <- nil
			}
		})
		state.mu.Lock()
		state.ready = false
		state.mu.Unlock()
		if first {
			ready <- err
			return
		}
		if errors.Is(err, errForwardStalled) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func forwardOnce(ctx context.Context, cfg Config, privateSocket string, state *workerState, onReady func()) error {
	data, err := os.ReadFile(cfg.IdentityFile)
	if err != nil {
		return errors.New("cannot read yard transport identity")
	}
	signer, err := ssh.ParsePrivateKey(data)
	clear(data)
	if err != nil {
		return errors.New("invalid yard transport identity")
	}
	hostKey, err := knownhosts.New(cfg.KnownHostsFile)
	if err != nil {
		return errors.New("cannot read pinned yard host key")
	}
	address := fmt.Sprintf("127.0.0.1:%d", cfg.SSHPort)
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", address)
	if err != nil {
		return err
	}
	defer conn.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, address, &ssh.ClientConfig{User: cfg.Developer, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: hostKey,
		// Yard setup pins the Ed25519 host key. Negotiate that exact key type
		// even when sshd also offers RSA or ECDSA host keys.
		HostKeyAlgorithms: []string{ssh.KeyAlgoED25519},
	})
	if err != nil {
		return err
	}
	client := ssh.NewClient(sshConn, chans, reqs)
	defer client.Close()
	socket := GuestSocket(cfg.Developer)
	session, err := client.NewSession()
	if err != nil {
		return err
	}
	// The dedicated path may contain a stale socket after a transport failure.
	// Never unlink a symlink, regular file, directory or another named object.
	command := "if [ -L '" + socket + "' ]; then exit 1; elif [ -S '" + socket + "' ]; then rm -- '" + socket + "'; elif [ -e '" + socket + "' ]; then exit 1; fi"
	err = session.Run(command)
	session.Close()
	if err != nil {
		return errors.New("guest agent socket path is unsafe or unavailable")
	}
	listener, err := listenUnixBounded(ctx, client, socket)
	if err != nil {
		return err
	}
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Use an ordinary channel: x/crypto v0.52.0 global request
				// handling can spin when a reply channel closes during a send.
				probe, err := client.NewSession()
				if err == nil {
					err = probe.Run("true")
					probe.Close()
				}
				if err != nil {
					client.Close()
					return
				}
				conn.SetDeadline(time.Now().Add(30 * time.Second))
			}
		}
	}()
	state.mu.Lock()
	state.ready = true
	state.mu.Unlock()
	onReady()
	slots := make(chan struct{}, 32)
	for {
		guest, err := listener.Accept()
		if err != nil {
			return err
		}
		select {
		case slots <- struct{}{}:
			go func() {
				defer func() { <-slots }()
				upstream, err := net.DialTimeout("unix", privateSocket, time.Second)
				if err != nil {
					guest.Close()
					return
				}
				serveFiltered(ctx, guest, upstream)
			}()
		default:
			guest.Close()
		}
	}
}

var errForwardStalled = errors.New("SSH forwarding setup stalled")

func listenUnixBounded(ctx context.Context, client *ssh.Client, path string) (net.Listener, error) {
	type result struct {
		listener net.Listener
		err      error
	}
	completed := make(chan result, 1)
	go func() { listener, err := client.ListenUnix(path); completed <- result{listener, err} }()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case response := <-completed:
		return response.listener, response.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, errForwardStalled
	}
}
