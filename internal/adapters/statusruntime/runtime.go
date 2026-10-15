package statusruntime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerpublication"
	"github.com/Subyard/Subyard/internal/ownerroute"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/uninstall"
)

type Runtime struct {
	Environment  map[string]string
	Resources    []resource.Definition
	Integrations []profile.IntegrationDefinition
	Profiles     []profile.Definition
	Program      string
	Security     ports.SecurityChecker
	Incus        ports.Incus
	Executor     ports.InstanceExecutor
	Now          func() time.Time
	ProbeTimeout time.Duration
}

type SpaceMeasurement struct {
	Figure     string
	MeasuredAt time.Time
}

func (runtime Runtime) ReadStatusFacts(
	ctx context.Context,
	yard domain.Context,
	running bool,
) (domain.StatusFacts, error) {
	security := "FAIL"
	if runtime.Security != nil {
		security, _ = runtime.Security.CheckSecurity(ctx, true, true)
		if security == "FAIL" {
			security, _ = runtime.Security.CheckSecurity(ctx, false, true)
		}
	}
	result := domain.StatusFacts{
		Profiles: strings.Fields(runtime.Environment["ENVIRONMENT_PROFILES"]),
		Security: security,
		Space:    runtime.space(ctx, yard, running),
	}
	result.Agents = runtime.agentStatus(ctx, yard, running)
	result.Shared = runtime.resourceStatus(ctx, running)
	result.Shared = append(result.Shared, runtime.profileDashboardStatus(ctx, yard, running)...)
	return result, nil
}

func (runtime Runtime) agentStatus(
	ctx context.Context,
	yard domain.Context,
	running bool,
) []domain.AgentStatus {
	agents := strings.Fields(runtime.Environment["CODING_TOOL_INTEGRATIONS"])
	health := runtime.ReadIntegrationHealth(ctx, yard, running)
	result := make([]domain.AgentStatus, 0, len(agents))
	for _, name := range agents {
		status := domain.AgentStatus{Name: name, State: "enabled"}
		var dashboard *profile.IntegrationDashboard
		for _, definition := range runtime.Integrations {
			if definition.Name == name {
				dashboard = definition.Dashboard
			}
		}
		if dashboard != nil {
			if port, err := strconv.Atoi(runtime.Environment[dashboard.PortSetting]); err == nil && port >= 1024 && port <= 65535 {
				status.DashboardPort = port
				status.DashboardGuestPort = dashboard.GuestPort
				status.DashboardScheme = dashboard.Scheme
				status.DashboardPath = dashboard.Path
			}
		}
		if state, present := health[name]; present {
			switch state {
			case "ready":
				status.State = "up"
				if dashboard != nil && yard.YardKind == domain.YardContainer {
					status.URL = runtime.integrationDashboardURL(ctx, yard, dashboard)
					if status.URL == "" {
						status.Hint = runtime.program() + " init"
					}
				}
			case "starting":
				status.State, status.Hint = "starting", "service started; readiness pending"
			case "failed":
				status.State, status.Hint = "down", runtime.program()+" init"
			default:
				status.State = "?"
			}
			result = append(result, status)
			continue
		}
		if dashboard != nil {
			status.State = "?"
			check := runtime.Environment["AGENT_"+name+"_CHECK"]
			if running && runtime.Executor != nil && check != "" {
				probeCtx, cancel := context.WithTimeout(ctx, runtime.probeTimeout())
				probe, err := runtime.Executor.Exec(probeCtx, yard.IncusProject, yard.YardInstanceName, ports.InstanceExecRequest{Command: []string{check}})
				timedOut := probeCtx.Err() != nil
				cancel()
				if timedOut {
					status.State = "?"
				} else if probe.ExitCode != 0 {
					status.State = "down"
					status.Hint = runtime.program() + " init"
				} else if err != nil {
					status.State = "?"
				} else {
					status.State = "up"
					if yard.YardKind == domain.YardContainer {
						status.URL = runtime.integrationDashboardURL(ctx, yard, dashboard)
						if status.URL == "" {
							status.Hint = runtime.program() + " init"
						}
					}
				}
			}
			result = append(result, status)
			continue
		}
		switch name {
		case "codex":
			status = runtime.codexRulesStatus(ctx, yard, running)
		case "claude", "opencode", "pi":
			status.State, status.Hint = "unverified", "runtime approvals have no supported offline check"
		}
		result = append(result, status)
	}
	return result
}

