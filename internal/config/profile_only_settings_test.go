package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestProfileOnlySettingsAreShippedScopeOnly(t *testing.T) {
	for _, name := range profileOnlySettings {
		for _, scope := range []SettingScope{ScopeHost, ScopeShared, ScopeYard, ScopeCommand} {
			if err := ValidateSetting(scope, name, "", false); err == nil {
				t.Errorf("%s is accepted in %s scope", name, scope)
			}
		}
		if err := ValidateSetting(ScopeShipped, name, "", false); err != nil {
			t.Errorf("%s is not a shipped setting: %v", name, err)
		}
	}
}

type profileOnlyFixture struct {
	root, home, configHome string
}

func newProfileOnlyFixture(t *testing.T) profileOnlyFixture {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	home := testkit.TempDir(t)
	return profileOnlyFixture{root: filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..")), home: home, configHome: filepath.Join(home, ".config", "subyard")}
}

func (fixture profileOnlyFixture) write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, []byte(content), 0o600)
}

func (fixture profileOnlyFixture) load(yard string, command map[string]string) (Loaded, error) {
	environment := map[string]string{"SUBYARD_OPERATOR_HOME": fixture.home, "SUBYARD_CONFIG_HOME": fixture.configHome, "SUBYARD_HOME": filepath.Join(fixture.home, ".subyard")}
	for name, value := range command {
		environment[name] = value
	}
	return Load(LoadOptions{RepositoryRoot: fixture.root, OperatorHome: fixture.home, YardName: yard, Environment: environment})
}

func renderedIgnored(loaded Loaded) []string {
	var rendered []string
	for _, assignment := range loaded.IgnoredAssignments() {
		rendered = append(rendered, assignment.String())
	}
	return rendered
}

func TestOperatorProfileOnlyAssignmentsAreIgnoredAndReported(t *testing.T) {
	fixture := newProfileOnlyFixture(t)
	host := filepath.Join(fixture.configHome, "config.env")
	yard := filepath.Join(fixture.configHome, "yards", "default", "config.env")
	git := filepath.Join(fixture.configHome, GitSettingsRelativePath, "overrides", "shared", "config.env")

	// Empty assignments are tolerated and never become effective settings.
	fixture.write(t, host, "YARD_CAPS=\nYARD_DEVICES=\n")
	fixture.write(t, yard, "YARD_MOUNTS=\n")
	if loaded, err := fixture.load("default", map[string]string{"YARD_CAPS": ""}); err != nil || len(loaded.IgnoredAssignments()) != 0 {
		t.Fatalf("empty profile-only assignments reported: %v %v", err, loaded.IgnoredAssignments())
	} else {
		for _, resolution := range loaded.Settings["YARD_CAPS"].Resolutions {
			if resolution.Status != "unset" {
				t.Fatalf("empty operator assignment became a setting: %#v", resolution)
			}
		}
	}

	fixture.write(t, host, "YARD_CAPS=\nYARD_DEVICES=fuse\nSSH_PORT=not-a-port\n")
	fixture.write(t, yard, "YARD_MOUNTS=cache:/srv/cache:rw:0755\n")
	// Another validation error in the same file is still reported.
	if _, err := fixture.load("default", nil); err == nil || !strings.Contains(err.Error(), "SSH_PORT") {
		t.Fatalf("an ignored assignment hid another validation error: %v", err)
	}
	fixture.write(t, host, "YARD_CAPS=\nYARD_DEVICES=fuse\n")
	fixture.write(t, git, "YARD_DEVICES=kvm\n")
	// Loading succeeds: each assignment is discarded and observed at its scope, never refused.
	loaded, err := fixture.load("default", map[string]string{"YARD_CAPS": "nesting"})
	if err != nil {
		t.Fatalf("ignored assignments refused the load: %v", err)
	}
	want := []string{
		"YARD_CAPS in the command environment is ignored: only environment profiles declare it; unset it",
		"YARD_DEVICES in " + git + ":1 is ignored: only environment profiles declare it; remove it with yard -Y default config unset YARD_DEVICES --scope shared --git",
		"YARD_DEVICES in " + host + ":2 is ignored: only environment profiles declare it; remove it with yard -Y default config unset YARD_DEVICES --scope host --local",
		"YARD_MOUNTS in " + yard + ":1 is ignored: only environment profiles declare it; remove it with yard -Y default config unset YARD_MOUNTS --scope yard --local",
	}
	if got := renderedIgnored(loaded); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("observations = %q\nwant %q", got, want)
	}
	if loaded.Context.YardName != "default" {
		t.Fatalf("load lost its context: %#v", loaded.Context)
	}
	for _, name := range profileOnlySettings {
		if value, ok := loaded.Environment[name]; ok && value != "" {
			t.Errorf("operator %s became effective: %q", name, value)
		}
	}
}

