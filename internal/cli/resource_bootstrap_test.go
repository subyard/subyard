package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/resourceendpoint"
	"github.com/Subyard/Subyard/internal/testkit"
)

func bootstrapCommandFixture(t *testing.T) (string, []string, string) {
	t.Helper()
	root, environment, applyLog := resourceCommandFixture(t)
	path := filepath.Join(root, "config", "profiles", "fixture", "resources", "demo.res")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content = []byte(strings.ReplaceAll(string(content), "run run host-change reversible", "run run bootstrap-change recreatable"))
	content = []byte(strings.ReplaceAll(string(content), `ACTION="run-purge run persistent-data-destruction irreversible"`+"\n", ""))
	content = append(content, []byte("BOOTSTRAP=profile\nPROXY=\"demo ORCA_ADVERTISE_HOST ORCA_HOST_PORT tcp:127.0.0.1:6768 loopback-or-tailscale\"\n")...)
	writeCLIFile(t, path, string(content), 0600)
	writeCLIFile(t, filepath.Join(root, "config", "host.env"), "ENVIRONMENT_PROFILES=existing\n", 0600)
	return root, environment, applyLog
}

func TestResourceBootstrapProfileCASFailureDoesNotReserveEndpoint(t *testing.T) {
	root, environment, _ := bootstrapCommandFixture(t)
	if err := os.MkdirAll(filepath.Join(root, "data"), 0700); err != nil {
		t.Fatal(err)
	}
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	definition, _ := program.resources.Lookup("demo")
	path := filepath.Join(root, "state", "yards", "default", "config.env")
	snapshot, err := readConfigAuthoringTarget(path)
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := program.resourceReservedPorts(loaded, definition)
	if err != nil {
		t.Fatal(err)
	}
	b := &resourceBootstrap{initial: loaded, loaded: loaded, definition: definition,
		selectionPath: path, selection: snapshot, profiles: "existing fixture",
		request: resourceendpoint.Request{Directory: filepath.Join(root, "data", "resource-endpoints"), Yard: "default", Resource: "fixture.demo", Host: "127.0.0.1", PreferredPort: 6768, ReservedPorts: reserved}}
	probes := 0
	b.manager = resourceendpoint.Manager{Occupied: func(context.Context, string, int) (bool, error) {
		probes++
		if probes == 3 {
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			writeCLIFile(t, path, "ENVIRONMENT_PROFILES=concurrent\n", 0600)
		}
		return false, nil
	}}
	plan, err := b.manager.Preview(context.Background(), b.request)
	if err != nil {
		t.Fatal(err)
	}
	b.endpoint = &plan
	err = b.apply(context.Background(), program)
	if !errors.Is(err, config.ErrPersistentTargetStale) {
		t.Fatalf("error=%v", err)
	}
	_, _, exists, readErr := resourceendpoint.ReadSaved(b.request.Directory, b.request.Yard, b.request.Resource)
	if readErr != nil || exists {
		t.Fatalf("stale profile wrote endpoint: exists=%v error=%v", exists, readErr)
	}
}

func TestResourceEndpointReservesPortsOfStoppedLocalYards(t *testing.T) {
	root, environment, _ := bootstrapCommandFixture(t)
	for name, contents := range map[string]string{
		"other":  "SSH_PORT=6769\nORCA_HOST_PORT=6768\nADB_PROXY_PORT=6770\n",
		"remote": "ACCESS_KIND=remote\nOWNER_ENDPOINT=owner.example\nOWNER_YARD_NAME=default\nSSH_PORT=6771\nORCA_HOST_PORT=6772\n",
	} {
		path := filepath.Join(root, "state", "yards", name, "config.env")
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		writeCLIFile(t, path, contents, 0600)
	}
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	definition, _ := program.resources.Lookup("demo")
	ports, err := program.resourceReservedPorts(loaded, definition)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []int{6768, 6769, 6770} {
		if !slices.Contains(ports, expected) {
			t.Fatalf("configured local port %d not reserved: %v", expected, ports)
		}
	}
	for _, remote := range []int{6771, 6772} {
		if slices.Contains(ports, remote) {
			t.Fatalf("reserved remote owner's port %d", remote)
		}
	}
}