func (runtime Runtime) integrationDashboardURL(ctx context.Context, yard domain.Context, dashboard *profile.IntegrationDashboard) string {
	if dashboard.ContainerRoute == nil {
		return ""
	}
	if runtime.Incus == nil {
		return ""
	}
	probeCtx, cancel := context.WithTimeout(ctx, runtime.probeTimeout())
	defer cancel()
	instance, err := runtime.Incus.Instance(probeCtx, yard.IncusProject, yard.YardInstanceName)
	if err != nil {
		return ""
	}
	if instance.Config[ownerpublication.Prefix+dashboard.ContainerRoute.DeviceName] != "" {
		return ""
	}
	host, port, ready := ownerroute.Owned(instance.Config[dashboard.ContainerRoute.MarkerKey], instance.Devices[dashboard.ContainerRoute.DeviceName], dashboard.GuestPort)
	if !ready || port != runtime.Environment[dashboard.PortSetting] {
		return ""
	}
	return dashboard.Scheme + "://" + host + ":" + port + dashboard.Path
}

func (runtime Runtime) probeTimeout() time.Duration {
	if runtime.ProbeTimeout > 0 {
		return runtime.ProbeTimeout
	}
	return 2 * time.Second
}

func (runtime Runtime) program() string {
	if runtime.Program != "" {
		return runtime.Program
	}
	return "yard"
}

func (runtime Runtime) space(ctx context.Context, yard domain.Context, running bool) string {
	dataHome := yard.Paths.DataHome
	if dataHome == "" {
		dataHome = runtime.Environment["SUBYARD_HOME"]
	}
	if !running {
		return fmt.Sprintf("—  (yard stopped; on-host size: sudo du -sh %s)", dataHome)
	}
	now := time.Now()
	if runtime.Now != nil {
		now = runtime.Now()
	}
	cache := spaceCachePath(dataHome, yard.YardName)
	figure, measured := readSpaceCache(cache)
	return describeSpaceCache(SpaceMeasurement{Figure: figure, MeasuredAt: measured}, now,
		runtime.Environment["SPACE_TTL"], func() bool {
			return ctx.Err() == nil && runtime.startSpaceRefresh(yard, cache)
		})
}

func (runtime Runtime) startSpaceRefresh(yard domain.Context, cache string) bool {
	directory := filepath.Dir(cache)
	generation, err := uninstall.CaptureRoot(directory)
	if err != nil || len(generation.Entries) == 0 {
		return false
	}
	epoch, epochErr := uninstall.ReadOwnerEpoch(directory)
	if epochErr != nil {
		return false
	}
	probeContext, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, release, err := uninstall.OwnerLock(probeContext, directory, nil)
	if err != nil {
		return false
	}
	defer func() {
		if release != nil {
			release()
		}
	}()
	if err := uninstall.Admit(directory, []uninstall.Tree{generation}, epoch, false, true); err != nil {
		return false
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return false
	}
	probe, err := os.CreateTemp(directory, ".space-refresh-probe-")
	if err != nil {
		return false
	}
	probeName := probe.Name()
	if closeErr := probe.Close(); closeErr != nil {
		_ = os.Remove(probeName)
		return false
	}
	if err := os.Remove(probeName); err != nil {
		return false
	}
	refresh := exec.Command(
		"/bin/sh", "-c", spaceRefreshScript, "yard-space-refresh",
		cache, yard.IncusProject, yard.YardInstanceName, uninstall.OwnerLockPath(directory), fmt.Sprintf("%d:%d", generation.Entries[0].Device, generation.Entries[0].Inode), strconv.FormatUint(epoch, 10),
	)
	refresh.Env = environment(runtime.Environment)
	refresh.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// The asynchronous child takes its own barrier and rechecks the original
	// generation. Release our completed probe before it can attempt that lock.
	release()
	release = nil
	if err := refresh.Start(); err != nil {
		return false
	}
	_ = refresh.Process.Release()
	return true
}

func spaceCachePath(dataHome, yardName string) string {
	suffix := ""
	if yardName != "" && yardName != "default" {
		suffix = "-" + yardName
	}
	return filepath.Join(dataHome, "space"+suffix+".cache")
}

func (runtime Runtime) ReadSpace(yard domain.Context) (SpaceMeasurement, bool) {
	dataHome := yard.Paths.DataHome
	if dataHome == "" {
		dataHome = runtime.Environment["SUBYARD_HOME"]
	}
	figure, measuredAt := readSpaceCache(spaceCachePath(dataHome, yard.YardName))
	return SpaceMeasurement{Figure: figure, MeasuredAt: measuredAt}, figure != ""
}

