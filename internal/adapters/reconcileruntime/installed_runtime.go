package reconcileruntime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/previewroute"
)

// Installed helpers have repeatable activation, separate from provisioning and
// credential initialization. No absent installation is created here.
func (runtime Runtime) ObserveInstalledRuntime(ctx context.Context, id string) (ports.RuntimeObservation, error) {
	switch id {
	case "core-guest-runtime":
		return runtime.observeInstalledGuestRuntime(ctx)
	case "key-tools-runtime":
		return runtime.observeInstalledKeyRuntime(ctx)
	default:
		return ports.RuntimeObservation{}, errors.New("unknown installed runtime activation")
	}
}

// Release readiness checks installed consumers using their applied configuration.
// Desired settings belong to ordinary init; changing them is not a new release.
// The normal observer still checks current source bytes, receipts and ownership.
func (runtime Runtime) ObserveInstalledReleaseRuntime(ctx context.Context, id string) (ports.RuntimeObservation, error) {
	if id == "key-tools-runtime" {
		return runtime.observeInstalledReleaseKeyRuntime(ctx)
	}
	if id != "core-guest-runtime" || !runtime.guestFactsAvailable() || runtime.Executor == nil {
		return runtime.ObserveInstalledRuntime(ctx, id)
	}
	presence, instance, err := runtime.installedGuestPresence(ctx)
	if err != nil || !strings.EqualFold(instance.Status, "running") || !presence.Preview && !presence.CCUsage {
		return runtime.ObserveInstalledRuntime(ctx, id)
	}
	program, err := os.ReadFile(filepath.Join(runtime.RepositoryRoot, guestFactsSource))
	if err != nil {
		return ports.RuntimeObservation{}, err
	}
	result, err := runtime.Executor.Exec(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, ports.InstanceExecRequest{
		Command: []string{"python3", "-B", "-c", "__name__ = 'subyard_release_observer'\n" + string(program) + installedGuestSettingsProjection},
	})
	if err != nil || result.ExitCode != 0 {
		// Invalid or missing ownership evidence must retain ordinary readiness.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || err != nil && result.ExitCode == 0 {
			return ports.RuntimeObservation{}, err
		}
		return runtime.ObserveInstalledRuntime(ctx, id)
	}
	var settings map[string]string
	if len(result.Stdout) > 16384 || json.Unmarshal(result.Stdout, &settings) != nil {
		return ports.RuntimeObservation{}, errors.New("invalid installed guest configuration projection")
	}
	allowed := map[string]bool{"ENVIRONMENT_PROFILES": true, "ALLOWS_CODING_TOOLS": true, "DEV_USER": true,
		"WEB_PREVIEW_HOST_PORT": true, "CCUSAGE_VERSION": true, "CCUSAGE_SHA256_AMD64": true, "CCUSAGE_SHA256_ARM64": true}
	environment := runtimeEnvironment(runtime.Environment)
	for name, value := range settings {
		if !allowed[name] {
			return ports.RuntimeObservation{}, errors.New("unexpected installed guest setting")
		}
		environment[name] = value
	}
	if user, ok := settings["DEV_USER"]; ok {
		runtime.Yard.DevUser = user
	}
	runtime.Environment = nil
	for name, value := range environment {
		runtime.Environment = append(runtime.Environment, name+"="+value)
	}
	return runtime.ObserveInstalledRuntime(ctx, id)
}

