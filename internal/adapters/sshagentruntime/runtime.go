// Package sshagentruntime owns a disposable, per-yard SSH signing grant.
package sshagentruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/sys/unix"
)

// MaxTTL is the largest lifetime accepted by OpenSSH ssh-add's signed-second parser.
const MaxTTL = time.Duration(math.MaxInt32) * time.Second

type Config struct {
	Directory      string
	Executable     string
	SSHPort        int
	Developer      string
	IdentityFile   string
	KnownHostsFile string
}

type Status struct {
	State            string    `json:"state"`
	ExpiresAt        time.Time `json:"expiresAt,omitzero"`
	RemainingSeconds int64     `json:"remainingSeconds,omitempty"`
}

type Manager struct {
	Config         Config
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	Environment    []string
}

type daemonConfig struct {
	Config Config
	TTL    time.Duration
}
type controlRequest struct {
	Action string `json:"action"`
}
type controlResponse struct {
	Status Status `json:"status"`
	Error  string `json:"error,omitempty"`
}

// Directory returns the stable per-owner, per-yard control directory. All callers
// use this name for granting, inspection and teardown. Manager validates ownership
// and permissions before reading or creating state.
func Directory(dataHome, yard string) string {
	digest := sha256.Sum256([]byte(filepath.Clean(dataHome) + "\x00" + yard))
	return filepath.Join("/tmp", "subyard-ssh-"+strconv.Itoa(os.Geteuid()), hex.EncodeToString(digest[:12]))
}

func GuestSocket(developer string) string { return "/home/" + developer + "/.ssh/subyard-agent.sock" }

