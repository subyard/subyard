// Package sshtrust handles first trust before an SSH command is sent. OpenSSH
// resolves aliases and negotiates the key; existing trust is never replaced here.
package sshtrust

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/transport"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerinventory"
	"github.com/Subyard/Subyard/internal/safefs"
	"github.com/Subyard/Subyard/internal/shellquote"
	"golang.org/x/crypto/ssh"
)

type Proposal struct {
	Target, Namespace, File, Algorithm, Fingerprint string
}

type Manager struct {
	Environment []string
	// ReadOnly refuses unknown keys without assessment, confirmation or publication.
	ReadOnly  bool
	Confirm   func(context.Context, []Proposal) error
	Pin       func(string) (*ownerinventory.SSHHostTrust, error)
	Witness   func(context.Context, string, string, ssh.PublicKey) error
	mu        sync.Mutex
	temporary []string
	gateMu    sync.Mutex
	accepted  map[string][]string
	drafts    map[string][]string
	pending   []candidate
}

type candidate struct {
	proposal Proposal
	line     string
	files    []string
	verify   func() error
}

type activeGate struct{}

func (manager *Manager) Close() {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	for _, path := range manager.temporary {
		_ = os.RemoveAll(path)
	}
	manager.temporary = nil
}

func (manager *Manager) run(ctx context.Context, program string, args ...string) ([]byte, error) {
	return (transport.Process{Program: program, Arguments: args, Env: manager.Environment,
		Timeout: 10 * time.Second, MaxBytes: 1 << 20}).Call(ctx, "", nil)
}

func strictOptions() []string {
	return []string{"-o", "StrictHostKeyChecking=yes", "-o", "UpdateHostKeys=no",
		"-o", "ControlMaster=no", "-o", "ControlPath=none"}
}

func (manager *Manager) temporaryFile(data []byte) (string, error) {
	directory, err := os.MkdirTemp("", "subyard-ssh-trust-*")
	if err != nil {
		return "", err
	}
	manager.mu.Lock()
	manager.temporary = append(manager.temporary, directory)
	manager.mu.Unlock()
	path := filepath.Join(directory, "known_hosts")
	return path, safefs.WriteFile(path, data, safefs.Options{Mode: 0o600, Durability: safefs.BestEffort})
}

func pinnedOptions(path string) []string {
	return append(strictOptions(), "-o", "UserKnownHostsFile="+path, "-o", "GlobalKnownHostsFile=/dev/null")
}

func (manager *Manager) Options(ctx context.Context, program, target string) ([]string, error) {
	if program == "" {
		program = "ssh"
	}
	if ctx.Value(activeGate{}) == manager {
		return manager.options(ctx, program, target, nil, nil)
	}
	manager.gateMu.Lock()
	defer manager.gateMu.Unlock()
	ctx = context.WithValue(ctx, activeGate{}, manager)
	manager.drafts, manager.pending = make(map[string][]string), nil
	defer func() { manager.drafts, manager.pending = nil, nil }()
	options, err := manager.options(ctx, program, target, nil, nil)
	if err != nil {
		return nil, err
	}
	if len(manager.pending) == 0 {
		return options, nil
	}
	// Even a known final target may fail authentication or have rotated while
	// a missing proxy key was assessed. Do not persist that proxy's trust first.
	if err := manager.verify(ctx, program, target, options); err != nil {
		return nil, err
	}
	proposals := make([]Proposal, 0, len(manager.pending))
	for _, candidate := range manager.pending {
		proposals = append(proposals, candidate.proposal)
	}
	if manager.Confirm == nil {
		return nil, domain.ErrConfirmationRequired
	}
	if err := manager.Confirm(ctx, proposals); err != nil {
		return nil, fmt.Errorf("SSH trust for %s: %w", target, err)
	}
	for _, candidate := range manager.pending {
		if err := candidate.verify(); err != nil {
			return nil, err
		}
	}
	if err := manager.verify(ctx, program, target, options); err != nil {
		return nil, err
	}
	if err := manager.publish(ctx); err != nil {
		return nil, err
	}
	if manager.accepted == nil {
		manager.accepted = make(map[string][]string)
	}
	for key, value := range manager.drafts {
		manager.accepted[key] = value
	}
	return options, nil
}

