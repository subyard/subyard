package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// One configuration command shares one OpenSSH control master per SSH
// endpoint across its plan, guard and apply phases, so a key without an agent
// is unlocked once.
//
// Git's first real connection starts the master (ControlMaster=auto), so the
// operator's configuration, including Match command blocks, applies to that
// authentication. ControlPersist=yes keeps it for the whole command, however
// long the confirmation waits.
//
// After a Git command, each new master reports its own PID over its socket
// (ssh -O check). The process is pinned by a pidfd and accepted only while it
// is this user's process titled exactly "ssh: <socket> [mux]". A watchdog,
// this executable re-run as a hidden helper, reads a pipe whose only writer is
// this process and pins each reported master the same way. When the command
// closes or this process dies, even by SIGKILL, the pipe reaches EOF and the
// watchdog stops the masters (configGitSSHStop) and removes the private
// directory; a normal close then repeats the stop for anything left. Signals
// go only through pidfds of verified masters and through the exit requests'
// own process handles. A master that never verified, or one left when both
// this process and the watchdog are killed by SIGKILL, survives until it is
// stopped manually.
const (
	configGitSSHPrefix  = "sy-git-ssh-"
	configGitSSHTimeout = 10 * time.Second
	// A Unix socket path has 104 bytes on BSD (108 on Linux), including the
	// terminating NUL. OpenSSH binds "<ControlPath>.<16 random characters>"
	// before renaming it, and %C expands to 40 hexadecimal characters.
	configGitSSHSocketLimit = 103
	configGitSSHSocketExtra = len("/") + 40 + len(".0123456789abcdef")
	// A dead connection fails after about three missed replies.
	configGitSSHAliveInterval = "15"
	// ConfigGitSSHWatchdogVerb is the hidden engine verb of the watchdog.
	ConfigGitSSHWatchdogVerb = "_config-git-ssh-watchdog"
)

var (
	// configGitSSHExitWait bounds the orderly exit of the masters before they
	// are terminated.
	configGitSSHExitWait = 5 * time.Second
	// configGitSSHCloseTimeout bounds the wait for the watchdog itself.
	configGitSSHCloseTimeout = 2 * configGitSSHTimeout
	// configGitSSHWatchdogRegistered records that this executable dispatches
	// ConfigGitSSHWatchdogVerb, so re-running it starts the watchdog.
	configGitSSHWatchdogRegistered bool
)

// RegisterConfigGitSSHWatchdog declares that this executable dispatches the
// watchdog verb. Other programs linking this package keep shared SSH with only
// the normal-close cleanup, instead of re-running themselves.
func RegisterConfigGitSSHWatchdog() { configGitSSHWatchdogRegistered = true }

// configGitSSHRemote is one effective Git URL and the remote program Git runs on it.
type configGitSSHRemote struct {
	url, program string
}

type configGitSSHSession struct {
	directory string
	command   string
	release   *os.File
	watchdog  *exec.Cmd
	ssh       string
	env       []string
	done      chan struct{}
	once      sync.Once
	mu        sync.Mutex
	closed    bool
	// masters maps each verified control socket to a pidfd of its master;
	// probed holds every socket already asked for its master.
	masters map[string]int
	probed  map[string]bool
}

// shareConfigGitSSH starts the command-scoped SSH sharing for the effective
// remotes when the operator has not chosen an SSH command or multiplexing of
// their own. It returns an idempotent release that closes every master and
// removes the private directory. Any failed probe keeps Git's previous
// per-connection behavior.
func (cli *CLI) shareConfigGitSSH(ctx context.Context, directory string, remotes []configGitSSHRemote) func() {
	if cli.configGitSSH != nil {
		return func() {}
	}
	session := cli.startConfigGitSSH(ctx, directory, remotes)
	if session == nil {
		return func() {}
	}
	cli.configGitSSH = session
	return func() {
		if cli.configGitSSH == session {
			cli.configGitSSH = nil
		}
		session.close()
	}
}

