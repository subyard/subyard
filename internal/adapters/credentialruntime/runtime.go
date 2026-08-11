package credentialruntime

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
)

const (
	credentialSchema = 1
	maximumPayload   = 1 << 20
	maximumOutput    = 4 << 20
)

type Target struct {
	Name          string
	Transport     string
	Destination   string
	OwnerYardName string
}

type Resolver func(context.Context, string) (Target, error)

type Config struct {
	RepositoryRoot    string
	Root              string
	ConsumerRoot      string
	ToolsDirectory    string
	HostBase          string
	Context           string
	Dispatcher        string
	Environment       []string
	TargetEnvironment []string
	Stdin             io.Reader
	Stdout            io.Writer
	Stderr            io.Writer
	Resolve           Resolver
}

type Runtime struct {
	config Config
	env    map[string]string

	idDirectory    string
	ageIdentity    string
	signingKey     string
	signingPublic  string
	identityFile   string
	allowedSigners string
	peersDirectory string
	stateDirectory string
	quarantine     string
	shared         string
	sharedBare     string
	local          string
	lockFile       string

	sops       string
	ageKeygen  string
	sshKeygen  string
	git        string
	sshTimeout int
}

type Identity struct {
	SchemaVersion int    `json:"schemaVersion"`
	ActorID       string `json:"actorId"`
	IdentityScope string `json:"identityScope"`
	AgeRecipient  string `json:"ageRecipient"`
	SigningPublic string `json:"signingPublic"`
}

func New(config Config) (*Runtime, error) {
	for name, value := range map[string]string{
		"repository root": config.RepositoryRoot,
		"credential root": config.Root,
		"consumer root":   config.ConsumerRoot,
		"host base":       config.HostBase,
	} {
		if value == "" || !filepath.IsAbs(value) {
			return nil, fmt.Errorf("%s must be absolute", name)
		}
	}
	if !domain.SafeName(config.Context) {
		return nil, fmt.Errorf("invalid credential yard context %q", config.Context)
	}
	if config.Dispatcher == "" {
		config.Dispatcher = filepath.Join(config.RepositoryRoot, "bin", "yard")
	}
	if config.TargetEnvironment == nil {
		config.TargetEnvironment = append([]string(nil), config.Environment...)
	}
	if config.Stdin == nil {
		config.Stdin = strings.NewReader("")
	}
	if config.Stdout == nil {
		config.Stdout = io.Discard
	}
	if config.Stderr == nil {
		config.Stderr = io.Discard
	}
	environment := environmentMap(config.Environment)
	tools := config.ToolsDirectory
	if tools == "" {
		tools = filepath.Join(environment["SUBYARD_HOME"], "tools")
	}
	if !filepath.IsAbs(tools) {
		return nil, errors.New("credential tools directory must be absolute")
	}
	runtime := &Runtime{config: config, env: environment}
	runtime.idDirectory = filepath.Join(config.Root, "identity")
	runtime.ageIdentity = filepath.Join(runtime.idDirectory, "age.txt")
	runtime.signingKey = filepath.Join(runtime.idDirectory, "signing_ed25519")
	runtime.signingPublic = runtime.signingKey + ".pub"
	runtime.identityFile = filepath.Join(config.Root, "identity.json")
	runtime.allowedSigners = filepath.Join(config.Root, "allowed_signers")
	runtime.peersDirectory = filepath.Join(config.Root, "peers")
	runtime.stateDirectory = filepath.Join(config.Root, "state")
	runtime.quarantine = filepath.Join(config.Root, "quarantine")
	runtime.shared = filepath.Join(config.Root, "shared")
	runtime.sharedBare = filepath.Join(config.Root, "shared.git")
	runtime.local = filepath.Join(config.Root, "local")
	runtime.lockFile = filepath.Join(config.Root, "ledger.lock")
	runtime.sops = firstNonEmpty(environment["SUBYARD_SOPS_BIN"], filepath.Join(tools, "bin", "sops"))
	runtime.ageKeygen = firstNonEmpty(environment["SUBYARD_AGE_KEYGEN_BIN"], filepath.Join(tools, "bin", "age-keygen"))
	runtime.sshKeygen = firstNonEmpty(environment["SUBYARD_SSH_KEYGEN_BIN"], "ssh-keygen")
	runtime.git = firstNonEmpty(environment["SUBYARD_GIT_BIN"], "git")
	runtime.sshTimeout = positiveInt(environment["SUBYARD_KEYS_SSH_TIMEOUT"], 8)
	return runtime, nil
}

