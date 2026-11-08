package reconcileruntime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/incusclient"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

type profileBatchEvent struct {
	Event, Name string
}

func TestObserveProfileRuntimeBatchBoundsOrdersAndDrainsNativeProcesses(t *testing.T) {
	for _, scenario := range []string{"ready", "pinned", "ordered errors", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			root := testkit.TempDir(t)
			listener, err := net.ListenUnix("unix", &net.UnixAddr{Net: "unix", Name: filepath.Join(root, "c")})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			eventsPath := filepath.Join(root, "events")
			testkit.WriteFile(t, eventsPath, nil, 0o600)
			runtime := Runtime{RepositoryRoot: root, Environment: []string{"BATCH_ROOT=" + root},
				Incus: &testkit.Incus{Instances: map[string]ports.InstanceInfo{"subyard/yard": {Status: "Running"}}},
				Yard:  domain.Context{IncusProject: "subyard", YardInstanceName: "yard"}}
			for _, name := range []string{"a", "b", "c", "d"} {
				profileRoot := filepath.Join(root, "config", "profiles", name)
				if err := os.MkdirAll(profileRoot, 0o700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, filepath.Join(profileRoot, "runtime.py"), []byte(`#!/usr/bin/python3
import json,os,subprocess,time
from pathlib import Path
root=Path(os.environ['BATCH_ROOT'])
name=Path.cwd().name
child=subprocess.Popen(['/usr/bin/python3','-c',r'''
import socket,sys
connection=socket.socket(socket.AF_UNIX)
connection.connect(sys.argv[1])
connection.sendall(sys.argv[2].encode()+b'\n')
print('ready',flush=True)
connection.recv(1)
''',str(root/'c'),name],stdout=subprocess.PIPE,stderr=subprocess.DEVNULL)
if child.stdout.readline()!=b'ready\n': raise SystemExit('child handshake failed')
child.stdout.close()
def emit(event):
    with (root/'events').open('ab',buffering=0) as stream:
        stream.write((json.dumps(dict(Event=event,Name=name))+'\n').encode())
emit('start')
while not (root/('release-'+name)).exists(): time.sleep(.005)
child.terminate()
child.wait()
emit('end')
output=(root/('output-'+name)).read_text()
if output=='exit': raise SystemExit(9)
print(output)
`), 0o700)
				runtime.Profiles = append(runtime.Profiles, profile.Definition{Name: name, Root: profileRoot,
					Runtime: &profile.RuntimeHook{ActivationID: name + "-runtime", Handler: "runtime.py"}})
				output := fmt.Sprintf(`{"state":"current","actual":%q,"desired":%q}`, strings.Repeat(name, 64), strings.Repeat(name, 64))
				if scenario == "ordered errors" && name == "a" {
					output = "invalid"
				} else if scenario == "ordered errors" && name == "b" {
					output = "exit"
				}
				testkit.WriteFile(t, filepath.Join(root, "output-"+name), []byte(output), 0o600)
			}
			if scenario == "pinned" {
				pin, err := os.Open(root)
				if err != nil {
					t.Fatal(err)
				}
				defer pin.Close()
				runtime.RepositoryRoot = fmt.Sprintf("/proc/self/fd/%d", pin.Fd())
				// Parallel command.Environ calls must not mutate shared spare capacity.
				runtime.Environment = append(make([]string, 0, 8), runtime.Environment...)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			done := make(chan struct{})
			var observations map[string]ports.RuntimeObservation
			var observeErr error
			go func() {
				observations, observeErr = runtime.ObserveProfileRuntimes(ctx)
				close(done)
			}()
			defer func() { cancel(); <-done }()
			readEvents := func() []profileBatchEvent {
				t.Helper()
				data, err := os.ReadFile(eventsPath)
				if err != nil {
					t.Fatal(err)
				}
				var events []profileBatchEvent
				for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
					if len(line) == 0 {
						continue
					}
					var event profileBatchEvent
					if err := json.Unmarshal(line, &event); err != nil {
						t.Fatal(err)
					}
					events = append(events, event)
				}
				return events
			}
			waitEvents := func(kind string, count int) {
				t.Helper()
				ticker := time.NewTicker(5 * time.Millisecond)
				defer ticker.Stop()
				for {
					found := 0
					for _, event := range readEvents() {
						if event.Event == kind {
							found++
						}
					}
					if found >= count {
						return
					}
					select {
					case <-ctx.Done():
						t.Fatalf("native batch did not reach %d %s events: %v", count, kind, ctx.Err())
					case <-ticker.C:
					}
				}
			}
			release := func(name string) { testkit.WriteFile(t, filepath.Join(root, "release-"+name), nil, 0o600) }
			waitEvents("start", 3)
			if scenario == "cancel" {
				cancel()
			} else {
				release("b")
				waitEvents("start", 4)
				release("c")
				release("d")
				waitEvents("end", 3)
				select {
				case <-done:
					t.Fatal("batch returned while its first native observation was still running")
				default:
				}
				release("a")
			}
			<-done
			active, peak := 0, 0
			children := make(map[string]bool)
			for _, event := range readEvents() {
				if event.Event == "start" {
					active++
					peak = max(peak, active)
					children[event.Name] = true
				} else {
					active--
				}
			}
			// Each start follows the child's connection handshake. A surviving
			// child keeps its connection open; process death produces EOF.
			exitDeadline := time.Now().Add(500 * time.Millisecond)
			if err := listener.SetDeadline(exitDeadline); err != nil {
				t.Fatal(err)
			}
			for remaining := len(children); remaining > 0; remaining-- {
				connection, err := listener.AcceptUnix()
				if err != nil {
					t.Fatal(err)
				}
				defer connection.Close()
				if err := connection.SetReadDeadline(exitDeadline); err != nil {
					t.Fatal(err)
				}
				reader := bufio.NewReader(connection)
				name, err := reader.ReadString('\n')
				name = strings.TrimSuffix(name, "\n")
				if err != nil || !children[name] {
					t.Fatalf("unexpected native child handshake %q: %v", name, err)
				}
				delete(children, name)
				if _, err := reader.ReadByte(); !errors.Is(err, io.EOF) {
					t.Fatalf("native child %s did not close its lifetime connection after batch completion: %v", name, err)
				}
			}
			if peak != 3 {
				t.Fatalf("native batch peak=%d, want three", peak)
			}
			switch scenario {
			case "ready", "pinned":
				if observeErr != nil || len(observations) != 4 || active != 0 {
					t.Fatalf("batch result=%v error=%v active=%d", observations, observeErr, active)
				}
				for _, name := range []string{"a", "b", "c", "d"} {
					if observations[name+"-runtime"].Actual != strings.Repeat(name, 64) {
						t.Fatalf("out-of-order result lost owner %s", name)
					}
				}
			case "ordered errors":
				if observeErr == nil || !strings.Contains(observeErr.Error(), "profile a-runtime observation failed (invalid-schema)") || !strings.Contains(observeErr.Error(), "profile b-runtime observation failed (native-exit-9)") || active != 0 {
					t.Fatalf("batch lost original-order failure or drain: %v active=%d", observeErr, active)
				}
			case "cancel":
				if !errors.Is(observeErr, context.Canceled) {
					t.Fatalf("batch lost cancellation: %v", observeErr)
				}
			}
		})
	}
}

