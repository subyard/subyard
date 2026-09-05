package statusruntime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/testkit"
)

type securityStub struct{ state string }

func (stub securityStub) CheckSecurity(context.Context, bool, bool) (string, error) {
	return stub.state, nil
}

type spaceExecutorStub struct {
	result ports.InstanceExecResult
	err    error
	calls  []ports.InstanceExecRequest
}

type statusExecutorFunc func(context.Context, string, string, ports.InstanceExecRequest) (ports.InstanceExecResult, error)

func (function statusExecutorFunc) Exec(
	ctx context.Context, project, instance string, request ports.InstanceExecRequest,
) (ports.InstanceExecResult, error) {
	return function(ctx, project, instance, request)
}

func (stub *spaceExecutorStub) Exec(
	_ context.Context,
	_, _ string,
	request ports.InstanceExecRequest,
) (ports.InstanceExecResult, error) {
	stub.calls = append(stub.calls, request)
	return stub.result, stub.err
}

func TestRuntimeReadsAndRefreshesSpaceSynchronously(t *testing.T) {
	root := t.TempDir()
	oldTime := time.Unix(100, 0)
	newTime := time.Unix(200, 0)
	cache := filepath.Join(root, "space-demo.cache")
	if err := writeSpaceCache(cache, "2G", oldTime); err != nil {
		t.Fatal(err)
	}
	executor := &spaceExecutorStub{result: ports.InstanceExecResult{Stdout: []byte("3G\n")}}
	runtime := Runtime{Executor: executor, Now: func() time.Time { return newTime }}
	yard := domain.Context{
		YardName: "demo", IncusProject: "subyard-demo", YardInstanceName: "yard-demo",
		Paths: domain.RuntimePaths{DataHome: root},
	}
	cached, ok := runtime.ReadSpace(yard)
	if !ok || cached.Figure != "2G" || !cached.MeasuredAt.Equal(oldTime) {
		t.Fatalf("cached measurement = %#v ok=%v", cached, ok)
	}
	refreshed, err := runtime.RefreshSpace(context.Background(), yard)
	if err != nil || refreshed.Figure != "3G" || !refreshed.MeasuredAt.Equal(newTime) {
		t.Fatalf("refreshed measurement = %#v err=%v", refreshed, err)
	}
	if len(executor.calls) != 1 || len(executor.calls[0].Command) != 3 ||
		executor.calls[0].Command[0] != "sh" || executor.calls[0].Command[1] != "-c" ||
		!strings.Contains(executor.calls[0].Command[2], "du -skx") ||
		!strings.Contains(executor.calls[0].Command[2], "/srv") {
		t.Fatalf("refresh command = %#v", executor.calls)
	}
	stored, ok := runtime.ReadSpace(yard)
	if !ok || stored != refreshed {
		t.Fatalf("stored measurement = %#v ok=%v, want %#v", stored, ok, refreshed)
	}
	executor.result = ports.InstanceExecResult{Stdout: []byte("1..2G\n")}
	if _, err := runtime.RefreshSpace(context.Background(), yard); err == nil {
		t.Fatal("refresh accepted an invalid measurement")
	}
	stored, ok = runtime.ReadSpace(yard)
	if !ok || stored != refreshed {
		t.Fatalf("invalid refresh changed cache: %#v ok=%v, want %#v", stored, ok, refreshed)
	}
}

