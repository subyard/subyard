package githubbroker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultSocketSuffix = ".local/share/subyard/github.sock"
	maxProtocolInput    = 16 << 10
	maxResponseBody     = 1 << 20
)

// Client talks to the owner-side GitHub broker over its Unix socket.
type Client struct {
	SocketPath string
}

func (client Client) socketPath() (string, error) {
	if client.SocketPath != "" {
		return client.SocketPath, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("cannot discover home directory")
	}
	return filepath.Join(home, defaultSocketSuffix), nil
}

func (client Client) httpClient(ctx context.Context) (*http.Client, error) {
	socket, err := client.socketPath()
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		DialContext: func(dialCtx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(dialCtx, "unix", socket)
		},
	}
	return &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}

func (client Client) request(ctx context.Context, method, path string, body io.Reader) ([]byte, int, error) {
	httpClient, err := client.httpClient(ctx)
	if err != nil {
		return nil, 0, err
	}
	defer httpClient.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, method, "http://github-broker"+path, body)
	if err != nil {
		return nil, 0, errors.New("create broker request")
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return nil, 0, errors.New("connect to GitHub broker")
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBody+1))
	if err != nil {
		return nil, response.StatusCode, errors.New("read broker response")
	}
	if len(payload) > maxResponseBody {
		return nil, response.StatusCode, errors.New("broker response is too large")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, response.StatusCode, errors.New("GitHub broker rejected the request")
	}
	return payload, response.StatusCode, nil
}

func (client Client) token(ctx context.Context) (Token, error) {
	body, _, err := client.request(ctx, http.MethodPost, "/token", nil)
	if err != nil {
		return Token{}, err
	}
	var token Token
	if err := json.Unmarshal(body, &token); err != nil {
		return Token{}, errors.New("invalid GitHub broker token response")
	}
	if len(token.Value) > 4096 || !tokenPattern.MatchString(token.Value) || token.ExpiresAt.IsZero() || !token.ExpiresAt.After(time.Now()) {
		return Token{}, errors.New("GitHub broker returned an expired token")
	}
	return token, nil
}

func runChild(ctx context.Context, command []string, token string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(command) == 0 {
		fmt.Fprintln(stderr, "github-broker: run requires a command")
		return 2
	}
	count := 0
	if raw := os.Getenv("GIT_CONFIG_COUNT"); raw != "" {
		var err error
		count, err = strconv.Atoi(raw)
		if err != nil || count < 0 || count > 256 {
			fmt.Fprintln(stderr, "github-broker: invalid inherited Git configuration count")
			return 2
		}
	}
	environment := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if ok && (key == "GH_TOKEN" || key == "GITHUB_TOKEN" || key == "GIT_CONFIG_COUNT") {
			continue
		}
		environment = append(environment, entry)
	}
	// Git reads these only for this command and its children. No credential store
	// or user configuration is changed. Store/erase are deliberately no-ops.
	const helper = `!f() { if [ "$1" = get ]; then printf 'username=x-access-token\npassword=%s\n\n' "$GH_TOKEN"; fi; }; f`
	environment = append(environment, "GH_TOKEN="+token,
		"GIT_CONFIG_COUNT="+strconv.Itoa(count+2),
		"GIT_CONFIG_KEY_"+strconv.Itoa(count)+"=credential.https://github.com.helper",
		"GIT_CONFIG_VALUE_"+strconv.Itoa(count)+"=",
		"GIT_CONFIG_KEY_"+strconv.Itoa(count+1)+"=credential.https://github.com.helper",
		"GIT_CONFIG_VALUE_"+strconv.Itoa(count+1)+"="+helper)

	process := exec.CommandContext(ctx, command[0], command[1:]...)
	process.Env = environment
	process.Stdin, process.Stdout, process.Stderr = stdin, stdout, stderr
	if err := process.Run(); err != nil {
		if ctx.Err() != nil {
			return 124
		}
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			return exitError.ExitCode()
		}
		fmt.Fprintln(stderr, "github-broker: command failed")
		return 1
	}
	return 0
}

type credentialInput struct {
	protocol string
	host     string
}

func readCredentialInput(input io.Reader) (credentialInput, error) {
	limited := &io.LimitedReader{R: input, N: maxProtocolInput + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 256), maxProtocolInput)
	result := credentialInput{}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			break
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.ContainsAny(key+value, "\r\x00") {
			return credentialInput{}, errors.New("invalid Git credential request")
		}
		switch key {
		case "protocol":
			result.protocol = value
		case "host":
			result.host = value
		}
	}
	if err := scanner.Err(); err != nil || limited.N == 0 {
		return credentialInput{}, errors.New("Git credential request is too large")
	}
	if result.protocol != "https" || result.host != "github.com" {
		return credentialInput{}, errors.New("GitHub credential helper only supports https://github.com")
	}
	return result, nil
}

func printHelp(output io.Writer) {
	fmt.Fprintln(output, "usage: subyard-github [--socket PATH] run -- COMMAND [ARGS...]")
	fmt.Fprintln(output, "       subyard-github [--socket PATH] credential get|store|erase")
	fmt.Fprintln(output, "       subyard-github [--socket PATH] status")
	fmt.Fprintln(output, "run supplies short-lived GitHub CLI and Git HTTPS authorization only to COMMAND.")
}

// RunClient executes the small user-facing broker client. It never prints a token
// except for the Git credential get protocol's password field on stdout.
func RunClient(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	client := Client{}
	for len(args) > 0 && args[0] == "--socket" {
		if len(args) < 2 || args[1] == "" {
			fmt.Fprintln(stderr, "github-broker: --socket requires a path")
			return 2
		}
		client.SocketPath, args = args[1], args[2:]
	}
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		printHelp(stdout)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	switch args[0] {
	case "run":
		if len(args) < 3 || args[1] != "--" {
			fmt.Fprintln(stderr, "github-broker: run requires -- COMMAND [ARGS...]")
			return 2
		}
		token, err := client.token(ctx)
		if err != nil {
			fmt.Fprintln(stderr, "github-broker:", err)
			return 1
		}
		return runChild(ctx, args[2:], token.Value, stdin, stdout, stderr)
	case "credential":
		if len(args) != 2 || (args[1] != "get" && args[1] != "store" && args[1] != "erase") {
			fmt.Fprintln(stderr, "github-broker: credential requires get, store, or erase")
			return 2
		}
		if _, err := readCredentialInput(stdin); err != nil {
			fmt.Fprintln(stderr, "github-broker:", err)
			return 1
		}
		if args[1] != "get" {
			return 0
		}
		token, err := client.token(ctx)
		if err != nil {
			fmt.Fprintln(stderr, "github-broker:", err)
			return 1
		}
		fmt.Fprintln(stdout, "protocol=https")
		fmt.Fprintln(stdout, "host=github.com")
		fmt.Fprintln(stdout, "username=x-access-token")
		fmt.Fprintln(stdout, "password="+token.Value)
		fmt.Fprintln(stdout)
		return 0
	case "status":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "github-broker: status takes no arguments")
			return 2
		}
		body, _, err := client.request(ctx, http.MethodGet, "/status", nil)
		if err != nil {
			fmt.Fprintln(stderr, "github-broker:", err)
			return 1
		}
		var status struct {
			Configured *bool `json:"configured"`
		}
		if json.Unmarshal(body, &status) != nil || status.Configured == nil {
			fmt.Fprintln(stderr, "github-broker: invalid broker status")
			return 1
		}
		_ = json.NewEncoder(stdout).Encode(status)
		return 0
	default:
		fmt.Fprintln(stderr, "github-broker: unknown command")
		return 2
	}
}
