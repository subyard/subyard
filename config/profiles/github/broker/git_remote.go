package githubbroker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const runContext = "SUBYARD_GITHUB_RUN_CONTEXT_V1"
const ephemeralToken = "SUBYARD_GITHUB_GIT_TOKEN_V1"

type gitRunContext struct {
	Version int    `json:"version"`
	Socket  string `json:"socket"`
}

// Only canonical URLs enter this transport. In particular, no shell command,
// userinfo password, port, query, alias, escaped path or arbitrary scheme can enter.
func gitRemoteURL(raw string) (repository string, https bool, err error) {
	path := ""
	if strings.HasPrefix(raw, "git@github.com:") {
		path = strings.TrimPrefix(raw, "git@github.com:")
	} else {
		parsed, e := url.Parse(raw)
		if e != nil || parsed.Host != "github.com" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" || parsed.Opaque != "" {
			return "", false, errors.New("invalid canonical GitHub URL")
		}
		https = parsed.Scheme == "https"
		if https {
			if parsed.User != nil {
				return "", false, errors.New("GitHub HTTPS userinfo is unsupported")
			}
		} else if parsed.Scheme != "ssh" || parsed.User == nil || parsed.User.String() != "git" {
			return "", false, errors.New("unsupported GitHub URL")
		}
		path = strings.TrimPrefix(parsed.Path, "/")
	}
	path = strings.TrimSuffix(strings.TrimSuffix(path, "/"), ".git")
	if !validRepository(path) {
		return "", false, errors.New("invalid GitHub repository path")
	}
	return path, https, nil
}

func gitCommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		// Drain descendants before returning from Cancel: a deferred timer dies
		// with this helper and could leave an SSH or credential child alive.
		time.Sleep(100 * time.Millisecond)
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err == syscall.ESRCH {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 3 * time.Second
	return cmd
}

func commandExit(err error) int {
	if err == nil {
		return 0
	}
	var failure *exec.ExitError
	if errors.As(err, &failure) {
		if failure.ExitCode() >= 0 {
			return failure.ExitCode()
		}
		if status, ok := failure.Sys().(syscall.WaitStatus); ok {
			return 128 + int(status.Signal())
		}
	}
	return 1
}

type gitOption struct{ name, value, line string }

// This is the intersection implemented by native fetch-pack/send-pack and curl.
// Unknown options are reported as unsupported, never silently acknowledged.
func parseGitOption(line string) (gitOption, string) {
	fields := strings.SplitN(line, " ", 3)
	if len(fields) != 3 {
		return gitOption{}, "error invalid option"
	}
	option := gitOption{name: fields[1], value: fields[2], line: line}
	if strings.HasPrefix(option.value, "\"") {
		value, err := strconv.Unquote(option.value)
		if err != nil {
			return option, "error invalid quoted value"
		}
		option.value = value
	}
	if strings.ContainsAny(option.value, "\x00\r\n") {
		return option, "error invalid value"
	}
	switch option.name {
	case "verbosity":
		if _, err := strconv.Atoi(option.value); err != nil {
			return option, "error invalid verbosity"
		}
	case "progress", "cloning", "check-connectivity", "atomic", "dry-run", "followtags", "deepen-relative":
		if option.value != "true" && option.value != "false" {
			return option, "error invalid boolean"
		}
	case "object-format":
		if option.value != "true" {
			return option, "unsupported"
		}
	case "depth":
		value, err := strconv.Atoi(option.value)
		if err != nil || value < 0 {
			return option, "error invalid depth"
		}
	case "deepen-since", "deepen-not", "filter", "cas", "push-option":
		if option.value == "" {
			return option, "error empty option"
		}
	default:
		return option, "unsupported"
	}
	return option, "ok"
}

type curlRemote struct {
	cmd         *exec.Cmd
	input       io.WriteCloser
	output      *bufio.Reader
	diagnostics *gitDiagnostics
}

// Tentative discovery is silent. Once selected, stream native diagnostics while
// retaining only suffixes that could be a split sensitive value.
type gitDiagnostics struct {
	mu        sync.Mutex
	output    io.Writer
	enabled   bool
	sensitive []string
	pending   []byte
}