func (cli *CLI) startConfigGitSSH(ctx context.Context, directory string, remotes []configGitSSHRemote) *configGitSSHSession {
	var endpoints []configGitSSHEndpoint
	for _, remote := range remotes {
		if endpoint, ok := configGitSSHEndpointFor(remote.url, remote.program); ok {
			endpoints = append(endpoints, endpoint)
		}
	}
	if len(endpoints) == 0 {
		return nil
	}
	ssh := lookPathIn("ssh", cli.env["PATH"])
	if ssh == "" || !configGitSSHSafePath(ssh) {
		return nil
	}
	if cli.configGitSSHOverridden(ctx, directory) {
		return nil
	}
	shared, keepAlive := cli.configGitSSHUnmultiplexed(ctx, ssh, endpoints)
	if !shared {
		return nil
	}
	private := configGitSSHDirectory()
	if private == "" {
		return nil
	}
	// Both paths are restricted to shell-safe characters.
	command := []string{ssh, "-o", "ControlMaster=auto", "-o", "ControlPath=" + filepath.Join(private, "%C"), "-o", "ControlPersist=yes"}
	if keepAlive {
		// A persistent master idles during confirmation; detect a dropped
		// connection unless the operator chose their own keepalive.
		command = append(command, "-o", "ServerAliveInterval="+configGitSSHAliveInterval)
	}
	session := &configGitSSHSession{directory: private, done: make(chan struct{}), command: strings.Join(command, " "),
		ssh: ssh, env: environmentList(cli.env, nil), masters: map[string]int{}, probed: map[string]bool{}}
	if !configGitSSHWatchdogRegistered {
		close(session.done)
		return session
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		_ = os.Remove(private)
		return nil
	}
	// /proc/self/exe remains this engine even if its installed file is replaced.
	watchdog := exec.Command("/proc/self/exe", ConfigGitSSHWatchdogVerb, ssh, private, configGitSSHExitWait.String())
	watchdog.Env = session.env
	watchdog.Stdin = reader
	watchdog.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if stderr, ok := cli.options.Stderr.(*os.File); ok {
		watchdog.Stderr = stderr
	}
	err = watchdog.Start()
	_ = reader.Close()
	if err != nil {
		_ = writer.Close()
		_ = os.Remove(private)
		return nil
	}
	session.release, session.watchdog = writer, watchdog
	go func() { _ = watchdog.Wait(); close(session.done) }()
	return session
}

// environment returns the per-invocation Git SSH command while the session is open.
func (session *configGitSSHSession) environment() (string, bool) {
	if session == nil {
		return "", false
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.command, !session.closed
}

// record identifies each new master once, after the Git command that started
// it while it can still answer on its socket, and passes it to the watchdog.
func (session *configGitSSHSession) record() {
	if session == nil {
		return
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed {
		return
	}
	entries, _ := os.ReadDir(session.directory)
	for _, entry := range entries {
		socket := filepath.Join(session.directory, entry.Name())
		if session.probed[socket] {
			continue
		}
		session.probed[socket] = true
		ctx, cancel := context.WithTimeout(context.Background(), configGitSSHTimeout)
		command := exec.CommandContext(ctx, session.ssh, "-F", "none", "-o", "ControlPath="+socket, "-O", "check", "subyard-config-git")
		command.Env = session.env
		command.WaitDelay = configGitWaitDelay
		output, _ := command.CombinedOutput()
		cancel()
		match := configGitSSHMasterPID.FindSubmatch(output)
		if match == nil {
			continue
		}
		pid, err := strconv.Atoi(string(match[1]))
		if err != nil {
			continue
		}
		// A new master may set its process title just after it answers.
		for deadline := time.Now().Add(time.Second); ; time.Sleep(20 * time.Millisecond) {
			if fd, ok := pinConfigGitSSHMaster(pid, socket); ok {
				session.masters[socket] = fd
				if session.release != nil {
					_, _ = fmt.Fprintf(session.release, "%d %s\n", pid, socket)
				}
				break
			}
			if time.Now().After(deadline) {
				break
			}
		}
	}
}

var configGitSSHMasterPID = regexp.MustCompile(`Master running \(pid=([0-9]+)\)`)

// pinConfigGitSSHMaster opens a pidfd for pid and returns it only if the
// pinned process is this user's process titled exactly as the master of
// socket. The process is still running after the check, so its PID was not
// reused during it and the identity belongs to the pidfd.
func pinConfigGitSSHMaster(pid int, socket string) (int, bool) {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return 0, false
	}
	process := "/proc/" + strconv.Itoa(pid)
	if info, err := os.Stat(process); err == nil && info.Sys().(*syscall.Stat_t).Uid == uint32(os.Getuid()) {
		cmdline, err := os.ReadFile(process + "/cmdline")
		title, _, _ := bytes.Cut(cmdline, []byte{0})
		if err == nil && string(bytes.TrimRight(title, " ")) == "ssh: "+socket+" [mux]" &&
			unix.PidfdSendSignal(fd, 0, nil, 0) == nil {
			return fd, true
		}
	}
	_ = unix.Close(fd)
	return 0, false
}

func (session *configGitSSHSession) close() {
	session.once.Do(func() {
		session.mu.Lock()
		session.closed = true
		session.mu.Unlock()
		if session.release != nil {
			_ = session.release.Close()
			select {
			case <-session.done:
			case <-time.After(configGitSSHCloseTimeout):
				_ = session.watchdog.Process.Kill()
				<-session.done
			}
		}
		_ = configGitSSHStop(session.ssh, session.env, session.directory, session.masters, configGitSSHExitWait)
	})
}

// RunConfigGitSSHWatchdog is the hidden watchdog verb: SSH DIRECTORY WAIT,
// with "PID SOCKET" records on standard input until EOF. It accepts only a
// pipe as input and only a private session directory: a real directory owned
// by this user with mode 0700. It never removes anything but sockets.
func RunConfigGitSSHWatchdog(arguments []string, input *os.File) int {
	if len(arguments) != 3 || !configGitSSHSafePath(arguments[0]) || !configGitSSHSafePath(arguments[1]) ||
		!strings.HasPrefix(filepath.Base(arguments[1]), configGitSSHPrefix) {
		return 2
	}
	wait, err := time.ParseDuration(arguments[2])
	if err != nil {
		return 2
	}
	if info, err := input.Stat(); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		return 2
	}
	info, err := os.Lstat(arguments[1])
	if err != nil || !info.IsDir() || info.Mode()&(os.ModeType|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != os.ModeDir ||
		info.Mode().Perm() != 0o700 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		return 2
	}
	signal.Ignore(syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)
	masters := map[string]int{}
	scanner := bufio.NewScanner(input)
	for scanner.Scan() {
		var pid int
		var socket string
		if _, err := fmt.Sscan(scanner.Text(), &pid, &socket); err != nil || filepath.Dir(socket) != arguments[1] {
			continue
		}
		if fd, ok := pinConfigGitSSHMaster(pid, socket); ok {
			masters[socket] = fd
		}
	}
	if err := configGitSSHStop(arguments[0], os.Environ(), arguments[1], masters, wait); err != nil {
		fmt.Fprintf(os.Stderr, "config Git SSH: %v\n", err)
		return 1
	}
	return 0
}