func TestProfileRuntimeColdInstallationPreservesDependencyLoss(t *testing.T) {
	for _, scenario := range []string{"cold", "retained", "installed", "custom socket"} {
		t.Run(scenario, func(t *testing.T) {
			runtime := profileRuntimeFixture(t, "#!/bin/sh\nexit 9\n")
			root := testkit.TempDir(t)
			t.Setenv("PATH", root)
			t.Setenv("INCUS_DIR", filepath.Join(root, "incus-state"))
			t.Setenv("INCUS_SOCKET", "")
			runtime.Incus = incusclient.New("")
			switch scenario {
			case "retained":
				if err := os.Mkdir(filepath.Join(root, "incus-state"), 0700); err != nil {
					t.Fatal(err)
				}
			case "installed":
				testkit.WriteFile(t, filepath.Join(root, "incus"), []byte("#!/bin/sh\nexit 9\n"), 0700)
			case "custom socket":
				runtime.Incus = incusclient.New(filepath.Join(root, "missing.socket"))
			}
			observations, err := runtime.ObserveProfileRuntimes(context.Background())
			if err == nil {
				t.Fatalf("dependency loss treated as absent: %#v", observations)
			}
			observation, presenceErr := runtime.ObserveProfileRuntime(context.Background(), "synthetic-runtime")
			if scenario == "cold" {
				if presenceErr != nil || observation.State != ports.RuntimeStateAbsent {
					t.Fatalf("verified cold presence not absent: %#v %v", observation, presenceErr)
				}
			} else if presenceErr == nil {
				t.Fatal("retained/ambiguous installation accepted as absent")
			}
			plan, err := runtime.PrepareProfileRuntimes(context.Background(), true, false)
			if scenario == "cold" {
				if err != nil || !plan.unavailable || plan.approved["synthetic-runtime"].State != ports.RuntimeStateDeferred {
					t.Fatalf("approved cold prerequisite was not deferred: %#v %v", plan, err)
				}
			} else if err == nil {
				t.Fatal("ambiguous dependency loss accepted as an approved cold prerequisite")
			}
		})
	}
}