func (runtime Runtime) observeInstalledReleaseKeyRuntime(ctx context.Context) (ports.RuntimeObservation, error) {
	tools := runtime.environmentDefault("SUBYARD_KEYS_TOOLS_DIR", filepath.Join(runtime.Yard.Paths.DataHome, "tools"))
	path := filepath.Join(tools, "bin", ".subyard-native-tools.json")
	// Keep ordinary readiness for absent/legacy/unsafe receipts. The installed
	// observer retains its exact mode/owner, binary hash and version checks.
	if _, err := installedRuntimeFile(path); err != nil {
		return ports.RuntimeObservation{}, err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return runtime.ObserveInstalledRuntime(ctx, "key-tools-runtime")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || info.Size() > 16384 {
		return runtime.ObserveInstalledRuntime(ctx, "key-tools-runtime")
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return ports.RuntimeObservation{}, err
	}
	var receipt struct {
		Schema      int `json:"schema"`
		Age, Sops   string
		AgeArchive  string `json:"age_archive"`
		SopsArchive string `json:"sops_archive"`
	}
	if json.Unmarshal(payload, &receipt) != nil || receipt.Schema != 1 {
		return runtime.ObserveInstalledRuntime(ctx, "key-tools-runtime")
	}
	settings := map[string]string{"SUBYARD_AGE_VERSION": receipt.Age, "SUBYARD_SOPS_VERSION": receipt.Sops,
		"SUBYARD_AGE_SHA256_AMD64": receipt.AgeArchive, "SUBYARD_AGE_SHA256_ARM64": receipt.AgeArchive,
		"SUBYARD_SOPS_SHA256_AMD64": receipt.SopsArchive, "SUBYARD_SOPS_SHA256_ARM64": receipt.SopsArchive}
	for name, value := range settings {
		if err := config.ValidateSetting(config.ScopeHost, name, value, false); err != nil {
			return runtime.ObserveInstalledRuntime(ctx, "key-tools-runtime")
		}
	}
	environment := runtimeEnvironment(runtime.Environment)
	for name, value := range settings {
		environment[name] = value
	}
	runtime.Environment = nil
	for name, value := range environment {
		runtime.Environment = append(runtime.Environment, name+"="+value)
	}
	return runtime.ObserveInstalledRuntime(ctx, "key-tools-runtime")
}

// Execute after the facts module has been loaded without invoking its CLI.
// These protected receipts supply configuration only, never desired source bytes.
const installedGuestSettingsProjection = `
value = read()
applied = value['last_success']
if applied is None:
    raise ValueError('no successful guest refresh')
def protected_json(path, mode):
    safe(path)
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != mode or info.st_uid != 0 or info.st_gid != 0 or info.st_nlink != 1 or info.st_size > 16384:
        raise ValueError('unsafe installed configuration receipt')
    return json.loads(path.read_bytes(), object_pairs_hook=unique_fields)
settings = {'ENVIRONMENT_PROFILES': ' '.join(applied['selected_profiles'])}
endpoint = protected_json(Path('/etc/subyard/preview.json'), 0o644)
if endpoint.get('version') != 1 or type(endpoint.get('port')) is not int or not 1 <= endpoint['port'] <= 65535:
    raise ValueError('invalid installed preview endpoint')
settings['WEB_PREVIEW_HOST_PORT'] = str(endpoint['port'])
core = Path('/var/lib/subyard/agent-skills/core.json')
settings['ALLOWS_CODING_TOOLS'] = 'true' if core.exists() or core.is_symlink() else 'false'
if settings['ALLOWS_CODING_TOOLS'] == 'true':
    import pwd
    receipt = protected_json(core, 0o600)
    users = [user.pw_name for user in pwd.getpwall() if user.pw_dir == receipt.get('home')]
    if len(users) != 1:
        raise ValueError('invalid installed skill user')
    settings['DEV_USER'] = users[0]
usage = Path('/usr/local/bin/ccusage.subyard-runtime.json')
if usage.exists() or usage.is_symlink():
    receipt = protected_json(usage, 0o600)
    if receipt.get('schema') != 1 or not re.fullmatch(r'[0-9]+\.[0-9]+\.[0-9]+([+-][0-9A-Za-z.-]+)?', receipt.get('version', '')) or not re.fullmatch('[0-9a-f]{64}', receipt.get('archive', '')):
        raise ValueError('invalid installed usage receipt')
    settings['CCUSAGE_VERSION'] = receipt['version']
    settings['CCUSAGE_SHA256_AMD64'] = settings['CCUSAGE_SHA256_ARM64'] = receipt['archive']
print(json.dumps(settings))
`

func (runtime Runtime) InstalledRuntimePresence(ctx context.Context, id string) (ports.RuntimePresence, error) {
	if id == "core-guest-runtime" {
		presence, _, err := runtime.installedGuestPresence(ctx)
		return presence, err
	}
	if id != "key-tools-runtime" {
		return ports.RuntimePresence{}, errors.New("unknown installed runtime activation")
	}
	facts := map[string]any{}
	tools := runtime.environmentDefault("SUBYARD_KEYS_TOOLS_DIR", filepath.Join(runtime.Yard.Paths.DataHome, "tools"))
	keys := runtime.environmentDefault("SUBYARD_KEYS_ROOT", filepath.Join(runtime.Yard.Paths.ConfigHome, "keys"))
	units := runtime.environmentDefault("SUBYARD_KEYS_SYSTEMD_DIR", filepath.Join(runtime.Yard.Paths.OperatorHome, ".config/systemd/user"))
	presence := ports.RuntimePresence{}
	for _, path := range []string{filepath.Join(tools, "bin"), filepath.Join(keys, "identity.json"), filepath.Join(units, "subyard-keys-sync.service"), filepath.Join(units, "subyard-keys-sync.timer")} {
		// Identity content is private. Bind only its metadata; public tool/unit
		// payloads are observed by their owning readiness checks.
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			facts[path] = "absent"
			continue
		}
		if err != nil {
			return ports.RuntimePresence{}, err
		}
		for parent := filepath.Dir(path); parent != string(filepath.Separator); parent = filepath.Dir(parent) {
			info, err := os.Lstat(parent)
			if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return ports.RuntimePresence{}, errors.New("unsafe installed runtime presence parent")
			}
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || info.Mode()&os.ModeSymlink != 0 {
			return ports.RuntimePresence{}, errors.New("unsafe installed runtime presence")
		}
		facts[path] = []any{info.Mode(), stat.Dev, stat.Ino, stat.Uid, stat.Gid, stat.Mtim, stat.Ctim}
		if path == filepath.Join(tools, "bin") || path == filepath.Join(keys, "identity.json") {
			presence.KeyTools = true
		} else {
			presence.KeyTimer = true
		}
	}
	if presence.KeyTimer {
		owned, err := runtime.scriptConverged(ctx, "install-keys-auto-sync.sh", "--check-owned")
		if err != nil {
			return ports.RuntimePresence{}, err
		}
		if !owned {
			return ports.RuntimePresence{}, errors.New("credential timer files are not owned by Subyard")
		}
		receipt, err := installedRuntimeFile(filepath.Join(units, ".subyard-keys-sync.json"))
		if err != nil {
			return ports.RuntimePresence{}, err
		}
		facts["timer_receipt"] = receipt
	}
	presence.Actual = installedRuntimeDigest(facts)
	return presence, nil
}