func (runtime Runtime) RefreshSpace(
	ctx context.Context,
	yard domain.Context,
) (SpaceMeasurement, error) {
	if runtime.Executor == nil {
		return SpaceMeasurement{}, errors.New("space refresh requires an instance executor")
	}
	dataHome := yard.Paths.DataHome
	if dataHome == "" {
		dataHome = runtime.Environment["SUBYARD_HOME"]
	}
	generation, err := uninstall.CaptureRoot(dataHome)
	if err != nil {
		return SpaceMeasurement{}, err
	}
	if len(generation.Entries) == 0 {
		return SpaceMeasurement{}, errors.New("space cache installation root is absent; no cache was created")
	}
	epoch, epochErr := uninstall.ReadOwnerEpoch(dataHome)
	if epochErr != nil {
		return SpaceMeasurement{}, epochErr
	}
	_, release, err := uninstall.OwnerLock(ctx, dataHome, nil)
	if err != nil {
		return SpaceMeasurement{}, err
	}
	defer release()
	if err := uninstall.Admit(dataHome, []uninstall.Tree{generation}, epoch, false, true); err != nil {
		return SpaceMeasurement{}, err
	}
	cache := spaceCachePath(dataHome, yard.YardName)
	if err := os.MkdirAll(filepath.Dir(cache), 0o700); err != nil {
		return SpaceMeasurement{}, fmt.Errorf("prepare space cache: %w", err)
	}
	lock, err := os.OpenFile(cache+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return SpaceMeasurement{}, fmt.Errorf("open space cache lock: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return SpaceMeasurement{}, fmt.Errorf("lock space cache: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	result, err := runtime.Executor.Exec(ctx, yard.IncusProject, yard.YardInstanceName,
		ports.InstanceExecRequest{Command: []string{"sh", "-c", spaceMeasureCommand}})
	if err != nil {
		return SpaceMeasurement{}, fmt.Errorf("measure in-yard space: %w", err)
	}
	fields := strings.Fields(string(result.Stdout))
	if result.ExitCode != 0 || len(fields) != 1 || !validSpaceFigure(fields[0]) {
		return SpaceMeasurement{}, errors.New("measure in-yard space: invalid result")
	}
	measuredAt := time.Now()
	if runtime.Now != nil {
		measuredAt = runtime.Now()
	}
	measurement := SpaceMeasurement{Figure: fields[0], MeasuredAt: measuredAt}
	if err := writeSpaceCache(cache, measurement.Figure, measurement.MeasuredAt); err != nil {
		return SpaceMeasurement{}, fmt.Errorf("write space cache: %w", err)
	}
	return measurement, nil
}

// Incus 6.0.6 returns no disk state for managed / and /srv devices on both containers and VMs,
// so detailed status keeps size off its foreground path and refreshes this compatible cache.
const spaceMeasureCommand = `
set -- /
if grep -qs " /srv " /proc/mounts; then
  root_device="$(stat -c %d / 2>/dev/null)" || exit
  srv_device="$(stat -c %d /srv 2>/dev/null)" || exit
  [ "$root_device" = "$srv_device" ] || set -- "$@" /srv
fi
storage_source=/srv/incus-e2e/storage
storage_alias=/var/lib/incus/storage-pools/default
storage_exclude=
is_device_inode() {
  case "$1" in
    *[!0-9:]*|*:*:*|:*|*:) return 1 ;;
    *:*) return 0 ;;
    *) return 1 ;;
  esac
}
source_identity="$(stat -c %d:%i "$storage_source" 2>/dev/null)" || source_identity=
alias_identity="$(stat -c %d:%i "$storage_alias" 2>/dev/null)" || alias_identity=
if is_device_inode "$source_identity" && is_device_inode "$alias_identity" &&
  [ "$source_identity" = "$alias_identity" ]; then
  storage_exclude="--exclude=$storage_alias"
fi
total=0
for path do
  if [ "$path" = / ] && [ -n "$storage_exclude" ]; then
    output="$(du -skx "$storage_exclude" "$path" 2>/dev/null)" || exit
  else
    output="$(du -skx "$path" 2>/dev/null)" || exit
  fi
  size="$(printf "%s\n" "$output" | awk "NR == 1 { print \$1 }")"
  case "$size" in ""|*[!0-9]*) exit 1 ;; esac
  total=$((total + size))
done
awk -v size="$total" "
BEGIN {
  split(\"K M G T P E Z Y\", units)
  unit = 1
  while (size >= 1024 && unit < 8) {
    size /= 1024
    unit++
  }
  if (size >= 10 || size == int(size))
    printf \"%.0f%s\\n\", size, units[unit]
  else
    printf \"%.1f%s\\n\", size, units[unit]
}"
`

const spaceFigurePattern = `[0-9]+([.][0-9]+)?[KMGTPEZY]?(i?B)?`

var spaceFigureRegexp = regexp.MustCompile("^" + spaceFigurePattern + "$")

const spaceRefreshScript = `
set -eu
cache=$1
project=$2
instance=$3
owner_lock=$4
root_identity=$5
approved_epoch=$6
# This existing stable account lock protects the cache root through publication.
# A queued process keeps its original root identity and never reinstalls it.
[ -f "$owner_lock" ] && [ ! -L "$owner_lock" ] || exit 0
exec 8<>"$owner_lock"
flock -n 8 || exit 0
[ ! -e "$(dirname "$cache").uninstall" ] || exit 0
# Publication is atomic in a separate nonsecret file: never replace fd 8's inode.
epoch=0
epoch_path=$owner_lock.epoch
if [ -e "$epoch_path" ] || [ -L "$epoch_path" ]; then
  [ -f "$epoch_path" ] && [ ! -L "$epoch_path" ] || exit 0
  [ "$(stat -c '%a:%u' "$epoch_path")" = "600:$(id -u)" ] || exit 0
  epoch=$(cat "$epoch_path") || exit 0
fi
[ "$epoch" = "$approved_epoch" ] || exit 0
[ "$(stat -c '%d:%i' "$(dirname "$cache")" 2>/dev/null)" = "$root_identity" ] || exit 0
lock=$cache.lock
temporary=$cache.tmp
umask 077
exec 9>"$lock"
flock -n 9 || exit 0
trap 'rm -f -- "$temporary"' EXIT HUP INT TERM
if [ -n "${SUBYARD_INCUS_SOCKET:-}" ]; then
  export INCUS_SOCKET=$SUBYARD_INCUS_SOCKET
fi
figure="$(
  timeout --signal=TERM --kill-after=1s 10s \
    incus exec "$instance" --project "$project" -- sh -c '` + spaceMeasureCommand + `'
)" || exit 0
printf '%s\n' "$figure" | grep -Eq '^` + spaceFigurePattern + `$' || exit 0
[ -e "$lock" ] || exit 0
printf '%s %s\n' "$figure" "$(date +%s)" >"$temporary"
mv -f -- "$temporary" "$cache"
trap - EXIT HUP INT TERM
`

func readSpaceCache(path string) (string, time.Time) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return "", time.Time{}
	}
	fields := strings.Fields(string(payload))
	if len(fields) != 2 || !validSpaceFigure(fields[0]) {
		return "", time.Time{}
	}
	epoch, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || epoch < 0 {
		return "", time.Time{}
	}
	return fields[0], time.Unix(epoch, 0)
}