// configGitSSHStop requests a bounded exit from every master socket, then
// terminates the pinned masters that are still running, and removes the
// sockets and the then empty private directory. Exit requests are signalled
// only through their own process handles and masters only through their
// pidfds. A directory with any other entry is left in place.
func configGitSSHStop(ssh string, environment []string, directory string, masters map[string]int, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		entries = nil
	}
	var requests []*exec.Cmd
	for _, entry := range entries {
		if entry.Type()&os.ModeSocket == 0 {
			continue
		}
		request := exec.CommandContext(ctx, ssh, "-F", "none", "-o", "ControlPath="+filepath.Join(directory, entry.Name()), "-O", "exit", "subyard-config-git")
		request.Env = environment
		if request.Start() == nil {
			requests = append(requests, request)
		}
	}
	for _, request := range requests {
		_ = request.Wait()
	}
	running := make([]int, 0, len(masters))
	for socket, fd := range masters {
		running = append(running, fd)
		delete(masters, socket)
	}
	running = awaitConfigGitSSHMasters(running, deadline)
	for _, terminate := range []unix.Signal{unix.SIGTERM, unix.SIGKILL} {
		if len(running) == 0 {
			break
		}
		for _, fd := range running {
			_ = unix.PidfdSendSignal(fd, terminate, nil, 0)
		}
		running = awaitConfigGitSSHMasters(running, time.Now().Add(2*time.Second))
	}
	for _, fd := range running {
		_ = unix.Close(fd)
	}
	for _, entry := range entries {
		if entry.Type()&os.ModeSocket != 0 {
			_ = os.Remove(filepath.Join(directory, entry.Name()))
		}
	}
	if err := os.Remove(directory); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("left %s in place: %w", directory, err)
	}
	return nil
}

