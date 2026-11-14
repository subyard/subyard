package config

import (
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
	return Load(LoadOptions{RepositoryRoot: fixture.root, OperatorHome: fixture.home, YardName: yard, DisablePrivate: true, Environment: environment})
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

// An ignored assignment has no side effects: neither its own nested defaults
// nor a nested default of it in another assignment changes any setting.
func TestIgnoredProfileOnlyExpressionsHaveNoSideEffects(t *testing.T) {
	fixture := newProfileOnlyFixture(t)
	host := filepath.Join(fixture.configHome, "config.env")
	fixture.write(t, host, "YARD_DEVICES=\"${LIMITS_MEMORY:=bogus}\"\nLIMITS_CPU=\"${LIMITS_CPU:-2}\"\nYARD_CAPS=\"${LIMITS_CPU:=9}\"\n")
	loaded, err := fixture.load("default", nil)
	if err != nil {
		t.Fatalf("a nested default of an ignored assignment refused the load: %v", err)
	}
	if value := loaded.Environment["LIMITS_MEMORY"]; value == "bogus" {
		t.Fatalf("ignored assignment set LIMITS_MEMORY=%q", value)
	}
	if value := loaded.Environment["LIMITS_CPU"]; value != "2" {
		t.Fatalf("ignored assignment changed LIMITS_CPU=%q", value)
	}
	if got := renderedIgnored(loaded); len(got) != 2 || !strings.Contains(got[0], "YARD_CAPS in "+host+":3") || !strings.Contains(got[1], "YARD_DEVICES in "+host+":1") {
		t.Fatalf("observations = %q", got)
	}

	fixture.write(t, host, "LIMITS_MEMORY=\"${YARD_DEVICES:=2GiB}\"\n")
	loaded, err = fixture.load("default", nil)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Environment["LIMITS_MEMORY"] != "2GiB" || loaded.Environment["YARD_DEVICES"] != "" {
		t.Fatalf("nested default leaked: LIMITS_MEMORY=%q YARD_DEVICES=%q", loaded.Environment["LIMITS_MEMORY"], loaded.Environment["YARD_DEVICES"])
	}
	// config unset cannot remove a nested default, so the repair describes
	// removing it from the other setting without inserting any text.
	if ignored := loaded.IgnoredAssignments(); len(ignored) != 1 || !ignored[0].Nested ||
		ignored[0].Repair() != "line 1 of "+host+`: remove the YARD_DEVICES default from LIMITS_MEMORY; it currently evaluates to "2GiB"` ||
		ignored[0].String() != "YARD_DEVICES in "+host+`:1 is ignored: only environment profiles declare it; remove the YARD_DEVICES default from LIMITS_MEMORY; it currently evaluates to "2GiB" on that line` {
		t.Fatalf("nested assignment observations = %q / %q", renderedIgnored(loaded), loaded.IgnoredAssignments()[0].Repair())
	}

	// Later records never see a nested default of a discarded name, and a
	// default inside its own fallback stays isolated too.
	fixture.write(t, host, "LIMITS_CPU=\"${YARD_CAPS:=4}\"\nLIMITS_MEMORY=\"${YARD_CAPS:-2}GiB\"\n: \"${YARD_DEVICES:=${LIMITS_MEMORY:=bogus}}\"\n")
	loaded, err = fixture.load("default", nil)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Environment["LIMITS_CPU"] != "4" || loaded.Environment["LIMITS_MEMORY"] != "2GiB" || loaded.Environment["YARD_CAPS"] != "" || loaded.Environment["YARD_DEVICES"] != "" {
		t.Fatalf("discarded defaults leaked: CPU=%q MEMORY=%q CAPS=%q DEVICES=%q", loaded.Environment["LIMITS_CPU"], loaded.Environment["LIMITS_MEMORY"], loaded.Environment["YARD_CAPS"], loaded.Environment["YARD_DEVICES"])
	}
	if got := renderedIgnored(loaded); len(got) != 2 || !strings.HasPrefix(got[0], "YARD_CAPS in "+host+":1 ") || !strings.HasPrefix(got[1], "YARD_DEVICES in "+host+":3 ") {
		t.Fatalf("nested observations = %q", got)
	}

	// A nested default inside a no-op record names that record; the current
	// value is Go-quoted, never inserted into the file.
	fixture.write(t, host, "GIT_USER_NAME=\"${YARD_DEVICES:=${UNSET_VALUE:=literal-\\$SSH_PORT}}\"\n: \"${YARD_CAPS:=fuse}\"\n")
	if loaded, err = fixture.load("default", nil); err != nil || loaded.Environment["GIT_USER_NAME"] != "literal-$SSH_PORT" {
		t.Fatalf("nested literal fixture = %q, %v", loaded.Environment["GIT_USER_NAME"], err)
	}
	want := []string{
		`line 2 of ` + host + `: remove the YARD_CAPS default from the : record; it currently evaluates to "fuse"`,
		`line 1 of ` + host + `: remove the YARD_DEVICES default from GIT_USER_NAME; it currently evaluates to "literal-$SSH_PORT"`,
	}
	if ignored := loaded.IgnoredAssignments(); len(ignored) != 2 || ignored[0].Repair() != want[0] || ignored[1].Repair() != want[1] {
		t.Fatalf("nested repairs = %q", renderedIgnored(loaded))
	}

	// A required check of a discarded name never refuses either, nor is its
	// message evaluated.
	fixture.write(t, host, "YARD_DEVICES=kvm\n: \"${YARD_DEVICES:?}\"\n: \"${YARD_DEVICES:?${MISSING:?required}}\"\n")
	if loaded, err = fixture.load("default", nil); err != nil || len(loaded.IgnoredAssignments()) != 1 {
		t.Fatalf("a required check of an ignored name refused the load: %v %q", err, renderedIgnored(loaded))
	}
	if _, err := ParsePersistentAssignments(host, []byte("YARD_DEVICES=kvm\n: \"${YARD_DEVICES:?}\"\n: \"${YARD_DEVICES:?${MISSING:?required}}\"\n")); err != nil {
		t.Fatalf("persistent parse refused a required check of an ignored name: %v", err)
	}

	// Evaluating an ignored expression never refuses loading.
	fixture.write(t, host, "YARD_DEVICES=\"${LIMITS_MEMORY:?required}\"\n")
	loaded, err = fixture.load("default", nil)
	if err != nil {
		t.Fatalf("an ignored expression refused the load: %v", err)
	}
	if got := renderedIgnored(loaded); len(got) != 1 || got[0] != "YARD_DEVICES in "+host+":1 is ignored: only environment profiles declare it; remove it with yard -Y default config unset YARD_DEVICES --scope host --local" {
		t.Fatalf("failed ignored expression observations = %q", got)
	}
}

// The repair names config unset only when that command writes the file the
// value came from; a flat registration names the exact line to remove.
func TestIgnoredProfileOnlyRepairTargetsTheSourceFile(t *testing.T) {
	fixture := newProfileOnlyFixture(t)
	flat := filepath.Join(fixture.configHome, "yards", "box.env")
	fixture.write(t, flat, "SSH_PORT=2290\nYARD_DEVICES=gpu\n")
	loaded, err := fixture.load("box", nil)
	if err != nil {
		t.Fatal(err)
	}
	ignored := loaded.IgnoredAssignments()
	if len(ignored) != 1 || ignored[0].Repair() != "remove line 2 from "+flat ||
		ignored[0].String() != "YARD_DEVICES in "+flat+":2 is ignored: only environment profiles declare it; remove that line" {
		t.Fatalf("flat registration repair = %#v %q", ignored, renderedIgnored(loaded))
	}

	nested := filepath.Join(fixture.configHome, "yards", "box", "config.env")
	fixture.write(t, nested, "SSH_PORT=2290\nYARD_DEVICES=gpu\n")
	loaded, err = fixture.load("box", nil)
	if err != nil {
		t.Fatal(err)
	}
	ignored = loaded.IgnoredAssignments()
	if len(ignored) != 1 || ignored[0].Path != nested || ignored[0].Repair() != "yard -Y box config unset YARD_DEVICES --scope yard --local" {
		t.Fatalf("canonical registration repair = %#v", ignored)
	}
}

// Every operator-file parse path discards a profile-only assignment as the
// loader does: a failing expression refuses none, and the advertised config
// unset still removes the assignment.
func TestIgnoredProfileOnlyExpressionsParseOnEveryOperatorPath(t *testing.T) {
	fixture := newProfileOnlyFixture(t)
	path := filepath.Join(fixture.configHome, "config.env")
	content := []byte("SSH_PORT=2290\nYARD_DEVICES=\"${LIMITS_MEMORY:?required}\"\nLIMITS_MEMORY=\"${YARD_DEVICES:-2}GiB\"\n")
	fixture.write(t, path, string(content))
	assignments, err := ParsePersistentAssignments(path, content)
	if err != nil {
		t.Fatalf("persistent parse refused an ignored expression: %v", err)
	}
	if len(assignments) != 3 || assignments[1].Name != "YARD_DEVICES" || assignments[2].Value != "2GiB" {
		t.Fatalf("persistent assignments = %#v", assignments)
	}
	names, err := AssignedSettingNames(path)
	if err != nil || strings.Join(names, " ") != "LIMITS_MEMORY SSH_PORT" {
		t.Fatalf("assigned names = %q, %v", names, err)
	}
	edited, err := EditPersistentAssignmentContent(path, content, "YARD_DEVICES", nil)
	if err != nil || string(edited) != "SSH_PORT=2290\nLIMITS_MEMORY=\"${YARD_DEVICES:-2}GiB\"\n" {
		t.Fatalf("config unset of an ignored expression = %q, %v", edited, err)
	}
}
