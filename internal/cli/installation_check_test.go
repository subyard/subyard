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
	"testing"

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
	var stdout, stderr bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: append([]string{"check", "--json"}, arguments...),
		Environment: environment, Stdout: &stdout, Stderr: &stderr, Incus: lifecycleIncus()})
	if err != nil {
		t.Fatal(err)
	}
	before := nativeTreeSnapshot(t, root)
	code := program.Run(context.Background())
	if after := nativeTreeSnapshot(t, root); !reflect.DeepEqual(before, after) {
		t.Fatalf("check changed configuration, data, known_hosts or audit:\n%v\n%v", before, after)
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
		"state.journals.cleanup.cleanup%2Ftoken":           application.FindingBlocker,
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
