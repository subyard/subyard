package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/application"
)

// installationCheckFixture enables audit: the check itself must not write a record.
func installationCheckFixture(t *testing.T) *cliFixture {
	t.Helper()
	// The installation check constructors leave WorkingDir empty, as before the fixture.
	fixture := newFixture(t, withAudit(), withWorkingDir(""))
	fixture.Write("config/commands.registry", fixture.Read("config/commands.registry")+"check||@check||forward|read|never|public|lifecycle|simple|check|check|--json --host --help|\n")
	for _, directory := range []string{"state", "data"} {
		if err := os.MkdirAll(filepath.Join(fixture.Root, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	fixture.Write(filepath.Join(fixture.Home, ".ssh", "known_hosts"), "retained host key\n")
	return fixture
}

func runInstallationCheck(t *testing.T, root string, environment []string, arguments ...string) (int, application.ConformanceReport, string) {
	t.Helper()
	return runInstallationCheckWith(t, root, environment, true, arguments...)
}

// runInstallationCheckWith compares the tree before and after when it is readable.
func runInstallationCheckWith(t *testing.T, root string, environment []string, snapshot bool, arguments ...string) (int, application.ConformanceReport, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: append([]string{"check", "--json"}, arguments...),
		Environment: environment, Stdout: &stdout, Stderr: &stderr, Incus: lifecycleIncus()})
	if err != nil {
		t.Fatal(err)
	}
	var before any
	if snapshot {
		before = nativeTreeSnapshot(t, root)
	}
	code := program.Run(context.Background())
	if snapshot {
		if after := nativeTreeSnapshot(t, root); !reflect.DeepEqual(before, after) {
			t.Fatalf("check changed configuration, data, known_hosts or audit:\n%v\n%v", before, after)
		}
	}
	var report application.ConformanceReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("code=%d stdout=%s stderr=%s: %v", code, &stdout, &stderr, err)
	}
	return code, report, stderr.String()
}

func reportFindings(report application.ConformanceReport) map[string]application.ConformanceFinding {
	findings := map[string]application.ConformanceFinding{}
	for _, section := range report.Sections {
		for _, finding := range section.Findings {
			findings[finding.ID] = finding
		}
	}
	return findings
}

func TestInstallationCheckIsReadOnlyWithNonWritableHomes(t *testing.T) {
	fixture := installationCheckFixture(t)
	root, environment := fixture.Root, fixture.Environment
	code, writable, stderr := runInstallationCheck(t, root, environment, "--yes")
	var sections []string
	for _, section := range writable.Sections {
		sections = append(sections, section.ID)
	}
	findings := reportFindings(writable)
	if code != 1 || stderr != "" || !slices.Equal(sections, []string{"config", "release", "host", "state"}) ||
		findings["release.read"].Class != application.FindingBlocker || findings["host.credentials.initialization"].Class != application.FindingDrift {
		t.Fatalf("code=%d stderr=%q report=%+v", code, stderr, writable)
	}
	for _, directory := range []string{"state", "data"} {
		if err := os.Chmod(filepath.Join(root, directory), 0o500); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, directory := range []string{"state", "data"} {
			_ = os.Chmod(filepath.Join(root, directory), 0o700)
		}
	})
	if code, readOnly, _ := runInstallationCheck(t, root, environment, "--yes"); code != 1 || !reflect.DeepEqual(readOnly, writable) {
		t.Fatalf("report depends on write access: code=%d\n%+v\n%+v", code, readOnly, writable)
	}
}