// awaitConfigGitSSHMasters polls the pidfds until each process exits or the
// deadline passes, closes those that exited and returns the rest.
func awaitConfigGitSSHMasters(running []int, deadline time.Time) []int {
	for len(running) != 0 {
		descriptors := make([]unix.PollFd, len(running))
		for index, fd := range running {
			descriptors[index] = unix.PollFd{Fd: int32(fd), Events: unix.POLLIN}
		}
		remaining := time.Until(deadline)
		if remaining < 0 {
			remaining = 0
		}
		if _, err := unix.Poll(descriptors, int(remaining.Milliseconds())); err != nil && err != unix.EINTR {
			return running
		}
		next := running[:0]
		for index, fd := range running {
			if descriptors[index].Revents == 0 {
				next = append(next, fd)
			} else {
				_ = unix.Close(fd)
			}
		}
		running = next
		if remaining == 0 {
			break
		}
	}
	return running
}

type configGitSSHEndpoint struct {
	destination []string
	command     string
}

// configGitSSHEndpointFor maps an effective Git SSH URL to the OpenSSH
// destination and remote command Git uses. Other transports, including file
// and HTTPS, return false.
func configGitSSHEndpointFor(remote, program string) (configGitSSHEndpoint, bool) {
	var host, user, port, path string
	if strings.Contains(remote, "://") {
		parsed, err := url.Parse(remote)
		if err != nil {
			return configGitSSHEndpoint{}, false
		}
		switch parsed.Scheme {
		case "ssh", "git+ssh", "ssh+git":
		default:
			return configGitSSHEndpoint{}, false
		}
		host, port, path = parsed.Hostname(), parsed.Port(), parsed.Path
		if strings.HasPrefix(path, "/~") {
			path = path[1:]
		}
		if parsed.User != nil {
			user = parsed.User.Username()
		}
	} else if configGitSCPStyle(remote) {
		colon := strings.IndexByte(remote, ':')
		host, path = remote[:colon], remote[colon+1:]
		if at := strings.LastIndexByte(host, '@'); at >= 0 {
			user, host = host[:at], host[at+1:]
		}
	} else {
		return configGitSSHEndpoint{}, false
	}
	for _, value := range []string{host, user, port} {
		if strings.HasPrefix(value, "-") || strings.ContainsAny(value, "[]%\x00\r\n\t ") {
			return configGitSSHEndpoint{}, false
		}
	}
	if host == "" || path == "" {
		return configGitSSHEndpoint{}, false
	}
	var destination []string
	if port != "" {
		destination = append(destination, "-p", port)
	}
	if user != "" {
		destination = append(destination, "-l", user)
	}
	return configGitSSHEndpoint{
		destination: append(destination, host),
		command:     program + " " + configGitShellQuote(path),
	}, true
}