func (diagnostics *gitDiagnostics) Write(value []byte) (int, error) {
	diagnostics.mu.Lock()
	defer diagnostics.mu.Unlock()
	if !diagnostics.enabled {
		return len(value), nil
	}
	diagnostics.pending = append(diagnostics.pending, value...)
	var safe bytes.Buffer
	for len(diagnostics.pending) > 0 {
		matched, partial := false, false
		for _, secret := range diagnostics.sensitive {
			if bytes.HasPrefix(diagnostics.pending, []byte(secret)) {
				safe.WriteString("[redacted]")
				diagnostics.pending = diagnostics.pending[len(secret):]
				matched = true
				break
			}
			partial = partial || strings.HasPrefix(secret, string(diagnostics.pending))
		}
		if matched {
			continue
		}
		if partial {
			break
		}
		safe.WriteByte(diagnostics.pending[0])
		diagnostics.pending = diagnostics.pending[1:]
	}
	_, err := diagnostics.output.Write(safe.Bytes())
	return len(value), err
}

func (diagnostics *gitDiagnostics) selectBackend() {
	diagnostics.mu.Lock()
	diagnostics.enabled = true
	diagnostics.mu.Unlock()
}
func (diagnostics *gitDiagnostics) flush() {
	diagnostics.mu.Lock()
	defer diagnostics.mu.Unlock()
	if len(diagnostics.pending) > 0 {
		_, _ = io.WriteString(diagnostics.output, "[redacted]")
		diagnostics.pending = nil
	}
}

// Let Git parse inherited COUNT/PARAMETERS, including quoting and includes.
// Native -c appends our overrides after that command configuration. Enumerating
// key names lets us reset more-specific URL auth sections as well as host ones.
func brokerConfig(ctx context.Context, helper string) ([]string, error) {
	probe := gitCommand(ctx, "config", "--null", "--name-only", "--get-regexp", `^(credential|http)\.`)
	raw, err := probe.Output()
	if err != nil && commandExit(err) != 1 {
		return nil, errors.New("cannot inspect Git authorization configuration")
	}
	resets := map[string]string{"credential.helper": "", "credential.username": "x-access-token", "credential.usehttppath": "true", "credential.interactive": "false", "http.extraheader": "", "http.proactiveauth": "basic", "http.emptyauth": "false"}
	args := []string{}
	seen := map[string]bool{}
	for _, key := range strings.Split(string(raw), "\x00") {
		lower := strings.ToLower(key)
		for base, value := range resets {
			section, variable, _ := strings.Cut(base, ".")
			if lower == base || strings.HasPrefix(lower, section+".") && strings.HasSuffix(lower, "."+variable) {
				if !seen[key] {
					args = append(args, "-c", key+"="+value)
					seen[key] = true
				}
				break
			}
		}
	}
	// The final helper reset also clears every matched URL-scoped helper before
	// adding the ephemeral client. Tokens never enter an argv or config file.
	for _, entry := range [][2]string{{"credential.helper", ""}, {"credential.helper", helper}, {"credential.username", "x-access-token"}, {"credential.useHttpPath", "true"}, {"credential.interactive", "false"}, {"http.extraHeader", ""}, {"http.proactiveAuth", "basic"}, {"http.emptyAuth", "false"}} {
		args = append(args, "-c", entry[0]+"="+entry[1])
	}
	return args, nil
}

func startCurl(ctx context.Context, name, target, token string, tentative bool, options []gitOption, stderr io.Writer) (*curlRemote, error) {
	// git --exec-path locates the stock helper, without a PATH shim or reimplementation.
	probe := gitCommand(ctx, "--exec-path")
	data, err := probe.Output()
	if err != nil {
		return nil, err
	}
	cmd := gitCommand(ctx)
	cmd.Path = strings.TrimSpace(string(data)) + "/git-remote-https"
	cmd.Args = []string{cmd.Path, name, target}
	if token != "" {
		executable, e := os.Executable()
		if e != nil {
			return nil, e
		}
		helper := "!" + shellQuote(executable) + " _github-git-credential"
		authorization, configErr := brokerConfig(ctx, helper)
		err = configErr
		if err != nil {
			return nil, err
		}
		cmd = gitCommand(ctx, append(authorization, "remote-https", name, target)...)
		for _, entry := range os.Environ() {
			key, _, _ := strings.Cut(entry, "=")
			if !strings.HasPrefix(key, "GIT_TRACE") && key != "GIT_CURL_VERBOSE" {
				cmd.Env = append(cmd.Env, entry)
			}
		}
		cmd.Env = append(cmd.Env, ephemeralToken+"="+token, "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never")
	}
	input, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		input.Close()
		return nil, err
	}
	diagnostics := &gitDiagnostics{output: stderr, enabled: !tentative}
	if token != "" {
		diagnostics.sensitive = []string{token, base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token)), target, strings.Replace(target, "x-access-token@", "", 1)}
	}
	cmd.Stderr = diagnostics
	if err = cmd.Start(); err != nil {
		input.Close()
		return nil, err
	}
	remote := &curlRemote{cmd: cmd, input: input, output: bufio.NewReader(output), diagnostics: diagnostics}
	if _, err = remote.exchange("capabilities", true); err != nil {
		remote.close()
		return nil, err
	}
	for _, option := range options {
		response, e := remote.exchange(option.line, false)
		if e != nil || response != "ok\n" {
			remote.close()
			return nil, errors.New("HTTPS transport cannot apply requested option")
		}
	}
	return remote, nil
}