func writeSpaceCache(path, figure string, measured time.Time) error {
	if path == "" || !validSpaceFigure(figure) {
		return fmt.Errorf("invalid status space cache")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, ".space-cache-")
	if err != nil {
		return err
	}
	temp := file.Name()
	defer os.Remove(temp)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := fmt.Fprintf(file, "%s %d\n", figure, measured.Unix()); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(temp, path)
}

func validSpaceFigure(value string) bool {
	return len(value) <= 32 && spaceFigureRegexp.MatchString(value)
}

func ageHuman(age time.Duration) string {
	seconds := int64(age / time.Second)
	switch {
	case seconds < 60:
		return fmt.Sprintf("%ds", seconds)
	case seconds < 3600:
		return fmt.Sprintf("%dm", seconds/60)
	case seconds < 86400:
		return fmt.Sprintf("%dh", seconds/3600)
	default:
		return fmt.Sprintf("%dd", seconds/86400)
	}
}

func (runtime Runtime) resourceStatus(ctx context.Context, running bool) []domain.SharedResourceStatus {
	result := make([]domain.SharedResourceStatus, 0, len(runtime.Resources))
	program := runtime.Program
	if program == "" {
		program = "yard"
	}
	timeout := runtime.ProbeTimeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	for _, definition := range runtime.Resources {
		status := domain.SharedResourceStatus{
			Profile: definition.Profile, Name: definition.Name, State: "?",
		}
		if running {
			probeCtx, cancel := context.WithTimeout(ctx, timeout)
			probe := exec.CommandContext(probeCtx, definition.HandlerPath(), "is-up")
			probe.Env = append(environment(runtime.Environment), "SUBYARD_PROFILE_SELECTED=1")
			err := probe.Run()
			timedOut := probeCtx.Err() != nil
			cancel()
			if timedOut {
				result = append(result, status)
				continue
			}
			if err == nil {
				status.State = "up"
				status.Hint = program + " " + definition.Command + " " + definition.Shutdown
				if hint := runtime.managementHint(definition); hint != "" {
					status.Hint = hint
				}
				status.URL = runtime.dashboardURL(definition)
			} else {
				status.State = "down"
				status.Hint = program + " " + definition.Command + " " + definition.BringUp
			}
		}
		result = append(result, status)
	}
	return result
}

