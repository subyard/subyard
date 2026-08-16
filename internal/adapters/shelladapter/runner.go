package shelladapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

const (
	ProtocolSchema   = 1
	defaultMaxOutput = 1024 * 1024
	defaultMaxSecret = 1024 * 1024
)

type Runner struct {
	RepositoryRoot string
	Actions        map[string]map[string]Action
	ContextKeys    map[string]struct{}
	Diagnostics    io.Writer
	Path           string
	Timeout        time.Duration
	MaxOutput      int64
	MaxSecret      int64
}

type Action struct {
	Path    string
	Direct  bool
	Capture bool
	Timeout time.Duration
}

func (runner Runner) Run(ctx context.Context, request domain.AdapterRequest, secret io.Reader) (domain.AdapterResult, string, error) {
	action, err := runner.validate(request)
	if err != nil {
		return domain.AdapterResult{}, "", err
	}
	secretBytes, err := readSecret(secret, runner.secretLimit())
	if err != nil {
		return domain.AdapterResult{}, "", err
	}
	defer clear(secretBytes)
	envelope, err := json.Marshal(request)
	if err != nil {
		return domain.AdapterResult{}, "", fmt.Errorf("encode adapter request: %w", err)
	}
	metadataRead, metadataWrite, err := os.Pipe()
	if err != nil {
		return domain.AdapterResult{}, "", fmt.Errorf("open adapter metadata pipe: %w", err)
	}
	defer metadataRead.Close()
	defer metadataWrite.Close()

	callContext := ctx
	cancel := func() {}
	timeout := runner.Timeout
	if action.Timeout > 0 {
		timeout = action.Timeout
	}
	if timeout > 0 {
		callContext, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	direct := action.Direct
	commandArguments := make([]string, 0, len(request.Arguments)+1)
	if !direct {
		commandArguments = append(commandArguments, request.Action)
	}
	commandArguments = append(commandArguments, request.Arguments...)
	command := exec.CommandContext(callContext, action.Path, commandArguments...)
	command.Dir = runner.RepositoryRoot
	executablePath := runner.Path
	if executablePath == "" {
		executablePath = "/usr/sbin:/usr/bin:/sbin:/bin"
	}
	command.Env = []string{
		"PATH=" + executablePath,
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
		"SUBYARD_ADAPTER_SCHEMA=1",
		"SUBYARD_METADATA_FD=3",
		"SUBYARD_OPERATION_ID=" + request.OperationID,
	}
	contextKeys := make([]string, 0, len(request.Context))
	for key := range request.Context {
		contextKeys = append(contextKeys, key)
	}
	sort.Strings(contextKeys)
	for _, key := range contextKeys {
		command.Env = append(command.Env, key+"="+request.Context[key])
	}
	command.Stdin = bytes.NewReader(secretBytes)
	command.ExtraFiles = []*os.File{metadataRead}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	command.WaitDelay = 2 * time.Second
	limit := runner.outputLimit()
	stdout := &limitedBuffer{limit: limit}
	stderr := &limitedBuffer{limit: limit}
	var liveDiagnostics io.Writer
	if runner.Diagnostics != nil && len(secretBytes) == 0 && direct && !action.Capture {
		liveDiagnostics = &lockedWriter{writer: runner.Diagnostics}
	}
	if direct && liveDiagnostics != nil {
		command.Stdout = io.MultiWriter(stdout, liveDiagnostics)
		command.Stderr = io.MultiWriter(stderr, liveDiagnostics)
	} else {
		command.Stdout = stdout
		command.Stderr = stderr
	}
	if err := command.Start(); err != nil {
		return domain.AdapterResult{}, "", fmt.Errorf("start adapter: %w", err)
	}
	if _, err := metadataWrite.Write(envelope); err != nil {
		_ = command.Cancel()
		_ = command.Wait()
		return domain.AdapterResult{}, "", fmt.Errorf("send adapter metadata: %w", err)
	}
	if err := metadataWrite.Close(); err != nil {
		_ = command.Cancel()
		_ = command.Wait()
		return domain.AdapterResult{}, "", fmt.Errorf("close adapter metadata: %w", err)
	}
	waitErr := command.Wait()
	diagnostics := stderr.String()
	if direct {
		diagnostics = stdout.String() + diagnostics
	}
	redactedStderr := redactText(diagnostics, request, secretBytes)
	if liveDiagnostics != nil {
		redactedStderr = ""
	}
	if stdout.exceeded || stderr.exceeded {
		return domain.AdapterResult{}, redactedStderr, errors.New("adapter output exceeded limit")
	}
	if callContext.Err() != nil {
		return domain.AdapterResult{}, redactedStderr, fmt.Errorf("adapter cancelled: %w", context.Cause(callContext))
	}
	if waitErr != nil {
		return domain.AdapterResult{}, redactedStderr, fmt.Errorf("adapter failed: %w", waitErr)
	}
	if direct {
		return domain.AdapterResult{
			Schema: ProtocolSchema, OperationID: request.OperationID, Status: "ok",
			Output: map[string]any{"command": request.Action},
		}, redactedStderr, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	decoder.UseNumber()
	var result domain.AdapterResult
	if err := decoder.Decode(&result); err != nil {
		return domain.AdapterResult{}, redactedStderr, fmt.Errorf("decode adapter result: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return domain.AdapterResult{}, redactedStderr, errors.New("adapter returned trailing output")
	}
	if err := validateResult(request, result); err != nil {
		return domain.AdapterResult{}, redactedStderr, err
	}
	result.Output = redactMap(result.Output, request, secretBytes)
	return result, redactedStderr, nil
}

func (runner Runner) validate(request domain.AdapterRequest) (Action, error) {
	if request.Schema != ProtocolSchema {
		return Action{}, fmt.Errorf("unsupported adapter schema %d", request.Schema)
	}
	if !domain.SafeID(request.OperationID) || !domain.SafeName(request.Adapter) || !domain.SafeName(request.Action) {
		return Action{}, errors.New("invalid adapter request identity")
	}
	actions, ok := runner.Actions[request.Adapter]
	if !ok {
		return Action{}, fmt.Errorf("adapter %q is not allowed", request.Adapter)
	}
	action, ok := actions[request.Action]
	if !ok {
		return Action{}, fmt.Errorf("adapter action %q/%q is not allowed", request.Adapter, request.Action)
	}
	root, err := filepath.Abs(runner.RepositoryRoot)
	if err != nil {
		return Action{}, err
	}
	path, err := filepath.Abs(action.Path)
	if err != nil {
		return Action{}, err
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return Action{}, errors.New("adapter executable escapes repository root")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return Action{}, fmt.Errorf("inspect adapter executable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o111 == 0 {
		return Action{}, errors.New("adapter executable must be a non-symlink executable file")
	}
	for key, value := range request.Context {
		if _, ok := runner.ContextKeys[key]; !ok {
			return Action{}, fmt.Errorf("adapter context key %q is not allowed", key)
		}
		if !config.ValidVariable(key) || reservedEnvironmentKey(key) {
			return Action{}, fmt.Errorf("adapter context key %q is not a safe environment key", key)
		}
		if sensitiveKey(key) {
			return Action{}, fmt.Errorf("secret-like adapter context key %q is forbidden", key)
		}
		if strings.ContainsRune(value, 0) {
			return Action{}, fmt.Errorf("adapter context value %q contains NUL", key)
		}
	}
	if hasSensitiveValue(request.Input) {
		return Action{}, errors.New("secret-like fields are forbidden in adapter metadata")
	}
	if len(request.Arguments) > 256 {
		return Action{}, errors.New("adapter argument count exceeds limit")
	}
	for _, argument := range request.Arguments {
		if strings.ContainsRune(argument, 0) || len(argument) > 64*1024 {
			return Action{}, errors.New("adapter argument is invalid")
		}
	}
	action.Path = path
	return action, nil
}

func reservedEnvironmentKey(value string) bool {
	switch value {
	case "PATH", "LANG", "LC_ALL", "SUBYARD_ADAPTER_SCHEMA", "SUBYARD_METADATA_FD", "SUBYARD_OPERATION_ID":
		return true
	default:
		return false
	}
}

func validateResult(request domain.AdapterRequest, result domain.AdapterResult) error {
	if result.Schema != ProtocolSchema {
		return fmt.Errorf("unsupported adapter result schema %d", result.Schema)
	}
	if result.OperationID != request.OperationID {
		return errors.New("adapter result operation ID mismatch")
	}
	if result.Status != "ok" && result.Status != "error" {
		return fmt.Errorf("invalid adapter status %q", result.Status)
	}
	if result.Status == "error" && result.ErrorCode == "" {
		return errors.New("adapter error result requires an error code")
	}
	return nil
}

type limitedBuffer struct {
	buffer   bytes.Buffer
	limit    int64
	exceeded bool
}

type lockedWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (writer *lockedWriter) Write(value []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.writer.Write(value)
}

func (buffer *limitedBuffer) Write(value []byte) (int, error) {
	remaining := buffer.limit - int64(buffer.buffer.Len())
	if remaining <= 0 {
		buffer.exceeded = true
		return len(value), nil
	}
	write := value
	if int64(len(write)) > remaining {
		write = write[:remaining]
		buffer.exceeded = true
	}
	_, _ = buffer.buffer.Write(write)
	return len(value), nil
}

func (buffer *limitedBuffer) Bytes() []byte  { return buffer.buffer.Bytes() }
func (buffer *limitedBuffer) String() string { return buffer.buffer.String() }

func (runner Runner) outputLimit() int64 {
	if runner.MaxOutput > 0 {
		return runner.MaxOutput
	}
	return defaultMaxOutput
}

func (runner Runner) secretLimit() int64 {
	if runner.MaxSecret > 0 {
		return runner.MaxSecret
	}
	return defaultMaxSecret
}

func readSecret(reader io.Reader, limit int64) ([]byte, error) {
	if reader == nil {
		return nil, nil
	}
	value, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read protected adapter input: %w", err)
	}
	if int64(len(value)) > limit {
		return nil, errors.New("protected adapter input exceeded limit")
	}
	return value, nil
}

func hasSensitiveValue(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			normalized := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(key))
			if strings.Contains(normalized, "secret") || strings.Contains(normalized, "password") ||
				strings.Contains(normalized, "privatekey") || strings.Contains(normalized, "token") {
				return true
			}
			if hasSensitiveValue(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if hasSensitiveValue(child) {
				return true
			}
		}
	}
	return false
}

var credentialURL = regexp.MustCompile(`(?i)(://[^/:@\s]+:)[^/@\s]+@|(?i)(://)[^/:@\s]+@`)

func redactText(value string, request domain.AdapterRequest, secret []byte) string {
	value = credentialURL.ReplaceAllString(value, `${1}${2}***@`)
	for key, contextValue := range request.Context {
		if sensitiveKey(key) && contextValue != "" {
			value = strings.ReplaceAll(value, contextValue, "[REDACTED]")
		}
	}
	if len(secret) != 0 {
		value = strings.ReplaceAll(value, string(secret), "[REDACTED]")
	}
	return value
}

func redactMap(values map[string]any, request domain.AdapterRequest, secret []byte) map[string]any {
	if values == nil {
		return nil
	}
	redacted := make(map[string]any, len(values))
	for key, value := range values {
		if sensitiveKey(key) {
			redacted[key] = "[REDACTED]"
			continue
		}
		redacted[key] = redactValue(value, request, secret)
	}
	return redacted
}

func redactValue(value any, request domain.AdapterRequest, secret []byte) any {
	switch typed := value.(type) {
	case string:
		return redactText(typed, request, secret)
	case map[string]any:
		return redactMap(typed, request, secret)
	case []any:
		items := make([]any, len(typed))
		for index, item := range typed {
			items[index] = redactValue(item, request, secret)
		}
		return items
	default:
		return value
	}
}

func sensitiveKey(key string) bool {
	normalized := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(key))
	return strings.Contains(normalized, "secret") || strings.Contains(normalized, "password") ||
		strings.Contains(normalized, "privatekey") || strings.Contains(normalized, "token")
}