func (runtime Runtime) installedGuestPresence(ctx context.Context) (ports.RuntimePresence, ports.InstanceInfo, error) {
	if runtime.Incus == nil {
		return ports.RuntimePresence{}, ports.InstanceInfo{}, errors.New("installed runtime observation requires Incus")
	}
	if probe, ok := runtime.Incus.(interface{ LocalInstallationAbsent() bool }); ok && probe.LocalInstallationAbsent() {
		return ports.RuntimePresence{Actual: installedRuntimeDigest("local-installation-absent")}, ports.InstanceInfo{}, nil
	}
	instance, err := runtime.Incus.Instance(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName)
	if errors.Is(err, ports.ErrInstanceNotFound) {
		return ports.RuntimePresence{Actual: installedRuntimeDigest("absent")}, ports.InstanceInfo{}, nil
	}
	if err != nil {
		return ports.RuntimePresence{}, ports.InstanceInfo{}, err
	}
	preview, _ := instance.EffectiveConfig("user.subyard.preview_sha256")
	usage, _ := instance.EffectiveConfig("user.subyard.ccusage_version")
	presence := ports.RuntimePresence{Preview: preview != "", CCUsage: usage != ""}
	if strings.EqualFold(instance.Status, "stopped") {
		// No guest probe wakes a stopped instance. Missing legacy markers cannot
		// prove that its previously installed helpers are absent.
		presence.Preview, presence.CCUsage = true, true
	} else if !strings.EqualFold(instance.Status, "running") {
		return ports.RuntimePresence{}, ports.InstanceInfo{}, fmt.Errorf("cannot inspect installed helpers while yard state is %q", instance.Status)
	}
	presence.Actual = installedRuntimeDigest(struct {
		Project, Instance, Status, Preview, Usage string
	}{runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, instance.Status, preview, usage})
	return presence, instance, nil
}

