package sshagentruntime

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/sys/unix"
)

func TestManagerRejectsUnsafeDirectoryAndInvalidTTL(t *testing.T) {
	root := testkit.TempDir(t)
	dir := filepath.Join(root, "grant")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	m := Manager{Config: Config{Directory: dir}}
	if _, err := m.Status(context.Background()); err == nil {
		t.Fatal("accepted public runtime directory")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	status, err := m.Status(context.Background())
	if err != nil || status.State != "locked" {
		t.Fatalf("status: %+v %v", status, err)
	}
	for _, ttl := range []time.Duration{0, -time.Second, 1500 * time.Millisecond, MaxTTL + time.Second} {
		if _, err := m.Unlock(context.Background(), "unused", ttl); err == nil || !strings.Contains(err.Error(), "TTL") {
			t.Fatalf("invalid TTL %s: %v", ttl, err)
		}
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	m.Config.Directory = link
	if _, err := m.Status(context.Background()); err == nil {
		t.Fatal("accepted symlink runtime directory")
	}
}

func TestStopAcceptsControlDisconnect(t *testing.T) {
	for name, response := range map[string]string{"empty": "", "partial": "{"} {
		t.Run(name, func(t *testing.T) {
			dir := testkit.TempDir(t)
			lock, err := acquireLock(context.Background(), filepath.Join(dir, "worker.lock"))
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			listener, err := net.Listen("unix", filepath.Join(dir, "control.sock"))
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			disconnected := make(chan struct{})
			go func() {
				defer close(disconnected)
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				var request controlRequest
				_ = json.NewDecoder(conn).Decode(&request)
				_, _ = conn.Write([]byte(response))
				_ = conn.Close() // The worker can exit before completing a lock response.
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- (Manager{Config: Config{Directory: dir}}).stop(ctx) }()
			<-disconnected
			if err := lock.Close(); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatalf("worker shutdown rejected after control disconnect: %v", err)
			}
		})
	}
}

func TestDaemonPendingGrantCanBeLockedAndCannotRestartGrant(t *testing.T) {
	root := testkit.TempDir(t)
	os.Chmod(root, 0700)
	dir := filepath.Join(root, "run")
	os.Mkdir(dir, 0700)
	cfg := daemonConfig{Config: Config{Directory: dir, SSHPort: 1, Developer: "dev", IdentityFile: "unused", KnownHostsFile: "unused"}, TTL: time.Minute}
	data, _ := json.Marshal(cfg)
	testkit.WriteFile(t, filepath.Join(dir, "worker.json"), data, 0600)
	done := make(chan error, 1)
	go func() { done <- RunDaemon(context.Background(), dir) }()
	m := Manager{Config: cfg.Config}
	deadline := time.Now().Add(3 * time.Second)
	for {
		status, err := m.Status(context.Background())
		if err == nil && status.State == "pending" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker not ready: %+v %v", status, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := m.Lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("worker survived lock")
	}
	if _, err := os.Stat(filepath.Join(dir, "private.sock")); !os.IsNotExist(err) {
		t.Fatal("private socket survived lock")
	}
	if err := RunDaemon(context.Background(), dir); err == nil {
		t.Fatal("worker restarted without fresh unlock")
	}
}

func TestUnsafePathsDoNotCreateOrTruncateFiles(t *testing.T) {
	root := testkit.TempDir(t)
	public := filepath.Join(root, "public")
	if err := os.Mkdir(public, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(public, 0o755); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(public, "grant")
	if err := validateDirectory(child, true); err == nil {
		t.Fatal("accepted public parent")
	}
	if _, err := os.Lstat(child); !os.IsNotExist(err) {
		t.Fatal("created directory before rejecting parent")
	}
	victim := filepath.Join(root, "victim")
	testkit.WriteFile(t, victim, []byte("preserve"), 0o600)
	link := filepath.Join(root, "linked")
	os.Link(victim, link)
	if f, err := protectedFile(link, os.O_WRONLY|os.O_TRUNC); err == nil {
		f.Close()
		t.Fatal("accepted hardlinked state")
	}
	content, _ := os.ReadFile(victim)
	if string(content) != "preserve" {
		t.Fatal("truncated unsafe file before validation")
	}
}

func TestEmptyAgentCannotActivateGrant(t *testing.T) {
	cfg, _ := syntheticYard(t)
	if err := validateDirectory(cfg.Directory, true); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(daemonConfig{Config: cfg, TTL: time.Minute})
	testkit.WriteFile(t, filepath.Join(cfg.Directory, "worker.json"), raw, 0600)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- RunDaemon(ctx, cfg.Directory) }()
	m := Manager{Config: cfg}
	deadline := time.Now().Add(3 * time.Second)
	for {
		status, _ := m.Status(ctx)
		if status.State == "pending" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker not ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	response, err := control(ctx, cfg.Directory, "activate")
	if err != nil {
		t.Fatal(err)
	}
	if response.Error == "" {
		t.Fatal("empty agent reported active grant")
	}
	m.Lock(context.Background())
	<-done
}

func TestActiveWorkerStopsAfterLock(t *testing.T) {
	cfg, _ := syntheticYard(t)
	validateDirectory(cfg.Directory, true)
	raw, _ := json.Marshal(daemonConfig{Config: cfg, TTL: time.Minute})
	testkit.WriteFile(t, filepath.Join(cfg.Directory, "worker.json"), raw, 0600)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- RunDaemon(ctx, cfg.Directory) }()
	m := Manager{Config: cfg}
	deadline := time.Now().Add(3 * time.Second)
	for {
		status, _ := m.Status(ctx)
		if status.State == "pending" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker not ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	preparedPending, err := m.PrepareLock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	conn, err := net.Dial("unix", filepath.Join(cfg.Directory, "private.sock"))
	if err != nil {
		t.Fatal(err)
	}
	err = agent.NewClient(conn).Add(agent.AddedKey{PrivateKey: key})
	conn.Close()
	if err != nil {
		t.Fatal(err)
	}
	response, err := control(ctx, cfg.Directory, "activate")
	if err != nil || response.Error != "" {
		t.Fatalf("activation: %v %s", err, response.Error)
	}
	if err := m.LockPrepared(ctx, preparedPending); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("newly activated grant accepted: %v", err)
	}
	prepared, err := m.PrepareLock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.LockPrepared(context.Background(), prepared); err != nil {
		t.Fatal(err)
	}
	<-done
}

func TestPrivateFilesRejectFIFOWithoutBlocking(t *testing.T) {
	root := testkit.TempDir(t)
	fifo := filepath.Join(root, "fifo")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	for _, read := range []func() error{
		func() error {
			f, err := protectedFile(fifo, unix.O_RDONLY)
			if f != nil {
				f.Close()
			}
			return err
		},
		func() error { _, err := readPrivateKey(fifo); return err },
	} {
		done := make(chan error, 1)
		go func() { done <- read() }()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("accepted FIFO")
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatal("blocked opening FIFO")
		}
	}
}