func (remote *curlRemote) exchange(line string, block bool) (string, error) {
	if _, err := fmt.Fprintln(remote.input, line); err != nil {
		return "", err
	}
	var response strings.Builder
	for {
		value, err := remote.output.ReadString('\n')
		if err != nil {
			return "", err
		}
		if response.Len()+len(value) > 64<<20 {
			return "", errors.New("Git ref response is too large")
		}
		response.WriteString(value)
		if !block || value == "\n" {
			return response.String(), nil
		}
	}
}
func (remote *curlRemote) close() error {
	_, _ = fmt.Fprintln(remote.input)
	_ = remote.input.Close()
	err := remote.cmd.Wait()
	if remote.diagnostics != nil {
		remote.diagnostics.flush()
	}
	return err
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }

func fallbackReason(state string) string {
	switch state {
	case "no-repo-access":
		return "GitHub App has no repository access; using original authentication"
	case "insufficient-permissions":
		return "GitHub App permissions are insufficient; using original authentication"
	case "unknown":
		return "GitHub App permissions are unknown; using original authentication"
	case "discovery-failed":
		return "GitHub broker ref discovery failed; using original authentication"
	default:
		return "GitHub broker is unavailable; using original authentication"
	}
}

func nativeList(ctx context.Context, target string, push bool, stderr io.Writer) (string, error) {
	args := []string{"-c", "url." + target + ".insteadOf=" + target, "-c", "protocol.version=0", "ls-remote", "--symref"}
	if push {
		args = append(args, "--upload-pack=git-receive-pack")
	}
	cmd := gitCommand(ctx, append(args, "--", target)...)
	cmd.Stderr = stderr
	raw, err := cmd.Output()
	if err != nil {
		return "", err
	}
	var response strings.Builder
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		value, name, ok := strings.Cut(line, "\t")
		if !ok {
			return "", errors.New("invalid native Git refs")
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		if strings.HasPrefix(value, "ref: ") {
			value = "@" + strings.TrimPrefix(value, "ref: ")
		}
		fmt.Fprintf(&response, "%s %s\n", value, name)
	}
	return response.String() + "\n", nil
}

func nativeBatch(ctx context.Context, target, verb string, batch []string, options []gitOption, stderr io.Writer) (string, error) {
	args := []string{}
	values := map[string]string{}
	for _, option := range options {
		values[option.name] = option.value
	}
	if verb == "fetch" {
		args = []string{"fetch-pack", "--stdin", "--lock-pack"}
		if values["progress"] == "false" {
			args = append(args, "--no-progress")
		}
		if values["cloning"] == "true" {
			args = append(args, "--cloning")
		}
		if values["check-connectivity"] == "true" {
			args = append(args, "--check-self-contained-and-connected")
		}
		if values["followtags"] == "true" {
			args = append(args, "--include-tag")
		}
		if values["deepen-relative"] == "true" {
			args = append(args, "--deepen-relative")
		}
		for _, key := range []string{"depth", "deepen-since", "filter"} {
			if value := values[key]; value != "" {
				flag := key
				if key == "deepen-since" {
					flag = "shallow-since"
				}
				args = append(args, "--"+flag+"="+value)
			}
		}
		for _, option := range options {
			if option.name == "deepen-not" {
				args = append(args, "--shallow-exclude="+option.value)
			}
		}
		// fetch-pack accepts OID/ref pairs on stdin and keeps the native pack engine.
		for index, line := range batch {
			fields := strings.SplitN(line, " ", 3)
			if len(fields) != 3 {
				return "", errors.New("invalid Git fetch batch")
			}
			batch[index] = fields[1] + " " + fields[2]
		}
	} else {
		args = []string{"send-pack", "--stdin", "--helper-status"}
		if values["atomic"] == "true" {
			args = append(args, "--atomic")
		}
		if values["dry-run"] == "true" {
			args = append(args, "--dry-run")
		}
		if values["progress"] == "false" {
			args = append(args, "--quiet")
		}
		for _, option := range options {
			switch option.name {
			case "cas":
				args = append(args, "--force-with-lease="+option.value)
			case "push-option":
				args = append(args, "--push-option="+option.value)
			}
		}
		for index, line := range batch {
			batch[index] = strings.TrimPrefix(line, "push ")
		}
	}
	cmd := gitCommand(ctx, append(args, target)...)
	cmd.Stdin = strings.NewReader(strings.Join(batch, "\n") + "\n")
	cmd.Stderr = stderr
	raw, err := cmd.Output()
	if verb == "push" {
		return string(raw) + "\n", err
	}
	var response strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "lock ") || line == "connectivity-ok" {
			fmt.Fprintln(&response, line)
		}
	}
	return response.String() + "\n", err
}