func installedRuntimeDigest(value any) string {
	payload, _ := json.Marshal(value)
	return fmt.Sprintf("%x", sha256.Sum256(payload))
}

func installedRuntimeObservation(ready bool, actual, desired any) ports.RuntimeObservation {
	digest := installedRuntimeDigest(desired)
	if ready {
		return ports.RuntimeObservation{State: ports.RuntimeStateCurrent, Actual: digest, Desired: digest}
	}
	return ports.RuntimeObservation{State: ports.RuntimeStateStale, Actual: installedRuntimeDigest(actual), Desired: digest}
}

func (runtime Runtime) installedRuntimeSources(paths ...string) (map[string]string, error) {
	result := map[string]string{}
	for _, path := range paths {
		payload, err := (config.MaterializedAsset{Source: filepath.Join(runtime.RepositoryRoot, path)}).ReadSource()
		if err != nil {
			return nil, err
		}
		result[path] = fmt.Sprintf("%x", sha256.Sum256(payload))
	}
	return result, nil
}

func (runtime Runtime) installedRuntimeSettings(names ...string) map[string]string {
	result := map[string]string{}
	for _, name := range names {
		result[name] = runtime.environmentValue(name)
	}
	return result
}

const installedGuestProbe = `
for path in /usr/local/bin/subyard-preview /etc/subyard/preview.json /usr/local/bin/ccusage /usr/local/bin/ccusage.subyard-runtime.json /var/lib/subyard/ccusage-ownership /etc/subyard/guest-runtime.json; do
  printf '%s\n' "$path"
  if [ -e "$path" ] || [ -L "$path" ]; then
    stat -c '%F|%a|%u|%g|%d|%i|%s|%Y|%Z' "$path"
    if [ -f "$path" ] && [ ! -L "$path" ]; then sha256sum "$path"; fi
  else printf 'absent\n'; fi
done
`

