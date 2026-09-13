// Package sshagentruntime manages one expiring, owner-host SSH agent per yard.
// Private keys are read only by ssh-add, after the caller confirms the action.
package sshagentruntime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"golang.org/x/sys/unix"
)

const maxTTL = 30 * 24 * time.Hour
const guestSocket = "~/.subyard/run/ssh-agent.sock"

type Config struct {
	StateRoot, Yard, SSHHost, OperatorHome, Dispatcher, DataHome, DevUser string
	SSHPort                                                               int
	Environment                                                           []string
	Stdin                                                                 io.Reader
	Stdout, Stderr                                                        io.Writer
}

type Runtime struct {
	cfg                         Config
	env                         []string
	directory, runtimeDir, unit string
}

type invocation struct {
	verb, key string
	ttl       time.Duration
	json      bool
}

type Prepared struct {
	Action       domain.ActionID
	Changed      bool
	Consequences []string
	runtime      *Runtime
	request      invocation
	keyInfo      os.FileInfo
}

type session struct {
	Yard         string    `json:"yard"`
	ExpiresAt    time.Time `json:"expires_at"`
	Token        string    `json:"token"`
	RuntimeDir   string    `json:"runtime_dir"`
	DataHome     string    `json:"data_home"`
	OperatorHome string    `json:"operator_home"`
	SSHPort      int       `json:"ssh_port"`
	DevUser      string    `json:"dev_user"`
	Stopped      bool      `json:"stopped,omitempty"`
}