func (manager *Manager) options(ctx context.Context, program, target string, chain, overrides []string) ([]string, error) {
	if !domain.SafeSSHTarget(target) {
		return nil, fmt.Errorf("invalid SSH target %q", target)
	}
	cacheKey := program + "\x00" + target + "\x00" + strings.Join(overrides, "\x00")
	for _, previous := range chain {
		if previous == cacheKey {
			return nil, errors.New("cyclic SSH proxy route")
		}
	}
	if len(chain) >= 8 {
		return nil, errors.New("SSH proxy route is too deep")
	}
	chain = append(chain, cacheKey)
	if options, ok := manager.accepted[cacheKey]; ok {
		return append([]string(nil), options...), nil
	}
	if options, ok := manager.drafts[cacheKey]; ok {
		return append([]string(nil), options...), nil
	}
	configArguments := append(append([]string{"-G"}, overrides...), target)
	configuration, err := manager.run(ctx, program, configArguments...)
	if err != nil {
		return nil, fmt.Errorf("resolve SSH target %s: %w", target, err)
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(configuration), "\n") {
		name, value, ok := strings.Cut(line, " ")
		if ok {
			values[name] = strings.TrimSpace(value)
		}
	}
	if values["hostname"] == "" || values["port"] == "" || values["userknownhostsfile"] == "" {
		return nil, errors.New("SSH configuration is missing hostname, port or trust store")
	}
	options := append(strictOptions(), overrides...)
	if jump := values["proxyjump"]; jump != "" && jump != "none" {
		hops := strings.Split(jump, ",")
		jumpTarget, jumpOverrides, err := parseJump(hops[len(hops)-1])
		if err != nil {
			return nil, err
		}
		if len(hops) > 1 {
			jumpOverrides = append(jumpOverrides, "-o", "ProxyJump="+strings.Join(hops[:len(hops)-1], ","))
		}
		jumpOptions, err := manager.options(ctx, program, jumpTarget, chain, jumpOverrides)
		if err != nil {
			return nil, err
		}
		proxy := append([]string{program}, jumpOptions...)
		for index := range proxy {
			proxy[index] = strings.ReplaceAll(proxy[index], "%", "%%")
		}
		proxy = append(proxy, "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "-W", "[%h]:%p", jumpTarget)
		// Prepend to override any inherited chain. Every child receives its own
		// strict trust settings, including a registered owner's managed pin.
		options = append([]string{"-o", "ProxyCommand=" + shellquote.Command(proxy), "-o", "ProxyJump=none"}, options...)
	}
	if manager.Pin != nil {
		pin, err := manager.Pin(target)
		if err != nil {
			return nil, err
		}
		if pin != nil {
			if err := pin.Validate(); err != nil {
				return nil, err
			}
			path, err := manager.temporaryFile([]byte(pin.KnownHostsLine + "\n"))
			if err != nil {
				return nil, err
			}
			return append(pinnedOptions(path), options...), nil
		}
	}
	namespace := values["hostkeyalias"]
	if namespace == "" || namespace == "none" {
		namespace = values["hostname"]
		if values["port"] != "22" {
			namespace = "[" + namespace + "]:" + values["port"]
		}
	}
	files := strings.Fields(values["userknownhostsfile"])
	allFiles := append(append([]string(nil), files...), strings.Fields(values["globalknownhostsfile"])...)
	known, err := manager.known(ctx, namespace, allFiles)
	if err != nil {
		return nil, err
	}
	if known {
		return options, nil
	}
	if manager.ReadOnly {
		return nil, fmt.Errorf("SSH target %s has no existing trusted key; prepare the connection separately", target)
	}
	if len(files) == 0 || !filepath.IsAbs(files[0]) || files[0] == "/dev/null" {
		return nil, errors.New("unknown SSH key has no writable persistent trust store")
	}
	path, err := manager.temporaryFile(nil)
	if err != nil {
		return nil, err
	}
	assessment, err := transport.SSHHostKeyAssessment(program, target, path, 5*time.Second)
	if err != nil {
		return nil, err
	}
	// Assessment's accept-new applies only to its private temporary file.
	assessment.Arguments = append(assessment.Arguments[:len(assessment.Arguments)-3], append(options, target, "--", "true")...)
	assessment.Env, assessment.Timeout = manager.Environment, 10*time.Second
	_, probeErr := assessment.Call(ctx, "", nil)
	trust, err := ownerinventory.ReadSSHHostTrust(path)
	if err != nil {
		return nil, fmt.Errorf("assess SSH key for %s: %w", target, errors.Join(err, probeErr))
	}
	_, hosts, key, _, _, err := ssh.ParseKnownHosts([]byte(trust.KnownHostsLine))
	if err != nil || len(hosts) != 1 || hosts[0] != namespace {
		return nil, errors.New("negotiated SSH key namespace differs from the assessed route")
	}
	if manager.Witness != nil {
		if err := manager.Witness(ctx, program, target, key); err != nil {
			return nil, err
		}
	}
	pinned := append(pinnedOptions(path), options...)
	verify := func() error { return manager.verify(ctx, program, target, pinned) }
	if err := verify(); err != nil {
		return nil, err
	}
	proposal := Proposal{Target: target, Namespace: namespace, File: files[0], Algorithm: key.Type(), Fingerprint: trust.Fingerprint}
	manager.pending = append(manager.pending, candidate{proposal: proposal, line: trust.KnownHostsLine, files: allFiles, verify: func() error {
		currentConfiguration, err := manager.run(ctx, program, configArguments...)
		if err != nil {
			return err
		}
		if !bytes.Equal(configuration, currentConfiguration) {
			return fmt.Errorf("%w: SSH route changed after assessment", domain.ErrPlanStale)
		}
		return verify()
	}})
	manager.drafts[cacheKey] = append([]string(nil), pinned...)
	// Keep this command pinned even if a concurrent writer changes known_hosts.
	return pinned, nil
}

func (manager *Manager) verify(ctx context.Context, program, target string, options []string) error {
	_, err := manager.run(ctx, program, append(append([]string(nil), options...), "-T", "-o", "BatchMode=yes", "-o", "ForwardAgent=no", "-o", "ClearAllForwardings=yes", "-o", "ConnectTimeout=5", target, "--", "true")...)
	if err != nil {
		return fmt.Errorf("verify assessed SSH key for %s: %w", target, err)
	}
	return nil
}

func parseJump(value string) (string, []string, error) {
	// OpenSSH accepts both [user@]host[:port] and ssh://[user@]host[:port].
	raw := strings.TrimPrefix(value, "ssh://")
	// ssh -G also brackets IPv4 addresses and aliases when a user is set.
	// URL brackets denote IPv6 literals, so remove only the non-IPv6 pair.
	hostStart := strings.LastIndex(raw, "@") + 1
	if strings.HasPrefix(raw[hostStart:], "[") {
		host, suffix, closed := strings.Cut(raw[hostStart+1:], "]")
		if closed && !strings.Contains(host, ":") && (suffix == "" || strings.HasPrefix(suffix, ":")) {
			raw = raw[:hostStart] + host + suffix
		}
	}
	parsed, err := url.Parse("ssh://" + raw)
	if err != nil || parsed.Hostname() == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", nil, errors.New("invalid SSH jump target")
	}
	host := parsed.Hostname()
	if parsed.User != nil {
		if _, hasPassword := parsed.User.Password(); hasPassword {
			return "", nil, errors.New("SSH jump target must not contain a password")
		}
		host = parsed.User.Username() + "@" + host
	}
	if !domain.SafeSSHTarget(host) {
		return "", nil, errors.New("invalid SSH jump target")
	}
	var options []string
	if port := parsed.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return "", nil, errors.New("invalid SSH jump port")
		}
		options = []string{"-p", port}
	}
	return host, options, nil
}

