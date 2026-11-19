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
	"github.com/Subyard/Subyard/internal/testkit"
)

func installationCheckFixture(t *testing.T) (string, []string) {
	t.Helper()
	root, environment, _ := nativeFixture(t)
	path := filepath.Join(root, "config", "commands.registry")
	manifest, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	writeInstallationFixture(t, path, append(manifest, "check||@check||forward|read|never|public|lifecycle|simple|check|check|--json --host --help|\n"...), 0o600)
	for _, directory := range []string{"state", "data"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeInstallationFixture(t, filepath.Join(root, "home", ".ssh", "known_hosts"), []byte("retained host key\n"), 0o600)
	// Audit is enabled: the check itself must not write a record.
	return root, slices.DeleteFunc(environment, func(value string) bool { return strings.HasPrefix(value, "SUBYARD_NO_AUDIT=") })
}

func writeInstallationFixture(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, data, mode)
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
	root, environment := installationCheckFixture(t)
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
	root, environment := installationCheckFixture(t)
	configHome, data := filepath.Join(root, "state"), filepath.Join(root, "data")
	writeInstallationFixture(t, filepath.Join(configHome, "config.env"), []byte("YARD_CAPS=ignored\n"), 0o600)
	writeInstallationFixture(t, filepath.Join(configHome, "yards", "broken", "config.env"), []byte("UNKNOWN_SETTING=1\n"), 0o600)
	for _, name := range []string{"a", "b"} {
		writeInstallationFixture(t, filepath.Join(configHome, "generated", name), []byte("fixture\n"), 0o666)
	}
	writeInstallationFixture(t, filepath.Join(data, "owner-inventory", "registration.json"), []byte("{}\n"), 0o600)
	writeInstallationFixture(t, filepath.Join(data, "cleanup", "token", "journal.json"), []byte("{}\n"), 0o600)
	writeInstallationFixture(t, filepath.Join(configHome, "projects", "bad.json"), []byte("{"), 0o600)
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
	root, environment := installationCheckFixture(t)
	keys := filepath.Join(root, "custom keys")
	environment = append(slices.DeleteFunc(environment, func(value string) bool { return strings.HasPrefix(value, "SUBYARD_KEYS_ROOT=") }), "SUBYARD_KEYS_ROOT="+keys)
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
	root, environment := installationCheckFixture(t)
	writeInstallationFixture(t, filepath.Join(root, "state", "generated", "a"), []byte("fixture\n"), 0o666)
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
		findings["release.config"].Class != application.FindingNotInspected || findings["config.permissions.generated%2Fa"].Class != application.FindingDrift {
		t.Fatalf("code=%d findings=%+v", code, findings)
	}
}

func TestInstallationCheckReportsLegacyInputsWithoutAYardLoad(t *testing.T) {
	root, environment := installationCheckFixture(t)
	configHome := filepath.Join(root, "state")
	writeInstallationFixture(t, filepath.Join(configHome, "config.env"), []byte("UNKNOWN_SETTING=1\n"), 0o600)
	for _, name := range []string{"a.env", "b.env"} {
		writeInstallationFixture(t, filepath.Join(configHome, "secrets", "legacy", name), []byte("fixture\n"), 0o600)
	}
	code, findings := reportOf(t, root, environment)
	if code != 1 || findings["config.schema.default"].Class != application.FindingBlocker ||
		findings["config.sources.legacy.secrets%2Flegacy%2Fa.env"].Resource != filepath.Join(configHome, "secrets", "legacy", "a.env") ||
		findings["config.sources.legacy.secrets%2Flegacy%2Fb.env"].Class != application.FindingNotice {
		t.Fatalf("code=%d findings=%+v", code, findings)
	}
}

func TestInstallationCheckJudgesConfigurationFromPersistedSources(t *testing.T) {
	root, environment := installationCheckFixture(t)
	writeInstallationFixture(t, filepath.Join(root, "state", "yards", "other", "config.env"), []byte("SSH_PORT=2224\n"), 0o600)
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
	root, environment := installationCheckFixture(t)
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
	root, environment := installationCheckFixture(t)
	keys := filepath.Join(root, "custom keys")
	writeInstallationFixture(t, filepath.Join(keys, "identity", "age.txt"), []byte("partial\n"), 0o600)
	environment = append(slices.DeleteFunc(environment, func(value string) bool { return strings.HasPrefix(value, "SUBYARD_KEYS_ROOT=") }), "SUBYARD_KEYS_ROOT="+keys)
	_, findings := reportOf(t, root, environment)
	if finding := findings["host.credentials.initialization"]; finding.Class != application.FindingBlocker || finding.Next != "" || !strings.Contains(finding.Message, "refuses") {
		t.Fatalf("partial identity: %+v", finding)
	}
}

type failingCheckOutput struct{}

func (failingCheckOutput) Write([]byte) (int, error) { return 0, syscall.ENOSPC }

func TestInstallationCheckOutputFailureIsExitTwo(t *testing.T) {
	root, environment := installationCheckFixture(t)
	for _, arguments := range [][]string{{"check", "--help"}, {"check", "--json"}, {"check"}} {
		var stderr bytes.Buffer
		program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: arguments,
			Environment: environment, Stdout: failingCheckOutput{}, Stderr: &stderr, Incus: lifecycleIncus()})
		if err != nil {
			t.Fatal(err)
		}
		if code := program.Run(context.Background()); code != 2 || !strings.Contains(stderr.String(), "no space left") {
			t.Fatalf("%v: code=%d stderr=%q", arguments, code, stderr.String())
		}
	}
}