func TestInstallationCheckReportsEveryIndependentFinding(t *testing.T) {
	fixture := installationCheckFixture(t)
	root, environment := fixture.Root, fixture.Environment
	configHome, data := filepath.Join(root, "state"), filepath.Join(root, "data")
	fixture.Write(filepath.Join(configHome, "config.env"), "YARD_CAPS=ignored\n")
	fixture.Write(filepath.Join(configHome, "yards", "broken", "config.env"), "UNKNOWN_SETTING=1\n")
	for _, name := range []string{"a", "b"} {
		fixture.Write(filepath.Join(configHome, "generated", name), "fixture\n", 0o666)
	}
	fixture.Write(filepath.Join(data, "owner-inventory", "registration.json"), "{}\n")
	fixture.Write(filepath.Join(data, "cleanup", "token", "journal.json"), "{}\n")
	fixture.Write(filepath.Join(configHome, "projects", "bad.json"), "{")
	code, report, _ := runInstallationCheck(t, root, environment)
	findings := reportFindings(report)
	want := map[string]application.FindingClass{
		"config.schema.broken":                             application.FindingBlocker,
		"config.sources.ignored.host.config.env.YARD_CAPS": application.FindingNotice,
		"config.permissions.generated%2Fa":                 application.FindingDrift, "config.permissions.generated%2Fb": application.FindingDrift,
		"state.journals.owner-inventory.registration.json": application.FindingBlocker,
		"state.journals.cleanup.token":                     application.FindingBlocker,
		"state.journals.cleanup.read":                      application.FindingBlocker,
		"state.stores.projects.default":                    application.FindingBlocker,
	}
	for id, class := range want {
		if _, found := findings[id]; !found {
			t.Errorf("missing %s", id)
		} else if findings[id].Class != class {
			t.Errorf("%s class=%s", id, findings[id].Class)
		}
	}
	if code != 1 || findings["config.permissions.generated%2Fa"].Next != "chmod go-w -- '"+filepath.Join(configHome, "generated", "a")+"'" {
		t.Fatalf("code=%d findings=%+v", code, findings)
	}
}

func TestInstallationCheckUsesEnvironmentSuppliedSettings(t *testing.T) {
	fixture := installationCheckFixture(t)
	root := fixture.Root
	keys := filepath.Join(root, "custom keys")
	environment := append(slices.DeleteFunc(slices.Clone(fixture.Environment), func(value string) bool { return strings.HasPrefix(value, "SUBYARD_KEYS_ROOT=") }), "SUBYARD_KEYS_ROOT="+keys)
	_, report, _ := runInstallationCheck(t, root, environment)
	if message := reportFindings(report)["host.credentials.initialization"].Message; !strings.Contains(message, filepath.Join(keys, "identity.json")) {
		t.Fatalf("credential inspection ignored SUBYARD_KEYS_ROOT: %q", message)
	}
}

func reportOf(t *testing.T, root string, environment []string) (int, map[string]application.ConformanceFinding) {
	t.Helper()
	code, report, _ := runInstallationCheck(t, root, environment)
	return code, reportFindings(report)
}

