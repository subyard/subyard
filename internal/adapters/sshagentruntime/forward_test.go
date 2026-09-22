package sshagentruntime

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
	"golang.org/x/sys/unix"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == "_ssh-agent-tty-test" {
		os.Exit(runTTYUnlockFixture(os.Args[2]))
	}
	if len(os.Args) == 3 && os.Args[1] == "_ssh-agent-worker" {
		if err := RunDaemon(context.Background(), os.Args[2]); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// This SSH protocol fixture exercises the real client, pinned host trust and
// streamlocal channel framing. Real sshd socket permissions remain a VM check.
func syntheticYard(t *testing.T) (Config, <-chan *ssh.ServerConn) {
	t.Helper()
	return syntheticYardWithHostKeys(t)
}

func syntheticYardWithHostKeys(t *testing.T, additionalKeys ...ssh.Signer) (Config, <-chan *ssh.ServerConn) {
	t.Helper()
	dir, err := os.MkdirTemp("", "sya-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	_, host, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, _ := ssh.NewSignerFromKey(host)
	_, identity, _ := ed25519.GenerateKey(rand.Reader)
	identitySigner, _ := ssh.NewSignerFromKey(identity)
	block, _ := ssh.MarshalPrivateKey(identity, "")
	identityPath := filepath.Join(dir, "transport")
	os.WriteFile(identityPath, pem.EncodeToMemory(block), 0600)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	hostPath := filepath.Join(dir, "known_hosts")
	os.WriteFile(hostPath, []byte(knownhosts.Line([]string{listener.Addr().String()}, hostSigner.PublicKey())+"\n"), 0600)
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	portNum, _ := strconv.Atoi(port)
	ready := make(chan *ssh.ServerConn, 4)
	serverCfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if string(key.Marshal()) != string(identitySigner.PublicKey().Marshal()) {
			return nil, fmt.Errorf("unexpected identity")
		}
		return nil, nil
	}}
	serverCfg.AddHostKey(hostSigner)
	for _, signer := range additionalKeys {
		serverCfg.AddHostKey(signer)
	}
	go func() {
		for {
			raw, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				conn, chans, requests, err := ssh.NewServerConn(raw, serverCfg)
				if err != nil {
					raw.Close()
					return
				}
				defer conn.Close()
				go func() {
					for ch := range chans {
						if ch.ChannelType() != "session" {
							ch.Reject(ssh.UnknownChannelType, "unsupported")
							continue
						}
						channel, reqs, err := ch.Accept()
						if err != nil {
							continue
						}
						go func() {
							defer channel.Close()
							for request := range reqs {
								if request.Type == "exec" {
									request.Reply(true, nil)
									channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
									return
								}
								request.Reply(false, nil)
							}
						}()
					}
				}()
				for request := range requests {
					switch request.Type {
					case "streamlocal-forward@openssh.com":
						request.Reply(true, nil)
						ready <- conn
					case "cancel-streamlocal-forward@openssh.com", "keepalive@openssh.com":
						request.Reply(true, nil)
					default:
						request.Reply(false, nil)
					}
				}
			}()
		}
	}()
	return Config{Directory: filepath.Join(dir, "run"), SSHPort: portNum, Developer: "dev", IdentityFile: identityPath, KnownHostsFile: hostPath}, ready
}