// configGitShellQuote matches Git's sq_quote for the remote command string.
func configGitShellQuote(value string) string {
	var quoted strings.Builder
	quoted.WriteByte('\'')
	for _, character := range value {
		if character == '\'' || character == '!' {
			quoted.WriteString(`'\` + string(character) + `'`)
			continue
		}
		quoted.WriteRune(character)
	}
	quoted.WriteByte('\'')
	return quoted.String()
}

// configGitSSHOverridden reports an explicit operator SSH command or variant.
// Unknown results also leave Git's SSH selection untouched.
func (cli *CLI) configGitSSHOverridden(ctx context.Context, directory string) bool {
	for _, name := range []string{"GIT_SSH_COMMAND", "GIT_SSH", "GIT_SSH_VARIANT"} {
		if cli.env[name] != "" {
			return true
		}
	}
	if directory == "" {
		directory = string(filepath.Separator)
	}
	for _, key := range []string{"core.sshCommand", "ssh.variant"} {
		ctx, cancel := context.WithTimeout(ctx, configGitSSHTimeout)
		command := exec.CommandContext(ctx, "git", "-C", directory, "config", "--get", key)
		command.Env = environmentList(cli.env, map[string]string{
			"GIT_OPTIONAL_LOCKS": "0", "GIT_TERMINAL_PROMPT": "0", "LC_ALL": "C",
		})
		command.WaitDelay = configGitWaitDelay
		output, err := command.Output()
		cancel()
		var exit *exec.ExitError
		switch {
		case err == nil && strings.TrimSpace(string(output)) != "":
			return true
		case err == nil, errors.As(err, &exit) && exit.ExitCode() == 1:
		default:
			return true
		}
	}
	return false
}

// configGitSSHUnmultiplexed resolves the operator's OpenSSH configuration as
// Git invokes each endpoint, including its remote command. A configured
// master or control path is the operator's own multiplexing and keeps
// precedence. Endpoints are grouped by the control socket OpenSSH derives for
// them (%C hashes the resolved host, port and user, so different aliases can
// share one). The first endpoint of a group starts its master and every later
// one would reuse that authentication, so sharing requires an identical
// resolved configuration within each group. Only the "host" line, the
// requested alias itself, may differ; it does not change the connection.
//
// keepAlive reports that every endpoint leaves ServerAliveInterval at 0.
func (cli *CLI) configGitSSHUnmultiplexed(ctx context.Context, ssh string, endpoints []configGitSSHEndpoint) (shared, keepAlive bool) {
	keepAlive = true
	resolved := make(map[string][]byte, len(endpoints))
	for _, endpoint := range endpoints {
		output, err := cli.configGitSSHResolve(ctx, ssh, nil, endpoint)
		unmultiplexed, alive := configGitSSHOutputUnmultiplexed(output)
		if err != nil || !unmultiplexed {
			return false, false
		}
		keepAlive = keepAlive && !alive
		keyed, err := cli.configGitSSHResolve(ctx, ssh, []string{"-o", "ControlPath=" + configGitSSHProbePath}, endpoint)
		socket := configGitSSHOutputValue(keyed, "controlpath")
		if err != nil || socket == "" {
			return false, false
		}
		var effective []byte
		for _, line := range bytes.SplitAfter(output, []byte("\n")) {
			if !bytes.HasPrefix(line, []byte("host ")) {
				effective = append(effective, line...)
			}
		}
		if previous, seen := resolved[socket]; seen && !bytes.Equal(previous, effective) {
			return false, false
		}
		resolved[socket] = effective
	}
	return true, keepAlive
}

// configGitSSHProbePath only names the socket OpenSSH would derive; it is never created.
const configGitSSHProbePath = "/subyard-config-git-probe/%C"

func (cli *CLI) configGitSSHResolve(ctx context.Context, ssh string, options []string, endpoint configGitSSHEndpoint) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, configGitSSHTimeout)
	defer cancel()
	arguments := append(append(append([]string{"-G"}, options...), endpoint.destination...), endpoint.command)
	command := exec.CommandContext(ctx, ssh, arguments...)
	command.Env = environmentList(cli.env, nil)
	command.Stderr = io.Discard
	command.WaitDelay = configGitWaitDelay
	return command.Output()
}

func configGitSSHOutputValue(output []byte, name string) string {
	for _, line := range strings.Split(string(output), "\n") {
		if key, value, found := strings.Cut(strings.TrimSpace(line), " "); found && strings.EqualFold(key, name) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// configGitSSHOutputUnmultiplexed parses ssh -G output. alive reports an
// operator ServerAliveInterval other than 0.
func configGitSSHOutputUnmultiplexed(output []byte) (unmultiplexed, alive bool) {
	master := ""
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		key, value, _ := strings.Cut(strings.TrimSpace(scanner.Text()), " ")
		switch strings.ToLower(key) {
		case "controlmaster":
			master = strings.ToLower(strings.TrimSpace(value))
		case "controlpath":
			if path := strings.TrimSpace(value); path != "" && !strings.EqualFold(path, "none") {
				return false, false
			}
		case "serveraliveinterval":
			alive = strings.TrimSpace(value) != "0"
		}
	}
	return scanner.Err() == nil && (master == "false" || master == "no"), alive
}

func configGitSSHDirectory() string {
	bases := []string{os.TempDir()}
	if bases[0] != "/tmp" {
		bases = append(bases, "/tmp")
	}
	for _, base := range bases {
		directory, err := os.MkdirTemp(base, configGitSSHPrefix)
		if err != nil {
			continue
		}
		if err := os.Chmod(directory, 0o700); err != nil ||
			len(directory)+configGitSSHSocketExtra > configGitSSHSocketLimit ||
			!configGitSSHSafePath(directory) {
			_ = os.Remove(directory)
			continue
		}
		return directory
	}
	return ""
}

// The path is used in a shell command and an OpenSSH token-expanded option.
func configGitSSHSafePath(path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	for _, character := range path {
		switch {
		case character >= 'a' && character <= 'z', character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9', strings.ContainsRune("/._-+", character):
		default:
			return false
		}
	}
	return true
}

func lookPathIn(name, path string) string {
	for _, directory := range filepath.SplitList(path) {
		if !filepath.IsAbs(directory) {
			continue
		}
		candidate := filepath.Join(directory, name)
		info, err := os.Stat(candidate)
		if err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return candidate
		}
	}
	return ""
}
