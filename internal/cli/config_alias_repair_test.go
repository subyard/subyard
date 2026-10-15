package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
)

type sourceRepairConsent struct {
	ready, proceed chan struct{}
	offset         int
}

func (reader *sourceRepairConsent) Read(target []byte) (int, error) {
	if reader.offset == 0 {
		close(reader.ready)
		<-reader.proceed
	}
	if reader.offset == len("yes\n") {
		return 0, io.EOF
	}
	n := copy(target, "yes\n"[reader.offset:])
	reader.offset += n
	return n, nil
}

func TestQueuedSourceRepairCannotRecreateRetiredConfiguration(t *testing.T) {
	_, home, configHome, environment := configCommandFixture(t)
	runtimeRoot := aliasRepairRuntimeFixture(t, home)
	root := filepath.Join(runtimeRoot, "releases", "1.2.3-owner")
	environment = append(environment, "YARD_RUNTIME_ROOT="+runtimeRoot, "SUBYARD_CONFIG_DIR="+filepath.Join(root, "config"))
	testkit.WriteFile(t, filepath.Join(configHome, "config.env"), []byte("AGENTS=claude\nCODING_TOOL_INTEGRATIONS=codex\n"), 0o600)
	consent := &sourceRepairConsent{ready: make(chan struct{}), proceed: make(chan struct{})}
	var stderr bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Environment: environment,
		Arguments: []string{"config", "repair-alias", "CODING_TOOL_INTEGRATIONS", "codex", "--scope", "host", "--local"}, Stdin: consent, Stderr: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	program.promptInputTerminal = func() bool { return true }
	dataHome := filepath.Join(home, ".subyard")
	_, release, err := uninstall.OwnerLock(context.Background(), dataHome, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if release != nil {
			release()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- program.Run(ctx) }()
	select {
	case <-consent.ready:
	case code := <-done:
		t.Fatalf("source repair did not reach consent: %d %s", code, stderr.String())
	case <-ctx.Done():
		t.Fatal("source repair did not reach consent before its deadline")
	}
	close(consent.proceed)
	select {
	case code := <-done:
		t.Fatalf("source repair did not wait for the owner barrier: %d", code)
	case <-time.After(200 * time.Millisecond):
	}
	if err := uninstall.AdvanceOwnerEpoch(dataHome, 0, 1); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(configHome); err != nil {
		t.Fatal(err)
	}
	release()
	release = nil
	if code := <-done; code == 0 || !strings.Contains(stderr.String(), "epoch changed") {
		t.Fatalf("retired source repair outcome = %d %s", code, stderr.String())
	}
	if _, err := os.Lstat(configHome); !os.IsNotExist(err) {
		t.Fatal("queued source repair recreated configuration or migration lock state")
	}
}

func runAliasRepairFixture(t *testing.T, options Options) int {
	t.Helper()
	program, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	return program.Run(context.Background())
}

func aliasRepairRuntimeFixture(t *testing.T, root string) string {
	t.Helper()
	runtimeRoot := filepath.Join(root, "runtime")
	release := filepath.Join(runtimeRoot, "releases", "1.2.3-owner")
	for _, directory := range []string{runtimeRoot, filepath.Join(runtimeRoot, "releases"), release, filepath.Join(release, "bin"), filepath.Join(release, "config")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	engine := []byte("#!/bin/sh\ncase \"${1:-}\" in --version) printf 'yard-engine 1.2.3\\n';; *) exit 99;; esac\n")
	testkit.WriteFile(t, filepath.Join(release, "bin", "yard-engine"), engine, 0o700)
	manifest := fmt.Sprintf("%x  ./bin/yard-engine\n", sha256.Sum256(engine))
	for _, tree := range []string{"config", "scripts"} {
		source := filepath.Join(repositoryRoot(t), tree)
		if err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			relative, err := filepath.Rel(source, path)
			if err != nil {
				return err
			}
			target := filepath.Join(release, tree, relative)
			if entry.IsDir() {
				return os.MkdirAll(target, 0o700)
			}
			payload, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			testkit.WriteFile(t, target, payload, info.Mode().Perm())
			manifest += fmt.Sprintf("%x  ./%s\n", sha256.Sum256(payload), filepath.ToSlash(filepath.Join(tree, relative)))
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	testkit.WriteFile(t, filepath.Join(release, "runtime-files.sha256"), []byte(manifest), 0o600)
	if err := os.Symlink("releases/1.2.3-owner", filepath.Join(runtimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
	return runtimeRoot
}

func TestConfigAliasRepairEarlyExplicitChoiceAndNoOp(t *testing.T) {
	root, home, configHome, environment := configCommandFixture(t)
	runtimeRoot := aliasRepairRuntimeFixture(t, home)
	root = filepath.Join(runtimeRoot, "releases", "1.2.3-owner")
	environment = append(environment, "YARD_RUNTIME_ROOT="+runtimeRoot, "SUBYARD_CONFIG_DIR="+filepath.Join(root, "config"))
	path := filepath.Join(configHome, "config.env")
	before := "# preserved record\nAGENTS='claude'\nCODING_TOOL_INTEGRATIONS='codex'\nSSH_PORT='2398'\n"
	testkit.WriteFile(t, path, []byte(before), 0o640)
	for _, stage := range []string{"preview", "apply", "repeat"} {
		var stdout, stderr bytes.Buffer
		arguments := []string{"config", "repair-alias", "CODING_TOOL_INTEGRATIONS", "codex", "--scope", "host", "--local", "--yes"}
		if stage == "preview" {
			arguments = append(arguments, "--check")
		}
		if code := runAliasRepairFixture(t, Options{RepositoryRoot: root, Environment: environment, Arguments: arguments, Stdout: &stdout, Stderr: &stderr}); code != 0 {
			t.Fatalf("%s failed: code=%d %s %s", stage, code, stdout.String(), stderr.String())
		}
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if stage == "preview" {
			if string(content) != before {
				t.Fatal("preview wrote invalid baseline")
			}
			if _, err := os.Lstat(filepath.Join(configHome, "migrations")); !os.IsNotExist(err) {
				t.Fatal("preview created release lock bookkeeping")
			}
		} else if strings.Contains(string(content), "AGENTS=") || !strings.Contains(string(content), "CODING_TOOL_INTEGRATIONS='codex'") || !strings.Contains(string(content), "SSH_PORT='2398'") {
			t.Fatal("repair changed unrelated records or retained alias")
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o640 {
			t.Fatal("repair changed exact permissions")
		}
	}
}

func TestConfigAliasRepairRejectsUnrelatedEditsAndDynamicChoice(t *testing.T) {
	root, _, home, environment := configCommandFixture(t)
	program := configExactProgram(t, root, environment, nil, nil)
	for _, content := range []string{"CODING_TOOL_INTEGRATIONS='claude'\n", "AGENTS=$SECRET_MARKER\n", "AGENTS=${SECRET_MARKER:-codex}\n"} {
		request, err := parseConfigAliasRepair([]string{"CODING_TOOL_INTEGRATIONS", "codex", "--scope", "host", "--local"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = program.aliasRepairContent(filepath.Join(home, "config.env"), []byte(content), request)
		if err == nil {
			t.Fatal("unrelated or dynamic repair accepted")
		}
		if strings.Contains((configProspectiveError{home, err}).Error(), "SECRET_MARKER") {
			t.Fatal("source value leaked")
		}
	}
	for _, arguments := range [][]string{{"SSH_PORT", "2399", "--scope", "host", "--local"}, {"YARD_IMAGE", "a", "--scope", "host"}, {"YARD_IMAGE", "a", "YARD_IMAGE", "b", "--scope", "host", "--local"}, {"YARD_IMAGE", "a", "--scope", "host", "--local", "--git"}} {
		if _, err := parseConfigAliasRepair(arguments); err == nil {
			t.Fatal("unsupported source ingress accepted")
		}
	}
}

func TestConfigValidateInvalidBaseAndNamedScopesIsSafeAndReadOnly(t *testing.T) {
	root, _, home, environment := configCommandFixture(t)
	testkit.WriteFile(t, filepath.Join(home, "config.env"), []byte("YARD_TEMPLATE='SECRET_MARKER'\n"), 0o600)
	writeConfigCommandFile(t, filepath.Join(home, "yards", "sample", "config.env"), "SSH_PORT=2399\n")
	var stdout, stderr bytes.Buffer
	if code := runAliasRepairFixture(t, Options{RepositoryRoot: root, Environment: environment, Arguments: []string{"config", "validate", "--all-local", "--json"}, Stdout: &stdout, Stderr: &stderr}); code != 1 {
		t.Fatalf("invalid scope validation code=%d", code)
	}
	if strings.Contains(stdout.String()+stderr.String(), "SECRET_MARKER") || !strings.Contains(stdout.String(), `"yard":"sample"`) || !strings.Contains(stdout.String(), `"yard":"default"`) || !strings.Contains(stdout.String(), `"retry":`) {
		t.Fatal("schema validation is unreachable or exposes a source value")
	}
	if _, err := os.Lstat(filepath.Join(home, "migrations")); !os.IsNotExist(err) {
		t.Fatal("schema validation mutated release state")
	}
}

func TestConfigValidateAliasConflictOffersExplicitRepairWithoutValues(t *testing.T) {
	root, _, home, environment := configCommandFixture(t)
	testkit.WriteFile(t, filepath.Join(home, "config.env"), []byte("YARD_PROFILES='SECRET_MARKER'\nENVIRONMENT_PROFILES=''\n"), 0o600)
	for _, arguments := range [][]string{{"config", "validate"}, {"config", "validate", "--json"}} {
		var stdout, stderr bytes.Buffer
		if runAliasRepairFixture(t, Options{RepositoryRoot: root, Environment: environment, Arguments: arguments, Stdout: &stdout, Stderr: &stderr}) != 1 {
			t.Fatal("alias conflict was not reported")
		}
		output := stdout.String() + stderr.String()
		if strings.Contains(output, "SECRET_MARKER") || !strings.Contains(output, "config repair-alias ENVIRONMENT_PROFILES CHOSEN_VALUE --scope host --local --check") {
			t.Fatalf("invalid source lacks safe reachable repair: %s", output)
		}
	}
}

func TestConfigAliasRepairRejectsForeignShippedOverride(t *testing.T) {
	_, home, configHome, environment := configCommandFixture(t)
	runtimeRoot := aliasRepairRuntimeFixture(t, home)
	root := filepath.Join(runtimeRoot, "releases", "1.2.3-owner")
	path := filepath.Join(configHome, "config.env")
	before := []byte("AGENTS='claude'\nCODING_TOOL_INTEGRATIONS='codex'\n")
	testkit.WriteFile(t, path, before, 0o600)
	environment = append(environment, "YARD_RUNTIME_ROOT="+runtimeRoot, "SUBYARD_CONFIG_DIR="+filepath.Join(home, "foreign-config"))
	if runAliasRepairFixture(t, Options{RepositoryRoot: root, Environment: environment, Arguments: []string{"config", "repair-alias", "CODING_TOOL_INTEGRATIONS", "codex", "--scope", "host", "--local", "--yes"}}) == 0 {
		t.Fatal("foreign shipped configuration admitted behind source gate")
	}
	if !bytes.Equal(before, mustReadConfigFixture(t, path)) {
		t.Fatal("rejected override changed source")
	}
}

func mustReadConfigFixture(t *testing.T, path string) []byte {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestConfigAliasRepairRejectsChangedInputsAfterConsent(t *testing.T) {
	for _, drift := range []string{"tooling mode", "pending authorization", "affected registration"} {
		t.Run(drift, func(t *testing.T) {
			_, home, configHome, environment := configCommandFixture(t)
			runtimeRoot := aliasRepairRuntimeFixture(t, home)
			root := filepath.Join(runtimeRoot, "releases", "1.2.3-owner")
			environment = append(environment, "YARD_RUNTIME_ROOT="+runtimeRoot, "SUBYARD_CONFIG_DIR="+filepath.Join(root, "config"))
			path := filepath.Join(configHome, "config.env")
			before := "AGENTS='claude'\nCODING_TOOL_INTEGRATIONS='codex'\n"
			testkit.WriteFile(t, path, []byte(before), 0o600)
			prompt := &callbackPrompt{callback: func() {
				switch drift {
				case "tooling mode":
					if err := os.Chmod(filepath.Join(root, "config", "commands.registry"), 0o640); err != nil {
						t.Fatal(err)
					}
				case "pending authorization":
					writeConfigCommandFile(t, filepath.Join(configHome, "release-transition", "v2", ".journal.json.pending"), "{}\n")
				case "affected registration":
					writeConfigCommandFile(t, filepath.Join(configHome, "yards", "neighbor", "config.env"), "YARD_TEMPLATE='SECRET_MARKER'\n")
				}
			}}
			var stdout, stderr bytes.Buffer
			code := runAliasRepairFixture(t, Options{RepositoryRoot: root, Environment: environment, Arguments: []string{"config", "repair-alias", "CODING_TOOL_INTEGRATIONS", "codex", "--scope", "host", "--local"}, Prompt: prompt, Stdout: &stdout, Stderr: &stderr})
			if code != 1 || len(prompt.requests) != 1 || !strings.Contains(stderr.String(), "stale") {
				t.Fatalf("changed dependency accepted: code=%d %s", code, stderr.String())
			}
			content, err := os.ReadFile(path)
			if err != nil || string(content) != before {
				t.Fatal("stale repair changed target")
			}
			if strings.Contains(stdout.String()+stderr.String(), "SECRET_MARKER") {
				t.Fatal("changed source value exposed")
			}
		})
	}
}

func TestConfigAliasRepairRejectsUnsealedToolingAndSelectors(t *testing.T) {
	root, home, configHome, environment := configCommandFixture(t)
	runtimeRoot := aliasRepairRuntimeFixture(t, home)
	environment = append(environment, "YARD_RUNTIME_ROOT="+runtimeRoot)
	path := filepath.Join(configHome, "config.env")
	before := "AGENTS='claude'\nCODING_TOOL_INTEGRATIONS='codex'\n"
	testkit.WriteFile(t, path, []byte(before), 0o600)
	for _, selector := range []string{"default", "owner/default"} {
		var stdout, stderr bytes.Buffer
		code := runAliasRepairFixture(t, Options{RepositoryRoot: root, Environment: environment, Arguments: []string{"-Y", selector, "config", "repair-alias", "CODING_TOOL_INTEGRATIONS", "codex", "--scope", "host", "--local", "--yes"}, Stdout: &stdout, Stderr: &stderr})
		if code == 0 {
			t.Fatal("unsealed or non-local repair tooling admitted")
		}
		content, err := os.ReadFile(path)
		if err != nil || string(content) != before {
			t.Fatal("rejected source repair wrote target")
		}
	}
}