const (
	testRuntimeOldDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testRuntimeNewDigest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestObserveProfileRuntimesClassifiesAbsentAndStoppedWithoutHandler(t *testing.T) {
	runtime := profileRuntimeFixture(t, `#!/bin/sh
exit 9
`)
	runtime.Incus = &testkit.Incus{}
	observations, err := runtime.ObserveProfileRuntimes(context.Background())
	if err != nil || observations["synthetic-runtime"].State != ports.RuntimeStateAbsent {
		t.Fatalf("absent observations = %#v, %v", observations, err)
	}

	var diagnostics bytes.Buffer
	runtime.Incus = &testkit.Incus{Instances: map[string]ports.InstanceInfo{
		"subyard/yard": {Status: "Stopped"},
	}}
	runtime.Stderr = &diagnostics
	observations, err = runtime.ObserveProfileRuntimes(context.Background())
	if err != nil || observations["synthetic-runtime"].State != ports.RuntimeStateDeferred {
		t.Fatalf("stopped observations = %#v, %v", observations, err)
	}
	if diagnostics.Len() != 0 {
		t.Fatalf("observe emitted diagnostics: %q", diagnostics.String())
	}
	converged, err := runtime.CheckStage(context.Background(), ports.ReconcileStageProfileRuntimes)
	if err != nil || !converged || !strings.Contains(diagnostics.String(), "deferred") {
		t.Fatalf("stopped check = %v, %v, diagnostics %q", converged, err, diagnostics.String())
	}
}

func TestObserveSingleProfileRuntimeSkipsUnrelatedHooksAndPreservesUnavailable(t *testing.T) {
	runtime := profileRuntimeFixture(t, "#!/bin/sh\nprintf '{\"state\":\"absent\"}\\n'\n")
	other := filepath.Join(runtime.RepositoryRoot, "config/profiles/unrelated")
	if err := os.MkdirAll(other, 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(other, "profile.json"), []byte(`{"schema_version":1,"runtime":{"activation_id":"unrelated-runtime","handler":"runtime.sh"}}`), 0600)
	testkit.WriteFile(t, filepath.Join(other, "runtime.sh"), []byte("#!/bin/sh\nexit 9\n"), 0700)
	observation, err := runtime.ObserveProfileRuntime(context.Background(), "synthetic-runtime")
	if err != nil || observation.State != ports.RuntimeStateAbsent {
		t.Fatalf("single owner observation failed: %#v %v", observation, err)
	}
	if _, err := runtime.ObserveProfileRuntimes(context.Background()); err == nil {
		t.Fatal("unrelated failing hook fixture was not exercised")
	}
	runtime.Incus = &testkit.Incus{Err: errors.New("native unavailable")}
	if _, err := runtime.ObserveProfileRuntime(context.Background(), "synthetic-runtime"); err == nil {
		t.Fatal("unavailable native transport accepted as absence")
	}
	if _, err := runtime.ObserveProfileRuntime(context.Background(), "missing-runtime"); err == nil {
		t.Fatal("unknown owner accepted as absence")
	}
}

func TestObserveProfileRuntimeThroughPinnedRepository(t *testing.T) {
	for _, handler := range []string{"runtime.sh", "hooks/runtime.sh"} {
		t.Run(handler, func(t *testing.T) {
			runtime := profileRuntimeFixture(t, `#!/bin/sh
set -eu
[ -r ./profile.json ]
[ -r "$SUBYARD_REPOSITORY_ROOT/config/profiles/synthetic/profile.json" ]
printf '{"state":"absent","actual":"","desired":""}\n'
`)
			profileRoot := filepath.Join(runtime.RepositoryRoot, "config", "profiles", "synthetic")
			if handler != "runtime.sh" {
				if err := os.Mkdir(filepath.Join(profileRoot, "hooks"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(filepath.Join(profileRoot, "runtime.sh"), filepath.Join(profileRoot, handler)); err != nil {
					t.Fatal(err)
				}
				declaration := fmt.Sprintf(`{"schema_version":1,"runtime":{"activation_id":"synthetic-runtime","handler":%q}}`, handler)
				testkit.WriteFile(t, filepath.Join(profileRoot, "profile.json"), []byte(declaration), 0o600)
			}
			pin, err := os.Open(runtime.RepositoryRoot)
			if err != nil {
				t.Fatal(err)
			}
			defer pin.Close()
			for _, owner := range []string{"self", fmt.Sprint(os.Getpid())} {
				runtime.RepositoryRoot = fmt.Sprintf("/proc/%s/fd/%d", owner, pin.Fd())
				runtime.Environment = []string{"SUBYARD_REPOSITORY_ROOT=" + runtime.RepositoryRoot}
				observations, err := runtime.ObserveProfileRuntimes(context.Background())
				if err != nil || observations["synthetic-runtime"].State != ports.RuntimeStateAbsent {
					t.Fatalf("pinned observations = %#v, %v", observations, err)
				}
			}
		})
	}
}

func TestObserveProfileRuntimesRejectsUnavailableAndAmbiguousState(t *testing.T) {
	runtime := profileRuntimeFixture(t, "#!/bin/sh\nexit 0\n")
	runtime.Incus = &testkit.Incus{Err: errors.New("incus unavailable")}
	if _, err := runtime.ObserveProfileRuntimes(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "incus unavailable") {
		t.Fatalf("Incus error = %v", err)
	}
	runtime.Incus = &testkit.Incus{Instances: map[string]ports.InstanceInfo{
		"subyard/yard": {Status: "Frozen"},
	}}
	if _, err := runtime.ObserveProfileRuntimes(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "Frozen") {
		t.Fatalf("ambiguous state error = %v", err)
	}
}

func TestObserveProfileRuntimeValidatesBoundedStrictJSON(t *testing.T) {
	tests := []struct {
		name   string
		output string
	}{
		{name: "unknown state", output: `{"state":"future","actual":"","desired":""}`},
		{name: "bad digest", output: `{"state":"current","actual":"abc","desired":"abc"}`},
		{name: "unknown field", output: `{"state":"absent","actual":"","desired":"","extra":true}`},
		{name: "trailing", output: `{"state":"absent","actual":"","desired":""} {}`},
		{name: "absent ownership without current", output: `{"state":"absent","ownership_absent":true}`},
		{name: "deferred ownership", output: `{"state":"deferred","ownership_absent":true}`},
		{name: "stale ownership", output: `{"state":"stale","actual":"` + testRuntimeOldDigest + `","desired":"` + testRuntimeNewDigest + `","ownership_absent":true}`},
		{name: "unbound ownership", output: `{"state":"current","ownership_absent":true}`},
		{name: "wrong ownership type", output: `{"state":"current","actual":"` + testRuntimeNewDigest + `","desired":"` + testRuntimeNewDigest + `","ownership_absent":"yes"}`},
		{name: "current consequence", output: `{"state":"current","actual":"` + testRuntimeNewDigest + `","desired":"` + testRuntimeNewDigest + `","consequence":"service restarts"}`},
		{name: "absent consequence", output: `{"state":"absent","actual":"","desired":"","consequence":"service restarts"}`},
		{name: "control consequence", output: `{"state":"stale","actual":"` + testRuntimeOldDigest + `","desired":"` + testRuntimeNewDigest + `","consequence":"service\nrestarts"}`},
		{name: "trailing space consequence", output: `{"state":"stale","actual":"` + testRuntimeOldDigest + `","desired":"` + testRuntimeNewDigest + `","consequence":"service restarts "}`},
		{name: "long consequence", output: `{"state":"stale","actual":"` + testRuntimeOldDigest + `","desired":"` + testRuntimeNewDigest + `","consequence":"` + strings.Repeat("a", profileRuntimeConsequenceBytes+1) + `"}`},
		{name: "list consequence", output: `{"state":"stale","actual":"` + testRuntimeOldDigest + `","desired":"` + testRuntimeNewDigest + `","consequence":["service restarts"]}`},
		{name: "private diagnostic value", output: `{"state":"stale","desired":"` + testRuntimeNewDigest + `","diagnostics":[{"parameter":"sample.state","installed":"private-value","desired":"ready"}]}`},
		{name: "duplicate diagnostic", output: `{"state":"stale","desired":"` + testRuntimeNewDigest + `","diagnostics":[{"parameter":"sample.state","installed":"unknown","desired":"ready"},{"parameter":"sample.state","installed":"unknown","desired":"ready"}]}`},
		{name: "current diagnostic", output: `{"state":"current","actual":"` + testRuntimeNewDigest + `","desired":"` + testRuntimeNewDigest + `","diagnostics":[{"parameter":"sample.state","installed":"unknown","desired":"ready"}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodeProfileRuntimeObservation([]byte(test.output)); err == nil {
				t.Fatal("invalid handler output was accepted")
			}
		})
	}
}

func TestObserveProfileRuntimeAcceptsStaleConsequence(t *testing.T) {
	consequence := "service restarts to apply its new unit; clients reconnect"
	observation, err := decodeProfileRuntimeObservation([]byte(`{"state":"stale","actual":"` + testRuntimeOldDigest + `","desired":"` + testRuntimeNewDigest + `","consequence":"` + consequence + `"}`))
	if err != nil || observation.Consequence != consequence {
		t.Fatalf("observation = %#v, err = %v", observation, err)
	}
}

func TestObserveProfileRuntimeAcceptsClosedDiagnostic(t *testing.T) {
	observation, err := decodeProfileRuntimeObservation([]byte(`{"state":"stale","actual":"` + testRuntimeOldDigest + `","desired":"` + testRuntimeNewDigest + `","diagnostics":[{"parameter":"sample.service","installed":"inactive","desired":"active"}]}`))
	if err != nil || observation.Diagnostics == nil || len(*observation.Diagnostics) != 1 || (*observation.Diagnostics)[0].Installed != "inactive" {
		t.Fatalf("observation = %#v, err = %v", observation, err)
	}
}

func TestProfileRuntimeDiagnosticsAreOptIn(t *testing.T) {
	base := `{"state":"stale","actual":"` + testRuntimeOldDigest + `","desired":"` + testRuntimeNewDigest + `"}`
	withDetail := `{"state":"stale","actual":"` + testRuntimeOldDigest + `","desired":"` + testRuntimeNewDigest + `","diagnostics":[{"parameter":"sample.service","installed":"inactive","desired":"active"}]}`
	runtime := profileRuntimeFixture(t, "#!/bin/sh\nif [ \"${SUBYARD_PROFILE_DIAGNOSTICS:-0}\" = 1 ]; then printf '%s' '"+withDetail+"'; else printf '%s' '"+base+"'; fi\n")
	runtime.Environment = []string{"SUBYARD_PROFILE_DIAGNOSTICS=1"}
	ordinary, err := runtime.ObserveProfileRuntimes(context.Background())
	if err != nil || ordinary["synthetic-runtime"].Diagnostics != nil {
		t.Fatalf("ordinary observation: %#v %v", ordinary, err)
	}
	diagnostic, err := runtime.ObserveProfileRuntimeDiagnostics(context.Background())
	if err != nil || diagnostic["synthetic-runtime"].Diagnostics == nil {
		t.Fatalf("diagnostic observation: %#v %v", diagnostic, err)
	}
}

func TestObserveProfileRuntimeBoundsOutputAndUsesStrictDecoder(t *testing.T) {
	for _, output := range []string{
		`{"state":"absent","actual":"","desired":"","extra":true}`,
		`{"state":"absent","actual":"","desired":""}` + strings.Repeat(" ", profileRuntimeOutputLimit),
	} {
		runtime := profileRuntimeFixture(t, "#!/bin/sh\nprintf '%s' \"$OUTPUT\"\n")
		runtime.Environment = []string{"OUTPUT=" + output}
		if _, err := runtime.ObserveProfileRuntimes(context.Background()); err == nil {
			t.Fatal("invalid native output accepted")
		}
	}
}

func TestApplyProfileRuntimeBindsAssessmentAndVerifies(t *testing.T) {
	root := testkit.TempDir(t)
	state := filepath.Join(root, "state")
	testkit.WriteFile(t, state, []byte("stale"), 0o600)
	log := filepath.Join(root, "arguments")
	handler := `#!/bin/sh
set -eu
case "$1" in
  observe)
    if [ "$(cat "$STATE")" = stale ]; then
      printf '{"state":"stale","actual":"%s","desired":"%s"}\n' "$OLD" "$NEW"
    else
      printf '{"state":"current","actual":"%s","desired":"%s","ownership_absent":true}\n' "$NEW" "$NEW"
    fi
    ;;
  apply)
    printf '%s\n' "$*" > "$LOG"
    [ "$2" = "$OPERATION" ] && [ "$3" = "$OLD" ] && [ "$4" = "$NEW" ]
    printf current > "$STATE"
    printf '{"state":"current","actual":"%s","desired":"%s","ownership_absent":true}\n' "$NEW" "$NEW"
    ;;
esac
`
	runtime := profileRuntimeFixtureAt(t, root, handler)
	runtime.Environment = []string{
		"STATE=" + state, "LOG=" + log, "OLD=" + testRuntimeOldDigest, "NEW=" + testRuntimeNewDigest,
		"OPERATION=op-12345678", "SUBYARD_OPERATION_ID=op-12345678",
	}
	if err := runtime.ApplyStage(context.Background(), ports.ReconcileStageProfileRuntimes); err != nil {
		t.Fatal(err)
	}
	arguments, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	want := "apply op-12345678 " + testRuntimeOldDigest + " " + testRuntimeNewDigest + "\n"
	if string(arguments) != want {
		t.Fatalf("apply arguments = %q, want %q", arguments, want)
	}
}

func TestApplyProfileRuntimeRejectsFailedVerification(t *testing.T) {
	handler := `#!/bin/sh
case "$1" in
  observe) printf '{"state":"stale","actual":"%s","desired":"%s"}\n' "$OLD" "$NEW" ;;
  apply) printf '{"state":"current","actual":"%s","desired":"%s"}\n' "$NEW" "$NEW" ;;
esac
`
	runtime := profileRuntimeFixture(t, handler)
	runtime.Environment = []string{
		"OLD=" + testRuntimeOldDigest, "NEW=" + testRuntimeNewDigest,
		"SUBYARD_OPERATION_ID=op-12345678",
	}
	if err := runtime.ApplyStage(context.Background(), ports.ReconcileStageProfileRuntimes); err == nil ||
		!strings.Contains(err.Error(), "did not retain") {
		t.Fatalf("verification error = %v", err)
	}
}

func TestApplyProfileRuntimeRejectsObservationChangesAfterAssessment(t *testing.T) {
	tests := []struct {
		name        string
		status      string
		output      string
		wantSuccess bool
	}{
		{name: "absent", status: "absent", wantSuccess: false},
		{name: "deferred", status: "Stopped", wantSuccess: false},
		{name: "different current", status: "Running", output: `{"state":"current","actual":"` + testRuntimeOldDigest + `","desired":"` + testRuntimeOldDigest + `"}`, wantSuccess: false},
		{name: "already desired current", status: "Running", output: `{"state":"current","actual":"` + testRuntimeNewDigest + `","desired":"` + testRuntimeNewDigest + `"}`, wantSuccess: true},
		{name: "changed stale actual", status: "Running", output: `{"state":"stale","actual":"` + testRuntimeNewDigest + `","desired":"` + testRuntimeNewDigest + `"}`, wantSuccess: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime := profileRuntimeFixture(t, "#!/bin/sh\nprintf '%s\\n' \"$OUTPUT\"\n")
			if test.status == "absent" {
				runtime.Incus = &testkit.Incus{}
			} else {
				runtime.Incus = &testkit.Incus{Instances: map[string]ports.InstanceInfo{
					"subyard/yard": {Status: test.status},
				}}
			}
			runtime.Environment = []string{"OUTPUT=" + test.output}
			err := runtime.ApplyProfileRuntime(context.Background(), "synthetic-runtime", "op-12345678", testRuntimeOldDigest, testRuntimeNewDigest)
			if test.wantSuccess && err != nil {
				t.Fatalf("unchanged current observation failed: %v", err)
			}
			if !test.wantSuccess && err == nil {
				t.Fatal("changed observation was accepted")
			}
		})
	}
}

func TestProvisionRefreshesProfileRuntimesAfterBaseBeforeIntegrationHooks(t *testing.T) {
	previewLoopbackFixture(t)
	sshHelper, err := filepath.Abs("../../../scripts/lib/ssh-config.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, failure := range []string{"", "route", "network", "base", "ssh", "runtime"} {
		t.Run("failure="+failure, func(t *testing.T) {
			root := testkit.TempDir(t)
			handler := `#!/bin/sh
set -eu
[ -e "$BASE_READY" ]
[ -e "$AGENT_READY" ]
[ -e "$SSH_READY" ]
[ -s "$SUBYARD_HOME/ssh/id_ed25519" ]
[ "$(stat -c %a "$SUBYARD_HOME/ssh/id_ed25519")" = 600 ]
case "$1" in
  observe)
    if [ -e "$RUNTIME_READY" ]; then
      printf '{"state":"current","actual":"%s","desired":"%s"}\n' "$NEW" "$NEW"
    else
      printf '{"state":"stale","actual":"%s","desired":"%s"}\n' "$OLD" "$NEW"
    fi
    ;;
  apply)
    [ "$FAIL_STAGE" != runtime ]
    : > "$RUNTIME_READY"
    printf '{"state":"current","actual":"%s","desired":"%s"}\n' "$NEW" "$NEW"
    ;;