// RunGitRemote implements only the standard helper command protocol. Parent Git
// retains hooks, refspec expansion, tracking refs, policy and command ownership.
func RunGitRemote(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 2 {
		fmt.Fprintln(stderr, "subyard-github: invalid remote helper invocation")
		return 2
	}
	done := make(chan struct{})
	defer close(done)
	if closer, ok := stdin.(io.Closer); ok {
		go func() {
			select {
			case <-ctx.Done():
				_ = closer.Close()
			case <-done:
			}
		}()
	}
	repository, https, err := gitRemoteURL(args[1])
	if err != nil {
		fmt.Fprintln(stderr, "subyard-github:", err)
		return 2
	}
	explicit := os.Getenv(runContext) != ""
	explicitClient := Client{}
	if explicit {
		var context gitRunContext
		if json.Unmarshal([]byte(os.Getenv(runContext)), &context) != nil || context.Version != 1 || context.Socket == "" || strings.ContainsAny(context.Socket, "\x00\r\n") {
			fmt.Fprintln(stderr, "subyard-github: invalid explicit Git authorization context")
			return 2
		}
		explicitClient.SocketPath = context.Socket
	}
	modeQuery := gitCommand(ctx, "config", "--get", "subyard.githubGitMode")
	mode, _ := modeQuery.Output()
	brokerLast := !explicit && strings.TrimSpace(string(mode)) == "broker-last"
	options := []gitOption{}
	var remote *curlRemote
	defer func() {
		if remote != nil {
			_ = remote.close()
		}
	}()
	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	selected := false
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			break
		}
		switch {
		case line == "capabilities":
			fmt.Fprint(stdout, "fetch\npush\noption\ncheck-connectivity\nobject-format\n\n")
		case strings.HasPrefix(line, "option "):
			option, response := parseGitOption(line)
			if response == "ok" {
				if remote != nil {
					value, e := remote.exchange(line, false)
					if e != nil {
						return 1
					}
					fmt.Fprint(stdout, value)
					if value == "ok\n" {
						options = append(options, option)
					}
					continue
				}
				options = append(options, option)
			}
			fmt.Fprintln(stdout, response)
		case line == "list" || line == "list for-push":
			if selected {
				fmt.Fprintln(stderr, "subyard-github: repeated ref discovery is unsupported")
				return 1
			}
			operation := "read"
			if line == "list for-push" {
				operation = "push"
			}
			var refs string
			discoverOriginal := func(diagnostics io.Writer) {
				if https {
					remote, err = startCurl(ctx, args[0], args[1], "", brokerLast, options, stderr)
					if err == nil {
						refs, err = remote.exchange(line, true)
					}
				} else {
					refs, err = nativeList(ctx, args[1], operation == "push", diagnostics)
				}
			}
			if brokerLast {
				discoverOriginal(io.Discard)
				if err != nil {
					if remote != nil {
						_ = remote.close()
						remote = nil
					}
					if ctx.Err() != nil {
						return commandExit(ctx.Err())
					}
					refs, err = "", nil
					fmt.Fprintln(stderr, "subyard-github: original Git ref discovery failed; trying GitHub broker")
				}
			}
			if ctx.Err() != nil {
				return commandExit(ctx.Err())
			}
			if explicit {
				// Explicit run retains its existing HTTPS-only credential policy.
				if https {
					token, tokenErr := explicitClient.token(ctx, repository)
					err = tokenErr
					if err == nil {
						remote, err = startCurl(ctx, args[0], "https://x-access-token@github.com/"+repository+".git", token.Value, true, options, stderr)
					}
					if err != nil {
						fmt.Fprintln(stderr, "subyard-github: explicit GitHub broker authorization failed")
						return commandExit(err)
					}
				}
			} else if refs == "" {
				access := (Client{}).gitAccess(ctx, repository, operation)
				if ctx.Err() != nil {
					return commandExit(ctx.Err())
				}
				if access.State == "available" {
					remote, err = startCurl(ctx, args[0], "https://x-access-token@github.com/"+repository+".git", access.Token.Value, true, options, stderr)
					if err == nil {
						refs, err = remote.exchange(line, true)
					}
					if err != nil {
						if remote != nil {
							_ = remote.close()
							remote = nil
						}
						if !brokerLast {
							err = nil
							fmt.Fprintln(stderr, "subyard-github: "+fallbackReason("discovery-failed"))
						}
					}
				} else if brokerLast {
					err = errors.New("GitHub broker authorization unavailable")
					fmt.Fprintln(stderr, "subyard-github: GitHub broker could not authorize the repository")
				} else {
					fmt.Fprintln(stderr, "subyard-github: "+fallbackReason(access.State))
				}
			}
			if refs == "" && err == nil && !brokerLast {
				if ctx.Err() != nil {
					return commandExit(ctx.Err())
				}
				if remote != nil {
					refs, err = remote.exchange(line, true)
				} else {
					discoverOriginal(stderr)
				}
			}
			if err != nil {
				fmt.Fprintln(stderr, "subyard-github: Git ref discovery failed")
				return commandExit(err)
			}
			selected = true
			if remote != nil && remote.diagnostics != nil {
				remote.diagnostics.selectBackend()
			}
			if remote == nil {
				for _, option := range options {
					if option.name == "object-format" {
						format := "sha1"
						for _, ref := range strings.Split(refs, "\n") {
							value, _, _ := strings.Cut(ref, " ")
							if len(value) == 64 {
								format = "sha256"
							}
						}
						fmt.Fprintln(stdout, ":object-format "+format)
						break
					}
				}
			}
			fmt.Fprint(stdout, refs)
		case strings.HasPrefix(line, "fetch ") || strings.HasPrefix(line, "push "):
			if !selected {
				fmt.Fprintln(stderr, "subyard-github: batch before ref discovery")
				return 1
			}
			verb, _, _ := strings.Cut(line, " ")
			batch := []string{line}
			for scanner.Scan() {
				if scanner.Text() == "" {
					break
				}
				if !strings.HasPrefix(scanner.Text(), verb+" ") {
					return 2
				}
				batch = append(batch, scanner.Text())
			}
			if scanner.Err() != nil {
				return 1
			}
			var response string
			if remote != nil {
				response, err = remote.exchange(strings.Join(batch, "\n")+"\n", true)
			} else {
				response, err = nativeBatch(ctx, args[1], verb, batch, options, stderr)
			}
			fmt.Fprint(stdout, response)
			if err != nil {
				fmt.Fprintln(stderr, "subyard-github: selected Git transport failed; operation was not retried")
				return commandExit(err)
			}
		default:
			fmt.Fprintln(stderr, "subyard-github: unsupported remote helper command")
			return 2
		}
	}
	if scanner.Err() != nil {
		return 1
	}
	if remote != nil {
		current := remote
		remote = nil
		return commandExit(current.close())
	}
	return 0
}

