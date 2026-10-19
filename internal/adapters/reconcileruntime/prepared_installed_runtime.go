package reconcileruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testyardmigration"
)

const MaxInstalledRecipe = 96 << 20

var installedRuntimeHelpers = []string{"download.sh", "engine-context.sh", "host-path-ownership.py", "host.sh", "native-install-custody.py", "native-install-custody.sh", "native-installed-stage.py", "native-installed-stage.sh", "owner-address.sh", "preview-proxy.sh", "runtime.sh", "ssh-config.sh", "ssh-listener.sh", "teardown-files.py", "teardown-plan.py", "teardown-profile-checks.py", "teardown-services.sh", "teardown-session.sh", "teardown-shared.py", "ui.sh"}

type installedRecipeFile struct {
	Path    string `json:"path"`
	Content []byte `json:"content"`
}
type installedSourceGuard struct {
	Path   string `json:"path"`
	Before string `json:"before"`
}
type installedRecipe struct {
	Version    int                    `json:"version"`
	Owner      string                 `json:"owner"`
	Source     string                 `json:"source"`
	Runtime    string                 `json:"runtime"`
	Resources  string                 `json:"resources"`
	Launcher   string                 `json:"launcher"`
	HookBefore string                 `json:"hookBefore"`
	Leaves     []string               `json:"leaves"`
	Files      []installedRecipeFile  `json:"files"`
	Profile    string                 `json:"profile,omitempty"`
	Handler    string                 `json:"handler,omitempty"`
	Native     []installedSourceGuard `json:"native,omitempty"`
	AfterOwner []byte                 `json:"afterOwner,omitempty"`
}