func (runtime Runtime) observeInstalledGuestRuntime(ctx context.Context) (ports.RuntimeObservation, error) {
	presence, instance, err := runtime.installedGuestPresence(ctx)
	if err != nil {
		return ports.RuntimeObservation{}, err
	}
	if strings.EqualFold(instance.Status, "stopped") {
		return ports.RuntimeObservation{State: ports.RuntimeStateDeferred, Actual: presence.Actual}, nil
	}
	// Provision publishes these ownership/convergence markers. Unprovisioned
	// guests and unowned same-name executables are outside release refresh.
	if !presence.Preview && !presence.CCUsage {
		return ports.RuntimeObservation{State: ports.RuntimeStateAbsent}, nil
	}
	if runtime.Executor == nil {
		return ports.RuntimeObservation{}, errors.New("installed runtime observation requires guest executor")
	}
	probe, err := runtime.Executor.Exec(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, ports.InstanceExecRequest{Command: []string{"sh", "-eu", "-c", installedGuestProbe}})
	if err != nil {
		return ports.RuntimeObservation{}, err
	}
	if probe.ExitCode != 0 || len(probe.Stdout) > 16384 {
		return ports.RuntimeObservation{}, errors.New("cannot inspect installed guest helpers")
	}
	sources, err := runtime.installedRuntimeSources("config/preview/subyard-preview", "config/agents/ccusage/provision.sh", "scripts/refresh-installed-guest-runtime.sh")
	if err != nil {
		return ports.RuntimeObservation{}, err
	}
	skillPaths, err := runtime.agentSkillSources()
	if err != nil {
		return ports.RuntimeObservation{}, err
	}
	if len(skillPaths) != 0 {
		skillSources, err := runtime.installedRuntimeSources(skillPaths...)
		if err != nil {
			return ports.RuntimeObservation{}, err
		}
		for path, hash := range skillSources {
			sources[path] = hash
		}
	}
	endpoint, err := runtime.previewEndpoint(ctx)
	if err != nil {
		return ports.RuntimeObservation{}, err
	}
	settings := runtime.installedRuntimeSettings("ALLOWS_CODING_TOOLS", "CCUSAGE_VERSION", "CCUSAGE_SHA256_AMD64", "CCUSAGE_SHA256_ARM64", "INCUS_PROJECT", "YARD_INSTANCE_NAME")
	if runtime.guestFactsAvailable() {
		settings["guest_runtime_facts"] = string(runtime.guestRuntimeFacts())
	}
	ready, err := runtime.previewConverged(ctx)
	if err != nil {
		return ports.RuntimeObservation{}, err
	}
	ready = ready && runtime.previewRouteConverged(ctx, instance)
	if len(skillPaths) != 0 && ready {
		skillsReady, err := runtime.agentSkillsConverged(ctx, skillPaths)
		if err != nil {
			return ports.RuntimeObservation{}, err
		}
		ready = ready && skillsReady
	}
	if runtime.environmentValue("ALLOWS_CODING_TOOLS") != "false" {
		payload, err := (config.MaterializedAsset{Source: filepath.Join(runtime.RepositoryRoot, "config/agents/ccusage/provision.sh")}).ReadSource()
		if err != nil {
			return ports.RuntimeObservation{}, err
		}
		// Keep expected inner drift separate from an Incus operation failure,
		// which can also carry nonzero guest-exit metadata.
		result, err := runtime.Executor.Exec(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, ports.InstanceExecRequest{
			Command: []string{"bash", "-c", `bash -eu -s -- --check >/dev/null
result=$?
case "$result" in
  0) printf 'ccusage-current\n' ;;
  1) printf 'ccusage-stale\n' ;;
  *) exit "$result" ;;
esac
`}, Stdin: payload,
			Environment: runtime.installedRuntimeSettings("CCUSAGE_VERSION", "CCUSAGE_SHA256_AMD64", "CCUSAGE_SHA256_ARM64"),
		})
		if err != nil {
			return ports.RuntimeObservation{}, err
		}
		if result.ExitCode != 0 || string(result.Stdout) != "ccusage-current\n" && string(result.Stdout) != "ccusage-stale\n" {
			return ports.RuntimeObservation{}, errors.New("ccusage readiness check failed")
		}
		ready = ready && string(result.Stdout) == "ccusage-current\n"
	}
	if ready && runtime.guestFactsAvailable() {
		ready, err = runtime.guestFactsConverged(ctx)
		if err != nil {
			return ports.RuntimeObservation{}, err
		}
	}
	markers := map[string]string{}
	for _, name := range []string{"user.subyard.preview_sha256", "user.subyard.preview_endpoint_sha256", "user.subyard.ccusage_version", previewroute.Key} {
		markers[name] = instance.LocalConfig[name]
	}
	return installedRuntimeObservation(ready, struct {
		Files   string
		Config  map[string]string
		Devices map[string]string
	}{string(probe.Stdout), markers, instance.LocalDevices[previewroute.DeviceName]}, struct {
		Sources, Settings map[string]string
		Endpoint          string
	}{sources, settings, string(endpoint)}), nil
}