func validateDirectory(dir string, create bool) error {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || len(filepath.Join(dir, "control.sock")) > 100 {
		return errors.New("SSH agent runtime directory must be an absolute, short path")
	}
	parent := filepath.Dir(dir)
	// Inspect existing ancestors before creating anything. In particular, an
	// attacker must not pre-create or replace the per-operator root under /tmp.
	for ancestor := filepath.Dir(parent); ; ancestor = filepath.Dir(ancestor) {
		info, err := os.Lstat(ancestor)
		if err != nil {
			return err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || (int(st.Uid) != os.Geteuid() && st.Uid != 0) || (info.Mode().Perm()&0022 != 0 && info.Mode()&os.ModeSticky == 0) {
			return errors.New("unsafe SSH agent runtime ancestor")
		}
		if ancestor == "/" {
			break
		}
	}
	for _, path := range []string{parent, dir} {
		if create {
			if err := os.Mkdir(path, 0700); err != nil && !os.IsExist(err) {
				return errors.New("cannot create SSH agent runtime directory")
			}
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || info.Mode().Perm() != 0700 || int(st.Uid) != os.Geteuid() {
			return errors.New("SSH agent runtime directory and parent must be owned by the operator with mode 0700")
		}
	}
	return nil
}

func protectedFile(path string, flags int) (*os.File, error) {
	fd, err := unix.Open(path, (flags&^unix.O_TRUNC)|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || int(st.Uid) != os.Geteuid() || st.Nlink != 1 {
		f.Close()
		return nil, errors.New("unsafe SSH agent control file")
	}
	if flags&unix.O_TRUNC != 0 {
		if err := f.Truncate(0); err != nil {
			f.Close()
			return nil, err
		}
	}
	return f, nil
}

func acquireLock(ctx context.Context, path string) (*os.File, error) {
	f, err := protectedFile(path, unix.O_CREAT|unix.O_RDWR)
	if err != nil {
		return nil, err
	}
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (m Manager) Status(ctx context.Context) (Status, error) {
	if err := validateDirectory(m.Config.Directory, false); err != nil {
		if os.IsNotExist(err) {
			return Status{State: "locked"}, nil
		}
		return Status{}, err
	}
	response, err := control(ctx, m.Config.Directory, "status")
	if err != nil {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
			return Status{State: "locked"}, nil
		}
		return Status{}, errors.New("cannot inspect SSH agent worker")
	}
	return response.Status, nil
}

func (m Manager) Lock(ctx context.Context) error {
	if err := validateDirectory(m.Config.Directory, false); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	lock, err := acquireLock(ctx, filepath.Join(m.Config.Directory, "manager.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	return m.stop(ctx)
}

func (m Manager) stop(ctx context.Context) error {
	_, err := control(ctx, m.Config.Directory, "lock")
	if err != nil && !errors.Is(err, syscall.ENOENT) && !errors.Is(err, syscall.ECONNREFUSED) {
		return errors.New("cannot revoke SSH agent worker")
	}
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lock, err := acquireLock(waitCtx, filepath.Join(m.Config.Directory, "worker.lock"))
	if err != nil {
		return errors.New("SSH agent worker did not stop")
	}
	return lock.Close()
}

func readPrivateKey(path string) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0177 != 0 || int(stat.Uid) != os.Geteuid() || stat.Nlink != 1 || info.Size() > 1<<20 {
		return nil, errors.New("SSH private key must be an owner-only regular file of at most 1 MiB")
	}
	return io.ReadAll(io.LimitReader(file, 1<<20))
}

// ValidateKey checks the selected encrypted key without changing local or guest
// state. Unlock repeats this check when it reads the bytes to load.
func ValidateKey(path string) error {
	data, err := readEncryptedPrivateKey(path)
	clear(data)
	return err
}

func readEncryptedPrivateKey(path string) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("SSH key path must be absolute")
	}
	data, err := readPrivateKey(path)
	if err != nil {
		clear(data)
		return nil, errors.New("cannot read selected SSH key")
	}
	_, err = ssh.ParseRawPrivateKey(data)
	var encrypted *ssh.PassphraseMissingError
	if !errors.As(err, &encrypted) {
		clear(data)
		return nil, errors.New("selected SSH key must be an encrypted private key")
	}
	return data, nil
}

func (m Manager) Unlock(ctx context.Context, key string, ttl time.Duration) (Status, error) {
	if ttl < time.Second || ttl%time.Second != 0 || ttl > MaxTTL {
		return Status{}, errors.New("SSH agent TTL must be a whole number of seconds between 1s and 2147483647s")
	}
	if !regexp.MustCompile(`^[a-z_][a-z0-9_-]*[$]?$`).MatchString(m.Config.Developer) || m.Config.SSHPort < 1 || m.Config.SSHPort > 65535 {
		return Status{}, errors.New("invalid SSH agent yard transport")
	}
	data, err := readEncryptedPrivateKey(key)
	if err != nil {
		return Status{}, err
	}
	defer clear(data)
	if err := validateDirectory(m.Config.Directory, true); err != nil {
		return Status{}, err
	}
	lock, err := acquireLock(ctx, filepath.Join(m.Config.Directory, "manager.lock"))
	if err != nil {
		return Status{}, err
	}
	defer lock.Close()
	if err := m.stop(ctx); err != nil {
		return Status{}, err
	}
	for _, name := range []string{"private.sock", "control.sock"} {
		if err := removeSocket(filepath.Join(m.Config.Directory, name)); err != nil {
			return Status{}, err
		}
	}
	cfg := daemonConfig{Config: m.Config, TTL: ttl}
	raw, _ := json.Marshal(cfg)
	file, err := protectedFile(filepath.Join(m.Config.Directory, "worker.json"), unix.O_WRONLY|unix.O_CREAT|unix.O_TRUNC)
	if err != nil {
		return Status{}, err
	}
	_, writeErr := file.Write(raw)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return Status{}, errors.New("cannot prepare SSH agent worker")
	}
	executable := m.Config.Executable
	if executable == "" {
		executable, err = os.Executable()
		if err != nil {
			return Status{}, err
		}
	}
	worker := exec.Command(executable, "_ssh-agent-worker", m.Config.Directory)
	worker.Env = []string{"PATH=/usr/bin:/bin"}
	worker.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := worker.Start(); err != nil {
		return Status{}, errors.New("cannot start SSH agent worker")
	}
	go worker.Wait()
	success := false
	defer func() {
		if !success {
			cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = m.stop(cleanup)
			_ = worker.Process.Kill()
		}
	}()
	readyCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		status, err := m.Status(readyCtx)
		if err == nil && status.State == "pending" {
			break
		}
		select {
		case <-readyCtx.Done():
			return Status{}, errors.New("SSH agent worker did not become ready")
		case <-time.After(20 * time.Millisecond):
		}
	}
	add := exec.CommandContext(ctx, "ssh-add", "-k", "-t", strconv.FormatInt(int64(ttl/time.Second), 10), "-")
	env := m.Environment
	if env == nil {
		env = os.Environ()
	}
	for _, entry := range env {
		if !strings.HasPrefix(entry, "SSH_AUTH_SOCK=") && !strings.HasPrefix(entry, "SSH_AGENT_PID=") {
			add.Env = append(add.Env, entry)
		}
	}
	add.Env = append(add.Env, "SSH_AUTH_SOCK="+filepath.Join(m.Config.Directory, "private.sock"))
	add.Stdin = bytes.NewReader(data)
	add.WaitDelay = 2 * time.Second
	add.Stdout = io.Discard
	add.Stderr = io.Discard
	if err := add.Run(); err != nil {
		return Status{}, errors.New("SSH key unlock failed or was cancelled")
	}
	response, err := control(ctx, m.Config.Directory, "activate")
	if err != nil {
		return Status{}, errors.New("cannot activate SSH agent forwarding")
	}
	if response.Error != "" {
		return Status{}, errors.New(response.Error)
	}
	success = true
	return response.Status, nil
}