func (p *PreparedInstalledRuntime) Recipe() []byte {
	r := installedRecipe{Version: 1, Owner: p.owner, Source: p.source, Runtime: p.runtime, Resources: p.resources, Launcher: p.keyLauncher, HookBefore: p.hookBefore, Leaves: p.leaves, Profile: p.profile, Handler: p.handler, Native: p.native, AfterOwner: p.afterOwner}
	paths := make([]string, 0, len(p.files))
	for path := range p.files {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	for _, path := range paths {
		r.Files = append(r.Files, installedRecipeFile{path, p.files[path]})
	}
	raw, _ := json.Marshal(r)
	return raw
}

// ShareSources interns only already retained bytes; it never reads a source.
func (p *PreparedInstalledRuntime) ShareSources(intern func([]byte) ([]byte, error)) error {
	for path, payload := range p.files {
		shared, err := intern(payload)
		if err != nil {
			return err
		}
		p.files[path] = shared
	}
	if p.configs != nil {
		for destination, payload := range p.configs.payloads {
			if shared, exists := p.files[materializedBlobPath(payload)]; exists {
				p.configs.payloads[destination] = shared
			}
		}
		for key, value := range p.configs.sources {
			if value.exists {
				if shared, exists := p.files[materializedBlobPath(value.payload)]; exists {
					value.payload = shared
					p.configs.sources[key] = value
				}
			}
		}
	}
	return nil
}

// BindInstalledRuntime restores only the two fixed component recipes. It never
// selects executable paths from an unrecognized recipe or re-reads payloads.
func (runtime Runtime) BindInstalledRuntime(raw []byte) (*PreparedInstalledRuntime, error) {
	return runtime.BindInstalledRuntimeContext(context.Background(), raw)
}

func (runtime Runtime) BindInstalledRuntimeContext(ctx context.Context, raw []byte) (*PreparedInstalledRuntime, error) {
	if len(raw) > MaxInstalledRecipe {
		return nil, errors.New("installed recipe bound")
	}
	var r installedRecipe
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&r); err != nil {
		return nil, err
	}
	canonical, _ := json.Marshal(r)
	if !bytes.Equal(raw, canonical) || r.Version != 1 {
		return nil, errors.New("invalid installed recipe")
	}
	var sourceBytes int
	for _, file := range r.Files {
		sourceBytes += len(file.Content)
		if len(file.Content) > 32<<20 || sourceBytes > 64<<20 || len(r.Files) > 8192 {
			return nil, errors.New("installed recipe payload bound")
		}
	}
	if !profileRuntimeDigest.MatchString(r.Source) || !profileRuntimeDigest.MatchString(r.Runtime) || !profileRuntimeDigest.MatchString(r.Resources) || (r.HookBefore != "" && !profileRuntimeDigest.MatchString(r.HookBefore)) || len(r.Launcher) > 4096 {
		return nil, errors.New("installed recipe identity bound")
	}
	if len(r.AfterOwner) > 32<<20 || r.AfterOwner != nil && runtime.afterOwnerConfigs == nil && !runtime.afterOwnerConfigSource && runtime.afterOwnerBroker == nil && runtime.afterOwnerRoutes == nil && !runtime.afterOwnerIntegrationSource {
		return nil, errors.New("conditional recipe header outside original bind")
	}
	if r.Owner == "materialized-files" || r.Owner == "materialized-integrations" {
		return runtime.bindMaterializedRecipe(ctx, r, raw)
	}
	if r.Owner == "test-vm-broker" {
		return runtime.bindBrokerRecipe(ctx, r, raw)
	}
	if r.Owner == "owner-native-runtime" {
		return runtime.bindOwnerRuntimeSources(ctx, r, raw)
	}
	if r.Owner == "route-consumers" {
		return runtime.bindRouteRecipe(ctx, r, raw)
	}
	if r.Profile != "" || r.Handler != "" || len(r.Native) != 0 {
		if r.Profile == "" {
			return runtime.bindHostInstallerSources(r, raw)
		}
		return runtime.bindProfileRuntimeSources(r, raw)
	}
	if r.Owner != "core-guest-runtime" && r.Owner != "key-tools-runtime" {
		return nil, errors.New("invalid installed recipe owner")
	}
	p := &PreparedInstalledRuntime{owner: r.Owner, source: r.Source, runtime: r.Runtime, resources: r.Resources, keyLauncher: r.Launcher, hookBefore: r.HookBefore, leaves: r.Leaves, files: map[string][]byte{}}
	allowed := map[string]bool{"scripts/lib-power.sh": true}
	for _, leaf := range installedRuntimeHelpers {
		allowed["scripts/lib/"+leaf] = true
	}
	if r.Owner == "core-guest-runtime" {
		for _, path := range []string{"scripts/refresh-installed-guest-runtime.sh", "scripts/prepare-preview-route.sh", "config/preview/subyard-preview", "config/agents/ccusage/provision.sh"} {
			allowed[path] = true
		}
		if r.HookBefore != "" {
			allowed["prepared/ccusage-provision.sh"] = true
			p.hookPath = runtime.environmentValue("CCUSAGE_PROVISION")
		}
		if !slices.Equal(r.Leaves, []string{"refresh-installed-guest-runtime.sh"}) || r.Launcher != "" {
			return nil, errors.New("invalid guest recipe")
		}
	} else {
		for _, path := range []string{"scripts/install-key-tools.sh", "scripts/install-keys-auto-sync.sh", "config/systemd/subyard-keys-sync.service.in", "config/systemd/subyard-keys-sync.timer.in"} {
			allowed[path] = true
		}
		if !slices.Equal(r.Leaves, runtime.installedKeyLeaves()) || r.HookBefore != "" {
			return nil, domain.ErrPlanStale
		}
		if slices.Contains(r.Leaves, "install-keys-auto-sync.sh") {
			launcher, err := runtime.preparedKeyLauncher()
			if err != nil || launcher != r.Launcher {
				return nil, domain.ErrPlanStale
			}
		} else if r.Launcher != "" {
			return nil, errors.New("unexpected timer launcher")
		}
	}
	var total int
	previous := ""
	for _, file := range r.Files {
		if !allowed[file.Path] || file.Path <= previous {
			return nil, errors.New("invalid installed recipe path")
		}
		previous = file.Path
		delete(allowed, file.Path)
		total += len(file.Content)
		if total > 32<<20 {
			return nil, errors.New("installed recipe source bound")
		}
		p.files[file.Path] = file.Content
	}
	if len(allowed) != 0 {
		return nil, errors.New("incomplete installed recipe")
	}
	p.binding = installedRuntimeDigest(json.RawMessage(raw))
	if err := runtime.validateInstalledRecipe(p); err != nil {
		return nil, err
	}
	return p, nil
}

func (runtime Runtime) validateInstalledRecipe(p *PreparedInstalledRuntime) error {
	if p != nil && p.afterOwnerRuntime {
		if p.closed.Load() {
			return domain.ErrPlanStale
		}
		return runtime.validateInstalledRecipe(p.afterOwnerSourceCopy())
	}
	if p == nil || p.closed.Load() || p.runtime != runtime.configRuntimeBinding() || p.resources != runtime.installedResourceBinding() {
		return domain.ErrPlanStale
	}
	if _, _, err := p.stageUsage(); err != nil {
		return err
	}
	sourceOwner := p.owner
	if p.profile == "" && p.handler != "" {
		sourceOwner = "core-guest-runtime"
	}
	if source, err := runtime.SelectedRuntimeInputs(sourceOwner); err != nil || source != p.source {
		return domain.ErrPlanStale
	}
	if p.profile != "" {
		return runtime.validateProfileRuntimeSources(p)
	}
	if p.handler != "" {
		return runtime.validateHostInstallerSources(p)
	}
	if p.hookPath != "" {
		before, err := installedRuntimeFile(p.hookPath)
		if err != nil || installedRuntimeDigest(before) != p.hookBefore {
			return domain.ErrPlanStale
		}
	}
	return nil
}