func RunGitCredential(args []string, stdin io.Reader, stdout io.Writer) int {
	if len(args) != 1 || (args[0] != "get" && args[0] != "store" && args[0] != "erase") {
		return 2
	}
	if _, err := readCredentialInput(stdin); err != nil {
		return 1
	}
	if args[0] == "get" {
		token := os.Getenv(ephemeralToken)
		if !tokenPattern.MatchString(token) {
			return 1
		}
		fmt.Fprintf(stdout, "username=x-access-token\npassword=%s\n\n", token)
	}
	return 0
}

func gitStatus(ctx context.Context, stdout io.Writer) int {
	query := func(key string) string {
		cmd := gitCommand(ctx, "config", "--get", key)
		raw, _ := cmd.Output()
		return strings.TrimSpace(string(raw))
	}
	mode := query("subyard.githubGitMode")
	if mode != "broker-first" && mode != "broker-last" {
		mode = "off"
	}
	state := "current"
	override := os.Getenv("GIT_CONFIG_GLOBAL") != "" || os.Getenv("XDG_CONFIG_HOME") != "" && os.Getenv("XDG_CONFIG_HOME") != os.Getenv("HOME")+"/.config"
	if override {
		state = "overridden"
	}
	for _, prefix := range []string{"https://github.com/", "git@github.com:", "ssh://git@github.com/"} {
		if mode != "off" && query("url.subyard-github::"+prefix+".insteadOf") != prefix {
			state = "overridden"
		}
	}
	_ = json.NewEncoder(stdout).Encode(map[string]any{"mode": mode, "state": state, "global_config_override": override})
	return 0
}