func TestIgnoredProfileOnlyAssignmentsStillRequireLiterals(t *testing.T) {
	fixture := newProfileOnlyFixture(t)
	host := filepath.Join(fixture.configHome, "config.env")
	fixture.write(t, host, "YARD_DEVICES=\"${LIMITS_MEMORY:=bogus}\"\nYARD_CAPS=$CAPABILITIES\n")
	_, err := fixture.load("default", nil)
	message, ok := SourceDiagnostic(fixture.configHome, err)
	if !ok || !strings.Contains(message, "config.env:1: YARD_DEVICES") || !strings.Contains(message, "only literal values") {
		t.Fatalf("expected source refusal, got %v", err)
	}
	if !strings.Contains(err.Error(), "config.env:2: YARD_CAPS") || strings.Contains(err.Error(), "bogus") || strings.Contains(err.Error(), "CAPABILITIES") {
		t.Fatalf("additional source refusal missing or input leaked: %v", err)
	}
}

// The repair selects the same local registration as config unset.
func TestIgnoredProfileOnlyRepairTargetsTheSourceFile(t *testing.T) {
	fixture := newProfileOnlyFixture(t)
	flat := filepath.Join(fixture.configHome, "yards", "box.env")
	fixture.write(t, flat, "SSH_PORT=2290\nYARD_DEVICES=gpu\n")
	loaded, err := fixture.load("box", nil)
	if err != nil {
		t.Fatal(err)
	}
	ignored := loaded.IgnoredAssignments()
	if len(ignored) != 1 || ignored[0].Repair() != "yard -Y box config unset YARD_DEVICES --scope yard --local" ||
		ignored[0].String() != "YARD_DEVICES in "+flat+":2 is ignored: only environment profiles declare it; remove it with yard -Y box config unset YARD_DEVICES --scope yard --local" {
		t.Fatalf("flat registration repair = %#v %q", ignored, renderedIgnored(loaded))
	}

	nested := filepath.Join(fixture.configHome, "yards", "box", "config.env")
	fixture.write(t, nested, "SSH_PORT=2290\nYARD_DEVICES=gpu\n")
	_, err = fixture.load("box", nil)
	var conflict *SettingsFileConflict
	if !errors.As(err, &conflict) || conflict.First != nested || conflict.Second != flat {
		t.Fatalf("duplicate registrations = %v", err)
	}
	if err := os.Remove(flat); err != nil {
		t.Fatal(err)
	}
	loaded, err = fixture.load("box", nil)
	if err != nil {
		t.Fatal(err)
	}
	ignored = loaded.IgnoredAssignments()
	if len(ignored) != 1 || ignored[0].Path != nested || ignored[0].Repair() != "yard -Y box config unset YARD_DEVICES --scope yard --local" {
		t.Fatalf("canonical registration repair = %#v", ignored)
	}
}

// Profile-only literals stay visible to repair while sync omits them.
func TestIgnoredProfileOnlyLiteralsParseOnEveryOperatorPath(t *testing.T) {
	fixture := newProfileOnlyFixture(t)
	path := filepath.Join(fixture.configHome, "config.env")
	content := []byte("SSH_PORT=2290\nYARD_DEVICES=gpu\nLIMITS_MEMORY=2GiB\n")
	fixture.write(t, path, string(content))
	assignments, err := ParsePersistentAssignments(path, content)
	if err != nil {
		t.Fatalf("persistent parse refused an ignored literal: %v", err)
	}
	if len(assignments) != 3 || assignments[1].Name != "YARD_DEVICES" || assignments[2].Value != "2GiB" {
		t.Fatalf("persistent assignments = %#v", assignments)
	}
	names, err := AssignedSettingNames(path, content)
	if err != nil || strings.Join(names, " ") != "LIMITS_MEMORY SSH_PORT" {
		t.Fatalf("assigned names = %q, %v", names, err)
	}
	edited, err := EditPersistentAssignmentContent(path, content, "YARD_DEVICES", nil)
	if err != nil || string(edited) != "SSH_PORT=2290\nLIMITS_MEMORY=2GiB\n" {
		t.Fatalf("config unset of an ignored literal = %q, %v", edited, err)
	}
}