// PreparedInstalledRuntime owns only the two existing installed helper recipes.
// It retains their exact leaves and bytes; release ordering remains in Rust.
type PreparedInstalledRuntime struct {
	owner, source, runtime, binding, directory string
	leaves                                     []string
	files                                      map[string][]byte
	keyLauncher                                string
	hookPath, hookBefore, resources            string
	close                                      sync.Once
	closed                                     atomic.Bool
	retain                                     atomic.Bool
	stageBinding                               string
	profile, handler                           string
	native                                     []installedSourceGuard
	configs                                    *PreparedConfigs
	integrations                               *PreparedIntegrations
	executionFiles                             map[string][]byte
	broker                                     *PreparedTestVMBroker
	routes                                     *testyardmigration.PreparedRouteDevices
	ownerInputs                                []ownerSourceInputBinding
	afterOwnerConfigs                          *afterOwnerConfigPhysical
	afterOwnerConfigSource                     bool
	afterOwnerRuntime                          bool
	afterOwnerIntegrations                     *afterOwnerIntegrationPhysical
	afterOwner                                 []byte
}

func (p *PreparedInstalledRuntime) Binding() string {
	if p == nil {
		return ""
	}
	return p.binding
}
func (p *PreparedInstalledRuntime) Close() error {
	if p == nil {
		return nil
	}
	var result error
	p.closed.Store(true)
	p.close.Do(func() {
		if p.broker != nil {
			_ = p.broker.Close()
		}
		if p.retain.Load() {
			return
		}
		if p.directory != "" {
			filepath.WalkDir(p.directory, func(path string, entry fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if entry.IsDir() {
					return os.Chmod(path, 0o700)
				}
				return nil
			})
			result = os.RemoveAll(p.directory)
		}
	})
	return result
}

func (runtime Runtime) PrepareInstalledRuntime(ctx context.Context, owner string) (*PreparedInstalledRuntime, error) {
	if owner != "core-guest-runtime" && owner != "key-tools-runtime" {
		return nil, errors.New("unknown prepared installed runtime")
	}
	p := &PreparedInstalledRuntime{owner: owner, runtime: runtime.configRuntimeBinding(), files: map[string][]byte{}}
	p.resources = runtime.installedResourceBinding()
	var err error
	p.source, err = runtime.SelectedRuntimeInputs(owner)
	if err != nil {
		return nil, err
	}
	paths := []string{}
	if owner == "core-guest-runtime" {
		p.leaves = []string{"refresh-installed-guest-runtime.sh"}
		paths = []string{"scripts/refresh-installed-guest-runtime.sh", "scripts/prepare-preview-route.sh", "config/preview/subyard-preview", "config/agents/ccusage/provision.sh"}
	} else {
		p.leaves = runtime.installedKeyLeaves()
		paths = []string{"scripts/install-key-tools.sh", "scripts/install-keys-auto-sync.sh", "config/systemd/subyard-keys-sync.service.in", "config/systemd/subyard-keys-sync.timer.in"}
		if slices.Contains(p.leaves, "install-keys-auto-sync.sh") {
			p.keyLauncher, err = runtime.preparedKeyLauncher()
			if err != nil {
				return nil, err
			}
		}
	}
	// This is the closed inventory sourced by the shipped fixed wrappers.
	for _, leaf := range installedRuntimeHelpers {
		paths = append(paths, "scripts/lib/"+leaf)
	}
	paths = append(paths, "scripts/lib-power.sh")
	slices.Sort(paths)
	paths = slices.Compact(paths)
	var size int
	read := func(path string) ([]byte, error) {
		asset := config.MaterializedAsset{Source: path}
		if runtime.SourceReader != nil {
			return runtime.SourceReader(asset)
		}
		return asset.ReadSource()
	}
	for _, relative := range paths {
		payload, err := read(filepath.Join(runtime.RepositoryRoot, relative))
		if err != nil {
			return nil, err
		}
		size += len(payload)
		if len(p.files) >= 512 || size > 32<<20 {
			return nil, errors.New("installed runtime prepared source bound")
		}
		p.files[relative] = payload
	}
	if owner == "core-guest-runtime" && runtime.environmentValue("ALLOWS_CODING_TOOLS") != "false" {
		hook := runtime.environmentValue("CCUSAGE_PROVISION")
		if hook == "" {
			return nil, errors.New("ccusage provision hook is not configured")
		}
		payload, err := read(hook)
		if err != nil {
			return nil, err
		}
		size += len(payload)
		if size > 32<<20 {
			return nil, errors.New("installed runtime prepared source bound")
		}
		p.files["prepared/ccusage-provision.sh"] = payload
		p.hookPath = hook
		before, err := installedRuntimeFile(hook)
		if err != nil {
			return nil, err
		}
		p.hookBefore = installedRuntimeDigest(before)
	}
	if current, err := runtime.SelectedRuntimeInputs(owner); err != nil || current != p.source {
		return nil, domain.ErrPlanStale
	}
	if _, _, err := p.stageUsage(); err != nil {
		return nil, err
	}
	p.binding = installedRuntimeDigest(json.RawMessage(p.Recipe()))
	return p, nil
}