esac
`
			runtime := profileRuntimeFixtureAt(t, root, handler)
			runtime.Yard.YardKind = domain.YardVM
			policy := &networkPolicyFixture{}
			if failure == "network" {
				policy.ensureErr = errors.New("fixture preview ingress error")
			}
			runtime.NetworkPolicy = policy
			for path, content := range map[string]string{
				"scripts/prepare-preview-route.sh": `#!/bin/sh
set -eu
[ "$1" = 127.0.0.1 ]
[ "$FAIL_STAGE" != route ]
: > "$ROUTE_READY"
`,
				"scripts/04-provision-subyard.sh": `#!/bin/sh
set -eu
[ "$1" = --yes ]
[ -n "$SUBYARD_PREVIEW_ENDPOINT" ]
[ -e "$ROUTE_READY" ]
[ ! -e "$BASE_READY" ]
[ "$FAIL_STAGE" != base ]
: > "$AGENT_READY"
: > "$BASE_READY"
`,
				"scripts/07-ssh-access.sh": `#!/bin/bash
set -euo pipefail
[ "$1" = --yes ]
[ -e "$BASE_READY" ]
[ -e "$AGENT_READY" ]
[ "$FAIL_STAGE" != ssh ]
. "$SSH_HELPER"
ssh_transport_identity_prepare "$SUBYARD_HOME" >/dev/null
: > "$SSH_READY"
`,
				"config/projects-changed.sh":        "#!/bin/sh\nexit 0\n",
				"scripts/reconcile-integrations.sh": "#!/bin/sh\nexit 0\n",
			} {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0o700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, filepath.Join(root, path), []byte(content), 0o700)
			}
			ready := filepath.Join(root, "runtime-ready")
			executor := &retryableIntegrationExecutor{pending: true, hookReady: ready}
			runtime.Executor = executor
			incus := runtime.Incus.(*testkit.Incus)
			instance := incus.Instances["subyard/yard"]
			instance.LocalConfig = map[string]string{"user.subyard.preview_proxy": "v1:pending:100.101.102.103:32222"}
			incus.Instances["subyard/yard"] = instance
			incus.Reconcile = ports.ReconcileState{InstanceFound: true, Instance: incus.Instances["subyard/yard"]}
			runtime.Environment = []string{
				"BASE_READY=" + filepath.Join(root, "base-ready"), "RUNTIME_READY=" + ready,
				"AGENT_READY=" + filepath.Join(root, "agent-ready"),
				"ROUTE_READY=" + filepath.Join(root, "route-ready"),
				"SSH_READY=" + filepath.Join(root, "ssh-ready"),
				"SSH_HELPER=" + sshHelper, "SUBYARD_HOME=" + filepath.Join(root, "data"),
				"FAIL_STAGE=" + failure, "OLD=" + testRuntimeOldDigest, "NEW=" + testRuntimeNewDigest,
				"SUBYARD_OPERATION_ID=op-12345678",
			}
			err := runtime.ApplyStage(context.Background(), ports.ReconcileStageProvision)
			wantEnsure := 1
			if failure == "route" {
				wantEnsure = 0
			}
			if len(policy.ensured) != wantEnsure {
				t.Fatalf("network reconciliation calls=%d want=%d", len(policy.ensured), wantEnsure)
			}
			if failure == "route" || failure == "network" {
				if _, err := os.Stat(filepath.Join(root, "base-ready")); !os.IsNotExist(err) {
					t.Fatal("guest provisioning ran before preview route/network readiness")
				}
			}
			if failure != "" {
				if err == nil || executor.hookAttempts != 0 || executor.commits != 0 {
					t.Fatalf("failed %s reached hooks/commit: err=%v hooks=%d commits=%d", failure, err, executor.hookAttempts, executor.commits)
				}
				return
			}
			if err != nil || executor.hookAttempts != 1 || executor.commits != 1 || executor.pending {
				t.Fatalf("provision order failed: err=%v hooks=%d commits=%d pending=%v", err, executor.hookAttempts, executor.commits, executor.pending)
			}
			identity := filepath.Join(root, "data", "ssh", "id_ed25519")
			before, err := os.ReadFile(identity)
			if err != nil {
				t.Fatal("canonical transport identity was not created")
			}
			// Re-running the generic owners must reuse the protected identity.
			if err := os.Remove(filepath.Join(root, "base-ready")); err != nil {
				t.Fatal(err)
			}
			if err := runtime.ApplyStage(context.Background(), ports.ReconcileStageProvision); err != nil {
				t.Fatalf("repeat provision failed: %v", err)
			}
			after, err := os.ReadFile(identity)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("repeat provision changed the canonical transport identity")
			}
			for path, mode := range map[string]os.FileMode{filepath.Dir(identity): 0o700, identity: 0o600, identity + ".pub": 0o644} {
				info, err := os.Stat(path)
				if err != nil || info.Mode().Perm() != mode {
					t.Fatalf("canonical identity mode differs: want %o", mode)
				}
			}
		})
	}
}

func TestBaseProvisionDefersNestedIntegrationProjectHooksUntilRestore(t *testing.T) {
	previewLoopbackFixture(t)
	for _, scenario := range []string{"base-only", "full-stage-before-restore", "base-failure", "network-failure", "planning", "legacy-identity"} {
		t.Run(scenario, func(t *testing.T) {
			root := testkit.TempDir(t)
			for path, content := range map[string]string{
				"scripts/prepare-preview-route.sh":  "#!/bin/sh\nexit 0\n",
				"scripts/04-provision-subyard.sh":   "#!/bin/sh\nset -eu\n[ -n \"$SUBYARD_PREVIEW_ENDPOINT\" ]\n[ \"$FAIL_BASE\" = 0 ]\n: > \"$BASE_READY\"\n",
				"scripts/07-ssh-access.sh":          "#!/bin/sh\nset -eu\n[ -e \"$BASE_READY\" ]\n: > \"$SSH_READY\"\n",
				"scripts/reconcile-integrations.sh": "#!/bin/sh\nexit 0\n",
				"config/projects-changed.sh":        "#!/bin/sh\nexit 0\n",
			} {
				full := filepath.Join(root, path)
				if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, full, []byte(content), 0700)
			}
			restored := filepath.Join(root, "restored-project-wrapper")
			executor := &retryableIntegrationExecutor{pending: true, hookReady: restored}
			instance := ports.InstanceInfo{Status: "Running", LocalConfig: map[string]string{"user.subyard.preview_proxy": "v1:pending:100.101.102.103:32222"}}
			incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{"test/yard": instance}, Reconcile: ports.ReconcileState{InstanceFound: true, Instance: instance}}
			policy := &networkPolicyFixture{}
			if scenario == "network-failure" {
				policy.ensureErr = errors.New("network unavailable")
			}
			failBase := "0"
			if scenario == "base-failure" {
				failBase = "1"
			}
			runtime := Runtime{RepositoryRoot: root, Incus: incus, Executor: executor, NetworkPolicy: policy,
				Profiles: []profile.Definition{}, ownerInitPlanning: scenario == "planning",
				Yard:        domain.Context{YardName: "example", IncusProject: "test", YardInstanceName: "yard", YardKind: domain.YardVM, DevUser: "dev", DevUID: os.Getuid()},
				Environment: []string{"BASE_READY=" + filepath.Join(root, "base-ready"), "SSH_READY=" + filepath.Join(root, "ssh-ready"), "FAIL_BASE=" + failBase}}
			if scenario == "legacy-identity" {
				runtime.LegacyContinuation = &domain.LegacyContinuation{}
			}
			// Exercise the real stage -> integration reconciliation -> project
			// hook chain, with a guest boundary that refuses a missing wrapper.
			var err error
			if scenario == "full-stage-before-restore" {
				err = runtime.ApplyStage(context.Background(), ports.ReconcileStageProvision)
				if err == nil || executor.hookAttempts != 1 || executor.commits != 0 {
					t.Fatalf("full stage did not expose the nested hook: err=%v hooks=%d commits=%d", err, executor.hookAttempts, executor.commits)
				}
				return
			}
			err = runtime.ProvisionBase(context.Background())
			if scenario != "base-only" {
				if err == nil || executor.hookAttempts != 0 || executor.commits != 0 {
					t.Fatalf("base failure entered hooks: err=%v hooks=%d commits=%d", err, executor.hookAttempts, executor.commits)
				}
				if _, err := os.Stat(filepath.Join(root, "base-ready")); !os.IsNotExist(err) {
					t.Fatal("failed base guard published readiness")
				}
				return
			}
			if err != nil || executor.hookAttempts != 0 || executor.commits != 0 {
				t.Fatalf("base setup reached projects before restore: err=%v hooks=%d commits=%d", err, executor.hookAttempts, executor.commits)
			}
			if _, err := os.Stat(filepath.Join(root, "ssh-ready")); !os.IsNotExist(err) {
				t.Fatal("base setup reached later reconciliation")
			}
			testkit.WriteFile(t, restored, []byte("exact restored project\n"), 0600)
			if err := runtime.ApplyStage(context.Background(), ports.ReconcileStageProvision); err != nil || executor.hookAttempts != 1 || executor.commits != 1 {
				t.Fatalf("restored project did not reach normal nested hooks: err=%v hooks=%d commits=%d", err, executor.hookAttempts, executor.commits)
			}
		})
	}
}

func profileRuntimeFixture(t *testing.T, handler string) Runtime {
	t.Helper()
	return profileRuntimeFixtureAt(t, testkit.TempDir(t), handler)
}

func profileRuntimeFixtureAt(t *testing.T, root, handler string) Runtime {
	t.Helper()
	profileRoot := filepath.Join(root, "config", "profiles", "synthetic")
	if err := os.MkdirAll(profileRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(profileRoot, "profile.json"), []byte(`{"schema_version":1,"runtime":{"activation_id":"synthetic-runtime","handler":"runtime.sh"}}`), 0o600)
	testkit.WriteFile(t, filepath.Join(profileRoot, "runtime.sh"), []byte(handler), 0o700)
	yard := domain.Context{IncusProject: "subyard", YardInstanceName: "yard"}
	return Runtime{
		RepositoryRoot: root,
		Incus: &testkit.Incus{Instances: map[string]ports.InstanceInfo{
			"subyard/yard": {Status: "Running"},
		}},
		Yard: yard,
	}
}

func TestProfileRuntimeObservationFailuresHaveSafePublicDiagnostics(t *testing.T) {
	for _, test := range []struct{ name, body, reason string }{
		{"native-exit", "#!/bin/sh\nprintf private-handler-detail >&2\nexit 37\n", "native-exit-37"},
		{"schema", "#!/bin/sh\nprintf private-handler-detail >&2\nprintf private-result-detail\n", "invalid-schema"},
		{"cancel", "#!/bin/sh\nsleep 5\n", "cancelled"},
		{"deadline", "#!/bin/sh\nsleep 5\n", "deadline"},
		{"missing", "#!/missing/private-interpreter\n", "command-not-found"},
		{"permission", "#!/forbidden\n", "permission-denied"},
		{"source", "#!/bin/sh\nexit 1\n", "unclassified"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime := profileRuntimeFixture(t, test.body)
			runtime.Yard.YardName = "fixture-yard"
			handler := filepath.Join(runtime.RepositoryRoot, "config/profiles/synthetic/runtime.sh")
			if test.name == "permission" {
				interpreter := filepath.Join(runtime.RepositoryRoot, "private-interpreter")
				testkit.WriteFile(t, interpreter, []byte("#!/bin/sh\n"), 0600)
				testkit.WriteFile(t, handler, []byte("#!"+interpreter+"\n"), 0700)
			}
			if test.name == "source" {
				definitions, err := profile.Load(runtime.RepositoryRoot)
				if err != nil {
					t.Fatal(err)
				}
				runtime.Profiles = definitions
				if err := os.Remove(handler); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if test.name == "cancel" {
				cancel()
			}
			if test.name == "deadline" {
				var closeDeadline context.CancelFunc
				ctx, closeDeadline = context.WithTimeout(ctx, 20*time.Millisecond)
				defer closeDeadline()
			}
			_, err := runtime.ObserveProfileRuntimes(ctx)
			var diagnostic interface{ ActivationDiagnostic() (string, string) }
			if err == nil || !errors.As(err, &diagnostic) {
				t.Fatal("observation lacks a safe public diagnostic", err)
			}
			message, retry := diagnostic.ActivationDiagnostic()
			want := "yard fixture-yard: profile synthetic-runtime observation failed (" + test.reason + ")"
			encoded, marshalErr := json.Marshal(err)
			if marshalErr != nil || strings.Contains(string(encoded), "private-") {
				t.Fatal("public error JSON leaked private native output")
			}
			var conflict interface{ ActivationConflict() bool }
			if errors.As(err, &conflict) && conflict.ActivationConflict() {
				t.Fatal("observation failure became an activation conflict")
			}
			if message != want || retry != "run yard update --check" || strings.Contains(message, "private-") || strings.Contains(message, runtime.RepositoryRoot) || strings.Contains(err.Error(), "private-") {
				t.Fatal("imprecise or unsafe runtime observation diagnostic", message, retry)
			}
			if test.name == "native-exit" {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 37 {
					t.Fatal("native cause lost", err)
				}
			}
			if test.name == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal("cancellation cause lost", err)
			}
			if test.name == "deadline" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("deadline cause lost", err)
			}
		})
	}
}

func TestProfileRuntimeDiagnosticRejectsUnsafeLabels(t *testing.T) {
	runtime := Runtime{Yard: domain.Context{YardName: "private-yard\nvalue"}}
	cause := errors.New("private-error-detail")
	err := runtime.profileObservationFailure("private-owner/value", "invalid-schema", cause)
	var diagnostic interface{ ActivationDiagnostic() (string, string) }
	if !errors.As(err, &diagnostic) || !errors.Is(err, cause) {
		t.Fatal("typed cause lost")
	}
	message, _ := diagnostic.ActivationDiagnostic()
	if message != "yard unknown: profile unknown observation failed (invalid-schema)" || strings.Contains(err.Error(), "private-") {
		t.Fatal("unsafe diagnostic label", message)
	}
}
