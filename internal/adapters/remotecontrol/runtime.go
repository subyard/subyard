package remotecontrol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/transport"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerinventory"
	"github.com/Subyard/Subyard/internal/shellquote"
	"golang.org/x/crypto/ssh"
)

type Runtime struct {
	SSH, Home, ConfigHome, ConfigDir, DataHome, PublicKey string
	Environment                                           []string
	Timeout                                               time.Duration
	processCall                                           func(context.Context, string, []string, []byte) ([]byte, error)
}

func (runtime Runtime) Lookup(_ context.Context, name string) (domain.RemoteRecord, bool, error) {
	if name == "default" {
		return domain.RemoteRecord{Spec: domain.RemoteSpec{LegacyAlias: name}}, true, nil
	}
	var path string
	for _, candidate := range config.YardFileCandidates(runtime.ConfigDir, runtime.ConfigHome, name) {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			path = candidate
			break
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return domain.RemoteRecord{}, false, err
		}
	}
	if path == "" {
		return domain.RemoteRecord{}, false, nil
	}
	values, err := config.ReadAssignments(path)
	if err != nil {
		return domain.RemoteRecord{}, false, err
	}
	port, _ := strconv.Atoi(valueOr(values["REMOTE_SSH_PORT"], values["SSH_PORT"]))
	record := domain.RemoteRecord{
		Spec: domain.RemoteSpec{
			LegacyAlias:   name,
			OwnerEndpoint: valueOr(values["OWNER_ENDPOINT"], values["REMOTE_DEST"]),
			OwnerYardName: valueOr(values["OWNER_YARD_NAME"], values["REMOTE_YARD"]),
		},
		Remote: valueOr(values["ACCESS_KIND"], values["YARD_TYPE"]) == "remote",
		Path:   path, SSHPort: port,
	}
	if _, lastProbe, err := runtime.readCache(name); err == nil {
		record.LastProbe = lastProbe
	}
	return record, true, nil
}

func (runtime Runtime) List(ctx context.Context) ([]domain.RemoteRecord, error) {
	names, err := config.YardNames(config.RegistryDirectories(runtime.ConfigDir, runtime.ConfigHome)...)
	if err != nil {
		return nil, err
	}
	result := make([]domain.RemoteRecord, 0, len(names))
	for _, name := range names {
		record, exists, lookupErr := runtime.Lookup(ctx, name)
		if lookupErr != nil {
			return nil, lookupErr
		}
		if exists && record.Remote {
			result = append(result, record)
		}
	}
	return result, nil
}

func (runtime Runtime) ProbeOwner(ctx context.Context, spec domain.RemoteSpec) (domain.RemoteInfo, error) {
	process, err := transport.SSHYard(runtime.SSH, spec.OwnerEndpoint, "", runtime.timeout())
	if err != nil {
		return domain.RemoteInfo{}, err
	}
	process.Env = runtime.Environment
	inventory, err := (ownerinventory.Client{Transport: process}).Fetch(ctx, "")
	if err != nil {
		return domain.RemoteInfo{}, err
	}
	yardName := spec.OwnerYardName
	if yardName == "" {
		yardName = "default"
	}
	for _, yard := range inventory.Yards {
		if yard.Name != yardName {
			continue
		}
		count := len(yard.Projects)
		return domain.RemoteInfo{
			YardName: yard.Name, AccessKind: string(domain.AccessLocal), Version: "",
			YardInstanceName: yard.Instance, State: yard.State, SSHPort: yard.SSHPort,
			DevUser: yard.DevUser, Projects: &count,
		}, nil
	}
	return domain.RemoteInfo{}, fmt.Errorf(
		"owner HostID %q has no yard %q", inventory.HostID, yardName,
	)
}

func (runtime Runtime) ScanYardKeys(ctx context.Context, spec domain.RemoteSpec, port int) ([]domain.RemoteKey, error) {
	payload, err := runtime.hostCall(ctx, spec.OwnerEndpoint, nil, "ssh-keyscan",
		"-T", strconv.Itoa(runtime.timeoutSeconds()), "-p", strconv.Itoa(port), "127.0.0.1")
	if err != nil {
		return nil, err
	}
	return parseKeys(payload, "")
}

func (runtime Runtime) RecordedYardKeys(_ context.Context, name string) ([]domain.RemoteKey, error) {
	payload, err := readOptional(runtime.knownHostsPath())
	if err != nil {
		return nil, err
	}
	return parseKeys(payload, hostKeyAlias(name))
}

