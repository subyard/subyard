//go:build linux

package migration

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestOriginalV1RustWholeEntryFixture(t *testing.T) {
	binary := os.Getenv("SUBYARD_RUST_V1_WHOLE_CLI_TEST_BINARY")
	if binary == "" {
		t.Skip("explicit whole-owner test binary required")
	}
	options, journal, _ := publishedV1CompatibilityFixture(t, "0.4.2")
	beforeRoot := filepath.Dir(options.ConfigHome)
	root, err := os.MkdirTemp("/tmp", "subyard-v1-whole-physical-")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(root); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(beforeRoot, root); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() || os.Getenv("SUBYARD_RUST_V1_WHOLE_KEEP_FIXTURE") != "" {
			t.Log("retained physical roots", root)
		} else if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	replace := func(s string) string { return strings.ReplaceAll(s, beforeRoot, root) }
	options.ConfigHome = replace(options.ConfigHome)
	options.DataHome = replace(options.DataHome)
	options.RuntimeRoot = replace(options.RuntimeRoot)
	options.RepositoryRoot = replace(options.RepositoryRoot)
	options.RegistryPath = replace(options.RegistryPath)
	options.Executable = replace(options.Executable)
	options.Incus = replace(options.Incus)
	journal = replace(journal)
	env := []string{"HOME=" + root, "PATH=" + root + ":/usr/bin:/bin", "SUBYARD_HOME=" + options.DataHome, "SUBYARD_CONFIG_HOME=" + options.ConfigHome, "SUBYARD_OPERATOR_HOME=" + root, "SUBYARD_NO_AUDIT=1"}
	for _, s := range options.Environment {
		name, _, _ := strings.Cut(s, "=")
		if strings.HasPrefix(name, "MIGRATION_") || strings.HasPrefix(name, "V1_") {
			env = append(env, replace(s))
		}
	}
	incusRaw, err := os.ReadFile(options.Incus)
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "physical-bin")
	if err = os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	options.Incus = filepath.Join(bin, "incus")
	testkit.WriteFile(t, options.Incus, incusRaw, 0700)
	env = replaceTestEnvironment(env, "PATH", bin+":/usr/bin:/bin")
	options.Environment = env
	reader, err := NewV1ImportReader(V1ImportOptions{ConfigHome: options.ConfigHome, RuntimeRoot: options.RuntimeRoot, Facts: mustOriginalV1Observer(t, options)})
	if err != nil {
		t.Fatal(err)
	}
	// Physical fixture preflight is recorded separately; its result never enters
	// the Rust process or its plan/intent as native authority.
	if _, err = reader.InspectBound(context.Background(), V1ImportRuntimePair{Current: testPublishedV1042Runtime, Previous: testPublishedV1PreviousRuntime}); err != nil {
		t.Fatal("physical original preflight", err)
	}
	descriptor := struct {
		ConfigHome, DataHome, RuntimeRoot, JournalPath string
		Environment                                    []string
	}{options.ConfigHome, options.DataHome, options.RuntimeRoot, journal, env}
	raw, err := json.Marshal(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "whole-entry.json")
	testkit.WriteFile(t, path, raw, 0600)

	t.Log("physical roots", root)
	command := exec.Command(binary, "-test.v", "-test.run=^TestRustOwnerOriginalV1WholeEntryRefusesRetiredSource$", "-test.timeout=2m")
	command.Env = append(env, "SUBYARD_RUST_V1_WHOLE_FIXTURE="+path, "SUBYARD_RUST_SOURCE_QUERY_TOOLING_ROOT="+os.Getenv("SUBYARD_RUST_SOURCE_QUERY_TOOLING_ROOT"))
	output, err := command.CombinedOutput()
	t.Logf("%s", output)
	if err != nil {
		t.Fatal("whole-entry process", err)
	}
}