func prepareSyntheticUnlock(t *testing.T, cfg Config) (Manager, string) {
	t.Helper()
	dir := filepath.Dir(cfg.Directory)
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKeyWithPassphrase(key, "synthetic", []byte("synthetic-passphrase"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "encrypted")
	os.WriteFile(path, pem.EncodeToMemory(block), 0600)
	askpass := filepath.Join(dir, "askpass")
	os.WriteFile(askpass, []byte("#!/bin/sh\nprintf '%s\\n' synthetic-passphrase\n"), 0700)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Executable = executable
	m := Manager{Config: cfg, Environment: []string{"PATH=/usr/bin:/bin", "DISPLAY=synthetic", "SSH_ASKPASS_REQUIRE=force", "SSH_ASKPASS=" + askpass}}
	t.Cleanup(func() { m.Lock(context.Background()) })
	return m, path
}

func syntheticUnlock(t *testing.T, cfg Config, ttl time.Duration) (Manager, Status) {
	t.Helper()
	m, path := prepareSyntheticUnlock(t, cfg)
	status, err := m.Unlock(context.Background(), path, ttl)
	if err != nil {
		t.Fatal(err)
	}
	return m, status
}

func guestAgent(t *testing.T, server *ssh.ServerConn) agent.ExtendedAgent {
	t.Helper()
	channel, requests, err := server.OpenChannel("forwarded-streamlocal@openssh.com", ssh.Marshal(struct{ SocketPath, Reserved string }{"/home/dev/.ssh/subyard-agent.sock", ""}))
	if err != nil {
		t.Fatal(err)
	}
	go ssh.DiscardRequests(requests)
	t.Cleanup(func() { channel.Close() })
	return agent.NewClient(channel)
}

func TestDetachedGrantSignsExpiresAndCutsExistingGuestConnection(t *testing.T) {
	cfg, ready := syntheticYard(t)
	m, status := syntheticUnlock(t, cfg, 2*time.Second)
	if status.State != "unlocked" || status.RemainingSeconds < 1 || status.RemainingSeconds > 2 {
		t.Fatalf("status: %+v", status)
	}
	client := guestAgent(t, <-ready)
	keys, err := client.List()
	if err != nil || len(keys) != 1 {
		t.Fatalf("keys: %v %d", err, len(keys))
	}
	sig, err := client.Sign(keys[0], []byte("challenge"))
	if err != nil {
		t.Fatal(err)
	}
	if err := keys[0].Verify([]byte("challenge"), sig); err != nil {
		t.Fatal(err)
	}
	if err := client.RemoveAll(); err == nil {
		t.Fatal("guest mutated the detached agent")
	}
	time.Sleep(time.Until(status.ExpiresAt) + 100*time.Millisecond)
	if _, err := client.List(); err == nil {
		t.Fatal("existing guest connection survived expiry")
	}
	status, err = m.Status(context.Background())
	if err != nil || status.State != "locked" {
		t.Fatalf("expired status: %+v %v", status, err)
	}
}

func TestExplicitLockCutsExistingConnectionAndOtherYardRemainsUsable(t *testing.T) {
	first, firstReady := syntheticYard(t)
	second, secondReady := syntheticYard(t)
	ttl := 365 * 24 * time.Hour
	a, status := syntheticUnlock(t, first, ttl)
	if status.State != "unlocked" || status.RemainingSeconds < int64(ttl/time.Second)-5 || status.RemainingSeconds > int64(ttl/time.Second) {
		t.Fatalf("year-long grant: %+v", status)
	}
	_, _ = syntheticUnlock(t, second, time.Minute)
	clientA := guestAgent(t, <-firstReady)
	clientB := guestAgent(t, <-secondReady)
	if _, err := clientA.List(); err != nil {
		t.Fatal(err)
	}
	if err := a.Lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := clientA.List(); err == nil {
		t.Fatal("connection survived explicit lock")
	}
	keys, err := clientB.List()
	if err != nil || len(keys) != 1 {
		t.Fatalf("other yard lost grant: %v %d", err, len(keys))
	}
}

func TestReconnectKeepsOriginalDeadline(t *testing.T) {
	cfg, ready := syntheticYard(t)
	m, original := syntheticUnlock(t, cfg, 6*time.Second)
	server := <-ready
	client := guestAgent(t, server)
	if _, err := client.List(); err != nil {
		t.Fatal(err)
	}
	server.Close()
	select {
	case server = <-ready:
	case <-time.After(4 * time.Second):
		t.Fatal("grant did not reconnect")
	}
	deadline := time.Now().Add(time.Second)
	for {
		status, err := m.Status(context.Background())
		if err == nil && status.State == "unlocked" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reconnected listener did not activate")
		}
		time.Sleep(10 * time.Millisecond)
	}
	client = guestAgent(t, server)
	if _, err := client.List(); err != nil {
		t.Fatal(err)
	}
	status, err := m.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.ExpiresAt.Equal(original.ExpiresAt) || status.RemainingSeconds >= original.RemainingSeconds {
		t.Fatalf("reconnect extended grant: before=%+v after=%+v", original, status)
	}
}

func TestFailedUnlockLeavesNoGrant(t *testing.T) {
	for _, failure := range []string{"wrong-passphrase", "host-key-mismatch"} {
		t.Run(failure, func(t *testing.T) {
			cfg, _ := syntheticYard(t)
			m, path := prepareSyntheticUnlock(t, cfg)
			if failure == "wrong-passphrase" {
				os.WriteFile(filepath.Join(filepath.Dir(cfg.Directory), "askpass"), []byte("#!/bin/sh\n[ ! -e \"$0.called\" ] || exit 1\n: > \"$0.called\"\nprintf '%s\\n' wrong-synthetic-passphrase\n"), 0700)
			} else {
				_, key, _ := ed25519.GenerateKey(rand.Reader)
				signer, _ := ssh.NewSignerFromKey(key)
				os.WriteFile(cfg.KnownHostsFile, []byte(knownhosts.Line([]string{fmt.Sprintf("127.0.0.1:%d", cfg.SSHPort)}, signer.PublicKey())+"\n"), 0600)
			}
			if _, err := m.Unlock(context.Background(), path, time.Minute); err == nil {
				t.Fatal("failed unlock succeeded")
			}
			status, err := m.Status(context.Background())
			if err != nil || status.State != "locked" {
				t.Fatalf("failed unlock left grant: %+v %v", status, err)
			}
			if _, err := os.Lstat(filepath.Join(cfg.Directory, "private.sock")); !os.IsNotExist(err) {
				t.Fatal("failed unlock left private agent socket")
			}
		})
	}
}

func TestWorkerCrashKillsPrivateAgent(t *testing.T) {
	cfg, _ := syntheticYard(t)
	_, _ = syntheticUnlock(t, cfg, time.Minute)
	conn, err := net.Dial("unix", filepath.Join(cfg.Directory, "private.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	raw, err := conn.(*net.UnixConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var credentials *unix.Ucred
	if err := raw.Control(func(fd uintptr) { credentials, err = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) }); err != nil {
		t.Fatal(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	privateAgent, err := os.FindProcess(int(credentials.Pid))
	if err != nil {
		t.Fatal(err)
	}
	defer privateAgent.Kill()
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", credentials.Pid))
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(stat)[strings.LastIndex(string(stat), ")")+2:])
	workerPID, err := strconv.Atoi(fields[1])
	if err != nil {
		t.Fatal(err)
	}
	worker, err := os.FindProcess(workerPID)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Release()
	args, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", workerPID))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(args, []byte("\x00_ssh-agent-worker\x00"+cfg.Directory+"\x00")) {
		t.Fatal("private agent parent is not the fixture worker")
	}
	if err := worker.Kill(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		conn.SetDeadline(time.Now().Add(100 * time.Millisecond))
		if _, err := agent.NewClient(conn).List(); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("private agent survived abrupt worker death")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPinnedEd25519HostKeySelectedWhenServerOffersECDSA(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := syntheticYardWithHostKeys(t, signer)
	_, status := syntheticUnlock(t, cfg, time.Minute)
	if status.State != "unlocked" {
		t.Fatalf("pinned host key did not authenticate: %+v", status)
	}
}
