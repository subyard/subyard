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
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/resource"
)

type Runtime struct {
	Environment  map[string]string
	Resources    []resource.Definition
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
	return result, nil
}

func (runtime Runtime) agentStatus(
	ctx context.Context,
	yard domain.Context,
	running bool,
) []domain.AgentStatus {
	agents := strings.Fields(runtime.Environment["CODING_TOOL_INTEGRATIONS"])
	result := make([]domain.AgentStatus, 0, len(agents))
	for _, name := range agents {
		status := domain.AgentStatus{Name: name, State: "enabled"}
		if name != "aiobserver" {
			result = append(result, status)
			continue
		}
		status.State = "?"
		if port, err := strconv.Atoi(runtime.Environment["AI_OBSERVER_HOST_PORT"]); err == nil &&
			port >= 1024 && port <= 65535 && strconv.Itoa(port) == runtime.Environment["AI_OBSERVER_HOST_PORT"] {
			status.DashboardPort = port
		}
		if running && runtime.Executor != nil {
			probeCtx, cancel := context.WithTimeout(ctx, runtime.probeTimeout())
			probe, err := runtime.Executor.Exec(
				probeCtx, yard.IncusProject, yard.YardInstanceName,
				ports.InstanceExecRequest{Command: []string{"/usr/local/bin/ai-observer-check"}},
			)
			timedOut := probeCtx.Err() != nil
			cancel()
			switch {
			case timedOut:
				status.State = "?"
			case probe.ExitCode != 0:
				status.State = "down"
				status.Hint = runtime.program() + " init"
			case err != nil:
				status.State = "?"
			default:
				status.State = "up"
				if yard.YardKind == domain.YardContainer && runtime.aiObserverRouteReady(ctx, yard) {
					status.URL = "http://127.0.0.1:" + runtime.Environment["AI_OBSERVER_HOST_PORT"] + "/"
				} else if yard.YardKind == domain.YardContainer {
					status.Hint = runtime.program() + " init"
				}
			}
		}
		result = append(result, status)
	}
	return result
}

func (runtime Runtime) aiObserverRouteReady(ctx context.Context, yard domain.Context) bool {
	port, err := strconv.Atoi(runtime.Environment["AI_OBSERVER_HOST_PORT"])
	if err != nil || port < 1024 || port > 65535 || strconv.Itoa(port) != runtime.Environment["AI_OBSERVER_HOST_PORT"] ||
		runtime.Incus == nil {
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, runtime.probeTimeout())
	defer cancel()
	instance, err := runtime.Incus.Instance(probeCtx, yard.IncusProject, yard.YardInstanceName)
	if err != nil {
		return false
	}
	device := instance.Devices["ai-observer"]
	if len(device) != 4 {
		return false
	}
	return instance.Config["user.subyard.ai_observer_proxy"] == "v1:"+strconv.Itoa(port) &&
		device["type"] == "proxy" && device["bind"] == "host" &&
		device["listen"] == "tcp:127.0.0.1:"+strconv.Itoa(port) &&
		device["connect"] == "tcp:127.0.0.1:8080"
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
	ttl := 10 * time.Minute
	if seconds, err := strconv.Atoi(runtime.Environment["SPACE_TTL"]); err == nil && seconds > 0 {
		ttl = time.Duration(seconds) * time.Second
	}
	stale := figure == "" || now.Sub(measured) > ttl
	refreshing := false
	if stale && ctx.Err() == nil {
		refreshing = runtime.startSpaceRefresh(yard, cache)
	}
	if figure == "" {
		if refreshing {
			return "in-yard size unavailable — refresh started"
		}
		return "in-yard size unavailable"
	}
	age := now.Sub(measured)
	if age < 0 {
		age = 0
	}
	note := ""
	if stale {
		note = ", refresh unavailable"
		if refreshing {
			note = ", refresh started"
		}
	}
	return fmt.Sprintf("%s  (in-yard rootfs, %s ago%s)", figure, ageHuman(age), note)
}

func (runtime Runtime) startSpaceRefresh(yard domain.Context, cache string) bool {
	directory := filepath.Dir(cache)
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
		cache, yard.IncusProject, yard.YardInstanceName,
	)
	refresh.Env = environment(runtime.Environment)
	refresh.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
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
			probe.Env = environment(runtime.Environment)
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
	host := runtime.Environment[definition.Dashboard.HostSetting]
	if net.ParseIP(host) == nil && !dashboardHostname.MatchString(host) {
		return ""
	}
	portText := runtime.Environment[definition.Dashboard.PortSetting]
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != portText {
		return ""
	}
	return (&url.URL{
		Scheme: definition.Dashboard.Scheme,
		Host:   net.JoinHostPort(host, portText),
		Path:   definition.Dashboard.Path,
	}).String()
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