var dashboardHostname = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?$`)

func (runtime Runtime) dashboardURL(definition resource.Definition) string {
	if definition.Dashboard == nil {
		return ""
	}
	address := runtime.endpointAddress(definition.Dashboard.HostSetting, definition.Dashboard.PortSetting)
	if address == "" {
		return ""
	}
	return (&url.URL{
		Scheme: definition.Dashboard.Scheme,
		Host:   address,
		Path:   definition.Dashboard.Path,
	}).String()
}

func (runtime Runtime) managementHint(definition resource.Definition) string {
	management := definition.Management
	if management == nil {
		return ""
	}
	address := runtime.endpointAddress(management.HostSetting, management.PortSetting)
	if address == "" {
		return ""
	}
	return "Manage in " + management.Application + ": " + management.User + "@" + address
}

func (runtime Runtime) endpointAddress(hostSetting, portSetting string) string {
	host := runtime.Environment[hostSetting]
	if net.ParseIP(host) == nil && !dashboardHostname.MatchString(host) {
		return ""
	}
	portText := runtime.Environment[portSetting]
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != portText {
		return ""
	}
	return net.JoinHostPort(host, portText)
}

func environment(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}

func (runtime Runtime) profileDashboardStatus(ctx context.Context, yard domain.Context, running bool) []domain.SharedResourceStatus {
	phaseCtx, cancel := context.WithTimeout(ctx, profile.ReadinessTimeout)
	defer cancel()
	var result []domain.SharedResourceStatus
	for _, definition := range runtime.Profiles {
		dashboard := definition.Dashboard
		if dashboard == nil || !definition.Selected(yard.YardName, runtime.Environment) {
			continue
		}
		status := domain.SharedResourceStatus{Profile: definition.Name, Name: "dashboard", State: "down", Hint: runtime.program() + " init"}
		if running {
			started := time.Now()
			err := phaseCtx.Err()
			if err == nil {
				probe := exec.CommandContext(phaseCtx, filepath.Join(definition.Root, definition.OwnerService), "--check")
				probe.Env = append(environment(runtime.Environment), "SUBYARD_PROFILE_SELECTED=1")
				probe.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
				probe.Cancel = func() error {
					if probe.Process == nil {
						return nil
					}
					return syscall.Kill(-probe.Process.Pid, syscall.SIGKILL)
				}
				probe.WaitDelay = 2 * time.Second
				err = probe.Run()
			}
			if err == nil && phaseCtx.Err() == nil {
				status.State = "up"
				if yard.YardKind == domain.YardVM {
					status.URL = dashboard.Scheme + "://127.0.0.1:" + runtime.Environment[dashboard.PortSetting] + dashboard.Path
				} else {
					status.URL = runtime.integrationDashboardURL(phaseCtx, yard, dashboard)
				}
				if status.URL == "" {
					status.State = "down"
				}
			}
			if contextErr := phaseCtx.Err(); err != nil || contextErr != nil {
				status.State, status.URL = "down", ""
				status.Hint += profileCheckDiagnostic(err, contextErr, time.Since(started))
			}
		}
		result = append(result, status)
	}
	return result
}

func profileCheckDiagnostic(err, contextErr error, elapsed time.Duration) string {
	category, native := "unavailable", "none"
	var exit *exec.ExitError
	switch {
	case errors.Is(contextErr, context.DeadlineExceeded):
		category = "deadline"
	case errors.Is(contextErr, context.Canceled):
		category = "cancelled"
	case errors.As(err, &exit) && exit.ExitCode() > 0 && exit.ExitCode() <= 255:
		category, native = "native-failure", strconv.Itoa(exit.ExitCode())
	}
	return fmt.Sprintf(" (profile check reason=%s native_exit=%s elapsed_ms=%d budget_ms=%d)", category, native, max(0, elapsed.Milliseconds()), profile.ReadinessTimeout.Milliseconds())
}