func TestResourceBootstrapPersistsEndpointAndLoadsItForNextCommand(t *testing.T) {
	root, environment, _ := bootstrapCommandFixture(t)
	path := filepath.Join(root, "config", "profiles", "fixture", "resources", "demo.res")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, path, string(content)+"ENDPOINT_DEFAULTS=\"tailscale-self 6768\"\n", 0600)
	writeCLIFile(t, filepath.Join(root, "config", "host.env"), "ENVIRONMENT_PROFILES=existing\nORCA_ADVERTISE_HOST=127.0.0.1\nORCA_HOST_PORT=16768\n", 0600)
	platform := newInitPlatformFixture()
	var stderr bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"demo", "run", "--yes"}, Environment: environment, WorkingDir: root, Stderr: &stderr, InitPlatform: platform})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	host, port, exists, err := resourceendpoint.ReadSaved(filepath.Join(root, "data", "resource-endpoints"), "default", "fixture.demo")
	if err != nil || !exists || host != "127.0.0.1" || port != 16768 {
		t.Fatalf("saved=%q:%d exists=%v err=%v", host, port, exists, err)
	}
	// Removing the manual defaults must retain the endpoint chosen on first up.
	writeCLIFile(t, filepath.Join(root, "config", "host.env"), "ENVIRONMENT_PROFILES=existing\n", 0600)
	var status bytes.Buffer
	next, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"demo", "status"}, Environment: environment, Stdout: &status})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := next.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Environment["ENVIRONMENT_PROFILES"] != "existing fixture" || loaded.Environment["ORCA_HOST_PORT"] != "16768" || loaded.Environment["ORCA_ADVERTISE_HOST"] != "127.0.0.1" {
		t.Fatal("next command lost saved profile or endpoint")
	}
	next, err = New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"demo", "status"}, Environment: environment, Stdout: &status})
	if err != nil {
		t.Fatal(err)
	}
	if code := next.Run(context.Background()); code != 0 {
		t.Fatalf("status failed: %d", code)
	}
	if !strings.Contains(status.String(), "127.0.0.1:16768") || !strings.Contains(status.String(), "saved") {
		t.Fatalf("status omitted saved endpoint: %s", status.String())
	}
}

func TestResourceBootstrapPreservesProfilesAndInitializesAfterConsent(t *testing.T) {
	root, environment, applyLog := bootstrapCommandFixture(t)
	platform := newInitPlatformFixture()
	selection := filepath.Join(root, "state", "yards", "default", "config.env")
	prompt := &callbackPrompt{callback: func() {
		if len(platform.applied) != 0 {
			t.Fatal("init ran before consent")
		}
		if _, err := os.Stat(selection); !os.IsNotExist(err) {
			t.Fatalf("profile written before consent: %v", err)
		}
	}}
	var stderr bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"demo", "run"}, Environment: environment, WorkingDir: root, Stderr: &stderr, Prompt: prompt, InitPlatform: platform})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if len(platform.applied) == 0 {
		t.Fatal("bootstrap skipped init")
	}
	content, err := os.ReadFile(selection)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "existing fixture") {
		t.Fatalf("lost existing profile: %s", content)
	}
	if got := readResourceApplyLog(t, applyLog); got != "run\n" {
		t.Fatalf("apply=%q", got)
	}
}

func TestResourceBootstrapDeclineLeavesSelectionAndInitUntouched(t *testing.T) {
	root, environment, applyLog := bootstrapCommandFixture(t)
	platform := newInitPlatformFixture()
	prompt := &testkit.Prompt{Answers: []bool{false}}
	var stderr bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"demo", "run"}, Environment: environment, WorkingDir: root, Stderr: &stderr, Prompt: prompt, InitPlatform: platform})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if len(prompt.Requests) != 1 || prompt.Requests[0].Default != "yes" {
		t.Fatalf("prompts=%#v", prompt.Requests)
	}
	if !strings.Contains(strings.Join(prompt.Requests[0].Consequences, "\n"), "profile") {
		t.Fatal("prompt omitted profile selection")
	}
	if len(platform.applied) != 0 {
		t.Fatal("declined bootstrap initialized yard")
	}
	for _, path := range []string{applyLog, filepath.Join(root, "state", "yards", "default", "config.env")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("declined bootstrap wrote %s: %v", path, err)
		}
	}
}

func TestResourceBootstrapRejectsSelectionDriftBeforeInit(t *testing.T) {
	root, environment, applyLog := bootstrapCommandFixture(t)
	platform := newInitPlatformFixture()
	selection := filepath.Join(root, "state", "yards", "default", "config.env")
	prompt := &callbackPrompt{callback: func() {
		if err := os.MkdirAll(filepath.Dir(selection), 0700); err != nil {
			t.Fatal(err)
		}
		writeCLIFile(t, selection, "ENVIRONMENT_PROFILES=changed\n", 0600)
	}}
	var stderr bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"demo", "run"}, Environment: environment, WorkingDir: root, Stderr: &stderr, Prompt: prompt, InitPlatform: platform})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if len(platform.applied) != 0 {
		t.Fatal("stale bootstrap initialized yard")
	}
	if _, err := os.Stat(applyLog); !os.IsNotExist(err) {
		t.Fatalf("stale bootstrap reached resource: %v", err)
	}
}
