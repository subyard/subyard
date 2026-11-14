package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/testkit"
)

// configFakeSSH is a synthetic OpenSSH client. A connection that cannot use
// an existing control master records one authentication, and the remote Git
// command runs locally against the fixture repository. Configuration probes
// report operator multiplexing or identities for matching remote commands.
// A master is a background process with OpenSSH's exact multiplexing title,
// listed in "$log.masters"; it reports its PID to a check request, and an exit
// request stops it unless the test stalls exits.
const configFakeSSH = `#!/bin/sh
log="$SUBYARD_TEST_SSH_LOG"
control="" master="" operation="" printConfig="" host="" alive=""
while [ $# -gt 0 ]; do
	case "$1" in
	-G) printConfig=1; shift ;;
	-o)
		case "$2" in
		ControlPath=*) control="${2#ControlPath=}" ;;
		ControlMaster=*) master="${2#ControlMaster=}" ;;
		ServerAliveInterval=*) alive="${2#ServerAliveInterval=}" ;;
		esac
		shift 2 ;;
	-O) operation="$2"; shift 2 ;;
	-p|-l|-F|-i|-J|-S) shift 2 ;;
	-*) shift ;;
	*) host="$1"; shift; break ;;
	esac
done
control=$(printf '%s' "$control" | sed 's/%C/connection/')
if [ -n "$printConfig" ]; then
	printf 'probe %s %s\n' "$host" "${1%% *}" >>"$log"
	matched=""
	case "$1" in "$SUBYARD_TEST_SSH_MATCH_COMMAND"*) matched=1 ;; esac
	if [ -n "$matched" ] && [ -n "$SUBYARD_TEST_SSH_MATCH_CONTROLMASTER" ]; then
		printf 'controlmaster %s\n' "$SUBYARD_TEST_SSH_MATCH_CONTROLMASTER"
	else
		printf 'controlmaster %s\n' "${SUBYARD_TEST_SSH_CONTROLMASTER:-false}"
	fi
	printf 'serveraliveinterval %s\n' "${SUBYARD_TEST_SSH_ALIVE:-0}"
	if [ -n "$SUBYARD_TEST_SSH_CONTROLPATH" ]; then
		printf 'controlpath %s\n' "$SUBYARD_TEST_SSH_CONTROLPATH"
	fi
	if [ -n "$matched" ] && [ -n "$SUBYARD_TEST_SSH_MATCH_IDENTITY" ]; then
		printf 'identityfile %s\n' "$SUBYARD_TEST_SSH_MATCH_IDENTITY"
	fi
	exit 0
fi
if [ "$operation" = check ]; then
	[ -f "$log.masters" ] || exit 255
	while read -r pid path; do
		if [ "$path" = "$control" ]; then
			printf 'Master running (pid=%s)\r\n' "$pid" >&2
			exit 0
		fi
	done <"$log.masters"
	exit 255
fi
if [ -n "$operation" ]; then
	printf '%s %s %s\n' "$operation" "$host" "$control" >>"$log"
	if [ -n "$SUBYARD_TEST_SSH_STALL_EXIT" ]; then
		exec sleep 60
	fi
	while read -r pid path; do
		[ "$path" = "$control" ] && kill "$pid" 2>/dev/null
	done <"$log.masters"
	[ -n "$control" ] && rm -f "$control"
	exit 0
fi
if [ -n "$control" ] && [ -e "$control" ]; then
	printf 'reuse %s %s\n' "$host" "$control" >>"$log"
else
	printf 'auth %s %s\n' "$host" "$control" >>"$log"
	if [ -n "$control" ] && [ "$master" = auto ]; then
		python3 -c 'import socket, sys; socket.socket(socket.AF_UNIX).bind(sys.argv[1])' "$control"
		printf '%s\n' "$alive" >"$log.alive"
		bash -c 'exec -a "$0" sleep 3600' "ssh: $control [mux]" </dev/null >/dev/null 2>&1 &
		printf '%s %s\n' "$!" "$control" >>"$log.masters"
	fi
fi
exec sh -c "$*"
`

type configSSHFixture struct {
	*configGitAuthoringFixture
	publisher, log, ssh string
}

type configSSHEvent struct {
	kind, host, detail string
}

// configSSHTempDir is a private fixture root under /tmp, independent of TMPDIR.
func configSSHTempDir(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "subyard-config-ssh-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	return directory
}

// configSSHMasters returns the live fake masters started since the fixture began.
func configSSHMasters(t *testing.T, log string) []int {
	t.Helper()
	content, err := os.ReadFile(log + ".masters")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var live []int
	for _, line := range strings.Split(strings.TrimSpace(string(content)), "\n") {
		var pid int
		if _, err := fmt.Sscan(line, &pid); err != nil {
			continue
		}
		// A terminated master may remain a zombie until its new parent reaps it.
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if fields := strings.Fields(string(stat[bytes.LastIndexByte(stat, ')')+1:])); err == nil && len(fields) != 0 && fields[0] != "Z" {
			live = append(live, pid)
		}
	}
	return live
}