func (runtime *Runtime) Initialized() bool {
	for _, path := range []string{runtime.identityFile, runtime.ageIdentity, runtime.signingKey} {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return false
		}
	}
	for _, path := range []string{
		filepath.Join(runtime.shared, ".git"), runtime.sharedBare, filepath.Join(runtime.local, ".git"),
	} {
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return false
		}
	}
	return true
}

func (runtime *Runtime) Initialize(ctx context.Context) error {
	if err := runtime.assertBoundary(); err != nil {
		return err
	}
	if err := runtime.requireTools(); err != nil {
		return err
	}
	if runtime.Initialized() {
		fmt.Fprintf(runtime.config.Stderr, "  [ ok ] host credential ledger already initialized at %s\n", runtime.config.Root)
		return nil
	}
	if _, err := os.Lstat(runtime.ageIdentity); err == nil {
		return fmt.Errorf("incomplete key identity already exists at %s", runtime.ageIdentity)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, directory := range []string{
		runtime.config.Root, runtime.idDirectory, runtime.peersDirectory,
		runtime.stateDirectory, runtime.quarantine,
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return err
		}
	}
	if _, err := runtime.run(ctx, runtime.ageKeygen, []string{"-o", runtime.ageIdentity}, nil, nil); err != nil {
		return fmt.Errorf("generate age identity: %w", err)
	}
	if err := os.Chmod(runtime.ageIdentity, 0o600); err != nil {
		return err
	}
	if _, err := runtime.run(ctx, runtime.sshKeygen,
		[]string{"-q", "-t", "ed25519", "-N", "", "-C", "subyard-credentials-host", "-f", runtime.signingKey}, nil, nil); err != nil {
		return fmt.Errorf("generate credential signing key: %w", err)
	}
	if err := os.Chmod(runtime.signingKey, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(runtime.signingPublic, 0o644); err != nil {
		return err
	}
	public, err := os.ReadFile(runtime.signingPublic)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(public)
	actor := "host-" + hex.EncodeToString(digest[:8])
	recipientOutput, err := runtime.run(ctx, runtime.ageKeygen, []string{"-y", runtime.ageIdentity}, nil, nil)
	if err != nil {
		return fmt.Errorf("derive age recipient: %w", err)
	}
	identity := Identity{
		SchemaVersion: credentialSchema, ActorID: actor, IdentityScope: "host",
		AgeRecipient: strings.TrimSpace(string(recipientOutput)), SigningPublic: strings.TrimSpace(string(public)),
	}
	if err := validateIdentity(identity); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(identity, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicWrite(runtime.identityFile, append(payload, '\n'), 0o600); err != nil {
		return err
	}
	if err := atomicWrite(runtime.allowedSigners, nil, 0o600); err != nil {
		return err
	}
	if err := runtime.addAllowedSigner(identity.ActorID, identity.SigningPublic); err != nil {
		return err
	}
	if err := runtime.initRepository(ctx, runtime.shared, runtime.sharedBare); err != nil {
		return err
	}
	if err := runtime.initRepository(ctx, runtime.local, ""); err != nil {
		return err
	}
	if err := atomicWrite(filepath.Join(runtime.stateDirectory, "counter"), []byte("0\n"), 0o600); err != nil {
		return err
	}
	fmt.Fprintf(runtime.config.Stderr, "  [ ok ] initialized host-only credential ledger at %s\n", runtime.config.Root)
	return nil
}

func (runtime *Runtime) Identity() (Identity, error) {
	if err := runtime.requireInitialized(); err != nil {
		return Identity{}, err
	}
	return runtime.rawIdentity()
}

func (runtime *Runtime) rawIdentity() (Identity, error) {
	var identity Identity
	if err := readProtectedJSON(runtime.identityFile, &identity); err != nil {
		return Identity{}, err
	}
	if err := validateIdentity(identity); err != nil {
		return Identity{}, err
	}
	return identity, nil
}

func (runtime *Runtime) assertBoundary() error {
	root, err := canonicalPath(runtime.config.Root)
	if err != nil {
		return err
	}
	repository, err := canonicalPath(runtime.config.RepositoryRoot)
	if err != nil {
		return err
	}
	hostBase, err := canonicalPath(runtime.config.HostBase)
	if err != nil {
		return err
	}
	if root == string(filepath.Separator) {
		return errors.New("SUBYARD_KEYS_ROOT cannot be the filesystem root")
	}
	if pathWithin(root, repository) {
		return fmt.Errorf("SUBYARD_KEYS_ROOT must stay outside the Subyard checkout: %s", root)
	}
	if pathWithin(root, hostBase) {
		return fmt.Errorf("SUBYARD_KEYS_ROOT must stay outside HOST_BASE and every managed yard mount: %s", root)
	}
	return nil
}

func (runtime *Runtime) requireInitialized() error {
	if err := runtime.assertBoundary(); err != nil {
		return err
	}
	if !runtime.Initialized() {
		return errors.New("host credential ledger is not initialized — run: yard init")
	}
	return runtime.requireTools()
}

func (runtime *Runtime) requireTools() error {
	for name, path := range map[string]string{
		"git": runtime.git, "ssh-keygen": runtime.sshKeygen,
	} {
		if _, err := exec.LookPath(path); err != nil {
			return fmt.Errorf("%s is required for encrypted credential ledgers", name)
		}
	}
	for name, path := range map[string]string{"SOPS": runtime.sops, "age": runtime.ageKeygen} {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("pinned %s is missing — run: yard init", name)
		}
	}
	return nil
}

func (runtime *Runtime) withLock(ctx context.Context, run func() error) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if err := os.MkdirAll(runtime.config.Root, 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(runtime.lockFile, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN) //nolint:errcheck
	return run()
}

func (runtime *Runtime) nextCounter() (int64, error) {
	path := filepath.Join(runtime.stateDirectory, "counter")
	payload, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	current := int64(0)
	if len(payload) != 0 {
		current, err = strconv.ParseInt(strings.TrimSpace(string(payload)), 10, 64)
		if err != nil || current < 0 {
			return 0, errors.New("invalid key actor counter")
		}
	}
	current++
	if err := atomicWrite(path, []byte(strconv.FormatInt(current, 10)+"\n"), 0o600); err != nil {
		return 0, err
	}
	return current, nil
}

func (runtime *Runtime) initRepository(ctx context.Context, repository, origin string) error {
	if origin != "" {
		if _, err := runtime.gitRun(ctx, "", "init", "--bare", "--initial-branch=main", origin); err != nil {
			return err
		}
		if _, err := runtime.gitRun(ctx, "", "clone", "-q", origin, repository); err != nil {
			return err
		}
	} else if _, err := runtime.gitRun(ctx, "", "init", "-q", "--initial-branch=main", repository); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(repository, "records"), 0o700); err != nil {
		return err
	}
	if err := atomicWrite(filepath.Join(repository, ".ledger"), nil, 0o600); err != nil {
		return err
	}
	if _, err := runtime.gitRun(ctx, repository, "add", ".ledger"); err != nil {
		return err
	}
	if _, err := runtime.gitSigned(ctx, repository, "commit", "--allow-empty", "-S", "-m", "Initialize encrypted credential ledger"); err != nil {
		return err
	}
	if origin != "" {
		_, err := runtime.gitRun(ctx, repository, "push", "-q", "-u", "origin", "main")
		return err
	}
	return nil
}