func installedRuntimeFile(path string) (any, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	for parent := filepath.Dir(path); parent != string(filepath.Separator); parent = filepath.Dir(parent) {
		info, err := os.Lstat(parent)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("unsafe installed runtime parent")
		}
	}
	// Public binaries/unit files only. Keys and ledger payloads are never read.
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, errors.New("installed runtime file metadata unavailable")
	}
	value := struct {
		Mode          os.FileMode
		Size          int64
		Modified      int64
		Device, Inode uint64
		UID, GID      uint32
		Changed       syscall.Timespec
		Hash          string
	}{Mode: info.Mode(), Size: info.Size(), Modified: info.ModTime().UnixNano(), Device: stat.Dev, Inode: stat.Ino, UID: stat.Uid, GID: stat.Gid, Changed: stat.Ctim}
	if info.Mode().IsRegular() {
		payload, err := (config.MaterializedAsset{Source: path}).ReadSource()
		if err != nil {
			return nil, err
		}
		value.Hash = fmt.Sprintf("%x", sha256.Sum256(payload))
	}
	return value, nil
}

func (runtime Runtime) observeInstalledKeyRuntime(ctx context.Context) (ports.RuntimeObservation, error) {
	tools := runtime.environmentDefault("SUBYARD_KEYS_TOOLS_DIR", filepath.Join(runtime.Yard.Paths.DataHome, "tools"))
	units := runtime.environmentDefault("SUBYARD_KEYS_SYSTEMD_DIR", filepath.Join(runtime.Yard.Paths.OperatorHome, ".config/systemd/user"))
	facts := map[string]any{}
	presence, err := runtime.InstalledRuntimePresence(ctx, "key-tools-runtime")
	if err != nil {
		return ports.RuntimeObservation{}, err
	}
	facts["presence"] = presence
	leaves := runtime.installedKeyLeaves()
	if len(leaves) == 0 {
		return ports.RuntimeObservation{State: ports.RuntimeStateAbsent}, nil
	}
	facts["leaves"] = leaves
	identity := filepath.Join(runtime.environmentDefault("SUBYARD_KEYS_ROOT", filepath.Join(runtime.Yard.Paths.ConfigHome, "keys")), "identity.json")
	if info, err := os.Lstat(identity); err == nil {
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			facts["identity_metadata"] = []any{info.Mode(), stat.Dev, stat.Ino, stat.Uid, stat.Gid, stat.Mtim, stat.Ctim}
		}
	} else if !os.IsNotExist(err) {
		return ports.RuntimeObservation{}, err
	}
	for _, path := range []string{tools, filepath.Join(tools, "bin"), units} {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return ports.RuntimeObservation{}, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return ports.RuntimeObservation{}, errors.New("unsafe installed tool directory")
		}
		facts[path], err = installedRuntimeFile(path)
		if err != nil {
			return ports.RuntimeObservation{}, err
		}
	}
	for _, path := range []string{filepath.Join(tools, "bin", "age"), filepath.Join(tools, "bin", "age-keygen"), filepath.Join(tools, "bin", "sops"), filepath.Join(tools, "bin", ".subyard-native-tools.json"), filepath.Join(units, "subyard-keys-sync.service"), filepath.Join(units, "subyard-keys-sync.timer"), filepath.Join(units, ".subyard-keys-sync.json")} {
		value, err := installedRuntimeFile(path)
		if err != nil {
			return ports.RuntimeObservation{}, err
		}
		facts[path] = value
	}
	sources, err := runtime.installedRuntimeSources("scripts/install-key-tools.sh", "scripts/install-keys-auto-sync.sh", "config/systemd/subyard-keys-sync.service.in", "config/systemd/subyard-keys-sync.timer.in")
	if err != nil {
		return ports.RuntimeObservation{}, err
	}
	ready := true
	for _, leaf := range leaves {
		check := "--check"
		if leaf == "install-keys-auto-sync.sh" {
			check = "--check-refresh"
		}
		current, err := runtime.scriptConverged(ctx, leaf, check)
		if err != nil {
			return ports.RuntimeObservation{}, err
		}
		facts[leaf] = current
		ready = ready && current
	}
	settings := runtime.installedRuntimeSettings("SUBYARD_AGE_VERSION", "SUBYARD_SOPS_VERSION", "SUBYARD_AGE_SHA256_AMD64", "SUBYARD_AGE_SHA256_ARM64", "SUBYARD_SOPS_SHA256_AMD64", "SUBYARD_SOPS_SHA256_ARM64", "SUBYARD_KEYS_ROOT", "SUBYARD_KEYS_TOOLS_DIR", "SUBYARD_KEYS_SYSTEMD_DIR", "SUBYARD_HOME", "SUBYARD_OPERATOR_HOME", "YARD_RUNTIME_ROOT")
	return installedRuntimeObservation(ready, facts, struct{ Sources, Settings map[string]string }{sources, settings}), nil
}