func writeConfigFakeSSH(t *testing.T) (bin, ssh, log string) {
	t.Helper()
	tools := configSSHTempDir(t)
	bin = filepath.Join(tools, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	ssh = filepath.Join(bin, "ssh")
	testkit.WriteFile(t, ssh, []byte(configFakeSSH), 0o700)
	log = filepath.Join(tools, "ssh.log")
	// A failed test must not leave a fake master behind; stop each listed one
	// that still carries its own title.
	t.Cleanup(func() {
		content, _ := os.ReadFile(log + ".masters")
		for _, line := range strings.Split(strings.TrimSpace(string(content)), "\n") {
			var pid int
			var control string
			if _, err := fmt.Sscan(line, &pid, &control); err != nil {
				continue
			}
			cmdline, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
			if title, _, _ := bytes.Cut(cmdline, []byte{0}); string(title) == "ssh: "+control+" [mux]" {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	return bin, ssh, log
}

func newConfigSSHFixture(t *testing.T, extra ...string) *configSSHFixture {
	t.Helper()
	root, _, _, environment := configCommandFixture(t)
	return newConfigSSHFixtureFrom(t, root, environment, extra...)
}

func newConfigSSHFixtureFrom(t *testing.T, root string, environment []string, extra ...string) *configSSHFixture {
	t.Helper()
	var home, configHome string
	for _, entry := range environment {
		if value, found := strings.CutPrefix(entry, "HOME="); found {
			home = value
		}
		if value, found := strings.CutPrefix(entry, "SUBYARD_CONFIG_HOME="); found {
			configHome = value
		}
	}
	remote, publisher := configBareSourceRepository(t, "owner-a", "# Preserve source records\nDEV_UID=2297\nDEV_SUDO=0\n")
	bin, ssh, log := writeConfigFakeSSH(t)
	environment = append(environment, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"SUBYARD_TEST_SSH_LOG="+log,
		"GIT_AUTHOR_NAME=Subyard Test", "GIT_AUTHOR_EMAIL=test@invalid",
		"GIT_COMMITTER_NAME=Subyard Test", "GIT_COMMITTER_EMAIL=test@invalid")
	f := &configSSHFixture{configGitAuthoringFixture: &configGitAuthoringFixture{t: t, root: root, home: home,
		configHome: configHome, remote: remote, environment: environment}, publisher: publisher, log: log, ssh: ssh}
	f.run(0, "config", "sync", "connect", "ssh://config.invalid"+remote, "--host-id", "owner-a", "--yes")
	f.environment = append(f.environment, extra...)
	f.events()
	return f
}

// events returns and resets the transport events since the last call.
func (f *configSSHFixture) events() []configSSHEvent {
	f.t.Helper()
	content, err := os.ReadFile(f.log)
	if err != nil && !os.IsNotExist(err) {
		f.t.Fatal(err)
	}
	if err := os.Remove(f.log); err != nil && !os.IsNotExist(err) {
		f.t.Fatal(err)
	}
	return parseConfigSSHEvents(string(content))
}

func parseConfigSSHEvents(content string) []configSSHEvent {
	var events []configSSHEvent
	for _, line := range strings.Split(strings.TrimSpace(content), "\n") {
		if fields := strings.Fields(line); len(fields) >= 2 {
			event := configSSHEvent{kind: fields[0], host: fields[1]}
			if len(fields) == 3 {
				event.detail = fields[2]
			}
			events = append(events, event)
		}
	}
	return events
}

// connections drops configuration probes, which open no connection.
func connections(events []configSSHEvent) []configSSHEvent {
	return slices.DeleteFunc(slices.Clone(events), func(event configSSHEvent) bool { return event.kind == "probe" })
}

// assertShared requires one authentication, reuse for every later connection,
// and a closed master whose private directory no longer exists.
func (f *configSSHFixture) assertShared(label string, count int) {
	f.t.Helper()
	events := connections(f.events())
	var kinds []string
	for _, event := range events {
		kinds = append(kinds, event.kind)
	}
	want := []string{"auth"}
	for range count - 1 {
		want = append(want, "reuse")
	}
	want = append(want, "exit")
	if !slices.Equal(kinds, want) {
		f.t.Fatalf("%s: SSH events %v, want %v", label, kinds, want)
	}
	control := events[0].detail
	for _, event := range events {
		host := "config.invalid"
		if event.kind == "exit" {
			host = "subyard-config-git"
		}
		if event.host != host || event.detail != control || !strings.HasPrefix(filepath.Base(filepath.Dir(control)), configGitSSHPrefix) {
			f.t.Fatalf("%s: unexpected SSH event %#v", label, event)
		}
	}
	if _, err := os.Lstat(filepath.Dir(control)); !os.IsNotExist(err) {
		f.t.Fatalf("%s: private control directory remains: %v", label, err)
	}
	if live := configSSHMasters(f.t, f.log); len(live) != 0 {
		f.t.Fatalf("%s: masters still running: %v", label, live)
	}
}

// assertUnshared requires separate authentications without a Subyard master.
func (f *configSSHFixture) assertUnshared(label string, count int) {
	f.t.Helper()
	events := connections(f.events())
	if len(events) != count {
		f.t.Fatalf("%s: SSH events %#v, want %d unshared connections", label, events, count)
	}
	for _, event := range events {
		if event.kind != "auth" || event.detail != "" {
			f.t.Fatalf("%s: Subyard changed the operator's SSH selection: %#v", label, events)
		}
	}
}

func (f *configSSHFixture) commitCount() string {
	return strings.TrimSpace(configSourceGitOutput(f.t, f.remote, "rev-list", "--count", "HEAD"))
}

func (f *configSSHFixture) runWithPrompt(prompt *callbackPrompt, arguments ...string) (int, string) {
	f.t.Helper()
	var stdout, stderr bytes.Buffer
	program, err := New(Options{RepositoryRoot: f.root, Program: "yard", Prompt: prompt, Environment: f.environment,
		WorkingDir: f.root, Stdout: &stdout, Stderr: &stderr, Arguments: arguments})
	if err != nil {
		f.t.Fatal(err)
	}
	return program.Run(context.Background()), stdout.String() + stderr.String()
}

func (f *configSSHFixture) publishRemoteChange(content string) {
	f.t.Helper()
	runConfigSyncGit(f.t, f.publisher, "pull", "-q", "--rebase")
	writeConfigCommandFile(f.t, filepath.Join(f.publisher, "shared", "config.env"), content)
	commitConfigSource(f.t, f.publisher, "remote change")
	runConfigSyncGit(f.t, f.publisher, "push", "-q")
}

func TestConfigGitSSHAuthenticatesOncePerCommand(t *testing.T) {
	f := newConfigSSHFixture(t)
	before := f.commitCount()
	f.run(0, "config", "set", "GIT_USER_NAME", "Subyard Operator", "GIT_USER_EMAIL", "operator@example.invalid",
		"--scope", "shared", "--git", "--yes")
	// Plan fetch, the post-consent upstream recheck, and push.
	f.assertShared("config set --git", 3)
	if after := f.commitCount(); after != "2" || before != "1" {
		t.Fatalf("commits before=%s after=%s, want one new commit", before, after)
	}
	versioned := configSourceGitOutput(t, f.remote, "show", "HEAD:shared/config.env")
	if versioned != "GIT_USER_NAME='Subyard Operator'\nGIT_USER_EMAIL='operator@example.invalid'\n" {
		t.Fatalf("shared settings: %q", versioned)
	}
	message := strings.TrimSpace(configSourceGitOutput(t, f.remote, "log", "-1", "--format=%s"))
	if message != "set GIT_USER_NAME, GIT_USER_EMAIL in shared settings" {
		t.Fatalf("commit message: %q", message)
	}
	f.assertSetting("GIT_USER_EMAIL", "operator@example.invalid")

	// A converged pull fetches once to plan and makes no further request.
	f.run(0, "config", "sync", "pull", "--yes")
	f.assertShared("converged config sync pull", 1)

	f.publishRemoteChange("GIT_USER_NAME='Remote Operator'\n")
	f.run(0, "config", "sync", "pull", "--yes")
	f.assertShared("changing config sync pull", 2)
	f.assertSetting("GIT_USER_NAME", "Remote Operator")
}

func TestConfigGitSSHKeepsStaleUpstreamGuardAndClosesOnFailure(t *testing.T) {
	f := newConfigSSHFixture(t)
	prompt := &callbackPrompt{callback: func() { f.publishRemoteChange("E2E_VM_CPU=3\n") }}
	code, output := f.runWithPrompt(prompt, "config", "set", "DEV_UID", "2302", "--scope", "host", "--git")
	if code != 1 || !strings.Contains(output, "operation plan is stale: upstream changed after preview") {
		t.Fatalf("concurrent remote change: code=%d output=%s", code, output)
	}
	f.assertShared("rejected config set --git", 2)
	if versioned := configSourceGitOutput(t, f.remote, "show", "HEAD:hosts/owner-a/config.env"); strings.Contains(versioned, "2302") {
		t.Fatalf("rejected change was pushed: %q", versioned)
	}
}

func TestConfigGitSSHKeepsStaleRemoteURLGuard(t *testing.T) {
	f := newConfigSSHFixture(t)
	checkout := filepath.Join(f.home, ".local", "share", "subyard-config")
	prompt := &callbackPrompt{callback: func() {
		runConfigSyncGit(t, checkout, "remote", "set-url", "origin", "ssh://other.invalid"+f.remote)
	}}
	code, output := f.runWithPrompt(prompt, "config", "set", "DEV_UID", "2302", "--scope", "host", "--git")
	if code != 1 || !strings.Contains(output, "operation plan is stale: registered upstream changed after preview") {
		t.Fatalf("remote URL change: code=%d output=%s", code, output)
	}
	f.assertShared("rejected config set --git", 1)
}

// startConfigSSHDecoys starts unrelated processes of this user that name the
// private directory, one with the exact master title of its socket.
func startConfigSSHDecoys(t *testing.T, directory string) []*exec.Cmd {
	t.Helper()
	socket := filepath.Join(directory, "connection")
	decoys := []*exec.Cmd{
		exec.Command("sh", "-c", "sleep 60; :", socket),
		exec.Command("bash", "-c", `exec -a "$0" sleep 60`, "ssh: "+socket+" [mux]"),
	}
	for _, decoy := range decoys {
		if err := decoy.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = decoy.Process.Kill(); _ = decoy.Wait() })
	}
	return decoys
}

func assertConfigSSHDecoysRunning(t *testing.T, decoys []*exec.Cmd) {
	t.Helper()
	for _, decoy := range decoys {
		// An exited decoy stays a zombie until waited; require a running one.
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", decoy.Process.Pid))
		if fields := strings.Fields(string(stat[bytes.LastIndexByte(stat, ')')+1:])); err != nil || len(fields) == 0 || fields[0] == "Z" {
			t.Fatalf("unrelated process %v is not running: %v", decoy.Args, err)
		}
		cmdline, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", decoy.Process.Pid))
		if !bytes.Contains(cmdline, []byte(filepath.Dir(decoy.Args[len(decoy.Args)-1]))) {
			t.Fatalf("decoy %v does not name the private directory: %q", decoy.Args, cmdline)
		}
	}
}

func TestConfigGitSSHTerminatesOnlyItsMasterThatDoesNotExit(t *testing.T) {
	previous := configGitSSHExitWait
	configGitSSHExitWait = time.Second
	t.Cleanup(func() { configGitSSHExitWait = previous })
	f := newConfigSSHFixture(t, "SUBYARD_TEST_SSH_STALL_EXIT=1")
	var decoys []*exec.Cmd
	prompt := &callbackPrompt{callback: func() {
		content, err := os.ReadFile(f.log)
		if err != nil {
			t.Fatal(err)
		}
		events := connections(parseConfigSSHEvents(string(content)))
		decoys = startConfigSSHDecoys(t, filepath.Dir(events[0].detail))
	}}
	if code, output := f.runWithPrompt(prompt, "config", "set", "GIT_USER_NAME", "Subyard Operator", "--scope", "shared", "--git"); code != 0 {
		t.Fatalf("config set --git: code=%d output=%s", code, output)
	}
	f.assertShared("stalled master exit", 3)
	assertConfigSSHDecoysRunning(t, decoys)
}

func TestConfigGitSSHTerminatesMasterAfterWatchdogLoss(t *testing.T) {
	f := newConfigSSHFixture(t)
	program, err := New(Options{RepositoryRoot: f.root, Program: "yard", Environment: f.environment,
		WorkingDir: f.root, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
	if err != nil {
		t.Fatal(err)
	}
	remote := "ssh://config.invalid" + f.remote
	release := program.shareConfigGitSSH(context.Background(), "/", []configGitSSHRemote{{remote, "git-upload-pack"}})
	session := program.configGitSSH
	if session == nil {
		t.Fatal("no shared session")
	}
	if _, err := program.configGitOutput(context.Background(), "/", "ls-remote", remote); err != nil {
		t.Fatal(err)
	}
	if live := configSSHMasters(t, f.log); len(live) != 1 {
		t.Fatalf("masters %v, want one", live)
	}
	if err := session.watchdog.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	<-session.done
	release()
	if live := configSSHMasters(t, f.log); len(live) != 0 {
		t.Fatalf("masters survived watchdog loss: %v", live)
	}
	if _, err := os.Lstat(session.directory); !os.IsNotExist(err) {
		t.Fatalf("private control directory remains: %v", err)
	}
}

func TestConfigGitSSHClosesPromptly(t *testing.T) {
	f := newConfigSSHFixture(t)
	program, err := New(Options{RepositoryRoot: f.root, Program: "yard", Environment: f.environment,
		WorkingDir: f.root, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
	if err != nil {
		t.Fatal(err)
	}
	remote := "ssh://config.invalid" + f.remote
	release := program.shareConfigGitSSH(context.Background(), "/", []configGitSSHRemote{{remote, "git-upload-pack"}})
	if program.configGitSSH == nil {
		t.Fatal("no shared session")
	}
	if _, err := program.configGitOutput(context.Background(), "/", "ls-remote", remote); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	release()
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("closing a responsive master took %v", elapsed)
	}
	if live := configSSHMasters(t, f.log); len(live) != 0 {
		t.Fatalf("masters still running: %v", live)
	}
}

func TestConfigGitSSHWithoutRegisteredWatchdogClosesNormally(t *testing.T) {
	configGitSSHWatchdogRegistered = false
	t.Cleanup(RegisterConfigGitSSHWatchdog)
	f := newConfigSSHFixture(t)
	program, err := New(Options{RepositoryRoot: f.root, Program: "yard", Environment: f.environment,
		WorkingDir: f.root, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
	if err != nil {
		t.Fatal(err)
	}
	remote := "ssh://config.invalid" + f.remote
	release := program.shareConfigGitSSH(context.Background(), "/", []configGitSSHRemote{{remote, "git-upload-pack"}})
	session := program.configGitSSH
	if session == nil || session.watchdog != nil {
		t.Fatalf("an unregistered program must share without a watchdog: %#v", session)
	}
	if _, err := program.configGitOutput(context.Background(), "/", "ls-remote", remote); err != nil {
		t.Fatal(err)
	}
	release()
	if live := configSSHMasters(t, f.log); len(live) != 0 {
		t.Fatalf("masters still running: %v", live)
	}
	if _, err := os.Lstat(session.directory); !os.IsNotExist(err) {
		t.Fatalf("private control directory remains: %v", err)
	}
}

func TestConfigGitSSHKeepsOperatorServerAlive(t *testing.T) {
	for _, test := range []struct {
		extra []string
		want  string
	}{{nil, configGitSSHAliveInterval + "\n"}, {[]string{"SUBYARD_TEST_SSH_ALIVE=30"}, "\n"}} {
		f := newConfigSSHFixture(t, test.extra...)
		f.run(0, "config", "sync", "pull", "--yes")
		f.assertShared("config sync pull", 1)
		if alive, err := os.ReadFile(f.log + ".alive"); err != nil || string(alive) != test.want {
			t.Fatalf("%v: master ServerAliveInterval %q, %v; want %q", test.extra, alive, err, test.want)
		}
	}
}

func TestConfigGitSSHRespectsOperatorSSHChoice(t *testing.T) {
	for _, test := range []struct {
		name  string
		extra func(*configSSHFixture) []string
	}{
		{"environment command", func(f *configSSHFixture) []string { return []string{"GIT_SSH_COMMAND=" + f.ssh} }},
		{"Git configuration", func(*configSSHFixture) []string {
			return []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.sshCommand", "GIT_CONFIG_VALUE_0=ssh"}
		}},
		{"OpenSSH master", func(*configSSHFixture) []string { return []string{"SUBYARD_TEST_SSH_CONTROLMASTER=auto"} }},
		{"OpenSSH control path", func(*configSSHFixture) []string {
			return []string{"SUBYARD_TEST_SSH_CONTROLPATH=~/.ssh/control-%C"}
		}},
		{"OpenSSH push command master", func(*configSSHFixture) []string {
			return []string{"SUBYARD_TEST_SSH_MATCH_COMMAND=git-receive-pack", "SUBYARD_TEST_SSH_MATCH_CONTROLMASTER=auto"}
		}},
		{"OpenSSH push command identity", func(*configSSHFixture) []string {
			return []string{"SUBYARD_TEST_SSH_MATCH_COMMAND=git-receive-pack", "SUBYARD_TEST_SSH_MATCH_IDENTITY=~/.ssh/deploy"}
		}},
		// A configured program changes the remote command OpenSSH matches.
		{"Git upload program", func(*configSSHFixture) []string {
			return []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=remote.origin.uploadpack", "GIT_CONFIG_VALUE_0=git-upload-pack"}
		}},
		{"Git receive program", func(*configSSHFixture) []string {
			return []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=remote.origin.receivepack", "GIT_CONFIG_VALUE_0=git-receive-pack"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newConfigSSHFixture(t)
			f.environment = append(f.environment, test.extra(f)...)
			f.run(0, "config", "set", "GIT_USER_NAME", "Subyard Operator", "--scope", "shared", "--git", "--yes")
			f.assertUnshared("config set --git", 3)
		})
	}
}

func TestConfigGitSSHProbesAsGitInvokesEachCommand(t *testing.T) {
	// A push-only Match command block does not affect a pull, which runs only
	// git-upload-pack, so the pull still shares its connection.
	f := newConfigSSHFixture(t, "SUBYARD_TEST_SSH_MATCH_COMMAND=git-receive-pack", "SUBYARD_TEST_SSH_MATCH_CONTROLMASTER=auto")
	f.publishRemoteChange("GIT_USER_NAME='Remote Operator'\n")
	f.run(0, "config", "sync", "pull", "--yes")
	content, err := os.ReadFile(f.log)
	if err != nil {
		t.Fatal(err)
	}
	if probes := slices.DeleteFunc(parseConfigSSHEvents(string(content)), func(event configSSHEvent) bool { return event.kind != "probe" }); !slices.Equal(probes,
		[]configSSHEvent{{"probe", "config.invalid", "git-upload-pack"}}) {
		t.Fatalf("pull probes: %#v", probes)
	}
	f.assertShared("config sync pull", 2)
}

func TestConfigGitSSHUsesEffectiveRemoteURL(t *testing.T) {
	f := newConfigSSHFixture(t)
	f.environment = append(f.environment, "GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=url.ssh://rewritten.invalid/.insteadOf", "GIT_CONFIG_VALUE_0=ssh://config.invalid/")
	// The registration identity check refuses a rewritten upstream before any
	// connection; the sharing decision already probes the rewritten host.
	f.run(1, "config", "sync", "pull", "--yes")
	if events := f.events(); !slices.Equal(events, []configSSHEvent{{"probe", "rewritten.invalid", "git-upload-pack"}}) {
		t.Fatalf("rewritten upstream: %#v", events)
	}

	checkout := filepath.Join(configSSHTempDir(t), "checkout")
	runConfigSyncGit(t, filepath.Dir(checkout), "init", "-q", "-b", "main", checkout)
	runConfigSyncGit(t, checkout, "remote", "add", "origin", "https://alias.invalid/config.git")
	runConfigSyncGit(t, checkout, "config", "branch.main.remote", "origin")
	runConfigSyncGit(t, checkout, "config", "branch.main.merge", "refs/heads/main")
	runConfigSyncGit(t, checkout, "fetch", "-q", f.remote, "main")
	runConfigSyncGit(t, checkout, "reset", "-q", "--hard", "FETCH_HEAD")
	runConfigSyncGit(t, checkout, "update-ref", "refs/remotes/origin/main", "HEAD")
	program, err := New(Options{RepositoryRoot: f.root, Program: "yard", Environment: append(slices.Clone(f.environment),
		"GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=url.ssh://fetch.invalid/.insteadOf", "GIT_CONFIG_VALUE_0=https://alias.invalid/",
		"GIT_CONFIG_KEY_1=url.git@push.invalid:.pushInsteadOf", "GIT_CONFIG_VALUE_1=https://alias.invalid/"),
		WorkingDir: f.root, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
	if err != nil {
		t.Fatal(err)
	}
	remotes, err := program.configGitEffectiveRemotes(context.Background(), checkout, true)
	if err != nil || !slices.Equal(remotes, []configGitSSHRemote{
		{"ssh://fetch.invalid/config.git", "git-upload-pack"}, {"git@push.invalid:config.git", "git-receive-pack"},
	}) {
		t.Fatalf("effective remotes %#v, %v", remotes, err)
	}
	for _, remote := range remotes {
		if _, ok := configGitSSHEndpointFor(remote.url, remote.program); !ok {
			t.Fatalf("rewritten SSH remote is not shared: %#v", remote)
		}
	}

	runConfigSyncGit(t, checkout, "config", "remote.origin.pushurl", "git@push.invalid:/srv/private config.git")
	remotes, err = program.configGitEffectiveRemotes(context.Background(), checkout, true)
	if err != nil || len(remotes) != 2 || remotes[1] != (configGitSSHRemote{"git@push.invalid:/srv/private config.git", "git-receive-pack"}) {
		t.Fatalf("push URL with a space: %#v, %v", remotes, err)
	}
	if endpoint, ok := configGitSSHEndpointFor(remotes[1].url, remotes[1].program); !ok || endpoint.command != "git-receive-pack '/srv/private config.git'" {
		t.Fatalf("push URL with a space: %#v", endpoint)
	}
}

func TestConfigGitSSHEndpoint(t *testing.T) {
	for remote, want := range map[string]*configGitSSHEndpoint{
		"ssh://git@config.invalid:2222/srv/config.git": {[]string{"-p", "2222", "-l", "git", "config.invalid"}, "git-upload-pack '/srv/config.git'"},
		"git+ssh://config.invalid/~owner/it's!.git":    {[]string{"config.invalid"}, `git-upload-pack '~owner/it'\''s'\!'.git'`},
		"git@config.invalid:owner/config.git":          {[]string{"-l", "git", "config.invalid"}, "git-upload-pack 'owner/config.git'"},
		"config-alias:config.git":                      {[]string{"config-alias"}, "git-upload-pack 'config.git'"},
		"https://config.invalid/owner/config.git":      nil,
		"file:///srv/config.git":                       nil,
		"/srv/config.git":                              nil,
		"ssh://-oProxyCommand=false/config.git":        nil,
		"[::1]:config.git":                             nil,
	} {
		got, ok := configGitSSHEndpointFor(remote, "git-upload-pack")
		if ok != (want != nil) || want != nil && (!slices.Equal(got.destination, want.destination) || got.command != want.command) {
			t.Errorf("%s: got %#v %v, want %#v", remote, got, ok, want)
		}
	}
}

// TestConfigGitSSHHelperProcess owns a shared session with a recorded master
// until it is killed.
func TestConfigGitSSHHelperProcess(t *testing.T) {
	if os.Getenv("SUBYARD_TEST_SSH_HELPER") != "1" {
		t.Skip("helper process")
	}
	configGitSSHExitWait = time.Second
	program, err := New(Options{RepositoryRoot: repositoryRoot(t), Program: "yard", Environment: os.Environ(),
		WorkingDir: "/", Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
	if err != nil {
		t.Fatal(err)
	}
	remote := "ssh://config.invalid" + os.Getenv("SUBYARD_TEST_SSH_REMOTE")
	program.shareConfigGitSSH(context.Background(), "/", []configGitSSHRemote{{remote, "git-upload-pack"}})
	session := program.configGitSSH
	if session == nil {
		t.Fatal("no shared session")
	}
	if _, err := program.configGitOutput(context.Background(), "/", "ls-remote", remote); err != nil {
		t.Fatal(err)
	}
	if len(session.masters) != 1 {
		t.Fatalf("recorded masters %v", session.masters)
	}
	fmt.Println("session", session.directory)
	time.Sleep(time.Minute)
}

func TestConfigGitSSHClosesOnlyItsMasterWhenOwnerIsKilled(t *testing.T) {
	for _, stall := range []bool{false, true} {
		t.Run(fmt.Sprintf("stalled exit %t", stall), func(t *testing.T) {
			bin, _, log := writeConfigFakeSSH(t)
			remote, _ := configBareSourceRepository(t, "owner-a", "DEV_UID=2297\n")
			helper := exec.Command(os.Args[0], "-test.run=^TestConfigGitSSHHelperProcess$", "-test.v")
			var helperErr bytes.Buffer
			helper.Stderr = &helperErr
			// The go command exports GIT_SSH_COMMAND to tests; an owner must not have it.
			helper.Env = append(slices.DeleteFunc(os.Environ(), func(entry string) bool { return strings.HasPrefix(entry, "GIT_SSH_COMMAND=") }),
				"SUBYARD_TEST_SSH_HELPER=1", "SUBYARD_TEST_SSH_LOG="+log, "SUBYARD_TEST_SSH_REMOTE="+remote,
				"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			if stall {
				helper.Env = append(helper.Env, "SUBYARD_TEST_SSH_STALL_EXIT=1")
			}
			stdout, err := helper.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := helper.Start(); err != nil {
				t.Fatal(err)
			}
			directory := ""
			var transcript strings.Builder
			scanner := bufio.NewScanner(stdout)
			for directory == "" && scanner.Scan() {
				transcript.WriteString(scanner.Text() + "\n")
				if path, found := strings.CutPrefix(scanner.Text(), "session "); found {
					directory = path
				}
			}
			if directory == "" {
				_ = helper.Process.Kill()
				t.Fatalf("helper did not start a session: %v: %s%s", helper.Wait(), transcript.String(), helperErr.String())
			}
			if live := configSSHMasters(t, log); len(live) != 1 {
				t.Fatalf("masters %v, want one", live)
			}
			decoys := startConfigSSHDecoys(t, directory)
			if err := helper.Process.Signal(syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
			_ = helper.Wait()
			deadline := time.Now().Add(10 * time.Second)
			for {
				_, statErr := os.Lstat(directory)
				if errors.Is(statErr, os.ErrNotExist) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("private control directory remains after SIGKILL: %v", statErr)
				}
				time.Sleep(20 * time.Millisecond)
			}
			if live := configSSHMasters(t, log); len(live) != 0 {
				t.Fatalf("master survived its owner: %v", live)
			}
			assertConfigSSHDecoysRunning(t, decoys)
		})
	}
}

// runConfigGitSSHWatchdogVerb runs the hidden verb in a child, as yard does.
func runConfigGitSSHWatchdogVerb(t *testing.T, directory string, stdin *os.File) (int, string) {
	t.Helper()
	command := exec.Command(os.Args[0], ConfigGitSSHWatchdogVerb, "/usr/bin/ssh", directory, "1s")
	command.Stdin = stdin
	output, err := command.CombinedOutput()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatal(err)
	}
	return command.ProcessState.ExitCode(), string(output)
}

func TestConfigGitSSHWatchdogRefusesForeignInputs(t *testing.T) {
	newDirectory := func(t *testing.T, mode os.FileMode) string {
		t.Helper()
		directory := filepath.Join(configSSHTempDir(t), configGitSSHPrefix+"fixture")
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(directory, "keep"), []byte("data\n"), 0o600)
		if err := os.Chmod(directory, mode); err != nil {
			t.Fatal(err)
		}
		return directory
	}
	assertKept := func(t *testing.T, directory string) {
		t.Helper()
		if content, err := os.ReadFile(filepath.Join(directory, "keep")); err != nil || string(content) != "data\n" {
			t.Fatalf("watchdog changed %s: %q, %v", directory, content, err)
		}
	}
	closedPipe := func(t *testing.T) *os.File {
		t.Helper()
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		_ = writer.Close()
		t.Cleanup(func() { _ = reader.Close() })
		return reader
	}
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()

	directory := newDirectory(t, 0o700)
	if code, output := runConfigGitSSHWatchdogVerb(t, directory, devNull); code != 2 {
		t.Fatalf("non-pipe input: code=%d output=%s", code, output)
	}
	assertKept(t, directory)

	widened := newDirectory(t, 0o750)
	if code, output := runConfigGitSSHWatchdogVerb(t, widened, closedPipe(t)); code != 2 {
		t.Fatalf("mode 0750: code=%d output=%s", code, output)
	}
	assertKept(t, widened)

	link := filepath.Join(configSSHTempDir(t), configGitSSHPrefix+"link")
	if err := os.Symlink(directory, link); err != nil {
		t.Fatal(err)
	}
	if code, output := runConfigGitSSHWatchdogVerb(t, link, closedPipe(t)); code != 2 {
		t.Fatalf("symlink: code=%d output=%s", code, output)
	}
	assertKept(t, directory)

	// A valid private directory with a non-socket entry is left in place.
	if code, output := runConfigGitSSHWatchdogVerb(t, directory, closedPipe(t)); code != 1 || !strings.Contains(output, "left "+directory+" in place") {
		t.Fatalf("directory with a regular file: code=%d output=%s", code, output)
	}
	assertKept(t, directory)
}

func TestConfigSetSeveralPairsRequiresGitAndReportsEveryRefusal(t *testing.T) {
	f := newConfigSSHFixture(t)
	f.run(2, "config", "set", "GIT_USER_NAME", "One", "GIT_USER_EMAIL", "one@example.invalid", "--scope", "shared")
	if !strings.Contains(f.stderr.String(), "several SETTING VALUE pairs require --git") {
		t.Fatalf("local pairs: %s", f.stderr.String())
	}
	f.run(2, "config", "set", "GIT_USER_NAME", "One", "GIT_USER_EMAIL", "--scope", "shared", "--git")
	if !strings.Contains(f.stderr.String(), "expects SETTING VALUE pairs") {
		t.Fatalf("odd arguments: %s", f.stderr.String())
	}
	f.run(2, "config", "set", "UNKNOWN_SETTING", "1", "SSH_PORT", "2222", "GIT_USER_NAME", "One", "GIT_USER_NAME", "Two",
		"--scope", "shared", "--git", "--yes")
	for _, refusal := range []string{"UNKNOWN_SETTING", "SSH_PORT", "GIT_USER_NAME and GIT_USER_NAME assign the same setting"} {
		if !strings.Contains(f.stderr.String(), refusal) {
			t.Fatalf("missing refusal %q: %s", refusal, f.stderr.String())
		}
	}
	f.run(2, "config", "set", "DEV_UID", "2297", "DEV_UID", "invalid", "--scope", "host", "--git", "--yes")
	if !strings.Contains(f.stderr.String(), "DEV_UID and DEV_UID assign the same setting") ||
		!strings.Contains(f.stderr.String(), "DEV_UID (scalar settings): invalid integer value") {
		t.Fatalf("duplicate and invalid value: %s", f.stderr.String())
	}
	if events := f.events(); len(events) != 0 {
		t.Fatalf("refused pairs contacted the remote: %#v", events)
	}
}

// withSyntheticYardTemplate replaces the shipped configuration directory with
// an exact-mode copy whose only yard template is a synthetic one that forbids
// coding tools.
func withSyntheticYardTemplate(t *testing.T, root string, environment []string) {
	t.Helper()
	configDir := ""
	for _, entry := range environment {
		if value, found := strings.CutPrefix(entry, "SUBYARD_CONFIG_DIR="); found {
			configDir = value
		}
	}
	if err := os.Remove(configDir); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "config")
	profiles := filepath.Join(source, "yards", "profiles")
	err := filepath.WalkDir(source, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != profiles && strings.HasPrefix(path, profiles+string(filepath.Separator)) {
			return nil
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(configDir, relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if err := os.Mkdir(target, 0o700); err != nil {
				return err
			}
			return os.Chmod(target, info.Mode().Perm())
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		testkit.WriteFile(t, target, content, info.Mode().Perm())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(configDir, "yards", "profiles", "no-tools.env"), []byte("ALLOWS_CODING_TOOLS=false\n"), 0o644)
}

func TestConfigSetSeveralPairsValidatesCombinedTarget(t *testing.T) {
	root, _, _, environment := configCommandFixture(t)
	withSyntheticYardTemplate(t, root, environment)
	f := newConfigSSHFixtureFrom(t, root, environment)
	f.run(0, "config", "set", "YARD_TEMPLATE", "no-tools", "--scope", "yard", "--git", "--yes")
	// The current template forbids coding tools; the combined target clears it.
	f.run(0, "config", "set", "YARD_TEMPLATE", "", "CODING_TOOL_INTEGRATIONS", "codex", "--scope", "yard", "--git", "--yes")
	versioned := configSourceGitOutput(t, f.remote, "show", "HEAD:hosts/owner-a/yards/default/config.env")
	if !strings.Contains(versioned, "YARD_TEMPLATE=''") || !strings.Contains(versioned, "CODING_TOOL_INTEGRATIONS='codex'") {
		t.Fatalf("combined yard settings: %q", versioned)
	}
	// The reverse combination is refused by the new template, before any connection.
	f.events()
	f.run(2, "config", "set", "YARD_TEMPLATE", "no-tools", "CODING_TOOL_INTEGRATIONS", "codex", "--scope", "yard", "--git", "--yes")
	if !strings.Contains(f.stderr.String(), "yard role forbids non-empty CODING_TOOL_INTEGRATIONS") {
		t.Fatalf("combined refusal: %s", f.stderr.String())
	}
	if events := f.events(); len(events) != 0 {
		t.Fatalf("refused pairs contacted the remote: %#v", events)
	}
}