func (manager *Manager) known(ctx context.Context, namespace string, files []string) (bool, error) {
	for _, path := range files {
		if path == "none" || path == "/dev/null" {
			continue
		}
		if !filepath.IsAbs(path) {
			return false, fmt.Errorf("SSH trust store must be absolute: %s", path)
		}
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return false, err
		}
		output, err := manager.run(ctx, "ssh-keygen", "-F", namespace, "-f", path)
		if err == nil && len(bytes.TrimSpace(output)) > 0 {
			return true, nil
		}
		var processErr *transport.ProcessError
		if err != nil && !(errors.As(err, &processErr) && processErr.ExitCode == 1) {
			return false, err
		}
	}
	return false, nil
}

func (manager *Manager) publish(ctx context.Context) error {
	// Hold every store lock through publication/rollback. Multiple missing
	// proxy/owner/yard keys are one trust action, including when they share a file.
	if err := ctx.Err(); err != nil {
		return err
	}
	type store struct {
		path          string
		before, after []byte
		mode          os.FileMode
		exists        bool
	}
	entries := make([]safefs.EntryID, len(manager.pending))
	stores := make(map[safefs.EntryID]*store)
	var ordered []*store
	directories := make(map[string]bool)
	for index, request := range manager.pending {
		path := request.proposal.File
		directory := filepath.Dir(path)
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return err
		}
		directory, err := filepath.EvalSymlinks(directory)
		if err != nil {
			return err
		}
		directories[directory] = true
		path, err = resolveKnownHosts(filepath.Join(directory, filepath.Base(path)))
		if err != nil {
			return err
		}
		directories[filepath.Dir(path)] = true
		entry, err := safefs.EntryIdentity(path)
		if err != nil {
			// Preserve the errno that opening the store's lock used to return.
			var pathErr *os.PathError
			if errors.As(err, &pathErr) && pathErr.Op == "open" {
				return pathErr.Err
			}
			return err
		}
		entries[index] = entry
		if stores[entry] == nil {
			destination := &store{path: path}
			stores[entry] = destination
			ordered = append(ordered, destination)
		}
	}
	sort.Slice(ordered, func(left, right int) bool {
		leftDirectory, rightDirectory := filepath.Dir(ordered[left].path), filepath.Dir(ordered[right].path)
		if leftDirectory != rightDirectory {
			return leftDirectory < rightDirectory
		}
		return ordered[left].path < ordered[right].path
	})
	// Lock both the configured and resolved directories in a common order.
	lockOrder := make([]string, 0, len(directories))
	for directory := range directories {
		lockOrder = append(lockOrder, directory)
	}
	sort.Strings(lockOrder)
	locked := make(map[[2]uint64]bool)
	for _, directory := range lockOrder {
		lock, err := openKnownHostsLock(ctx, directory)
		if err != nil {
			return err
		}
		inode, err := lock.Identity()
		if err != nil || locked[inode] {
			lock.Release()
			if err != nil {
				return err
			}
			continue
		}
		defer lock.Release()
		if err := lock.Acquire(ctx, false, 10*time.Millisecond); err != nil {
			return err
		}
		locked[inode] = true
	}
	for index, request := range manager.pending {
		path, err := resolveKnownHosts(request.proposal.File)
		var entry safefs.EntryID
		if err == nil {
			entry, err = safefs.EntryIdentity(path)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			return err
		}
		if err != nil || entry != entries[index] {
			return fmt.Errorf("%w: SSH trust changed after assessment", domain.ErrPlanStale)
		}
	}
	for _, destination := range ordered {
		info, err := os.Lstat(destination.path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("refusing non-regular SSH trust store")
		}
		data, err := os.ReadFile(destination.path)
		if err != nil {
			return err
		}
		destination.before, destination.mode, destination.exists = data, info.Mode().Perm(), true
		destination.after = bytes.Clone(data)
	}
	type trustKey struct {
		entry     safefs.EntryID
		namespace string
	}
	seen := make(map[trustKey]string)
	for index, candidate := range manager.pending {
		proposal := candidate.proposal
		identity := trustKey{entry: entries[index], namespace: proposal.Namespace}
		if line, ok := seen[identity]; ok {
			if line != candidate.line {
				return fmt.Errorf("%w: conflicting SSH keys for %s", domain.ErrPlanStale, proposal.Namespace)
			}
			continue
		}
		seen[identity] = candidate.line
		known, err := manager.known(ctx, proposal.Namespace, candidate.files)
		if err != nil {
			return err
		}
		if known {
			return fmt.Errorf("%w: SSH trust changed after assessment", domain.ErrPlanStale)
		}
		destination := stores[entries[index]]
		data := destination.after
		if len(data) > 0 && data[len(data)-1] != '\n' {
			data = append(data, '\n')
		}
		destination.after = append(data, candidate.line+"\n"...)
	}
	for index, destination := range ordered {
		if published, err := writeFile(destination.path, destination.after, 0o600); err != nil {
			if published {
				index++
			}
			for _, written := range ordered[:index] {
				if written.exists {
					_, restoreErr := writeFile(written.path, written.before, written.mode)
					err = errors.Join(err, restoreErr)
				} else {
					err = errors.Join(err, removeKnownHosts(written.path))
				}
			}
			return err
		}
	}
	return nil
}