func (runtime Runtime) installedKeyLeaves() []string {
	tools := runtime.environmentDefault("SUBYARD_KEYS_TOOLS_DIR", filepath.Join(runtime.Yard.Paths.DataHome, "tools"))
	units := runtime.environmentDefault("SUBYARD_KEYS_SYSTEMD_DIR", filepath.Join(runtime.Yard.Paths.OperatorHome, ".config/systemd/user"))
	var result []string
	keys := runtime.environmentDefault("SUBYARD_KEYS_ROOT", filepath.Join(runtime.Yard.Paths.ConfigHome, "keys"))
	// Existing tool publication or a durable key identity permits tool repair.
	// The identity is inspected by metadata only; it is never imported or reset.
	for _, path := range []string{filepath.Join(tools, "bin"), filepath.Join(keys, "identity.json")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			result = append(result, "install-key-tools.sh")
			break
		}
	}
	for _, name := range []string{"subyard-keys-sync.service", "subyard-keys-sync.timer"} {
		if _, err := os.Lstat(filepath.Join(units, name)); !os.IsNotExist(err) {
			result = append(result, "install-keys-auto-sync.sh")
			break
		}
	}
	return result
}

func (runtime Runtime) ApplyInstalledRuntime(ctx context.Context, id, operationID, actual, desired string) error {
	return runtime.applyInstalledRuntimeLeaves(ctx, id, operationID, actual, desired, runtime.installedKeyLeaves())
}

func (runtime Runtime) applyInstalledRuntimeLeaves(ctx context.Context, id, operationID, actual, desired string, leaves []string) error {
	if !domain.SafeID(operationID) {
		return errors.New("installed runtime refresh requires operation ID")
	}
	state, err := runtime.ObserveInstalledRuntime(ctx, id)
	if err != nil {
		return err
	}
	if state.State == ports.RuntimeStateCurrent && state.Actual == desired {
		return nil
	}
	if state.State != ports.RuntimeStateStale || state.Actual != actual || state.Desired != desired {
		return domain.ErrPlanStale
	}
	if id == "core-guest-runtime" {
		endpoint, err := runtime.preparePreview(ctx)
		if err != nil {
			return err
		}
		if err := runtime.runScriptEnvironment(ctx, runtime.Stderr, map[string]string{"SUBYARD_PREVIEW_ENDPOINT": string(endpoint), "SUBYARD_PREPARED_INSTALLED_CLEANUP": runtime.preparedInstalledCleanup()}, "refresh-installed-guest-runtime.sh", "--yes"); err != nil {
			return err
		}
	} else {
		for index, script := range leaves {
			arguments := []string{"--yes"}
			if script == "install-keys-auto-sync.sh" {
				arguments = []string{"--refresh", "--yes"}
			}
			environment := map[string]string{}
			if index == len(leaves)-1 {
				environment["SUBYARD_PREPARED_INSTALLED_CLEANUP"] = runtime.preparedInstalledCleanup()
			}
			if err := runtime.runScriptEnvironment(ctx, runtime.Stderr, environment, script, arguments...); err != nil {
				return err
			}
		}
	}
	verified, err := runtime.ObserveInstalledRuntime(ctx, id)
	if err != nil {
		return err
	}
	if verified.State != ports.RuntimeStateCurrent || verified.Desired != desired {
		return errors.New("installed runtime refresh did not converge")
	}
	return nil
}

func (runtime Runtime) preparedInstalledCleanup() string {
	if runtime.environmentValue("SUBYARD_PREPARED_INSTALLED_STAGE") != "" {
		return "1"
	}
	return ""
}