func (p *PreparedInstalledRuntime) materialize() error {
	if _, _, err := p.stageUsage(); err != nil {
		return err
	}
	var err error
	if p.stageBinding != "" {
		parent := fmt.Sprintf("/tmp/subyard-native-installed-%d", os.Getuid())
		if err = os.Mkdir(parent, 0o700); err != nil && !os.IsExist(err) {
			return err
		}
		if err == nil {
			if err := os.Chmod(parent, 0o700); err != nil {
				return err
			}
		}
		info, e := os.Lstat(parent)
		if e != nil {
			return e
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || info.Mode().Perm() != 0o700 || stat.Uid != uint32(os.Getuid()) {
			return errors.New("unsafe installed staging namespace")
		}
		p.directory = filepath.Join(parent, p.stageBinding+"-"+p.owner)
		err = os.Mkdir(p.directory, 0o700)
	} else {
		p.directory, err = os.MkdirTemp("/tmp", "subyard-prepared-installed-")
	}
	if err != nil {
		p.directory = ""
		return err
	}
	ok := false
	defer func() {
		if !ok {
			p.Close()
		}
	}()
	if err := os.Chmod(p.directory, 0o700); err != nil {
		return err
	}
	files := p.files
	if p.executionFiles != nil {
		files = p.executionFiles
	}
	for relative, payload := range files {
		path := filepath.Join(p.directory, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(path, payload, 0o500); err != nil {
			return err
		}
		if err := os.Chmod(path, 0o500); err != nil {
			return err
		}
	}
	if p.stageBinding != "" {
		info, err := os.Lstat(p.directory)
		if err != nil {
			return err
		}
		stat := info.Sys().(*syscall.Stat_t)
		marker, _ := json.Marshal(struct {
			Version int    `json:"version"`
			Binding string `json:"binding"`
			Recipe  string `json:"recipe"`
			Owner   string `json:"owner"`
			Device  uint64 `json:"device"`
			Inode   uint64 `json:"inode"`
			Parent  int    `json:"parent"`
		}{1, p.stageBinding, p.binding, p.owner, uint64(stat.Dev), stat.Ino, os.Getpid()})
		if err := os.WriteFile(filepath.Join(p.directory, ".stage.json"), append(marker, '\n'), 0o600); err != nil {
			return err
		}
		if err := os.Chmod(filepath.Join(p.directory, ".stage.json"), 0o600); err != nil {
			return err
		}
	}
	if err := filepath.WalkDir(p.directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.Chmod(path, 0o500)
		}
		return nil
	}); err != nil {
		return err
	}
	ok = true
	return nil
}

// stageUsage charges the exact future tree, including the fixed marker and
// every unique parent directory. Prepare and bind call it before consent or
// effects; the transport's file ceiling alone is not an execution admission.
func (p *PreparedInstalledRuntime) stageUsage() (int, int, error) {
	files := p.files
	if p.executionFiles != nil {
		files = p.executionFiles
	}
	members := map[string]bool{".stage.json": true}
	bytes := 0
	for path, payload := range files {
		if !validMaterializedLayout(path) || path == ".stage.json" {
			return 0, 0, errors.New("invalid installed stage path")
		}
		members[path] = true
		bytes += len(payload)
		for parent := filepath.Dir(path); parent != "."; parent = filepath.Dir(parent) {
			if _, conflict := files[parent]; conflict {
				return 0, 0, errors.New("installed execution stage path collision")
			}
			members[parent] = true
		}
	}
	if len(members) > 8192 || bytes > 64<<20 {
		return len(members), bytes, errors.New("installed execution stage capacity")
	}
	return len(members), bytes, nil
}