func TestSpaceMeasureCommandFailsWhenDuFails(t *testing.T) {
	directory := t.TempDir()
	du := filepath.Join(directory, "du")
	if err := os.WriteFile(du, []byte("#!/bin/sh\nprintf '9\\t/\\n'\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("sh", "-c", spaceMeasureCommand)
	command.Env = append(os.Environ(), "PATH="+directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("measurement succeeded after du failure: %q", output)
	}
}

func TestSpaceMeasureCommandAddsMountedSrvSeparately(t *testing.T) {
	directory := t.TempDir()
	calls := filepath.Join(directory, "calls")
	if err := os.WriteFile(filepath.Join(directory, "grep"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	statScript := "#!/bin/sh\ncase \"$*\" in */srv) printf '2\\n' ;; *) printf '1\\n' ;; esac\n"
	if err := os.WriteFile(filepath.Join(directory, "stat"), []byte(statScript), 0o700); err != nil {
		t.Fatal(err)
	}
	duScript := `#!/bin/sh
printf '%s\n' "$*" >>"$SPACE_CALLS"
printf '512\tpath\n'
`
	if err := os.WriteFile(filepath.Join(directory, "du"), []byte(duScript), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("sh", "-c", spaceMeasureCommand)
	command.Env = append(os.Environ(),
		"PATH="+directory+string(os.PathListSeparator)+os.Getenv("PATH"),
		"SPACE_CALLS="+calls,
	)
	output, err := command.CombinedOutput()
	if err != nil || string(output) != "1M\n" {
		t.Fatalf("measurement output=%q err=%v", output, err)
	}
	payload, err := os.ReadFile(calls)
	if err != nil || strings.Count(string(payload), "\n") != 2 ||
		!strings.Contains(string(payload), "/srv") {
		t.Fatalf("du calls=%q err=%v", payload, err)
	}
}

func TestSpaceMeasureCommandDoesNotDoubleCountSameDeviceSrv(t *testing.T) {
	directory := t.TempDir()
	calls := filepath.Join(directory, "calls")
	for name, script := range map[string]string{
		"grep": "#!/bin/sh\nexit 0\n",
		"stat": "#!/bin/sh\nprintf '1\\n'\n",
		"du":   "#!/bin/sh\nprintf '%s\\n' \"$*\" >>\"$SPACE_CALLS\"\nprintf '1024\\tpath\\n'\n",
	} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.Command("sh", "-c", spaceMeasureCommand)
	command.Env = append(os.Environ(),
		"PATH="+directory+string(os.PathListSeparator)+os.Getenv("PATH"),
		"SPACE_CALLS="+calls,
	)
	output, err := command.CombinedOutput()
	if err != nil || string(output) != "1M\n" {
		t.Fatalf("measurement output=%q err=%v", output, err)
	}
	payload, err := os.ReadFile(calls)
	if err != nil || strings.Count(string(payload), "\n") != 1 || strings.Contains(string(payload), "/srv") {
		t.Fatalf("du calls=%q err=%v", payload, err)
	}
}

func TestSpaceMeasureCommandExcludesOnlyVerifiedInnerIncusStorageAlias(t *testing.T) {
	directory := t.TempDir()
	calls := filepath.Join(directory, "calls")
	for name, script := range map[string]string{
		"grep": "#!/bin/sh\nexit 0\n",
		"stat": `#!/bin/sh
		case "$*" in
		  *"%d:%i /srv/incus-e2e/storage"|*"%d:%i /var/lib/incus/storage-pools/default") printf '7:11\n' ;;
		  */srv) printf '2\n' ;;
		  *) printf '1\n' ;;
		esac
`,
		"du": `#!/bin/sh
		printf '%s\n' "$*" >>"$SPACE_CALLS"
		printf '512\tpath\n'
`,
	} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	output, err := runSpaceMeasureCommand(directory, calls)
	if err != nil || string(output) != "1M\n" {
		t.Fatalf("measurement output=%q err=%v", output, err)
	}
	payload, err := os.ReadFile(calls)
	lines := strings.Split(strings.TrimSpace(string(payload)), "\n")
	if err != nil || len(lines) != 2 || strings.Count(string(payload), "--exclude=") != 1 ||
		lines[0] != "-skx --exclude=/var/lib/incus/storage-pools/default /" ||
		lines[1] != "-skx /srv" {
		t.Fatalf("du calls=%q err=%v", payload, err)
	}
}

func TestSpaceMeasureCommandDoesNotExcludeUnverifiedInnerIncusStorageAlias(t *testing.T) {
	for _, test := range []struct {
		name       string
		statScript string
	}{
		{
			name:       "different identities",
			statScript: "#!/bin/sh\ncase \"$*\" in *'/srv/incus-e2e/storage') printf '7:11\\n' ;; *'/var/lib/incus/storage-pools/default') printf '7:12\\n' ;; */srv) printf '2\\n' ;; *) printf '1\\n' ;; esac\n",
		},
		{
			name:       "absent alias",
			statScript: "#!/bin/sh\ncase \"$*\" in *'/var/lib/incus/storage-pools/default') exit 1 ;; */srv) printf '2\\n' ;; *) printf '1\\n' ;; esac\n",
		},
		{
			name:       "absent source",
			statScript: "#!/bin/sh\ncase \"$*\" in *'/srv/incus-e2e/storage') exit 1 ;; */srv) printf '2\\n' ;; *) printf '1\\n' ;; esac\n",
		},
		{
			name:       "failed identity probe",
			statScript: "#!/bin/sh\ncase \"$*\" in *'%d:%i'*) exit 1 ;; */srv) printf '2\\n' ;; *) printf '1\\n' ;; esac\n",
		},
		{
			name:       "malformed identical identities",
			statScript: "#!/bin/sh\ncase \"$*\" in *'%d:%i'*) printf '1\\n' ;; */srv) printf '2\\n' ;; *) printf '1\\n' ;; esac\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			calls := filepath.Join(directory, "calls")
			for name, script := range map[string]string{
				"grep": "#!/bin/sh\nexit 0\n",
				"stat": test.statScript,
				"du":   "#!/bin/sh\nprintf '%s\\n' \"$*\" >>\"$SPACE_CALLS\"\nprintf '512\\tpath\\n'\n",
			} {
				if err := os.WriteFile(filepath.Join(directory, name), []byte(script), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			output, err := runSpaceMeasureCommand(directory, calls)
			if err != nil || string(output) != "1M\n" {
				t.Fatalf("measurement output=%q err=%v", output, err)
			}
			payload, err := os.ReadFile(calls)
			if err != nil || strings.Count(string(payload), "\n") != 2 || strings.Contains(string(payload), "--exclude=") {
				t.Fatalf("du calls=%q err=%v", payload, err)
			}
		})
	}
}

func TestRuntimeUsesTheSameMeasurementCommandForSyncAndAsyncRefresh(t *testing.T) {
	root := t.TempDir()
	commandPath := filepath.Join(root, "async-command")
	executor := &spaceExecutorStub{result: ports.InstanceExecResult{Stdout: []byte("1G\n")}}
	yard := domain.Context{IncusProject: "subyard", YardInstanceName: "yard", Paths: domain.RuntimePaths{DataHome: root}}
	runtime := Runtime{Executor: executor, Environment: fakeIncusEnvironment(t, `
		for argument do command=$argument; done
		printf '%s' "$command" >"$SPACE_COMMAND"
		printf '1G\n'
	`)}
	if _, err := runtime.RefreshSpace(context.Background(), yard); err != nil {
		t.Fatal(err)
	}
	runtime.Environment["SPACE_COMMAND"] = commandPath
	if !runtime.startSpaceRefresh(yard, filepath.Join(root, "async-space.cache")) {
		t.Fatal("async refresh did not start")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if payload, err := os.ReadFile(commandPath); err == nil {
			if got, want := string(payload), executor.calls[0].Command[2]; got != want {
				t.Fatalf("async measurement command = %q, want %q", got, want)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("async refresh did not execute the measurement command")
}

func runSpaceMeasureCommand(directory, calls string) ([]byte, error) {
	command := exec.Command("sh", "-c", spaceMeasureCommand)
	command.Env = append(os.Environ(),
		"PATH="+directory+string(os.PathListSeparator)+os.Getenv("PATH"),
		"SPACE_CALLS="+calls,
	)
	return command.CombinedOutput()
}

func TestValidSpaceFigureUsesStrictGrammar(t *testing.T) {
	for _, value := range []string{"0", "1G", "1.5GiB", "12MB"} {
		if !validSpaceFigure(value) {
			t.Errorf("validSpaceFigure(%q) = false", value)
		}
	}
	for _, value := range []string{"", ".", "1..2", "G", "1.5.2G", "1 GB", "-1G"} {
		if validSpaceFigure(value) {
			t.Errorf("validSpaceFigure(%q) = true", value)
		}
	}
}

func TestRuntimeStartsAndReusesAsyncSpaceRefresh(t *testing.T) {
	root := t.TempDir()
	environment := fakeIncusEnvironment(t, "printf '1.5G\\n'\n")
	yard := domain.Context{
		YardName: "default", IncusProject: "subyard", YardInstanceName: "yard",
		Paths: domain.RuntimePaths{DataHome: root},
	}
	facts, err := (Runtime{
		Environment: environment, Security: securityStub{state: "live"},
	}).ReadStatusFacts(context.Background(), yard, true)
	if err != nil || len(facts.Shared) != 0 || facts.Security != "live" ||
		facts.Space != "in-yard size unavailable — refresh started" {
		t.Fatalf("structured status result changed: %#v err=%v", facts, err)
	}
	cache := filepath.Join(root, "space.cache")
	waitSpaceCache(t, cache, "1.5G")
	facts, err = (Runtime{
		Environment: environment,
	}).ReadStatusFacts(context.Background(), yard, true)
	if err != nil || !strings.HasPrefix(facts.Space, "1.5G  (in-yard rootfs, ") ||
		strings.Contains(facts.Space, "refresh started") {
		t.Fatalf("fresh native cache was not reused: %#v err=%v", facts, err)
	}
	facts, err = (Runtime{}).ReadStatusFacts(context.Background(), yard, false)
	if err != nil || facts.Space != "—  (yard stopped; on-host size: sudo du -sh "+root+")" {
		t.Fatalf("stopped status = %#v err=%v", facts, err)
	}
	if got := spaceCachePath(root, "default"); got != filepath.Join(root, "space.cache") {
		t.Fatalf("default yard cache path = %q", got)
	}
	if got := spaceCachePath(root, "demo"); got != filepath.Join(root, "space-demo.cache") {
		t.Fatalf("named yard cache path = %q", got)
	}
}

func TestRuntimeKeepsStaleSpaceWhenAsyncRefreshFails(t *testing.T) {
	root := t.TempDir()
	measured := time.Now().Add(-2 * time.Minute).Truncate(time.Second)
	if err := writeSpaceCache(filepath.Join(root, "space-demo.cache"), "2G", measured); err != nil {
		t.Fatal(err)
	}
	environment := fakeIncusEnvironment(t, "exit 1\n")
	facts, err := (Runtime{
		Environment: mapMerge(environment, map[string]string{"SPACE_TTL": "1"}),
		Now:         func() time.Time { return measured.Add(2 * time.Minute) },
	}).ReadStatusFacts(context.Background(), domain.Context{
		YardName: "demo", IncusProject: "subyard-demo", YardInstanceName: "yard-demo",
		Paths: domain.RuntimePaths{DataHome: root},
	}, true)
	if err != nil || facts.Space != "2G  (in-yard rootfs, 2m ago, refresh started)" {
		t.Fatalf("stale status result = %#v err=%v", facts, err)
	}
	time.Sleep(100 * time.Millisecond)
	figure, cachedAt := readSpaceCache(filepath.Join(root, "space-demo.cache"))
	if figure != "2G" || !cachedAt.Equal(measured) {
		t.Fatalf("failed refresh replaced valid cache: figure=%q measured=%s", figure, cachedAt)
	}
}

func TestRuntimeSkipsRefreshWhenCacheCannotBeWritten(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	facts, err := (Runtime{Environment: fakeIncusEnvironment(t, "printf '1G\\n'\n")}).
		ReadStatusFacts(context.Background(), domain.Context{
			YardName: "demo", IncusProject: "subyard-demo", YardInstanceName: "yard-demo",
			Paths: domain.RuntimePaths{DataHome: filepath.Join(blocker, "data")},
		}, true)
	if err != nil || facts.Space != "in-yard size unavailable" {
		t.Fatalf("unwritable cache started expensive refresh: facts=%#v err=%v", facts, err)
	}
}

func TestRuntimeProbesPreparedResources(t *testing.T) {
	root := t.TempDir()
	resources := filepath.Join(root, "config", "profiles", "demo", "resources")
	if err := os.MkdirAll(filepath.Join(resources, "service"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resources, "service.res"), []byte(
		"COMMAND=svc\nHANDLER=resources/service/handler.sh\nTITLE=Service\nDASHBOARD=\"https SERVICE_HOST SERVICE_PORT /ui/\"\nACTION=\"up up yard-change reversible\"\nACTION=\"down down yard-change reversible\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	handler := filepath.Join(resources, "service", "handler.sh")
	if err := os.WriteFile(handler, []byte("#!/bin/sh\n[ \"$1\" = is-up ]\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	registry, err := resource.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	dataHome := filepath.Join(root, "data")
	if err := writeSpaceCache(filepath.Join(dataHome, "space.cache"), "1G", time.Now()); err != nil {
		t.Fatal(err)
	}
	facts, err := (Runtime{
		Environment: map[string]string{
			"PATH": "/usr/bin:/bin", "SERVICE_HOST": "dashboard.example", "SERVICE_PORT": "8443",
		},
		Resources: registry.Definitions(), Program: "yard", Security: securityStub{state: "live"},
	}).ReadStatusFacts(context.Background(), domain.Context{
		IncusProject: "subyard", YardInstanceName: "yard",
		Paths: domain.RuntimePaths{DataHome: dataHome},
	}, true)
	if err != nil || len(facts.Shared) != 1 {
		t.Fatalf("resource status failed: %#v err=%v", facts, err)
	}
	status := facts.Shared[0]
	if status.Profile != "demo" || status.Name != "service" || status.State != "up" ||
		status.Hint != "yard svc down" || status.URL != "https://dashboard.example:8443/ui/" {
		t.Fatalf("unexpected resource status: %#v", status)
	}
}

func TestRuntimeReportsSelectedProfilesAndAgentsWithVerifiedDashboard(t *testing.T) {
	root := t.TempDir()
	dataHome := filepath.Join(root, "data")
	if err := writeSpaceCache(filepath.Join(dataHome, "space.cache"), "1G", time.Now()); err != nil {
		t.Fatal(err)
	}
	executor := &spaceExecutorStub{result: ports.InstanceExecResult{ExitCode: 0}}
	incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{
		"subyard/yard": {
			Name: "yard", Project: "subyard", Status: "Running",
			Config: map[string]string{"user.subyard.ai_observer_proxy": "v1:18080"},
			Devices: map[string]map[string]string{"ai-observer": {
				"type": "proxy", "listen": "tcp:127.0.0.1:18080",
				"connect": "tcp:127.0.0.1:8080", "bind": "host",
			}},
		},
	}}
	facts, err := (Runtime{
		Environment: map[string]string{
			"ENVIRONMENT_PROFILES": "android orca", "CODING_TOOL_INTEGRATIONS": "codex aiobserver",
			"AI_OBSERVER_HOST_PORT": "18080",
		},
		Executor: executor, Incus: incus,
	}).ReadStatusFacts(context.Background(), domain.Context{
		YardName: "default", YardKind: domain.YardContainer,
		IncusProject: "subyard", YardInstanceName: "yard",
		Paths: domain.RuntimePaths{DataHome: dataHome},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(facts.Profiles, []string{"android", "orca"}) {
		t.Fatalf("profiles = %#v", facts.Profiles)
	}
	wantAgents := []domain.AgentStatus{
		{Name: "codex", State: "enabled"},
		{Name: "aiobserver", State: "up", URL: "http://127.0.0.1:18080/", DashboardPort: 18080},
	}
	if !reflect.DeepEqual(facts.Agents, wantAgents) {
		t.Fatalf("agents = %#v, want %#v", facts.Agents, wantAgents)
	}
	if len(executor.calls) != 1 || !slices.Equal(executor.calls[0].Command, []string{"/usr/local/bin/ai-observer-check"}) {
		t.Fatalf("AI Observer probes = %#v", executor.calls)
	}
}

func TestRuntimeDoesNotPublishUnverifiedAIObserverDashboard(t *testing.T) {
	tests := []struct {
		name      string
		yardKind  domain.YardKind
		result    ports.InstanceExecResult
		execErr   error
		device    map[string]string
		marker    string
		port      string
		wantState string
	}{
		{
			name: "service down", result: ports.InstanceExecResult{ExitCode: 1},
			execErr: errors.New("instance exec exited with status 1"), wantState: "down",
		},
		{name: "probe failed", execErr: errors.New("transport failed"), wantState: "?"},
		{
			name: "container route divergent", result: ports.InstanceExecResult{ExitCode: 0},
			device: map[string]string{
				"type": "proxy", "listen": "tcp:127.0.0.1:18081",
				"connect": "tcp:127.0.0.1:8080", "bind": "host",
			}, wantState: "up",
		},
		{
			name: "container route pending", result: ports.InstanceExecResult{ExitCode: 0},
			device: map[string]string{
				"type": "proxy", "listen": "tcp:127.0.0.1:18080",
				"connect": "tcp:127.0.0.1:8080", "bind": "host",
			}, marker: "v1:pending:18080", wantState: "up",
		},
		{name: "vm needs tunnel", yardKind: domain.YardVM, result: ports.InstanceExecResult{ExitCode: 0}, wantState: "up"},
		{
			name: "privileged host port", result: ports.InstanceExecResult{ExitCode: 0}, port: "80",
			device: map[string]string{
				"type": "proxy", "listen": "tcp:127.0.0.1:80",
				"connect": "tcp:127.0.0.1:8080", "bind": "host",
			}, marker: "v1:80", wantState: "up",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			yardKind := test.yardKind
			if yardKind == "" {
				yardKind = domain.YardContainer
			}
			root := t.TempDir()
			if err := writeSpaceCache(filepath.Join(root, "space.cache"), "1G", time.Now()); err != nil {
				t.Fatal(err)
			}
			incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{
				"subyard/yard": {Name: "yard", Project: "subyard", Status: "Running", Devices: map[string]map[string]string{}},
			}}
			if test.device != nil {
				incus.Instances["subyard/yard"].Devices["ai-observer"] = test.device
			}
			if test.marker != "" {
				instance := incus.Instances["subyard/yard"]
				instance.Config = map[string]string{
					"user.subyard.ai_observer_proxy": test.marker,
				}
				incus.Instances["subyard/yard"] = instance
			}
			port := test.port
			if port == "" {
				port = "18080"
			}
			facts, err := (Runtime{
				Environment: map[string]string{
					"CODING_TOOL_INTEGRATIONS": "aiobserver", "AI_OBSERVER_HOST_PORT": port,
				},
				Executor: &spaceExecutorStub{result: test.result, err: test.execErr}, Incus: incus,
			}).ReadStatusFacts(context.Background(), domain.Context{
				YardKind: yardKind, IncusProject: "subyard", YardInstanceName: "yard",
				Paths: domain.RuntimePaths{DataHome: root},
			}, true)
			if err != nil {
				t.Fatal(err)
			}
			wantPort := 18080
			if test.port == "80" {
				wantPort = 0
			}
			if len(facts.Agents) != 1 || facts.Agents[0].State != test.wantState ||
				facts.Agents[0].URL != "" || facts.Agents[0].DashboardPort != wantPort {
				t.Fatalf("agent status = %#v", facts.Agents)
			}
		})
	}
}

func TestRuntimeBoundsProfileResourceProbe(t *testing.T) {
	root := t.TempDir()
	resources := filepath.Join(root, "config", "profiles", "demo", "resources")
	if err := os.MkdirAll(filepath.Join(resources, "service"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resources, "service.res"), []byte(
		"COMMAND=svc\nHANDLER=resources/service/handler.sh\nTITLE=Service\nACTION=\"up up yard-change reversible\"\nACTION=\"down down yard-change reversible\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	handler := filepath.Join(resources, "service", "handler.sh")
	if err := os.WriteFile(handler, []byte("#!/bin/sh\nsleep 2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	registry, err := resource.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	dataHome := filepath.Join(root, "data")
	if err := writeSpaceCache(filepath.Join(dataHome, "space.cache"), "1G", time.Now()); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	facts, err := (Runtime{
		Environment:  map[string]string{"PATH": "/usr/bin:/bin"},
		Resources:    registry.Definitions(),
		ProbeTimeout: 20 * time.Millisecond,
	}).ReadStatusFacts(context.Background(), domain.Context{
		Paths: domain.RuntimePaths{DataHome: dataHome},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("profile probe exceeded its deadline: %s", elapsed)
	}
	if len(facts.Shared) != 1 || facts.Shared[0].State != "?" || facts.Shared[0].Hint != "" {
		t.Fatalf("timed out profile did not degrade independently: %#v", facts.Shared)
	}
}

func TestRuntimeRunsOnlyOneConcurrentSpaceRefresh(t *testing.T) {
	root := t.TempDir()
	calls := filepath.Join(root, "calls")
	environment := fakeIncusEnvironment(t, "printf 'x\\n' >>\"$SPACE_CALLS\"\nsleep 0.2\nprintf '3G\\n'\n")
	environment["SPACE_CALLS"] = calls
	runtime := Runtime{Environment: environment}
	yard := domain.Context{
		YardName: "demo", IncusProject: "subyard-demo", YardInstanceName: "yard-demo",
		Paths: domain.RuntimePaths{DataHome: root},
	}
	for range 3 {
		if _, err := runtime.ReadStatusFacts(context.Background(), yard, true); err != nil {
			t.Fatal(err)
		}
	}
	waitSpaceCache(t, filepath.Join(root, "space-demo.cache"), "3G")
	payload, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(payload), "\n"); got != 1 {
		t.Fatalf("concurrent status started %d expensive refreshes: %q", got, payload)
	}
}

func fakeIncusEnvironment(t *testing.T, body string) map[string]string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "incus"), []byte("#!/bin/sh\nset -eu\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return map[string]string{"PATH": bin + string(os.PathListSeparator) + os.Getenv("PATH")}
}

func waitSpaceCache(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if figure, _ := readSpaceCache(path); figure == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	payload, err := os.ReadFile(path)
	t.Fatalf("space cache did not reach %q: payload=%q err=%v", want, payload, err)
}

func mapMerge(base, extra map[string]string) map[string]string {
	result := make(map[string]string, len(base)+len(extra))
	for key, value := range base {
		result[key] = value
	}
	for key, value := range extra {
		result[key] = value
	}
	return result
}