type Status struct {
	Yard      string     `json:"yard"`
	State     string     `json:"state"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

func New(cfg Config) (*Runtime, error) {
	if cfg.Yard == "" {
		cfg.Yard = "default"
	}
	if !domain.SafeName(cfg.Yard) || !filepath.IsAbs(cfg.StateRoot) || !filepath.IsAbs(cfg.OperatorHome) {
		return nil, errors.New("invalid owner SSH-agent context")
	}
	if cfg.Stdout == nil {
		cfg.Stdout = io.Discard
	}
	if cfg.Stderr == nil {
		cfg.Stderr = io.Discard
	}
	if cfg.Stdin == nil {
		cfg.Stdin = strings.NewReader("")
	}
	env := make(map[string]string)
	for _, item := range cfg.Environment {
		if k, v, ok := strings.Cut(item, "="); ok {
			env[k] = v
		}
	}
	runtimeRoot := env["XDG_RUNTIME_DIR"]
	if runtimeRoot == "" {
		runtimeRoot = fmt.Sprintf("/run/user/%d", os.Geteuid())
	}
	if !filepath.IsAbs(runtimeRoot) {
		return nil, errors.New("XDG_RUNTIME_DIR must be absolute")
	}
	env["HOME"], env["XDG_RUNTIME_DIR"] = cfg.OperatorHome, runtimeRoot
	// Use this user's manager, never an inherited SSH-forwarded bus or agent.
	env["DBUS_SESSION_BUS_ADDRESS"] = "unix:path=" + filepath.Join(runtimeRoot, "bus")
	delete(env, "SSH_AUTH_SOCK")
	delete(env, "SSH_AGENT_PID")
	env["SSH_ASKPASS_REQUIRE"] = "never"
	directory := filepath.Join(cfg.StateRoot, cfg.Yard)
	hash := sha256.Sum256([]byte(directory))
	id := hex.EncodeToString(hash[:12])
	runtimeDir := filepath.Join(runtimeRoot, "subyard-ssh-agent-"+id)
	if len(filepath.Join(runtimeDir, "agent.sock")) >= 104 {
		return nil, errors.New("SSH-agent runtime socket path is too long")
	}
	r := &Runtime{cfg: cfg, directory: directory, runtimeDir: runtimeDir, unit: "subyard-ssh-agent-" + id + ".service"}
	for k, v := range env {
		r.env = append(r.env, k+"="+v)
	}
	return r, nil
}

func parse(arguments []string) (invocation, error) {
	req := invocation{verb: "help"}
	if len(arguments) == 0 {
		return req, nil
	}
	req.verb = arguments[0]
	if req.verb == "--help" || req.verb == "-h" {
		req.verb = "help"
	}
	seen := map[string]bool{}
	for i := 1; i < len(arguments); i++ {
		option := arguments[i]
		if option == "--help" || option == "-h" {
			return invocation{verb: "help"}, nil
		}
		if seen[option] {
			return req, fmt.Errorf("duplicate SSH-agent option %s", option)
		}
		seen[option] = true
		switch option {
		case "--json":
			req.json = true
		case "--key", "--ttl":
			if i+1 == len(arguments) {
				return req, fmt.Errorf("%s requires a value", option)
			}
			i++
			if option == "--key" {
				req.key = arguments[i]
			} else {
				duration := arguments[i]
				if strings.HasSuffix(duration, "d") {
					days, err := strconv.ParseUint(strings.TrimSuffix(duration, "d"), 10, 8)
					if err != nil || days > 30 {
						return req, errors.New("TTL must be between 1s and 30d")
					}
					req.ttl = time.Duration(days) * 24 * time.Hour
				} else {
					var err error
					req.ttl, err = time.ParseDuration(duration)
					if err != nil {
						return req, errors.New("invalid TTL; use a duration such as 30m, 20h or 14d")
					}
				}
			}
		default:
			return req, fmt.Errorf("unknown SSH-agent option %s", option)
		}
	}
	switch req.verb {
	case "help":
	case "status":
		if seen["--key"] || seen["--ttl"] {
			return req, errors.New("status accepts only --json")
		}
	case "stop":
		if len(seen) > 0 {
			return req, errors.New("stop takes no options")
		}
	case "start":
		if req.key == "" || req.ttl < time.Second || req.ttl > maxTTL || req.json {
			return req, errors.New("start requires --key PATH --ttl DURATION (1s to 30d)")
		}
		var err error
		req.key, err = filepath.Abs(req.key)
		if err != nil {
			return req, errors.New("invalid key path")
		}
	default:
		return req, errors.New("expected ssh-agent start, status or stop")
	}
	return req, nil
}

func (r *Runtime) Prepare(ctx context.Context, arguments []string) (Prepared, error) {
	req, err := parse(arguments)
	p := Prepared{runtime: r, request: req, Action: domain.ActionID("ssh-agent." + req.verb)}
	if err != nil {
		return p, err
	}
	if req.verb == "help" {
		return p, nil
	}
	if err = privateDirectory(r.cfg.StateRoot, false); err != nil {
		return p, err
	}
	if err = privateDirectory(r.directory, false); err != nil {
		return p, err
	}
	status, err := r.status(ctx)
	if err != nil {
		return p, err
	}
	switch req.verb {
	case "start":
		if status.State == "active" || status.State == "connecting" || status.State == "loading" {
			return p, errors.New("SSH-agent access already exists; use status or stop before starting again")
		}
		if err = r.preflight(ctx); err != nil {
			return p, err
		}
		p.keyInfo, err = keyMetadata(req.key)
		if err != nil {
			return p, err
		}
		p.Changed = true
		p.Consequences = []string{
			fmt.Sprintf("Allow every process of %s in yard %s to authenticate using the selected key for at most %s.", r.cfg.DevUser, r.cfg.Yard, req.ttl),
			"Keep the private key on this owner host; stopping access does not terminate previously authenticated connections.",
		}
	case "stop":
		p.Changed = status.State == "active" || status.State == "connecting" || status.State == "loading"
		p.Consequences = []string{"Stop this yard's dedicated SSH agent and deny new authentications using it."}
	}
	return p, nil
}

func (r *Runtime) preflight(ctx context.Context) error {
	if !domain.SafeName(r.cfg.DevUser) || r.cfg.SSHPort < 1 || r.cfg.SSHPort > 65535 || !filepath.IsAbs(r.cfg.DataHome) || !filepath.IsAbs(r.cfg.Dispatcher) {
		return errors.New("SSH-agent access requires a configured local owner yard; run yard init on its owner host")
	}
	for _, name := range []string{"ssh", "ssh-add", "ssh-agent", "systemctl", "systemd-run", "loginctl"} {
		if _, err := r.program(name); err != nil {
			return fmt.Errorf("%s is required on the owner host", name)
		}
	}
	// Possession of the dedicated owner login is required. Guest coding agents do
	// not receive it; neither the key path nor its contents travel through RPC.
	for _, path := range []string{filepath.Join(r.cfg.DataHome, "ssh", "id_ed25519"), filepath.Join(r.cfg.DataHome, "ssh", "known_hosts")} {
		if _, err := keyMetadata(path); err != nil {
			return errors.New("dedicated yard SSH login is unavailable; run yard init on the owner host")
		}
	}
	if err := privateDirectory(r.envValue("XDG_RUNTIME_DIR"), false); err != nil {
		return err
	}
	probe, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := r.output(probe, "systemctl", "--user", "show", "--property=Version", "--value"); err != nil {
		return errors.New("owner systemd user manager is unavailable; run yard init on the owner host")
	}
	output, err := r.output(probe, "loginctl", "show-user", strconv.Itoa(os.Geteuid()), "--property=Linger", "--value")
	if err != nil || strings.TrimSpace(string(output)) != "yes" {
		return errors.New("owner user lingering is required to retain access after logout; run yard init on the owner host")
	}
	return nil
}

func (p Prepared) Execute(ctx context.Context) error {
	r := p.runtime
	switch p.request.verb {
	case "help":
		fmt.Fprintln(r.cfg.Stdout, "Usage: yard [-Y NAME] ssh-agent start --key PATH --ttl DURATION\n       yard [-Y NAME] ssh-agent status [--json]\n       yard [-Y NAME] ssh-agent stop\n\nRun on the yard's owner host. DURATION is 1s to 30d. Start/stop accept --yes.\nThe selected key remains on the owner; all dev sessions share its permissions.\nNo key is reloaded after an owner reboot. Reconnect never extends the deadline.")
		return nil
	case "status":
		status, err := r.status(ctx)
		if err != nil {
			return err
		}
		return r.printStatus(status, p.request.json)
	}
	lock, err := r.lock()
	if err != nil {
		return err
	}
	defer lock.Close()
	if p.request.verb == "stop" {
		return r.stop(ctx)
	}
	if err = r.preflight(ctx); err != nil {
		return err
	}
	status, err := r.status(ctx)
	if err != nil {
		return err
	}
	if status.State == "active" || status.State == "connecting" || status.State == "loading" {
		return errors.New("SSH-agent access changed after assessment; inspect status before retrying")
	}
	return r.start(ctx, p)
}

func (r *Runtime) start(ctx context.Context, p Prepared) (err error) {
	// O_NOFOLLOW and SameFile bind the post-consent read to the assessed inode.
	fd, err := unix.Open(p.request.key, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return errors.New("cannot open the selected key")
	}
	key := os.NewFile(uintptr(fd), "selected SSH key")
	defer key.Close()
	info, err := key.Stat()
	if err != nil || !safeFileInfo(info) || !os.SameFile(p.keyInfo, info) {
		return errors.New("selected key changed after assessment")
	}
	if err = privateDirectory(r.runtimeDir, true); err != nil {
		return err
	}
	// Stop the previous, expired instance before reusing its runtime paths.
	if err = r.stopUnit(ctx); err != nil {
		return err
	}
	for _, name := range []string{"agent.sock", "ready", "connection"} {
		if err = removeOwned(filepath.Join(r.runtimeDir, name)); err != nil {
			return err
		}
	}
	token := make([]byte, 16)
	if _, err = rand.Read(token); err != nil {
		return err
	}
	s := session{Yard: r.cfg.Yard, ExpiresAt: time.Now().UTC().Add(p.request.ttl), Token: hex.EncodeToString(token), RuntimeDir: r.runtimeDir, DataHome: r.cfg.DataHome, OperatorHome: r.cfg.OperatorHome, SSHPort: r.cfg.SSHPort, DevUser: r.cfg.DevUser}
	if err = writeJSON(filepath.Join(r.directory, "session.json"), s); err != nil {
		return err
	}
	started := false
	defer func() {
		if err == nil {
			return
		}
		if started {
			cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			cleanupErr := r.stopUnit(cleanup)
			cancel()
			if cleanupErr != nil {
				err = errors.Join(err, cleanupErr)
				return
			}
		}
		s.Stopped = true
		if stateErr := writeJSON(filepath.Join(r.directory, "session.json"), s); stateErr != nil {
			err = errors.Join(err, stateErr)
		}
	}()
	args := []string{"--user", "--quiet", "--collect", "--unit=" + r.unit,
		"--property=Type=exec", "--property=KillMode=control-group", "--property=TimeoutStopSec=5s",
		"--property=UMask=0077", "--property=StandardOutput=null", "--property=StandardError=null",
		fmt.Sprintf("--property=RuntimeMaxSec=%ds", int64(math.Ceil(p.request.ttl.Seconds()))),
		"--setenv=HOME=" + r.cfg.OperatorHome, "--setenv=PATH=/usr/local/bin:/usr/bin:/bin",
		"--", r.cfg.Dispatcher, "_ssh-agent-worker", filepath.Join(r.directory, "session.json")}
	started = true
	if _, err = r.output(ctx, "systemd-run", args...); err != nil {
		return errors.New("could not start dedicated SSH-agent user service")
	}
	setup, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err = waitUntil(setup, func() bool { return ownedSocket(filepath.Join(r.runtimeDir, "agent.sock")) }); err != nil {
		return errors.New("dedicated SSH agent did not become ready")
	}
	remaining := time.Until(s.ExpiresAt)
	if remaining <= 0 {
		return errors.New("SSH-agent access expired during startup")
	}
	loadContext, loadCancel := context.WithDeadline(ctx, s.ExpiresAt)
	defer loadCancel()
	command, err := r.command(loadContext, "ssh-add", "-q", "-t", strconv.FormatInt(int64(math.Ceil(remaining.Seconds())), 10), "/proc/self/fd/3")
	if err != nil {
		return err
	}
	command.ExtraFiles = []*os.File{key}
	command.Env = append(command.Env, "SSH_AUTH_SOCK="+filepath.Join(r.runtimeDir, "agent.sock"))
	command.Stdin, command.Stdout, command.Stderr = r.cfg.Stdin, io.Discard, r.cfg.Stderr
	if err = command.Run(); err != nil {
		return errors.New("could not load selected key; SSH-agent access was stopped")
	}
	if !time.Now().Before(s.ExpiresAt) {
		return errors.New("SSH-agent access expired while loading the key")
	}
	if err = writeJSON(filepath.Join(r.runtimeDir, "ready"), s.Token); err != nil {
		return err
	}
	connect, connectCancel := context.WithTimeout(ctx, 20*time.Second)
	defer connectCancel()
	if err = waitUntil(connect, func() bool {
		var connection string
		return readJSON(filepath.Join(r.runtimeDir, "connection"), &connection) == nil && connection == s.Token
	}); err != nil {
		return errors.New("yard SSH-agent connection failed; check yard SSH access and run yard init to install its shared-agent configuration")
	}
	return r.printStatus(Status{Yard: s.Yard, State: "active", ExpiresAt: &s.ExpiresAt}, false)
}

func (r *Runtime) status(ctx context.Context) (Status, error) {
	status := Status{Yard: r.cfg.Yard, State: "stopped"}
	var s session
	if err := readJSON(filepath.Join(r.directory, "session.json"), &s); errors.Is(err, os.ErrNotExist) {
		return status, nil
	} else if err != nil {
		return status, err
	}
	if s.Yard != r.cfg.Yard || s.RuntimeDir != r.runtimeDir {
		return status, errors.New("SSH-agent state does not match this owner and yard")
	}
	status.ExpiresAt = &s.ExpiresAt
	if s.Stopped {
		return status, nil
	}
	if !time.Now().Before(s.ExpiresAt) {
		status.State = "expired"
		return status, nil
	}
	active, err := r.unitActive(ctx)
	if err != nil {
		return status, err
	}
	if !active {
		return status, nil
	}
	status.State = "connecting"
	var ready, connection string
	if readJSON(filepath.Join(r.runtimeDir, "ready"), &ready) != nil || ready != s.Token {
		status.State = "loading"
	} else if readJSON(filepath.Join(r.runtimeDir, "connection"), &connection) == nil && connection == s.Token {
		status.State = "active"
	}
	return status, nil
}

func (r *Runtime) stop(ctx context.Context) error {
	var s session
	if err := readJSON(filepath.Join(r.directory, "session.json"), &s); errors.Is(err, os.ErrNotExist) {
		return r.printStatus(Status{Yard: r.cfg.Yard, State: "stopped"}, false)
	} else if err != nil {
		return err
	}
	if s.Yard != r.cfg.Yard || s.RuntimeDir != r.runtimeDir {
		return errors.New("SSH-agent state does not match this owner and yard")
	}
	if err := r.stopUnit(ctx); err != nil {
		return err
	}
	s.Stopped = true
	if err := writeJSON(filepath.Join(r.directory, "session.json"), s); err != nil {
		return err
	}
	return r.printStatus(Status{Yard: r.cfg.Yard, State: "stopped", ExpiresAt: &s.ExpiresAt}, false)
}

func (r *Runtime) printStatus(status Status, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(r.cfg.Stdout).Encode(status)
	}
	if status.ExpiresAt == nil {
		fmt.Fprintf(r.cfg.Stdout, "yard=%s ssh-agent=%s\n", status.Yard, status.State)
	} else {
		fmt.Fprintf(r.cfg.Stdout, "yard=%s ssh-agent=%s expires=%s\n", status.Yard, status.State, status.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return nil
}

func (r *Runtime) unitActive(ctx context.Context) (bool, error) {
	out, err := r.output(ctx, "systemctl", "--user", "show", r.unit, "--property=LoadState", "--property=ActiveState")
	if err != nil {
		return false, errors.New("cannot inspect owner SSH-agent service; check the systemd user manager")
	}
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || (key != "LoadState" && key != "ActiveState") || values[key] != "" {
			return false, errors.New("invalid SSH-agent service state")
		}
		values[key] = value
	}
	if values["LoadState"] == "" || values["ActiveState"] == "" {
		return false, errors.New("incomplete SSH-agent service state")
	}
	switch values["ActiveState"] {
	case "inactive", "failed":
		return false, nil
	case "active", "activating", "deactivating", "reloading":
		return true, nil
	default:
		return false, errors.New("unknown SSH-agent service state")
	}
}

func (r *Runtime) stopUnit(ctx context.Context) error {
	_, stopErr := r.output(ctx, "systemctl", "--user", "stop", r.unit)
	active, inspectErr := r.unitActive(ctx)
	if inspectErr != nil {
		return errors.New("cannot verify SSH-agent revocation; check the owner systemd user manager")
	}
	if active {
		return errors.New("SSH-agent service is still running")
	}
	// Stopping an unloaded transient unit may fail; a verified inactive unit is safe.
	_ = stopErr
	return nil
}

func (r *Runtime) lock() (*os.File, error) {
	for _, dir := range []string{r.cfg.StateRoot, r.directory} {
		if err := privateDirectory(dir, true); err != nil {
			return nil, err
		}
	}
	fd, err := unix.Open(filepath.Join(r.directory, "lock"), unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, errors.New("cannot open SSH-agent state lock")
	}
	file := os.NewFile(uintptr(fd), "SSH-agent lock")
	if err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		return nil, errors.New("another SSH-agent operation is in progress")
	}
	return file, nil
}

func privateDirectory(path string, create bool) error {
	if create {
		if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return errors.New("cannot create private SSH-agent directory")
		}
	}
	info, err := os.Lstat(path)
	if !create && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 || !owned(info) {
		return errors.New("SSH-agent directory must be owned by the operator, mode 0700, and not a symlink")
	}
	return nil
}

func owned(info os.FileInfo) bool {
	if info == nil {
		return false
	}
	s, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(s.Uid) == os.Geteuid()
}
func safeFileInfo(info os.FileInfo) bool {
	return info != nil && info.Mode().IsRegular() && (info.Mode().Perm() == 0600 || info.Mode().Perm() == 0400) && owned(info)
}
func keyMetadata(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil || !safeFileInfo(info) {
		return nil, errors.New("selected key must be an operator-owned regular non-symlink file with mode 0600 or 0400")
	}
	return info, nil
}
func ownedSocket(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSocket != 0 && owned(info)
}
func removeOwned(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !owned(info) || info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("unsafe SSH-agent runtime entry")
	}
	return os.Remove(path)
}

func writeJSON(path string, value any) error {
	if info, err := os.Lstat(path); err == nil && !safeFileInfo(info) {
		return errors.New("unsafe SSH-agent state file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".ssh-agent-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	err = json.NewEncoder(file).Encode(value)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(file.Name(), path)
	}
	return err
}
func readJSON(path string, value any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !safeFileInfo(info) || info.Size() > 8192 {
		return errors.New("unsafe SSH-agent state file")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 8193))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(value); err != nil {
		return errors.New("invalid SSH-agent state")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("invalid SSH-agent state trailer")
	}
	return nil
}
func (r *Runtime) envValue(key string) string {
	for _, item := range r.env {
		if v, ok := strings.CutPrefix(item, key+"="); ok {
			return v
		}
	}
	return ""
}
func (r *Runtime) program(name string) (string, error) {
	for _, dir := range filepath.SplitList(r.envValue("PATH")) {
		if dir == "" {
			continue
		}
		path := filepath.Join(dir, name)
		if info, err := os.Stat(path); err == nil && !info.IsDir() && info.Mode()&0111 != 0 {
			return path, nil
		}
	}
	return "", fmt.Errorf("%s is unavailable", name)
}
func (r *Runtime) command(ctx context.Context, name string, args ...string) (*exec.Cmd, error) {
	path, err := r.program(name)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = r.env
	return cmd, nil
}
func (r *Runtime) output(ctx context.Context, name string, args ...string) ([]byte, error) {
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd, err := r.command(bounded, name, args...)
	if err != nil {
		return nil, err
	}
	return cmd.Output()
}
func waitUntil(ctx context.Context, check func() bool) error {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if check() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Inspect observes this owner's grant without creating files or contacting the guest.
func (r *Runtime) Inspect(ctx context.Context) (Status, error) {
	for _, directory := range []string{r.cfg.StateRoot, r.directory} {
		if err := privateDirectory(directory, false); err != nil {
			return Status{}, err
		}
	}
	return r.status(ctx)
}

// Revoke is used by the already-confirmed teardown workflow. Absence is a no-op.
func (r *Runtime) Revoke(ctx context.Context) error {
	if _, err := os.Lstat(filepath.Join(r.directory, "session.json")); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	lock, err := r.lock()
	if err != nil {
		return err
	}
	defer lock.Close()
	return r.stop(ctx)
}