func control(ctx context.Context, dir, action string) (controlResponse, error) {
	var result controlResponse
	conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", filepath.Join(dir, "control.sock"))
	if err != nil {
		return result, err
	}
	defer conn.Close()
	deadline := time.Now().Add(15 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	conn.SetDeadline(deadline)
	if err := json.NewEncoder(conn).Encode(controlRequest{Action: action}); err != nil {
		return result, err
	}
	err = json.NewDecoder(io.LimitReader(conn, 4096)).Decode(&result)
	return result, err
}

func removeSocket(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return errors.New("refusing to remove a non-socket SSH agent path")
	}
	return os.Remove(path)
}

type workerState struct {
	mu      sync.Mutex
	expires time.Time
	active  bool
	ready   bool
}

func (w *workerState) status() Status {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.active {
		return Status{State: "pending"}
	}
	seconds := int64(math.Ceil(time.Until(w.expires).Seconds()))
	if seconds < 0 {
		seconds = 0
	}
	state := "reconnecting"
	if w.ready {
		state = "unlocked"
	}
	return Status{State: state, ExpiresAt: w.expires, RemainingSeconds: seconds}
}

// RunDaemon is called only by the hidden worker entrypoint. A fresh, consumed
// owner-only configuration is required; no grant is reconstructed after restart.
func RunDaemon(ctx context.Context, directory string) error {
	if err := validateDirectory(directory, false); err != nil {
		return err
	}
	lockCtx, cancelLock := context.WithTimeout(ctx, time.Second)
	defer cancelLock()
	lock, err := acquireLock(lockCtx, filepath.Join(directory, "worker.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	file, err := protectedFile(filepath.Join(directory, "worker.json"), unix.O_RDONLY)
	if err != nil {
		return errors.New("missing fresh SSH agent worker configuration")
	}
	var cfg daemonConfig
	err = json.NewDecoder(io.LimitReader(file, 16<<10)).Decode(&cfg)
	file.Close()
	if err != nil || cfg.Config.Directory != directory || cfg.TTL < time.Second || cfg.TTL%time.Second != 0 || cfg.TTL > MaxTTL {
		return errors.New("invalid SSH agent worker configuration")
	}
	if err := os.Remove(filepath.Join(directory, "worker.json")); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	privateSocket := filepath.Join(directory, "private.sock")
	if err := removeSocket(privateSocket); err != nil {
		return err
	}
	// Debian ships ssh-agent setgid. A privilege-changing exec clears
	// Pdeathsig, so prohibit privilege gain before starting our child. Keep
	// its creator thread alive: Linux ties the death signal to that thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return errors.New("cannot constrain isolated SSH agent privileges")
	}
	child := exec.Command("ssh-agent", "-D", "-a", privateSocket)
	child.Env = []string{"PATH=/usr/bin:/bin"}
	child.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := child.Start(); err != nil {
		return errors.New("cannot start isolated OpenSSH agent")
	}
	childDone := make(chan struct{})
	go func() { _ = child.Wait(); close(childDone); cancel() }()
	defer func() { _ = child.Process.Kill(); <-childDone; _ = removeSocket(privateSocket) }()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		conn, err := net.DialTimeout("unix", privateSocket, 50*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		select {
		case <-runCtx.Done():
			return errors.New("isolated SSH agent stopped")
		case <-deadline.C:
			return errors.New("isolated SSH agent did not start")
		case <-time.After(10 * time.Millisecond):
		}
	}
	controlPath := filepath.Join(directory, "control.sock")
	if err := removeSocket(controlPath); err != nil {
		return err
	}
	listener, err := net.Listen("unix", controlPath)
	if err != nil {
		return err
	}
	defer listener.Close()
	defer removeSocket(controlPath)
	if err := os.Chmod(controlPath, 0600); err != nil {
		return err
	}
	state := &workerState{}
	pending := time.AfterFunc(5*time.Minute, cancel)
	defer pending.Stop()
	var forwarding sync.WaitGroup
	var grantTimer *time.Timer
	defer func() {
		if grantTimer != nil {
			grantTimer.Stop()
		}
	}()
	go func() { <-runCtx.Done(); listener.Close() }()
	for {
		conn, err := listener.Accept()
		if err != nil {
			break
		}
		// Control clients are trusted local operators; bounded serial handling keeps
		// activation atomic and avoids overlapping reverse listeners.
		func() {
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(15 * time.Second))
			var req controlRequest
			if json.NewDecoder(io.LimitReader(conn, 4096)).Decode(&req) != nil {
				return
			}
			response := controlResponse{}
			switch req.Action {
			case "status":
				response.Status = state.status()
				if response.Status.State != "pending" && !hasOneIdentity(privateSocket) {
					response.Status = Status{State: "locked"}
				}
			case "lock":
				response.Status = Status{State: "locked"}
				defer cancel()
			case "activate":
				if !hasOneIdentity(privateSocket) {
					response.Error = "SSH agent requires exactly one loaded identity"
					defer cancel()
					break
				}
				state.mu.Lock()
				active := state.active
				if !active {
					state.active = true
					state.expires = time.Now().Add(cfg.TTL)
				}
				state.mu.Unlock()
				if active {
					response.Error = "SSH agent grant is already active"
					break
				}
				pending.Stop()
				grantTimer = time.AfterFunc(cfg.TTL, cancel)
				ready := make(chan error, 1)
				forwarding.Add(1)
				go func() {
					defer forwarding.Done()
					defer cancel()
					forwardGrant(runCtx, cfg.Config, privateSocket, state, ready)
				}()
				select {
				case err := <-ready:
					if err != nil {
						response.Error = "cannot establish pinned SSH agent transport"
						cancel()
					}
				case <-runCtx.Done():
					response.Error = "SSH agent grant expired or was cancelled"
				}
				response.Status = state.status()
			default:
				response.Error = "unsupported SSH agent control request"
			}
			_ = json.NewEncoder(conn).Encode(response)
		}()
	}
	cancel()
	forwarding.Wait()
	return nil
}

func hasOneIdentity(socket string) bool {
	conn, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		return false
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(time.Second))
	identities, err := agent.NewClient(conn).List()
	return err == nil && len(identities) == 1
}