func (runtime *Runtime) gitRun(ctx context.Context, repository string, arguments ...string) ([]byte, error) {
	args := make([]string, 0, len(arguments)+3)
	if repository != "" {
		args = append(args, "-C", repository, "-c", "core.hooksPath=/dev/null")
	}
	args = append(args, arguments...)
	return runtime.run(ctx, runtime.git, args, nil, nil)
}

func (runtime *Runtime) gitSigned(ctx context.Context, repository string, arguments ...string) ([]byte, error) {
	identity, err := runtime.rawIdentity()
	if err != nil {
		return nil, err
	}
	args := []string{
		"-C", repository, "-c", "core.hooksPath=/dev/null",
		"-c", "user.name=" + identity.ActorID, "-c", "user.email=" + identity.ActorID + "@subyard.invalid",
		"-c", "gpg.format=ssh", "-c", "user.signingkey=" + runtime.signingKey,
		"-c", "gpg.ssh.allowedSignersFile=" + runtime.allowedSigners, "-c", "commit.gpgsign=true",
	}
	args = append(args, arguments...)
	return runtime.run(ctx, runtime.git, args, nil, nil)
}

func (runtime *Runtime) run(
	ctx context.Context,
	program string,
	arguments []string,
	stdin io.Reader,
	extraEnvironment map[string]string,
) ([]byte, error) {
	return runtime.runWithEnvironment(
		ctx, program, arguments, stdin, runtime.config.Environment, extraEnvironment,
	)
}