func (runtime Runtime) Apply(ctx context.Context, prepared domain.RemotePrepared) (domain.RemoteResult, error) {
	switch prepared.Action {
	case domain.RemoteList:
		return domain.RemoteResult{Records: prepared.Records}, nil
	case domain.RemoteAdd, domain.RemoteRepairKey, domain.RemoteRemove:
	default:
		return domain.RemoteResult{}, errors.New("unknown prepared remote action")
	}
	unlock, err := runtime.lockMutation(ctx)
	if err != nil {
		return domain.RemoteResult{}, err
	}
	defer unlock()
	if err := ctx.Err(); err != nil {
		return domain.RemoteResult{}, fmt.Errorf("apply remote mutation: %w", context.Cause(ctx))
	}
	switch prepared.Action {
	case domain.RemoteAdd:
		return runtime.applyAdd(ctx, prepared)
	case domain.RemoteRepairKey:
		return runtime.applyRepair(ctx, prepared)
	default:
		return runtime.applyRemove(ctx, prepared)
	}
}

func (runtime Runtime) lockMutation(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("lock remote mutation: %w", context.Cause(ctx))
	}
	directory := filepath.Join(runtime.Home, ".ssh")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("prepare remote mutation lock: %w", err)
	}
	path := filepath.Join(directory, ".subyard-remote.lock")
	fd, err := syscall.Open(path,
		syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open remote mutation lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("secure remote mutation lock: %w", err)
	}
	for {
		err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = file.Close()
			return nil, fmt.Errorf("lock remote mutation: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, fmt.Errorf("lock remote mutation: %w", context.Cause(ctx))
		case <-time.After(5 * time.Millisecond):
		}
	}
	return func() {
		_ = syscall.Flock(fd, syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}

func (runtime Runtime) applyAdd(ctx context.Context, prepared domain.RemotePrepared) (domain.RemoteResult, error) {
	if err := runtime.verifyCurrent(ctx, prepared); err != nil {
		return domain.RemoteResult{}, err
	}
	publicKey, identity, err := runtime.ensureIdentity(ctx)
	if err != nil {
		return domain.RemoteResult{}, err
	}
	if _, err := runtime.ownerCall(ctx, prepared.Spec, publicKey, "_authorize"); err != nil {
		return domain.RemoteResult{}, fmt.Errorf("authorize controller key: %w", err)
	}
	envPath := filepath.Join(runtime.ConfigHome, "yards", prepared.Spec.LegacyAlias, "config.env")
	if prepared.Existing != nil {
		envPath = prepared.Existing.Path
	}
	snippet, sshConfig, known, cache := runtime.snippetPath(prepared.Spec.LegacyAlias), runtime.sshConfigPath(), runtime.knownHostsPath(), runtime.cachePath(prepared.Spec.LegacyAlias)
	err = transactional([]string{envPath, snippet, sshConfig, known, cache}, func() error {
		current, err := readOptional(envPath)
		if err != nil {
			return err
		}
		configData, err := readOptional(sshConfig)
		if err != nil {
			return err
		}
		knownData, err := readOptional(known)
		if err != nil {
			return err
		}
		for path, payload := range map[string][]byte{
			envPath: renderContext(prepared, current), snippet: runtime.renderSnippet(prepared, identity),
			sshConfig: addInclude(configData, filepath.Base(snippet)), known: knownData,
		} {
			if err := atomicWrite(path, payload, 0o600); err != nil {
				return err
			}
		}
		if err := runtime.verifyData(ctx, prepared); err != nil {
			return err
		}
		return runtime.writeCache(cache, prepared.Owner)
	})
	if err != nil {
		return domain.RemoteResult{}, classifyProbe(err)
	}
	return domain.RemoteResult{Message: fmt.Sprintf(
		"remote yard %q is ready\nUse: yard -Y %s sync <project-dir>\nSecurity: the remote host can read everything explicitly synced into this yard", prepared.Spec.LegacyAlias, prepared.Spec.LegacyAlias)}, nil
}

func (runtime Runtime) applyRepair(ctx context.Context, prepared domain.RemotePrepared) (domain.RemoteResult, error) {
	if err := runtime.verifyCurrent(ctx, prepared); err != nil {
		return domain.RemoteResult{}, err
	}
	known := runtime.knownHostsPath()
	err := transactional([]string{known}, func() error {
		payload, err := readOptional(known)
		if err != nil {
			return err
		}
		if err := atomicWrite(known, removeKnownHost(payload, hostKeyAlias(prepared.Spec.LegacyAlias)), 0o600); err != nil {
			return err
		}
		return runtime.verifyData(ctx, prepared)
	})
	if err != nil {
		return domain.RemoteResult{}, classifyProbe(err)
	}
	return domain.RemoteResult{Message: fmt.Sprintf("remote yard ssh key %q rotated and verified", hostKeyAlias(prepared.Spec.LegacyAlias))}, nil
}

func (runtime Runtime) applyRemove(ctx context.Context, prepared domain.RemotePrepared) (domain.RemoteResult, error) {
	if err := runtime.verifyCurrent(ctx, prepared); err != nil {
		return domain.RemoteResult{}, err
	}
	envPath, snippet, sshConfig, known, cache := prepared.Existing.Path, runtime.snippetPath(prepared.Spec.LegacyAlias), runtime.sshConfigPath(), runtime.knownHostsPath(), runtime.cachePath(prepared.Spec.LegacyAlias)
	err := transactional([]string{envPath, snippet, sshConfig, known, cache}, func() error {
		configData, err := readOptional(sshConfig)
		if err != nil {
			return err
		}
		knownData, err := readOptional(known)
		if err != nil {
			return err
		}
		for _, path := range []string{envPath, snippet, cache} {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if len(configData) != 0 {
			if err := atomicWrite(sshConfig, removeInclude(configData, filepath.Base(snippet)), 0o600); err != nil {
				return err
			}
		}
		if len(knownData) != 0 {
			return atomicWrite(known, removeKnownHost(knownData, hostKeyAlias(prepared.Spec.LegacyAlias)), 0o600)
		}
		return nil
	})
	if err != nil {
		return domain.RemoteResult{}, err
	}
	return domain.RemoteResult{Message: fmt.Sprintf("remote yard %q unregistered; local project state kept", prepared.Spec.LegacyAlias)}, nil
}

func (runtime Runtime) verifyData(ctx context.Context, prepared domain.RemotePrepared) error {
	if err := runtime.probeData(ctx, prepared.Spec.LegacyAlias); err != nil {
		return err
	}
	accepted, err := runtime.RecordedYardKeys(ctx, prepared.Spec.LegacyAlias)
	if err == nil && !domain.RemoteKeysOverlap(accepted, prepared.Scanned) {
		return errors.New("key accepted through ProxyJump does not match the owner-host scan")
	}
	return err
}

func (runtime Runtime) verifyCurrent(ctx context.Context, prepared domain.RemotePrepared) error {
	record, exists, err := runtime.Lookup(ctx, prepared.Spec.LegacyAlias)
	if err != nil {
		return err
	}
	if prepared.Existing == nil {
		if exists {
			return errors.New("remote context changed after planning; prepare the command again")
		}
		return nil
	}
	if !exists || !record.Remote || record.Path != prepared.Existing.Path || record.Spec != prepared.Existing.Spec {
		return errors.New("remote context changed after planning; prepare the command again")
	}
	return nil
}

func (runtime Runtime) ownerCall(ctx context.Context, spec domain.RemoteSpec, stdin []byte, arguments ...string) ([]byte, error) {
	owner := []string{"yard"}
	if spec.OwnerYardName != "" {
		owner = append(owner, "-Y", spec.OwnerYardName)
	}
	owner = append(owner, arguments...)
	command := shellquote.Command(owner)
	return runtime.hostCall(ctx, spec.OwnerEndpoint, stdin, "bash", "-lc", shellquote.Word(command))
}

func (runtime Runtime) hostCall(ctx context.Context, destination string, stdin []byte, arguments ...string) ([]byte, error) {
	command := []string{"-T", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=" + strconv.Itoa(runtime.timeoutSeconds()), destination, "--"}
	return runtime.call(ctx, append(command, arguments...), stdin)
}

func (runtime Runtime) probeData(ctx context.Context, name string) error {
	_, err := runtime.call(ctx, []string{"-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=" + strconv.Itoa(runtime.timeoutSeconds()), "-o", "ControlMaster=no", "-o", "ControlPath=none", "yard-" + name, "--", "true"}, nil)
	return err
}

func (runtime Runtime) call(ctx context.Context, arguments []string, stdin []byte) ([]byte, error) {
	program := runtime.SSH
	if program == "" {
		program = "ssh"
	}
	if runtime.processCall != nil {
		return runtime.processCall(ctx, program, arguments, stdin)
	}
	return (transport.Process{Program: program, Arguments: arguments, Env: runtime.Environment, Timeout: runtime.timeout()}).Call(ctx, "", stdin)
}

func (runtime Runtime) ensureIdentity(ctx context.Context) ([]byte, string, error) {
	public := runtime.PublicKey
	if public == "" {
		for _, name := range []string{"id_ed25519.pub", "id_ecdsa.pub", "id_rsa.pub"} {
			candidate := filepath.Join(runtime.Home, ".ssh", name)
			if _, err := os.Stat(candidate); err == nil {
				public = candidate
				break
			}
		}
	}
	if public == "" {
		identity := filepath.Join(runtime.DataHome, "ssh", "id_ed25519")
		if err := os.MkdirAll(filepath.Dir(identity), 0o700); err != nil {
			return nil, "", err
		}
		if _, err := os.Stat(identity); errors.Is(err, os.ErrNotExist) {
			process := transport.Process{Program: "ssh-keygen", Arguments: []string{"-q", "-t", "ed25519", "-N", "", "-C", "subyard-remote", "-f", identity}, Env: runtime.Environment, Timeout: runtime.timeout()}
			if _, err := process.Call(ctx, "", nil); err != nil {
				return nil, "", err
			}
		}
		public = identity + ".pub"
	}
	identity, ok := strings.CutSuffix(public, ".pub")
	if !ok {
		return nil, "", errors.New("controller public key path must end in .pub")
	}
	payload, err := os.ReadFile(public)
	if err != nil {
		return nil, "", err
	}
	key, _, _, rest, err := ssh.ParseAuthorizedKey(payload)
	if err != nil || len(bytes.TrimSpace(rest)) != 0 {
		return nil, "", errors.New("controller public key is invalid")
	}
	return ssh.MarshalAuthorizedKey(key), identity, nil
}

func (runtime Runtime) renderSnippet(prepared domain.RemotePrepared, identity string) []byte {
	return []byte(fmt.Sprintf("# Managed by Subyard; regenerated by 'yard remote add'.\nHost yard-%s\n    HostName 127.0.0.1\n    Port %d\n    User %s\n    ProxyJump %s\n    HostKeyAlias %s\n    IdentityFile %s\n    IdentitiesOnly yes\n    ForwardAgent no\n    ControlMaster auto\n    ControlPath ~/.ssh/subyard-cm-%%C\n    ControlPersist 60s\n    StrictHostKeyChecking accept-new\n    HashKnownHosts no\n    UserKnownHostsFile %s\n", prepared.Spec.LegacyAlias, prepared.Owner.SSHPort, prepared.Owner.DevUser, prepared.Spec.OwnerEndpoint, hostKeyAlias(prepared.Spec.LegacyAlias), identity, runtime.knownHostsPath()))
}

func (runtime Runtime) writeCache(path string, info domain.RemoteInfo) error {
	if info.Projects == nil {
		if cached, _, err := runtime.readCachePath(path); err == nil {
			info.Projects = cached.Projects
		}
	}
	payload, err := json.Marshal(info)
	if err != nil {
		return err
	}
	return atomicWrite(path, []byte(fmt.Sprintf("%d\n%s\n", time.Now().Unix(), payload)), 0o600)
}

func (runtime Runtime) readCache(name string) (domain.RemoteInfo, time.Time, error) {
	return runtime.readCachePath(runtime.cachePath(name))
}

func (runtime Runtime) readCachePath(path string) (domain.RemoteInfo, time.Time, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return domain.RemoteInfo{}, time.Time{}, err
	}
	epochLine, jsonLine, ok := bytes.Cut(payload, []byte("\n"))
	if !ok {
		return domain.RemoteInfo{}, time.Time{}, errors.New("invalid remote status cache")
	}
	epoch, err := strconv.ParseInt(strings.TrimSpace(string(epochLine)), 10, 64)
	if err != nil {
		return domain.RemoteInfo{}, time.Time{}, err
	}
	var info domain.RemoteInfo
	if err := json.Unmarshal(bytes.TrimSpace(jsonLine), &info); err != nil {
		return domain.RemoteInfo{}, time.Time{}, err
	}
	return info, time.Unix(epoch, 0), nil
}

func (runtime Runtime) timeout() time.Duration {
	if runtime.Timeout > 0 {
		return runtime.Timeout
	}
	return 10 * time.Second
}
func (runtime Runtime) timeoutSeconds() int { return max(1, int(runtime.timeout()/time.Second)) }
func (runtime Runtime) snippetPath(name string) string {
	return filepath.Join(runtime.Home, ".ssh", "subyard-"+name+".config")
}
func (runtime Runtime) sshConfigPath() string { return filepath.Join(runtime.Home, ".ssh", "config") }
func (runtime Runtime) knownHostsPath() string {
	return filepath.Join(runtime.DataHome, "ssh", "known_hosts")
}
func (runtime Runtime) cachePath(name string) string {
	return filepath.Join(runtime.DataHome, "remote-"+name+".cache")
}

func renderContext(prepared domain.RemotePrepared, current []byte) []byte {
	const marker = "# --- Subyard user overrides (preserved by remote add) ---"
	overrides := ""
	if _, tail, ok := strings.Cut(string(current), marker+"\n"); ok {
		overrides = tail
	} else {
		managed := map[string]bool{
			"ACCESS_KIND": true, "OWNER_ENDPOINT": true, "OWNER_YARD_NAME": true,
			"YARD_TYPE": true, "REMOTE_DEST": true, "REMOTE_YARD": true,
			"REMOTE_SSH_PORT": true, "REMOTE_DEV_USER": true, "SSH_PORT": true,
		}
		for _, line := range strings.Split(string(current), "\n") {
			name, _, ok := strings.Cut(line, "=")
			if ok && !managed[name] && !(name == "FORWARD_SSH_AGENT" && line == "FORWARD_SSH_AGENT=0") {
				overrides += line + "\n"
			}
		}
	}
	return []byte(fmt.Sprintf("# Generated by 'yard remote add'.\nACCESS_KIND=remote\nOWNER_ENDPOINT=%s\nOWNER_YARD_NAME=%s\nREMOTE_SSH_PORT=%d\nREMOTE_DEV_USER=%s\nSSH_PORT=%d\nFORWARD_SSH_AGENT=0\n%s\n%s", prepared.Spec.OwnerEndpoint, prepared.Spec.OwnerYardName, prepared.Owner.SSHPort, prepared.Owner.DevUser, prepared.Owner.SSHPort, marker, overrides))
}

func addInclude(payload []byte, name string) []byte {
	return []byte("Include " + name + "\n" + string(removeInclude(payload, name)))
}
func removeInclude(payload []byte, name string) []byte {
	lines := strings.Split(string(payload), "\n")
	kept := lines[:0]
	for _, line := range lines {
		if line != "Include "+name {
			kept = append(kept, line)
		}
	}
	return []byte(strings.Join(kept, "\n"))
}

func hostKeyAlias(name string) string { return "subyard-remote-" + name }

func parseKeys(payload []byte, host string) ([]domain.RemoteKey, error) {
	seen := make(map[string]bool)
	var result []domain.RemoteKey
	for _, line := range bytes.Split(payload, []byte("\n")) {
		_, hosts, key, _, _, err := ssh.ParseKnownHosts(line)
		if err != nil || host != "" && !slices.Contains(hosts, host) {
			continue
		}
		material := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
		if !seen[material] {
			seen[material] = true
			result = append(result, domain.RemoteKey{Material: material, Fingerprint: ssh.FingerprintSHA256(key)})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Material < result[j].Material })
	return result, nil
}

func removeKnownHost(payload []byte, host string) []byte {
	lines := strings.Split(string(payload), "\n")
	kept := lines[:0]
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 0 || !slices.Contains(strings.Split(fields[0], ","), host) {
			kept = append(kept, line)
		}
	}
	return []byte(strings.Join(kept, "\n"))
}

func classifyProbe(err error) error {
	message := err.Error()
	switch {
	case strings.Contains(message, "Permission denied"):
		return errors.New("the yard sshd rejected the controller key")
	case strings.Contains(message, "REMOTE HOST IDENTIFICATION HAS CHANGED"), strings.Contains(message, "Host key verification failed"):
		return errors.New("yard ssh host key changed; refusing automatic replacement")
	case strings.Contains(message, "Connection refused"), strings.Contains(message, "stdio forwarding failed"):
		return errors.New("the yard loopback proxy or sshd failed the data-plane probe")
	default:
		return fmt.Errorf("data-plane probe failed: %w", err)
	}
}

type snapshot struct {
	path   string
	data   []byte
	mode   os.FileMode
	exists bool
}

func transactional(paths []string, apply func() error) error {
	files := make([]snapshot, 0, len(paths))
	for _, path := range paths {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			files = append(files, snapshot{path: path})
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("refusing non-regular transaction target %s", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files = append(files, snapshot{path: path, data: data, mode: info.Mode().Perm(), exists: true})
	}
	if err := apply(); err != nil {
		rollback := error(nil)
		for _, file := range files {
			if file.exists {
				rollback = errors.Join(rollback, atomicWrite(file.path, file.data, file.mode))
			} else if removeErr := os.Remove(file.path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				rollback = errors.Join(rollback, removeErr)
			}
		}
		return errors.Join(err, rollback)
	}
	return nil
}

func atomicWrite(path string, payload []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("refusing non-regular file %s", path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".subyard-remote-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer temporary.Close()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(mode); err != nil {
		return err
	}
	if _, err := temporary.Write(payload); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, path)
}

func readOptional(path string) ([]byte, error) {
	payload, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return payload, err
}

func valueOr(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