func (runtime Runtime) preparedKeyLauncher() (string, error) {
	root := runtime.environmentDefault("YARD_RUNTIME_ROOT", filepath.Join(runtime.Yard.Paths.DataHome, "runtime"))
	path := filepath.Join(root, "current/bin/yard")
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
		return path, nil
	}
	for _, relative := range []string{".build/yard", "bin/yard"} {
		if relative == "bin/yard" && runtime.environmentValue("SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE") != "1" {
			break
		}
		candidate := filepath.Join(runtime.RepositoryRoot, relative)
		info, err := os.Lstat(candidate)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return runtime.bindingPath(candidate), nil
		}
	}
	return "", errors.New("installed credential timer launcher is unavailable")
}

func (runtime Runtime) ApplyPreparedInstalledRuntime(ctx context.Context, p *PreparedInstalledRuntime, operation, actual, desired string) error {
	if err := runtime.validateInstalledRecipe(p); err != nil {
		return err
	}
	if p.owner == "key-tools-runtime" && !slices.Equal(runtime.installedKeyLeaves(), p.leaves) {
		return domain.ErrPlanStale
	}
	before, err := runtime.ObserveInstalledRuntime(ctx, p.owner)
	if err != nil {
		return err
	}
	if before.State == "current" && before.Actual == desired {
		return nil
	}
	if before.State != "stale" || before.Actual != actual || before.Desired != desired {
		return domain.ErrPlanStale
	}
	execution, err := runtime.preparedSourceExecution(ctx, p)
	if err != nil {
		return err
	}
	defer p.Close()
	err = execution.applyInstalledRuntimeLeaves(ctx, p.owner, operation, actual, desired, p.leaves)
	if err != nil && p.stageBinding != "" {
		p.retain.Store(true)
	}
	return err
}

func preparedInstalledEnvironment(environment []string, name, value string) []string {
	prefix := name + "="
	for index, entry := range environment {
		if strings.HasPrefix(entry, prefix) {
			environment[index] = prefix + value
			return environment
		}
	}
	return append(environment, fmt.Sprintf("%s=%s", name, value))
}

func (runtime Runtime) installedResourceBinding() string {
	paths := runtime.Yard.Paths
	paths.RepositoryRoot = runtime.bindingPath(paths.RepositoryRoot)
	paths.ConfigDir = runtime.bindingPath(paths.ConfigDir)
	return installedRuntimeDigest(paths)
}

func (runtime Runtime) preparedSourceExecution(ctx context.Context, p *PreparedInstalledRuntime) (Runtime, error) {
	if ports.NativeOperationObserverFrom(ctx) != nil {
		p.stageBinding = ports.NativeInvocationKey(ctx)
		if !profileRuntimeDigest.MatchString(p.stageBinding) {
			return Runtime{}, errors.New("native installed staging binding unavailable")
		}
	}
	if err := p.materialize(); err != nil {
		return Runtime{}, err
	}
	// Source paths can change after this guard; execution consumes only this
	// already retained private byte cache, never the original source path.
	execution := runtime
	execution.RepositoryRoot = p.directory
	execution.LogicalRepositoryRoot = runtime.LogicalRepositoryRoot
	if execution.LogicalRepositoryRoot == "" {
		execution.LogicalRepositoryRoot = runtime.RepositoryRoot
	}
	execution.Environment = append([]string{}, runtime.Environment...)

	if _, ok := p.files["prepared/ccusage-provision.sh"]; ok {
		execution.Environment = preparedInstalledEnvironment(execution.Environment, "CCUSAGE_PROVISION", filepath.Join(p.directory, "prepared/ccusage-provision.sh"))
	}
	if p.keyLauncher != "" {
		execution.Environment = preparedInstalledEnvironment(execution.Environment, "SUBYARD_PREPARED_KEYS_YARD_BIN", p.keyLauncher)
	}
	if p.stageBinding != "" {
		for key, value := range map[string]string{"SUBYARD_PREPARED_INSTALLED_STAGE": p.directory, "SUBYARD_PREPARED_INSTALLED_BINDING": p.stageBinding, "SUBYARD_PREPARED_INSTALLED_RECIPE": p.binding, "SUBYARD_PREPARED_INSTALLED_OWNER": p.owner, "SUBYARD_PREPARED_INSTALLED_CLEANUP": ""} {
			execution.Environment = preparedInstalledEnvironment(execution.Environment, key, value)
		}
	}
	return execution, nil
}