func resolveKnownHosts(path string) (string, error) {
	target, err := safefs.ResolveFile(path)
	if errors.Is(err, syscall.ELOOP) {
		return "", errors.New("refusing non-regular SSH trust store")
	}
	return target, err
}

func removeKnownHosts(path string) error {
	if err := os.Remove(path); err != nil {
		return err
	}
	return safefs.SyncDir(filepath.Dir(path))
}

func writeFile(path string, data []byte, mode os.FileMode) (published bool, err error) {
	pending, err := safefs.Create(path, safefs.Options{
		Mode: mode, ExactMode: true, TempPattern: ".subyard-known-hosts-*",
	})
	if err != nil {
		return false, knownHostsWriteError(err)
	}
	defer pending.Abort()
	if _, err := pending.File().Write(data); err != nil {
		return false, err
	}
	err = pending.Commit()
	return pending.Published(), knownHostsWriteError(err)
}

func knownHostsWriteError(err error) error {
	if errors.Is(err, safefs.ErrSymlink) || errors.Is(err, safefs.ErrNotRegular) {
		return errors.New("refusing non-regular SSH trust store")
	}
	return err
}

// LockKnownHosts shares the shell SSH adapter's lock, so independent Subyard
// writers cannot lose unrelated entries. Call only after mutation consent.
func LockKnownHosts(ctx context.Context, path string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	lock, err := openKnownHostsLock(ctx, directory)
	if err != nil {
		return nil, err
	}
	if err := lock.Acquire(ctx, false, 10*time.Millisecond); err != nil {
		lock.Release()
		return nil, err
	}
	return lock.Release, nil
}

func openKnownHostsLock(ctx context.Context, directory string) (*safefs.LockFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lock, err := safefs.OpenLock(filepath.Join(directory, ".subyard-known-hosts.lock"), 0o600)
	if err != nil {
		return nil, errors.Unwrap(err)
	}
	return lock, nil
}