func TestInstallationCheckNeverWaitsForAConfigurationWriter(t *testing.T) {
	fixture := installationCheckFixture(t)
	root, environment := fixture.Root, fixture.Environment
	fixture.Write(filepath.Join(fixture.ConfigHome, "generated", "a"), "fixture\n", 0o666)
	directory, err := os.Open(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	if err := syscall.Flock(int(directory.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	code, findings := reportOf(t, root, environment)
	if code != 1 || time.Since(started) > 10*time.Second || findings["config.root.lock"].Class != application.FindingBlocker ||
		findings["release.config"].Class != application.FindingNotInspected || findings["config.permissions.generated%2Fa"].Class != application.FindingDrift ||
		findings["host.credentials.config"].Class != application.FindingNotInspected || findings["state.stores.projects.config"].Class != application.FindingNotInspected {
		t.Fatalf("code=%d findings=%+v", code, findings)
	}
}

func TestInstallationCheckReportsLegacyInputsWithoutAYardLoad(t *testing.T) {
	fixture := installationCheckFixture(t)
	root, environment := fixture.Root, fixture.Environment
	configHome := filepath.Join(root, "state")
	fixture.Write(filepath.Join(configHome, "config.env"), "UNKNOWN_SETTING=1\n")
	for _, name := range []string{"a.env", "b.env"} {
		fixture.Write(filepath.Join(configHome, "secrets", "legacy", name), "fixture\n")
	}
	code, findings := reportOf(t, root, environment)
	if code != 1 || findings["config.schema.default"].Class != application.FindingBlocker ||
		findings["config.sources.legacy.secrets%2Flegacy%2Fa.env"].Resource != filepath.Join(configHome, "secrets", "legacy", "a.env") ||
		findings["config.sources.legacy.secrets%2Flegacy%2Fb.env"].Class != application.FindingNotice {
		t.Fatalf("code=%d findings=%+v", code, findings)
	}
}

func TestInstallationCheckJudgesConfigurationFromPersistedSources(t *testing.T) {
	fixture := installationCheckFixture(t)
	root, environment := fixture.Root, fixture.Environment
	fixture.Write(filepath.Join(fixture.ConfigHome, "yards", "other", "config.env"), "SSH_PORT=2224\n")
	// The fixture exports SSH_PORT=2222, which would reserve one port for both yards.
	_, findings := reportOf(t, root, environment)
	if _, conflict := findings["config.conflicts.listeners"]; conflict {
		t.Fatalf("exported SSH_PORT produced a listener conflict: %+v", findings)
	}
	rejected := append(slices.DeleteFunc(slices.Clone(environment), func(value string) bool { return strings.HasPrefix(value, "SSH_PORT=") }), "SSH_PORT=bogus")
	code, findings := reportOf(t, root, rejected)
	for id, finding := range findings {
		if strings.HasPrefix(id, "config.schema.") {
			t.Fatalf("a rejected environment value failed persisted schema %s: %+v", id, finding)
		}
	}
	if code != 1 || findings["host.environment.default"].Class != application.FindingBlocker || findings["host.environment.other"].Class != application.FindingBlocker {
		t.Fatalf("code=%d findings=%+v", code, findings)
	}
}

func TestInstallationCheckSeparatesUnreadableCleanupAndConfigPaths(t *testing.T) {
	fixture := installationCheckFixture(t)
	root, environment := fixture.Root, fixture.Environment
	data, configHome := filepath.Join(root, "data"), filepath.Join(root, "state")
	if err := os.MkdirAll(filepath.Join(data, "cleanup", "fifo"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(data, "cleanup", "fifo", "journal.json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(configHome, "generated"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(configHome, "generated"), 0o222); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(configHome, "generated"), 0o700) })
	code, report, _ := runInstallationCheckWith(t, root, environment, false)
	findings := reportFindings(report)
	if code != 1 || findings["state.journals.cleanup.fifo"].Class != application.FindingBlocker || findings["state.journals.cleanup.read"].Class != application.FindingBlocker ||
		findings["config.permissions.generated"].Class != application.FindingDrift ||
		findings["config.permissions.generated.read"].Class != application.FindingBlocker || findings["config.permissions.generated.read"].Next != "" ||
		strings.Contains(findings["config.permissions.generated"].Message, "permission denied") {
		t.Fatalf("code=%d findings=%+v", code, findings)
	}
	if err := os.Chmod(filepath.Join(data, "cleanup"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(data, "cleanup"), 0o700) })
	if _, report, _ = runInstallationCheckWith(t, root, environment, false); reportFindings(report)["state.journals.cleanup.read"].Class != application.FindingBlocker {
		t.Fatalf("unreadable cleanup directory was skipped: %+v", findings)
	}
}

func TestInstallationCheckBlocksAPartialIdentityThatInitRefuses(t *testing.T) {
	fixture := installationCheckFixture(t)
	root := fixture.Root
	keys := filepath.Join(root, "custom keys")
	fixture.Write(filepath.Join(keys, "identity", "age.txt"), "partial\n")
	environment := append(slices.DeleteFunc(slices.Clone(fixture.Environment), func(value string) bool { return strings.HasPrefix(value, "SUBYARD_KEYS_ROOT=") }), "SUBYARD_KEYS_ROOT="+keys)
	_, findings := reportOf(t, root, environment)
	if finding := findings["host.credentials.initialization"]; finding.Class != application.FindingBlocker || finding.Next != "" || !strings.Contains(finding.Message, "refuses") {
		t.Fatalf("partial identity: %+v", finding)
	}
}

type failingCheckOutput struct{}

func (failingCheckOutput) Write([]byte) (int, error) { return 0, syscall.ENOSPC }

func TestInstallationCheckOutputFailureIsExitTwo(t *testing.T) {
	fixture := installationCheckFixture(t)
	for _, arguments := range [][]string{{"check", "--help"}, {"check", "--json"}, {"check"}} {
		var stderr bytes.Buffer
		program := fixture.New(Options{Arguments: arguments, Stdout: failingCheckOutput{}, Stderr: &stderr, Incus: lifecycleIncus()})
		if code := program.Run(context.Background()); code != 2 || !strings.Contains(stderr.String(), "no space left") {
			t.Fatalf("%v: code=%d stderr=%q", arguments, code, stderr.String())
		}
	}
}