func TestValidateKeyRejectsUnencryptedOrPublicFiles(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(testkit.TempDir(t), "key")
	block, err := ssh.MarshalPrivateKeyWithPassphrase(key, "synthetic", []byte("synthetic-passphrase"))
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, pem.EncodeToMemory(block), 0600)
	if err := ValidateKey(path); err != nil {
		t.Fatalf("encrypted key rejected: %v", err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if err := ValidateKey(path); err == nil {
		t.Fatal("publicly readable key accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	block, err = ssh.MarshalPrivateKey(key, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, pem.EncodeToMemory(block), 0600)
	if err := ValidateKey(path); err == nil {
		t.Fatal("unencrypted key accepted")
	}
	testkit.WriteFile(t, path, []byte("invalid private key"), 0600)
	if err := ValidateKey(path); err == nil {
		t.Fatal("malformed key accepted")
	}
}

func shortRuntimeRoot(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("", "sya-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestWorkerConfigKeepsControlFileChecksBeforeReplacement(t *testing.T) {
	dir := shortRuntimeRoot(t)
	path := filepath.Join(dir, "worker.json")
	assertPreserved := func(name string) {
		t.Helper()
		if content, err := os.ReadFile(path); err != nil || string(content) != "preserve" {
			t.Fatalf("%s: control file changed: %q %v", name, content, err)
		}
	}
	testkit.WriteFile(t, path, []byte("preserve"), 0o644)
	if err := writeWorkerConfig(path, []byte("{}")); err == nil || err.Error() != "unsafe SSH agent control file" {
		t.Fatalf("broad control file: %v", err)
	}
	assertPreserved("broad mode")
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "linked")
	if err := os.Link(path, link); err != nil {
		t.Fatal(err)
	}
	if err := writeWorkerConfig(path, []byte("{}")); err == nil || err.Error() != "unsafe SSH agent control file" {
		t.Fatalf("hard-linked control file: %v", err)
	}
	assertPreserved("hard link")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(dir, "symlinked")
	if err := os.Symlink(path, symlink); err != nil {
		t.Fatal(err)
	}
	if err := writeWorkerConfig(symlink, []byte("{}")); err == nil {
		t.Fatal("followed a symlinked control file")
	}
	assertPreserved("symlink")
	if info, err := os.Lstat(symlink); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink replaced: %v", err)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 {
		if err := writeWorkerConfig(path, []byte("{}")); !errors.Is(err, unix.EACCES) || err.Error() != "permission denied" {
			t.Fatalf("read-only control file: %v", err)
		}
		assertPreserved("read-only")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(dir, "directory.json")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeWorkerConfig(directory, []byte("{}")); !errors.Is(err, unix.EISDIR) || err.Error() != "is a directory" {
		t.Fatalf("directory control file: %v", err)
	}
	for _, name := range []string{"replace", "create"} {
		if err := writeWorkerConfig(path, []byte("{}")); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s: control file metadata: %v %v", name, info, err)
		}
		if content, _ := os.ReadFile(path); string(content) != "{}" {
			t.Fatalf("%s: control file content %q", name, content)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
}

func TestValidateDirectoryFinishesNarrowAndRefusesBroadDirectory(t *testing.T) {
	dir := filepath.Join(shortRuntimeRoot(t), "run")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := validateDirectory(dir, true); err != nil {
		t.Fatalf("narrow owned directory: %v", err)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("narrow directory not finished: %v %v", info, err)
	}
	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	want := "SSH agent runtime directory and parent must be owned by the operator with mode 0700"
	if err := validateDirectory(dir, true); err == nil || err.Error() != want {
		t.Fatalf("broad directory: %v", err)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o750 {
		t.Fatalf("broad directory changed: %v %v", info, err)
	}
}