func (runtime *Runtime) runWithEnvironment(
	ctx context.Context,
	program string,
	arguments []string,
	stdin io.Reader,
	environment []string,
	extraEnvironment map[string]string,
) ([]byte, error) {
	command := exec.CommandContext(ctx, program, arguments...)
	command.Dir = runtime.config.RepositoryRoot
	command.Env = append([]string(nil), environment...)
	for key, value := range extraEnvironment {
		command.Env = append(command.Env, key+"="+value)
	}
	command.Stdin = stdin
	stdout := &limitedBuffer{limit: maximumOutput}
	stderr := &limitedBuffer{limit: maximumOutput}
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return nil, errors.New(message)
	}
	if stdout.exceeded || stderr.exceeded {
		return nil, errors.New("credential tool output exceeded limit")
	}
	return append([]byte(nil), stdout.Bytes()...), nil
}

func (runtime *Runtime) addAllowedSigner(actor, public string) error {
	if !domain.SafeID(actor) {
		return fmt.Errorf("invalid key actor ID %q", actor)
	}
	fields := strings.Fields(public)
	if len(fields) < 2 || fields[0] != "ssh-ed25519" {
		return fmt.Errorf("peer %q did not provide an ed25519 signing key", actor)
	}
	payload, err := os.ReadFile(runtime.allowedSigners)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, line := range strings.Split(string(payload), "\n") {
		parts := strings.Fields(line)
		if len(parts) != 0 && parts[0] == actor {
			return nil
		}
	}
	payload = append(payload, []byte(fmt.Sprintf("%s %s %s\n", actor, fields[0], fields[1]))...)
	return atomicWrite(runtime.allowedSigners, payload, 0o600)
}

func validateIdentity(identity Identity) error {
	if identity.SchemaVersion != credentialSchema || identity.IdentityScope != "host" ||
		!domain.SafeID(identity.ActorID) || !strings.HasPrefix(identity.AgeRecipient, "age1") ||
		strings.ContainsAny(identity.AgeRecipient, "\r\n\x00") ||
		!strings.HasPrefix(identity.SigningPublic, "ssh-ed25519 ") ||
		strings.ContainsAny(identity.SigningPublic, "\r\n\x00") {
		return errors.New("credential host identity is invalid")
	}
	return nil
}

func readProtectedJSON(path string, target any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return errors.New("protected JSON is not a private regular file")
	}
	if info.Size() > maximumOutput {
		return errors.New("protected JSON exceeds size limit")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, maximumOutput+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("protected JSON has trailing data")
	}
	return nil
}

func atomicWrite(path string, payload []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".subyard-write-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(payload); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func randomHex(bytesCount int) (string, error) {
	payload := make([]byte, bytesCount)
	if _, err := rand.Read(payload); err != nil {
		return "", err
	}
	return hex.EncodeToString(payload), nil
}

func canonicalPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	probe := absolute
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(probe)
		if err == nil {
			for index := len(suffix) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, suffix[index])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return absolute, nil
		}
		suffix = append(suffix, filepath.Base(probe))
		probe = parent
	}
}

func pathWithin(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func environmentMap(environment []string) map[string]string {
	result := make(map[string]string, len(environment))
	for _, pair := range environment {
		key, value, ok := strings.Cut(pair, "=")
		if ok {
			result[key] = value
		}
	}
	return result
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func positiveInt(value string, fallback int) int {
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 {
		return fallback
	}
	return parsed
}

type limitedBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (buffer *limitedBuffer) Write(payload []byte) (int, error) {
	written := len(payload)
	remaining := buffer.limit - buffer.Len()
	if remaining <= 0 {
		buffer.exceeded = true
		return written, nil
	}
	if len(payload) > remaining {
		payload = payload[:remaining]
		buffer.exceeded = true
	}
	_, _ = buffer.Buffer.Write(payload)
	return written, nil
}

func (runtime *Runtime) now() time.Time { return time.Now().UTC().Truncate(time.Second) }
