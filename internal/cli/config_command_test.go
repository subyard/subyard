package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestConfigPathsShowsEffectiveLayersWithoutValues(t *testing.T) {
	root, home, configHome, environment := configCommandFixture(t)
	hostRule := filepath.Join(configHome, "overrides", "host", "agents", "codex", "rules", "repo.rules")
	writeConfigCommandFile(t, hostRule, "private-canary-value\n")
	writeConfigCommandFile(t, filepath.Join(configHome, "config.env"),
		"AGENT_codex_RULES=\"$SUBYARD_CONFIG_DIR/../private/agents/codex/rules/repo.rules\"\n", 0o600)
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"config", "paths"},
		Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("config paths failed: code=%d stderr=%s", code, stderr.String())
	}
	output := stdout.String()
	for _, expected := range []string{
		"shipped-defaults: " + filepath.Join(root, "config"),
		"configuration-root: " + configHome,
		"source host-scalar-settings: " + filepath.Join(configHome, "config.env") + " (present)",
		"file-setting codex.rules: " + hostRule + " (scope=host, role=file settings, consumer=",
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("config paths omitted %q:\n%s", expected, output)
		}
	}
	if strings.Contains(output, "private-canary-value") || strings.Contains(output, home+"/.subyard/operator-overlay") {
		t.Fatalf("config paths leaked a value or legacy source:\n%s", output)
	}
}

func TestConfigShowExplainsScalarDerivedAndFileSettingsWithoutSecrets(t *testing.T) {
	root, _, configHome, environment := configCommandFixture(t)
	hostRule := filepath.Join(
		configHome, "overrides", "host", "agents", "codex", "rules", "repo.rules",
	)
	writeConfigCommandFile(t, hostRule, "secret-file-contents\n")
	writeConfigCommandFile(t, filepath.Join(configHome, "config.env"),
		"SSH_PORT=2200\n")
	yardFile := filepath.Join(configHome, "yards", "named", "config.env")
	writeConfigCommandFile(t, yardFile, "SSH_PORT=2300\n")
	environment = append(environment, "SSH_PORT=2400", "SECRET_TOKEN=s3cr3t-command-value")

	for _, test := range []struct {
		name      string
		arguments []string
		contains  []string
	}{
		{
			name: "summary", arguments: []string{"-Y", "named", "config", "show"},
			contains: []string{
				"SETTING", "SSH_PORT", "2400", "command", "environment", "next command",
				"AGENT_codex_RULES", hostRule, "config apply",
			},
		},
		{
			name:      "scalar precedence",
			arguments: []string{"-Y", "named", "config", "show", "SSH_PORT"},
			contains: []string{
				"setting: SSH_PORT", "effective: 2400", "shipped defaults",
				filepath.Join(configHome, "config.env") + ":1", yardFile + ":1",
				"environment", "overridden", "effective",
			},
		},
		{
			name:      "derived setting",
			arguments: []string{"-Y", "named", "config", "show", "YARD_INSTANCE_NAME"},
			contains: []string{
				"setting: YARD_INSTANCE_NAME", "effective: yard-named", "derived from yard name",
			},
		},
		{
			name:      "file setting",
			arguments: []string{"-Y", "named", "config", "show", "AGENT_codex_RULES"},
			contains: []string{
				"setting: AGENT_codex_RULES", hostRule, "file settings",
				"effective file source",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			program, err := New(Options{
				RepositoryRoot: root, Program: "yard", Arguments: test.arguments,
				Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr,
			})
			if err != nil {
				t.Fatal(err)
			}
			if code := program.Run(context.Background()); code != 0 {
				t.Fatalf("config show failed: code=%d stderr=%s", code, stderr.String())
			}
			output := stdout.String()
			for _, expected := range test.contains {
				if !strings.Contains(output, expected) {
					t.Fatalf("config show omitted %q:\n%s", expected, output)
				}
			}
			for _, secret := range []string{
				"s3cr3t-host-value", "s3cr3t-command-value", "secret-file-contents",
			} {
				if strings.Contains(output+stderr.String(), secret) {
					t.Fatalf("config show leaked %q:\n%s%s", secret, output, stderr.String())
				}
			}
		})
	}

	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"-Y", "named", "config", "show", "SECRET_TOKEN"},
		Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 2 ||
		!strings.Contains(stderr.String(), `unknown setting "SECRET_TOKEN"`) {
		t.Fatalf("unknown setting diagnostic: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	for _, secret := range []string{"s3cr3t-host-value", "s3cr3t-command-value"} {
		if strings.Contains(stdout.String()+stderr.String(), secret) {
			t.Fatalf("unknown setting diagnostic leaked %q", secret)
		}
	}
}

func TestConfigStatusAndApplyAllLocalExcludeRemoteYards(t *testing.T) {
	root, _, configHome, environment := configCommandFixture(t)
	environment = append(environment,
		"SUBYARD_ENGINE_CONTEXT=1", "SUBYARD_CONFIG_LOADED=1", "ACCESS_KIND=local")
	writeConfigCommandFile(t, filepath.Join(configHome, "yards", "named", "config.env"),
		"SSH_PORT=3333\n")
	writeConfigCommandFile(t, filepath.Join(configHome, "yards", "remote", "config.env"),
		"ACCESS_KIND=remote\nREMOTE_DEST=owner.example\nREMOTE_YARD=inner\nSSH_PORT=4444\n")

	defaultLoaded := loadConfigCommandContext(t, root, environment, "default")
	namedLoaded := loadConfigCommandContext(t, root, environment, "named")
	fake := &testkit.Incus{Instances: map[string]ports.InstanceInfo{
		defaultLoaded.Context.IncusProject + "/" + defaultLoaded.Context.YardInstanceName: {
			Name: defaultLoaded.Context.YardInstanceName, Project: defaultLoaded.Context.IncusProject,
			Status: "Running",
		},
		namedLoaded.Context.IncusProject + "/" + namedLoaded.Context.YardInstanceName: {
			Name: namedLoaded.Context.YardInstanceName, Project: namedLoaded.Context.IncusProject,
			Status: "Running",
		},
	}}
	appendHashSteps(t, fake, defaultLoaded)
	appendHashSteps(t, fake, namedLoaded)
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"config", "status", "--all-local"},
		Environment: environment, WorkingDir: root,
		Incus: fake, Executor: fake, Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("config status failed: code=%d stderr=%s stdout=%s", code, stderr.String(), stdout.String())
	}
	if !strings.Contains(stdout.String(), "yard default materialized-config: converged") ||
		!strings.Contains(stdout.String(), "yard named materialized-config: converged") ||
		strings.Contains(stdout.String(), "yard remote materialized-config:") {
		t.Fatalf("all-local selection is wrong:\n%s", stdout.String())
	}

	fake.ExecSteps = nil
	appendHashSteps(t, fake, defaultLoaded)
	appendHashSteps(t, fake, namedLoaded)
	applier := &recordingConfigApplier{}
	stdout.Reset()
	stderr.Reset()
	program, err = New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"config", "apply", "--all-local", "--yes"},
		Environment: environment, WorkingDir: root,
		Incus: fake, Executor: fake, Config: applier, Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("config apply failed: code=%d stderr=%s stdout=%s", code, stderr.String(), stdout.String())
	}
	if len(applier.yards) != 0 {
		t.Fatalf("config apply refreshed converged yards %#v", applier.yards)
	}
}

func TestDispatcherConfigApplierSelectsDirectAndDriftAwareCommands(t *testing.T) {
	root := t.TempDir()
	capture := filepath.Join(root, "arguments")
	dispatcher := filepath.Join(root, "yard")
	writeConfigCommandFile(t, dispatcher, "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$CAPTURE\"\n", 0o700)
	for _, test := range []struct {
		name       string
		applyDrift bool
		want       string
	}{
		{name: "direct", want: "-Y\nnamed\ninit\n--configs\n--yes\n"},
		{name: "drift-aware", applyDrift: true, want: "-Y\nnamed\nconfig\napply\n--yes\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			applier := dispatcherConfigApplier{
				path: dispatcher, environment: map[string]string{"CAPTURE": capture},
				applyDrift: test.applyDrift,
			}
			if err := applier.ApplyConfig(context.Background(), "named"); err != nil {
				t.Fatal(err)
			}
			arguments, err := os.ReadFile(capture)
			if err != nil || string(arguments) != test.want {
				t.Fatalf("dispatcher arguments = %q, want %q, err=%v", arguments, test.want, err)
			}
		})
	}
}

func TestConfigStatusDetectsGuestDriftWithoutPrintingContents(t *testing.T) {
	root, _, _, environment := configCommandFixture(t)
	loaded := loadConfigCommandContext(t, root, environment, "default")
	fake := &testkit.Incus{Instances: map[string]ports.InstanceInfo{
		loaded.Context.IncusProject + "/" + loaded.Context.YardInstanceName: {
			Name: loaded.Context.YardInstanceName, Project: loaded.Context.IncusProject, Status: "Running",
		},
	}}
	appendMismatchedHashSteps(t, fake, loaded, "0")
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"config", "status"},
		Environment: environment, WorkingDir: root,
		Incus: fake, Executor: fake, Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 ||
		!strings.Contains(stderr.String(), "agent config drift") {
		t.Fatalf("drift was not detected: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), "permissions") {
		t.Fatal("status printed agent config contents")
	}
}

func TestConfigApplyRejectsUnsafeSettingsTreeBeforeMutation(t *testing.T) {
	root, _, configHome, environment := configCommandFixture(t)
	unsafe := filepath.Join(configHome, "overrides", "host", "unsafe.conf")
	writeConfigCommandFile(t, unsafe, "value\n")
	if err := os.Chmod(unsafe, 0o666); err != nil {
		t.Fatal(err)
	}
	applier := &recordingConfigApplier{}
	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"config", "apply", "--yes"},
		Environment: environment, WorkingDir: root,
		Config: applier, Stdout: &bytes.Buffer{}, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 ||
		!strings.Contains(stderr.String(), "group/world writable") {
		t.Fatalf("unsafe tree was not rejected: code=%d stderr=%s", code, stderr.String())
	}
	if len(applier.yards) != 0 {
		t.Fatalf("unsafe tree was applied to %#v", applier.yards)
	}
}

func TestConfigValidationAllowsReadableManagedFilesAndRejectsUnsafePermissions(t *testing.T) {
	for _, test := range []struct {
		name     string
		relative string
		mode     os.FileMode
		wantErr  string
	}{
		{name: "host config group readable", relative: "config.env", mode: 0o640},
		{name: "host config world readable", relative: "config.env", mode: 0o644},
		{name: "secret group readable", relative: "secrets/token", mode: 0o640},
		{name: "secret world readable", relative: "secrets/token", mode: 0o644},
		{name: "generated group readable", relative: "generated/codex/config", mode: 0o640},
		{name: "generated world readable", relative: "generated/codex/config", mode: 0o644},
		{name: "group writable", relative: "secrets/token", mode: 0o620, wantErr: "group/world writable"},
		{name: "world writable", relative: "generated/codex/config", mode: 0o602, wantErr: "group/world writable"},
		{name: "group readable and writable", relative: "config.env", mode: 0o660, wantErr: "group/world writable"},
		{name: "group and world writable", relative: "config.env", mode: 0o666, wantErr: "group/world writable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			configHome := filepath.Join(t.TempDir(), "config")
			path := filepath.Join(configHome, filepath.FromSlash(test.relative))
			writeConfigCommandFile(t, path, "value\n", test.mode)
			if err := os.Chmod(path, test.mode); err != nil {
				t.Fatal(err)
			}

			err := validateManagedConfigTree(configHome)
			if test.wantErr == "" && err != nil {
				t.Fatalf("mode %04o was rejected: %v", test.mode, err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("mode %04o error = %v, want %q", test.mode, err, test.wantErr)
			}
		})
	}
}

func TestConfigValidationKeepsOwnershipSymlinkAndTypeChecks(t *testing.T) {
	t.Run("foreign owner", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.env")
		writeConfigCommandFile(t, path, "value\n", 0o600)
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateConfigOwnerMode(path, info, false, uint32(os.Getuid()+1)); err == nil ||
			!strings.Contains(err.Error(), "not operator-owned") {
			t.Fatalf("foreign owner error = %v", err)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		configHome := filepath.Join(t.TempDir(), "config")
		target := filepath.Join(t.TempDir(), "target")
		writeConfigCommandFile(t, target, "value\n", 0o600)
		if err := os.MkdirAll(configHome, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(configHome, "config.env")); err != nil {
			t.Fatal(err)
		}
		if err := validateManagedConfigTree(configHome); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("symlink error = %v", err)
		}
	})

	t.Run("non regular file", func(t *testing.T) {
		configHome := filepath.Join(t.TempDir(), "config")
		path := filepath.Join(configHome, "config.env")
		if err := os.MkdirAll(configHome, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := validateManagedConfigTree(configHome); err == nil ||
			!strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("non-regular error = %v", err)
		}
	})
}

func TestConfigValidationExcludesRuntimeStateTrees(t *testing.T) {
	_, _, configHome, _ := configCommandFixture(t)
	for _, path := range []string{
		filepath.Join(configHome, "keys", "ledger.lock"),
		filepath.Join(configHome, "projects", "default.json"),
		filepath.Join(configHome, "tools", "bin", "sops"),
	} {
		writeConfigCommandFile(t, path, "runtime state\n")
		if err := os.Chmod(path, 0o666); err != nil {
			t.Fatal(err)
		}
	}
	if err := validateManagedConfigTree(configHome); err != nil {
		t.Fatalf("runtime state was treated as managed settings: %v", err)
	}
}

func TestConfigAuthoringNoOpsDoNotPromptOrRewriteTargets(t *testing.T) {
	for _, test := range []struct {
		name      string
		initial   string
		arguments []string
	}{
		{
			name: "equal set", initial: "SSH_PORT='2299'\n",
			arguments: []string{"config", "set", "SSH_PORT", "2299", "--scope", "host"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, _, configHome, environment := configCommandFixture(t)
			target := filepath.Join(configHome, "config.env")
			writeConfigCommandFile(t, target, test.initial)
			before, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			prompt := &testkit.Prompt{}
			var stdout, stderr bytes.Buffer
			program, err := New(Options{
				RepositoryRoot: root, Program: "yard", Arguments: test.arguments,
				Environment: environment, WorkingDir: root, Prompt: prompt,
				Stdout: &stdout, Stderr: &stderr,
			})
			if err != nil {
				t.Fatal(err)
			}
			if code := program.Run(context.Background()); code != 0 {
				t.Fatalf("command failed: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			after, err := os.ReadFile(target)
			if err != nil || !bytes.Equal(after, before) {
				t.Fatalf("target changed: before=%q after=%q err=%v", before, after, err)
			}
			if len(prompt.Requests) != 0 {
				t.Fatalf("no-op prompted: %#v", prompt.Requests)
			}
		})
	}
}

func TestConfigUnsetAbsentTargetDoesNotPrompt(t *testing.T) {
	root, _, configHome, environment := configCommandFixture(t)
	target := filepath.Join(configHome, "overrides", "shared", "config.env")
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	prompt := &testkit.Prompt{}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"config", "unset", "E2E_VM_CPU", "--scope", "shared"},
		Environment: environment, WorkingDir: root, Prompt: prompt,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("absent unset failed: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("absent scalar target was created: %v", err)
	}
	if len(prompt.Requests) != 0 {
		t.Fatalf("absent unset prompted: %#v", prompt.Requests)
	}
}

func TestConfigImportUnchangedDoesNotPromptOrRewriteTarget(t *testing.T) {
	root, _, configHome, environment := configCommandFixture(t)
	content := "model = \"fixture\"\n"
	source := filepath.Join(t.TempDir(), "config.toml")
	writeConfigCommandFile(t, source, content)
	target := filepath.Join(configHome, "overrides", "host", "agents", "codex", "config.toml")
	writeConfigCommandFile(t, target, content)
	before, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	prompt := &testkit.Prompt{}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments: []string{
			"config", "import", "AGENT_codex_CONFIG", source, "--scope", "host",
		},
		Environment: environment, WorkingDir: root, Prompt: prompt,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("import failed: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	after, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(after, before) {
		t.Fatalf("target changed: before=%q after=%q err=%v", before, after, err)
	}
	if len(prompt.Requests) != 0 {
		t.Fatalf("unchanged import prompted: %#v", prompt.Requests)
	}
}

func TestConfigImportEmptyContentCreatesMissingTarget(t *testing.T) {
	root, _, configHome, environment := configCommandFixture(t)
	source := filepath.Join(t.TempDir(), "config.toml")
	writeConfigCommandFile(t, source, "")
	target := filepath.Join(configHome, "overrides", "host", "agents", "codex", "config.toml")
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	prompt := &testkit.Prompt{Answers: []bool{true}}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments: []string{
			"config", "import", "AGENT_codex_CONFIG", source, "--scope", "host",
		},
		Environment: environment, WorkingDir: root, Prompt: prompt,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("empty import failed: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if got, err := os.ReadFile(target); err != nil || len(got) != 0 {
		t.Fatalf("empty target = %q err=%v", got, err)
	}
	if len(prompt.Requests) != 1 {
		t.Fatalf("empty import prompts = %#v", prompt.Requests)
	}
}

func TestConfigEditUnchangedDoesNotPromptAfterEditingDraft(t *testing.T) {
	root, _, configHome, environment := configCommandFixture(t)
	content := "model = \"fixture\"\n"
	target := filepath.Join(configHome, "overrides", "host", "agents", "codex", "config.toml")
	writeConfigCommandFile(t, target, content)
	marker := filepath.Join(t.TempDir(), "editor-ran")
	editor := writeConfigAuthoringEditor(t, ": >\"$EDITOR_MARKER\"\n")
	environment = append(environment, "EDITOR="+editor, "EDITOR_MARKER="+marker)
	prompt := &testkit.Prompt{}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"config", "edit", "AGENT_codex_CONFIG", "--scope", "host"},
		Environment: environment, WorkingDir: root, Prompt: prompt,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("edit failed: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("editor did not run before planning: %v", err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != content {
		t.Fatalf("target changed: %q err=%v", got, err)
	}
	if len(prompt.Requests) != 0 {
		t.Fatalf("unchanged edit prompted: %#v", prompt.Requests)
	}
}

func TestConfigEditChangedDeclineLeavesPersistentTargetUntouched(t *testing.T) {
	root, _, configHome, environment := configCommandFixture(t)
	target := filepath.Join(configHome, "overrides", "host", "agents", "codex", "config.toml")
	writeConfigCommandFile(t, target, "model = \"before\"\n")
	marker := filepath.Join(t.TempDir(), "editor-ran")
	editor := writeConfigAuthoringEditor(t, "printf '%s\\n' 'model = \\\"after\\\"' >\"$1\"\n: >\"$EDITOR_MARKER\"\n")
	environment = append(environment, "EDITOR="+editor, "EDITOR_MARKER="+marker)
	prompt := &testkit.Prompt{Answers: []bool{false}}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"config", "edit", "AGENT_codex_CONFIG", "--scope", "host"},
		Environment: environment, WorkingDir: root, Prompt: prompt,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 || !strings.Contains(stderr.String(), "operation declined") {
		t.Fatalf("declined edit: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("editor did not run before confirmation: %v", err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "model = \"before\"\n" {
		t.Fatalf("declined edit changed target: %q err=%v", got, err)
	}
	if len(prompt.Requests) != 1 || prompt.Requests[0].Default != domain.ConfirmationDefaultYes {
		t.Fatalf("edit prompt = %#v", prompt.Requests)
	}
}

func TestConfigEditInvalidDraftFailsBeforePrompt(t *testing.T) {
	root, _, configHome, environment := configCommandFixture(t)
	target := filepath.Join(configHome, "overrides", "host", "agents", "codex", "config.toml")
	writeConfigCommandFile(t, target, "model = \"before\"\n")
	editor := writeConfigAuthoringEditor(t, "printf '%s\\n' 'password=secret' >\"$1\"\n")
	environment = append(environment, "EDITOR="+editor)
	prompt := &testkit.Prompt{}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"config", "edit", "AGENT_codex_CONFIG", "--scope", "host"},
		Environment: environment, WorkingDir: root, Prompt: prompt,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 || !strings.Contains(stderr.String(), "secret material") {
		t.Fatalf("invalid edit: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "model = \"before\"\n" {
		t.Fatalf("invalid draft changed target: %q err=%v", got, err)
	}
	if len(prompt.Requests) != 0 {
		t.Fatalf("invalid draft prompted: %#v", prompt.Requests)
	}
}

func TestConfigSetChangedUsesOneDefaultYesPrompt(t *testing.T) {
	root, _, configHome, environment := configCommandFixture(t)
	target := filepath.Join(configHome, "config.env")
	writeConfigCommandFile(t, target, "SSH_PORT='2299'\n")
	prompt := &testkit.Prompt{Answers: []bool{true}}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"config", "set", "SSH_PORT", "2300", "--scope", "host"},
		Environment: environment, WorkingDir: root, Prompt: prompt,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("changed set: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "SSH_PORT='2300'\n" {
		t.Fatalf("target = %q err=%v", got, err)
	}
	if len(prompt.Requests) != 1 || prompt.Requests[0].Summary != "Set persistent configuration" ||
		prompt.Requests[0].Default != domain.ConfirmationDefaultYes {
		t.Fatalf("set prompt = %#v", prompt.Requests)
	}
}

func TestConfigUnsetExistingEmptyTargetRemovesIt(t *testing.T) {
	root, _, configHome, environment := configCommandFixture(t)
	target := filepath.Join(configHome, "config.env")
	writeConfigCommandFile(t, target, "")
	prompt := &testkit.Prompt{Answers: []bool{true}}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"config", "unset", "SSH_PORT", "--scope", "host"},
		Environment: environment, WorkingDir: root, Prompt: prompt,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("empty unset failed: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty scalar target still exists: %v", err)
	}
	if len(prompt.Requests) != 1 {
		t.Fatalf("empty unset prompts = %#v", prompt.Requests)
	}
}

func TestConfigSetRejectsConcurrentTargetChangeAfterConfirmation(t *testing.T) {
	root, _, configHome, environment := configCommandFixture(t)
	target := filepath.Join(configHome, "config.env")
	writeConfigCommandFile(t, target, "SSH_PORT='2299'\n")
	prompt := &callbackPrompt{callback: func() {
		writeConfigCommandFile(t, target, "SSH_PORT='2400'\n# concurrent\n")
	}}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"config", "set", "SSH_PORT", "2300", "--scope", "host"},
		Environment: environment, WorkingDir: root, Prompt: prompt,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 ||
		!strings.Contains(stderr.String(), domain.ErrPlanStale.Error()) {
		t.Fatalf("stale set: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if got, err := os.ReadFile(target); err != nil ||
		string(got) != "SSH_PORT='2400'\n# concurrent\n" {
		t.Fatalf("concurrent target = %q err=%v", got, err)
	}
}

func TestConfigSetRejectsMissingTargetCreatedAfterConfirmation(t *testing.T) {
	root, _, configHome, environment := configCommandFixture(t)
	target := filepath.Join(configHome, "overrides", "shared", "config.env")
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	prompt := &callbackPrompt{callback: func() {
		writeConfigCommandFile(t, target, "E2E_VM_CPU=6\n")
	}}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"config", "set", "E2E_VM_CPU", "3", "--scope", "shared"},
		Environment: environment, WorkingDir: root, Prompt: prompt,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 ||
		!strings.Contains(stderr.String(), domain.ErrPlanStale.Error()) {
		t.Fatalf("stale create: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "E2E_VM_CPU=6\n" {
		t.Fatalf("concurrent target = %q err=%v", got, err)
	}
}

func TestConfigImportRejectsTargetRemovedAfterConfirmation(t *testing.T) {
	root, _, configHome, environment := configCommandFixture(t)
	target := filepath.Join(configHome, "overrides", "host", "agents", "codex", "config.toml")
	writeConfigCommandFile(t, target, "model = \"before\"\n")
	source := filepath.Join(t.TempDir(), "config.toml")
	writeConfigCommandFile(t, source, "model = \"after\"\n")
	prompt := &callbackPrompt{callback: func() {
		if err := os.Remove(target); err != nil {
			t.Fatal(err)
		}
	}}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments: []string{
			"config", "import", "AGENT_codex_CONFIG", source, "--scope", "host",
		},
		Environment: environment, WorkingDir: root, Prompt: prompt,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 ||
		!strings.Contains(stderr.String(), domain.ErrPlanStale.Error()) {
		t.Fatalf("stale import: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed target was recreated: %v", err)
	}
}

func TestConfigAuthoringNoOpWithAssumeYesStillSkipsWriterSafetyChecks(t *testing.T) {
	root, _, configHome, environment := configCommandFixture(t)
	target := filepath.Join(configHome, "config.env")
	writeConfigCommandFile(t, target, "SSH_PORT='2299'\n")
	linked := filepath.Join(t.TempDir(), "linked-config.env")
	if err := os.Link(target, linked); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"config", "set", "SSH_PORT", "2299", "--scope", "host", "--yes"},
		Environment: environment, WorkingDir: root,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("assume-yes no-op: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
}

func TestConfigApplyConvergedSkipsPromptAndApplier(t *testing.T) {
	root, _, _, environment := configCommandFixture(t)
	loaded := loadConfigCommandContext(t, root, environment, "default")
	fake := &testkit.Incus{Instances: map[string]ports.InstanceInfo{
		loaded.Context.IncusProject + "/" + loaded.Context.YardInstanceName: {
			Name: loaded.Context.YardInstanceName, Project: loaded.Context.IncusProject, Status: "Running",
		},
	}}
	appendHashSteps(t, fake, loaded)
	prompt := &testkit.Prompt{}
	applier := &recordingConfigApplier{}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"config", "apply"},
		Environment: environment, WorkingDir: root, Prompt: prompt, Incus: fake, Executor: fake,
		Config: applier, Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("converged apply: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if len(prompt.Requests) != 0 || len(applier.yards) != 0 {
		t.Fatalf("converged apply prompted=%#v applied=%#v", prompt.Requests, applier.yards)
	}
}

func TestConfigApplyAssessesDriftBeforeOneDefaultYesPrompt(t *testing.T) {
	root, _, configHome, environment := configCommandFixture(t)
	writeConfigCommandFile(t, filepath.Join(configHome, "yards", "named", "config.env"), "SSH_PORT=2299\n")
	writeConfigCommandFile(t, filepath.Join(configHome, "yards", "stopped", "config.env"), "SSH_PORT=2300\n")
	defaultLoaded := loadConfigCommandContext(t, root, environment, "default")
	namedLoaded := loadConfigCommandContext(t, root, environment, "named")
	stoppedLoaded := loadConfigCommandContext(t, root, environment, "stopped")
	fake := &testkit.Incus{Instances: map[string]ports.InstanceInfo{
		defaultLoaded.Context.IncusProject + "/" + defaultLoaded.Context.YardInstanceName: {
			Name: defaultLoaded.Context.YardInstanceName, Project: defaultLoaded.Context.IncusProject, Status: "Running",
		},
		namedLoaded.Context.IncusProject + "/" + namedLoaded.Context.YardInstanceName: {
			Name: namedLoaded.Context.YardInstanceName, Project: namedLoaded.Context.IncusProject, Status: "Running",
		},
		stoppedLoaded.Context.IncusProject + "/" + stoppedLoaded.Context.YardInstanceName: {
			Name: stoppedLoaded.Context.YardInstanceName, Project: stoppedLoaded.Context.IncusProject, Status: "Stopped",
		},
	}}
	appendMismatchedHashSteps(t, fake, defaultLoaded, "0")
	appendHashSteps(t, fake, namedLoaded)
	appendMismatchedHashSteps(t, fake, defaultLoaded, "0")
	appendHashSteps(t, fake, namedLoaded)
	appendHashSteps(t, fake, defaultLoaded)
	prompt := &testkit.Prompt{Answers: []bool{true}}
	applier := &recordingConfigApplier{}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"config", "apply", "--all-local"},
		Environment: environment, WorkingDir: root, Prompt: prompt, Incus: fake, Executor: fake,
		Config: applier, Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("mixed apply: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if strings.Join(applier.yards, ",") != "default" || len(prompt.Requests) != 1 ||
		prompt.Requests[0].Summary != "Apply Subyard file settings" || prompt.Requests[0].Default != domain.ConfirmationDefaultYes {
		t.Fatalf("mixed apply requests=%#v yards=%#v", prompt.Requests, applier.yards)
	}
}

func TestConfigApplyDeclineLeavesApplierUntouched(t *testing.T) {
	root, _, _, environment := configCommandFixture(t)
	loaded := loadConfigCommandContext(t, root, environment, "default")
	fake := &testkit.Incus{Instances: map[string]ports.InstanceInfo{
		loaded.Context.IncusProject + "/" + loaded.Context.YardInstanceName: {
			Name: loaded.Context.YardInstanceName, Project: loaded.Context.IncusProject, Status: "Running",
		},
	}}
	appendMismatchedHashSteps(t, fake, loaded, "0")
	prompt := &testkit.Prompt{Answers: []bool{false}}
	applier := &recordingConfigApplier{}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"config", "apply"},
		Environment: environment, WorkingDir: root, Prompt: prompt, Incus: fake, Executor: fake,
		Config: applier, Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 || !strings.Contains(stderr.String(), "operation declined") {
		t.Fatalf("declined apply: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if len(applier.yards) != 0 || len(prompt.Requests) != 1 || prompt.Requests[0].Default != domain.ConfirmationDefaultYes {
		t.Fatalf("declined apply requests=%#v yards=%#v", prompt.Requests, applier.yards)
	}
}

func TestConfigApplyRejectsMaterializedDriftChangeAfterConfirmation(t *testing.T) {
	root, _, _, environment := configCommandFixture(t)
	loaded := loadConfigCommandContext(t, root, environment, "default")
	fake := &testkit.Incus{Instances: map[string]ports.InstanceInfo{
		loaded.Context.IncusProject + "/" + loaded.Context.YardInstanceName: {
			Name: loaded.Context.YardInstanceName, Project: loaded.Context.IncusProject, Status: "Running",
		},
	}}
	appendMismatchedHashSteps(t, fake, loaded, "0")
	appendMismatchedHashSteps(t, fake, loaded, "1")
	applier := &recordingConfigApplier{}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"config", "apply"},
		Environment: environment, WorkingDir: root, Prompt: &callbackPrompt{},
		Incus: fake, Executor: fake, Config: applier, Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 ||
		!strings.Contains(stderr.String(), domain.ErrPlanStale.Error()) {
		t.Fatalf("stale materialized state: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if len(applier.yards) != 0 {
		t.Fatalf("stale apply reached applier: %#v", applier.yards)
	}
}

func TestConfigApplyRejectsDesiredChangeAfterConfirmation(t *testing.T) {
	root, _, configHome, environment := configCommandFixture(t)
	source := filepath.Join(configHome, "overrides", "host", "agents", "codex", "config.toml")
	writeConfigCommandFile(t, source, "model = \"before\"\n")
	writeConfigCommandFile(t, filepath.Join(configHome, "config.env"),
		"AGENTS=codex\n"+
			"AGENT_codex_CONFIG='"+source+"'\n"+
			"AGENT_codex_CONFIG_DEST='.codex/config.toml'\n")
	loaded := loadConfigCommandContext(t, root, environment, "default")
	fake := &testkit.Incus{Instances: map[string]ports.InstanceInfo{
		loaded.Context.IncusProject + "/" + loaded.Context.YardInstanceName: {
			Name: loaded.Context.YardInstanceName, Project: loaded.Context.IncusProject, Status: "Running",
		},
	}}
	appendMismatchedHashSteps(t, fake, loaded, "0")
	appendMismatchedHashSteps(t, fake, loaded, "0")
	prompt := &callbackPrompt{callback: func() {
		writeConfigCommandFile(t, source, "model = \"after\"\n")
	}}
	applier := &recordingConfigApplier{}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"config", "apply"},
		Environment: environment, WorkingDir: root, Prompt: prompt,
		Incus: fake, Executor: fake, Config: applier, Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 ||
		!strings.Contains(stderr.String(), domain.ErrPlanStale.Error()) {
		t.Fatalf("stale desired state: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if len(applier.yards) != 0 {
		t.Fatalf("stale apply reached applier: %#v", applier.yards)
	}
}

func TestConfigApplySkipsDriftThatConvergedAfterConfirmation(t *testing.T) {
	root, _, _, environment := configCommandFixture(t)
	loaded := loadConfigCommandContext(t, root, environment, "default")
	fake := &testkit.Incus{Instances: map[string]ports.InstanceInfo{
		loaded.Context.IncusProject + "/" + loaded.Context.YardInstanceName: {
			Name: loaded.Context.YardInstanceName, Project: loaded.Context.IncusProject, Status: "Running",
		},
	}}
	appendMismatchedHashSteps(t, fake, loaded, "0")
	appendHashSteps(t, fake, loaded)
	applier := &recordingConfigApplier{}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"config", "apply"},
		Environment: environment, WorkingDir: root, Prompt: &callbackPrompt{},
		Incus: fake, Executor: fake, Config: applier, Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("converged-after-consent: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if len(applier.yards) != 0 {
		t.Fatalf("converged target reached applier: %#v", applier.yards)
	}
}

func TestConfigApplySkipsDriftWhenTargetStopsOrDisappearsAfterConfirmation(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(map[string]ports.InstanceInfo, string)
		state  string
	}{
		{
			name: "stopped",
			mutate: func(instances map[string]ports.InstanceInfo, key string) {
				instance := instances[key]
				instance.Status = "Stopped"
				instances[key] = instance
			},
			state: "stopped",
		},
		{
			name: "absent",
			mutate: func(instances map[string]ports.InstanceInfo, key string) {
				delete(instances, key)
			},
			state: "absent",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, _, _, environment := configCommandFixture(t)
			loaded := loadConfigCommandContext(t, root, environment, "default")
			key := loaded.Context.IncusProject + "/" + loaded.Context.YardInstanceName
			fake := &testkit.Incus{Instances: map[string]ports.InstanceInfo{
				key: {
					Name:    loaded.Context.YardInstanceName,
					Project: loaded.Context.IncusProject,
					Status:  "Running",
				},
			}}
			appendMismatchedHashSteps(t, fake, loaded, "0")
			prompt := &callbackPrompt{callback: func() {
				test.mutate(fake.Instances, key)
			}}
			applier := &recordingConfigApplier{}
			var stdout, stderr bytes.Buffer
			program, err := New(Options{
				RepositoryRoot: root, Program: "yard", Arguments: []string{"config", "apply"},
				Environment: environment, WorkingDir: root, Prompt: prompt,
				Incus: fake, Executor: fake, Config: applier,
				Stdout: &stdout, Stderr: &stderr,
			})
			if err != nil {
				t.Fatal(err)
			}
			if code := program.Run(context.Background()); code != 0 {
				t.Fatalf("%s after consent: code=%d stdout=%s stderr=%s",
					test.name, code, stdout.String(), stderr.String())
			}
			if len(applier.yards) != 0 || len(prompt.requests) != 1 ||
				!strings.Contains(stdout.String(), "materialized-config: "+test.state+" after confirmation; skipped") {
				t.Fatalf("%s after consent: prompts=%#v yards=%#v stdout=%s stderr=%s",
					test.name, prompt.requests, applier.yards, stdout.String(), stderr.String())
			}
		})
	}
}

func TestConfigApplyRejectsDesiredChangeForStoppedTargetInConfirmedSet(t *testing.T) {
	root, _, configHome, environment := configCommandFixture(t)
	stoppedSource := filepath.Join(configHome, "overrides", "stopped-codex.toml")
	writeConfigCommandFile(t, stoppedSource, "model = \"before\"\n")
	writeConfigCommandFile(t, filepath.Join(configHome, "yards", "stopped", "config.env"),
		"SSH_PORT=2300\n"+
			"CODING_TOOL_INTEGRATIONS=codex\n"+
			"AGENT_codex_CONFIG='"+stoppedSource+"'\n"+
			"AGENT_codex_CONFIG_DEST='.codex/config.toml'\n")
	defaultLoaded := loadConfigCommandContext(t, root, environment, "default")
	stoppedLoaded := loadConfigCommandContext(t, root, environment, "stopped")
	fake := &testkit.Incus{Instances: map[string]ports.InstanceInfo{
		defaultLoaded.Context.IncusProject + "/" + defaultLoaded.Context.YardInstanceName: {
			Name:    defaultLoaded.Context.YardInstanceName,
			Project: defaultLoaded.Context.IncusProject,
			Status:  "Running",
		},
		stoppedLoaded.Context.IncusProject + "/" + stoppedLoaded.Context.YardInstanceName: {
			Name:    stoppedLoaded.Context.YardInstanceName,
			Project: stoppedLoaded.Context.IncusProject,
			Status:  "Stopped",
		},
	}}
	appendMismatchedHashSteps(t, fake, defaultLoaded, "0")
	appendMismatchedHashSteps(t, fake, defaultLoaded, "0")
	appendHashSteps(t, fake, defaultLoaded)
	prompt := &callbackPrompt{callback: func() {
		writeConfigCommandFile(t, stoppedSource, "model = \"after\"\n")
	}}
	applier := &recordingConfigApplier{}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"config", "apply", "--all-local"},
		Environment: environment, WorkingDir: root, Prompt: prompt,
		Incus: fake, Executor: fake, Config: applier,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 ||
		!strings.Contains(stderr.String(), domain.ErrPlanStale.Error()) ||
		!strings.Contains(stderr.String(), "yard stopped") {
		t.Fatalf("stopped desired change: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	if len(applier.yards) != 0 {
		t.Fatalf("stale stopped target reached applier: %#v", applier.yards)
	}
}

func TestConfigApplyRejectsDesiredChangeThatConvergedAfterConfirmation(t *testing.T) {
	root, _, configHome, environment := configCommandFixture(t)
	source := filepath.Join(configHome, "overrides", "host", "agents", "codex", "config.toml")
	after := "model = \"after\"\n"
	writeConfigCommandFile(t, source, "model = \"before\"\n")
	writeConfigCommandFile(t, filepath.Join(configHome, "config.env"),
		"AGENTS=codex\n"+
			"AGENT_codex_CONFIG='"+source+"'\n"+
			"AGENT_codex_CONFIG_DEST='.codex/config.toml'\n")
	loaded := loadConfigCommandContext(t, root, environment, "default")
	fake := &testkit.Incus{Instances: map[string]ports.InstanceInfo{
		loaded.Context.IncusProject + "/" + loaded.Context.YardInstanceName: {
			Name: loaded.Context.YardInstanceName, Project: loaded.Context.IncusProject, Status: "Running",
		},
	}}
	appendMismatchedHashSteps(t, fake, loaded, "0")
	assets, err := effectiveConfigAssets(loaded)
	if err != nil {
		t.Fatal(err)
	}
	for _, asset := range assets {
		var hash string
		if asset.Source == source {
			digest := sha256.Sum256([]byte(after))
			hash = fmt.Sprintf("%x", digest)
		} else {
			hash, err = hashRegularFile(asset.Source)
			if err != nil {
				t.Fatal(err)
			}
		}
		fake.ExecSteps = append(fake.ExecSteps, testkit.IncusExecStep{
			Result: ports.InstanceExecResult{
				Stdout: []byte(fmt.Sprintf("%s  %s\n", hash, asset.Destination)), ExitCode: 0,
			},
		})
	}
	prompt := &callbackPrompt{callback: func() {
		writeConfigCommandFile(t, source, after)
	}}
	applier := &recordingConfigApplier{}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"config", "apply"},
		Environment: environment, WorkingDir: root, Prompt: prompt,
		Incus: fake, Executor: fake, Config: applier, Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 ||
		!strings.Contains(stderr.String(), domain.ErrPlanStale.Error()) {
		t.Fatalf("changed desired converged: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if len(applier.yards) != 0 {
		t.Fatalf("stale apply reached applier: %#v", applier.yards)
	}
}

func TestConfigApplyRejectsExpandedAllLocalSelectionAfterConfirmation(t *testing.T) {
	root, _, configHome, environment := configCommandFixture(t)
	loaded := loadConfigCommandContext(t, root, environment, "default")
	fake := &testkit.Incus{Instances: map[string]ports.InstanceInfo{
		loaded.Context.IncusProject + "/" + loaded.Context.YardInstanceName: {
			Name: loaded.Context.YardInstanceName, Project: loaded.Context.IncusProject, Status: "Running",
		},
	}}
	appendMismatchedHashSteps(t, fake, loaded, "0")
	prompt := &callbackPrompt{callback: func() {
		writeConfigCommandFile(t, filepath.Join(configHome, "yards", "named", "config.env"),
			"SSH_PORT=2299\n")
	}}
	applier := &recordingConfigApplier{}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"config", "apply", "--all-local"},
		Environment: environment, WorkingDir: root, Prompt: prompt,
		Incus: fake, Executor: fake, Config: applier, Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 ||
		!strings.Contains(stderr.String(), domain.ErrPlanStale.Error()) {
		t.Fatalf("expanded selection: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if len(applier.yards) != 0 {
		t.Fatalf("expanded apply reached applier: %#v", applier.yards)
	}
}

func TestConfigApplyExecErrorFailsPreflightWithAssumeYes(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "transport", err: errors.New("transport disconnected")},
		{name: "cancellation", err: context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, _, _, environment := configCommandFixture(t)
			loaded := loadConfigCommandContext(t, root, environment, "default")
			fake := &testkit.Incus{
				Instances: map[string]ports.InstanceInfo{
					loaded.Context.IncusProject + "/" + loaded.Context.YardInstanceName: {
						Name: loaded.Context.YardInstanceName, Project: loaded.Context.IncusProject,
						Status: "Running",
					},
				},
				ExecSteps: []testkit.IncusExecStep{{Err: test.err}},
			}
			prompt := &testkit.Prompt{}
			applier := &recordingConfigApplier{}
			var stdout, stderr bytes.Buffer
			program, err := New(Options{
				RepositoryRoot: root, Program: "yard",
				Arguments:   []string{"config", "apply", "--yes"},
				Environment: environment, WorkingDir: root, Prompt: prompt,
				Incus: fake, Executor: fake, Config: applier,
				Stdout: &stdout, Stderr: &stderr,
			})
			if err != nil {
				t.Fatal(err)
			}
			if code := program.Run(context.Background()); code != 1 ||
				!strings.Contains(stderr.String(), test.err.Error()) {
				t.Fatalf("exec error: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			if len(prompt.Requests) != 0 || len(applier.yards) != 0 {
				t.Fatalf("exec error prompted=%#v applied=%#v", prompt.Requests, applier.yards)
			}
		})
	}
}

func TestConfigApplyCompletedNonzeroProbeRemainsDrift(t *testing.T) {
	root, _, _, environment := configCommandFixture(t)
	loaded := loadConfigCommandContext(t, root, environment, "default")
	fake := &testkit.Incus{
		Instances: map[string]ports.InstanceInfo{
			loaded.Context.IncusProject + "/" + loaded.Context.YardInstanceName: {
				Name: loaded.Context.YardInstanceName, Project: loaded.Context.IncusProject,
				Status: "Running",
			},
		},
	}
	appendUnavailableHashSteps(t, fake, loaded)
	appendUnavailableHashSteps(t, fake, loaded)
	appendHashSteps(t, fake, loaded)
	prompt := &testkit.Prompt{}
	applier := &recordingConfigApplier{}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"config", "apply", "--yes"},
		Environment: environment, WorkingDir: root, Prompt: prompt,
		Incus: fake, Executor: fake, Config: applier,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("completed nonzero probe: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if len(prompt.Requests) != 0 || strings.Join(applier.yards, ",") != "default" {
		t.Fatalf("completed nonzero prompted=%#v applied=%#v", prompt.Requests, applier.yards)
	}
}

func TestConfigSyncCheckIsReadOnlyAndMutationPromptsOnce(t *testing.T) {
	root, _, configHome, environment := configCommandFixture(t)
	environment = append(environment, "SUBYARD_HOST_ID=owner-a")
	source := filepath.Join(t.TempDir(), "source")
	if err := os.MkdirAll(filepath.Join(source, "hosts", "owner-a"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeConfigCommandFile(t, filepath.Join(source, "subyard-config.json"),
		"{\n  \"schemaVersion\": 1\n}\n")
	writeConfigCommandFile(t, filepath.Join(source, "hosts", "owner-a", "config.env"),
		"SSH_PORT=2290\n")
	runConfigSyncGit(t, source, "init", "-q")
	runConfigSyncGit(t, source, "add", "-A")
	runConfigSyncGit(t, source,
		"-c", "user.name=Subyard Test", "-c", "user.email=test@invalid",
		"commit", "-q", "-m", "initial")

	checkPrompt := &testkit.Prompt{}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"config", "sync", source, "--check", "--adopt"},
		Environment: environment, WorkingDir: root, Prompt: checkPrompt,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 ||
		!strings.Contains(stdout.String(), "changes required") {
		t.Fatalf("config sync check: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	if len(checkPrompt.Seen) != 0 {
		t.Fatalf("read-only check prompted: %#v", checkPrompt.Seen)
	}
	content, err := os.ReadFile(filepath.Join(configHome, "config.env"))
	if err != nil || len(content) != 0 {
		t.Fatalf("read-only check changed live config: %q %v", content, err)
	}
	if _, err := os.Lstat(filepath.Join(configHome, ".sync", "manifest.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only check wrote a manifest: %v", err)
	}

	applyPrompt := &testkit.Prompt{Answers: []bool{true}}
	stdout.Reset()
	stderr.Reset()
	program, err = New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"config", "sync", source, "--adopt"},
		Environment: environment, WorkingDir: root, Prompt: applyPrompt,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("config sync apply: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	if len(applyPrompt.Requests) != 1 ||
		applyPrompt.Requests[0].Summary != "Synchronize persistent configuration" ||
		applyPrompt.Requests[0].Default != domain.ConfirmationDefaultYes {
		t.Fatalf("config sync requests=%#v", applyPrompt.Requests)
	}
	content, err = os.ReadFile(filepath.Join(configHome, "config.env"))
	if err != nil || string(content) != "SSH_PORT=2290\n" {
		t.Fatalf("config sync did not apply host settings: %q %v", content, err)
	}

	convergedPrompt := &testkit.Prompt{}
	stdout.Reset()
	stderr.Reset()
	program, err = New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"config", "sync", source},
		Environment: environment, WorkingDir: root, Prompt: convergedPrompt,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 ||
		!strings.Contains(stdout.String(), "already converged") {
		t.Fatalf("converged config sync: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	if len(convergedPrompt.Seen) != 0 {
		t.Fatalf("converged sync prompted: %#v", convergedPrompt.Seen)
	}

	manifestBefore, err := os.ReadFile(configsync.ManifestPath(configHome))
	if err != nil {
		t.Fatal(err)
	}
	writeConfigCommandFile(t, filepath.Join(source, "subyard-config.json"),
		"{\"schemaVersion\":1}\n")
	commitConfigSource(t, source, "reformat source manifest")
	manifestOnlyPrompt := &testkit.Prompt{Answers: []bool{false}}
	stdout.Reset()
	stderr.Reset()
	program, err = New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"config", "sync", source},
		Environment: environment, WorkingDir: root, Prompt: manifestOnlyPrompt,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 ||
		!strings.Contains(stderr.String(), "operation declined") {
		t.Fatalf("manifest-only decline: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	if len(manifestOnlyPrompt.Requests) != 1 ||
		manifestOnlyPrompt.Requests[0].Summary != "Synchronize persistent configuration" ||
		manifestOnlyPrompt.Requests[0].Default != domain.ConfirmationDefaultYes ||
		!reflect.DeepEqual(manifestOnlyPrompt.Requests[0].Consequences,
			[]string{"update versioned configuration manifest metadata"}) {
		t.Fatalf("manifest-only requests=%#v", manifestOnlyPrompt.Requests)
	}
	manifestAfter, err := os.ReadFile(configsync.ManifestPath(configHome))
	if err != nil || !bytes.Equal(manifestAfter, manifestBefore) {
		t.Fatalf("declined manifest-only sync changed manifest: equal=%v err=%v",
			bytes.Equal(manifestAfter, manifestBefore), err)
	}
}

func TestConfigSyncCheckLeavesRecoveryPendingAndMutationRecoversFirst(t *testing.T) {
	root, _, configHome, environment := configCommandFixture(t)
	environment = append(environment, "SUBYARD_HOST_ID=owner-a")
	transactionID := "1-dddddddddddddddd"
	transactionRoot := filepath.Join(
		configHome, ".sync", "transactions", transactionID,
	)
	if err := os.MkdirAll(transactionRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	before := []byte("SSH_PORT=2291\n")
	after := []byte("SSH_PORT=2999\n")
	writeConfigCommandFile(t, filepath.Join(configHome, "config.env"), string(after))
	writeConfigCommandFile(
		t, filepath.Join(transactionRoot, "backup", "config.env"), string(before),
	)
	writeConfigCommandFile(
		t, filepath.Join(configHome, ".sync", "transaction.json"),
		fmt.Sprintf(`{
  "schemaVersion": 1,
  "id": %q,
  "phase": "applying",
  "planDigest": %q,
  "newManifestDigest": %q,
  "applied": 1,
  "entries": [{
    "path": "config.env",
    "action": "update",
    "existed": true,
    "beforeDigest": %q,
    "afterDigest": %q,
    "beforeMode": 384,
    "afterMode": 384
  }]
}
`, transactionID, strings.Repeat("a", 64), strings.Repeat("b", 64),
			fmt.Sprintf("%x", sha256.Sum256(before)), fmt.Sprintf("%x", sha256.Sum256(after))),
	)
	source := filepath.Join(t.TempDir(), "source")
	if err := os.MkdirAll(filepath.Join(source, "hosts", "owner-a"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeConfigCommandFile(t, filepath.Join(source, "subyard-config.json"),
		"{\n  \"schemaVersion\": 1\n}\n")
	writeConfigCommandFile(t, filepath.Join(source, "hosts", "owner-a", "config.env"),
		"SSH_PORT=2291\n")
	runConfigSyncGit(t, source, "init", "-q")
	runConfigSyncGit(t, source, "add", "-A")
	runConfigSyncGit(t, source,
		"-c", "user.name=Subyard Test", "-c", "user.email=test@invalid",
		"commit", "-q", "-m", "initial")

	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"config", "sync", "status", "--offline"},
		Environment: environment, WorkingDir: root, Prompt: &testkit.Prompt{},
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 ||
		!strings.Contains(stdout.String(), "recovery: required") ||
		!strings.Contains(stdout.String(), "live: blocked") {
		t.Fatalf("pending recovery status: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	if _, err := os.Lstat(filepath.Join(configHome, ".sync", "transaction.json")); err != nil {
		t.Fatalf("read-only status changed pending recovery state: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	program, err = New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"config", "sync", source, "--check", "--adopt"},
		Environment: environment, WorkingDir: root, Prompt: &testkit.Prompt{},
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 ||
		!strings.Contains(stderr.String(), "interrupted transaction requires recovery") {
		t.Fatalf("pending recovery check: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	if _, err := os.Lstat(filepath.Join(configHome, ".sync", "transaction.json")); err != nil {
		t.Fatalf("read-only check changed pending recovery state: %v", err)
	}

	prompt := &testkit.Prompt{Answers: []bool{true}}
	stdout.Reset()
	stderr.Reset()
	program, err = New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"config", "sync", source, "--adopt"},
		Environment: environment, WorkingDir: root, Prompt: prompt,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("recovery-first sync: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	if len(prompt.Requests) != 1 ||
		prompt.Requests[0].Summary != "Synchronize persistent configuration" ||
		prompt.Requests[0].Default != domain.ConfirmationDefaultYes ||
		!strings.Contains(strings.Join(prompt.Requests[0].Consequences, "\n"), "adopt config.env") {
		t.Fatalf("recovery-first sync requests=%#v", prompt.Requests)
	}
	if _, err := os.Lstat(filepath.Join(configHome, ".sync", "transaction.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("successful recovery left its journal: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(configHome, "config.env"))
	if err != nil || string(content) != "SSH_PORT=2291\n" {
		t.Fatalf("recovery-first sync did not apply source: %q %v", content, err)
	}
}

func TestConfigSyncAssumeYesRejectsInvalidRecoveryJournalBeforePlanning(t *testing.T) {
	root, _, configHome, environment := configCommandFixture(t)
	target := filepath.Join(configHome, "config.env")
	writeConfigCommandFile(t, target, "SSH_PORT=2299\n")
	transactionID := "1-eeeeeeeeeeeeeeee"
	writeConfigCommandFile(
		t, filepath.Join(configHome, ".sync", "transaction.json"),
		fmt.Sprintf(`{
  "schemaVersion": 1,
  "id": %q,
  "phase": "untrusted",
  "planDigest": %q,
  "newManifestDigest": %q,
  "applied": 0,
  "entries": []
}

`, transactionID, strings.Repeat("a", 64), strings.Repeat("b", 64)),
	)
	prompt := &testkit.Prompt{}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"config", "sync", filepath.Join(t.TempDir(), "source"), "--yes"},
		Environment: environment, WorkingDir: root, Prompt: prompt,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 ||
		!strings.Contains(stderr.String(), "unknown config sync transaction phase") {
		t.Fatalf("invalid recovery: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "SSH_PORT=2299\n" {
		t.Fatalf("invalid recovery changed target: %q err=%v", got, err)
	}
	if len(prompt.Requests) != 0 {
		t.Fatalf("invalid recovery prompted: %#v", prompt.Requests)
	}
	if _, err := os.Lstat(configsync.TransactionPath(configHome)); err != nil {
		t.Fatalf("invalid recovery discarded its journal: %v", err)
	}
}

func TestConfigSyncNamedStatusLeavesRecoveryPending(t *testing.T) {
	root, _, configHome, environment := configCommandFixture(t)
	target, transaction := seedConfigSyncRecoveryJournal(t, configHome)
	writeConfigCommandFile(t, filepath.Join(configHome, "yards", "named", "config.env"), "")
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"-Y", "named", "config", "sync", "status", "--offline"},
		Environment: environment, WorkingDir: root, Prompt: &testkit.Prompt{},
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 ||
		!strings.Contains(stdout.String(), "recovery: required") {
		t.Fatalf("named recovery status: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	if _, err := os.Lstat(transaction); err != nil {
		t.Fatalf("named status consumed recovery journal: %v", err)
	}
	if content, err := os.ReadFile(target); err != nil || string(content) != "SSH_PORT=2999\n" {
		t.Fatalf("named status recovered live target: %q err=%v", content, err)
	}
}

func TestConfigSyncReadOnlyModesDoNotRecoverUnrelatedOwnerState(t *testing.T) {
	for _, recovery := range []struct {
		name string
		path func(string, []string) string
	}{
		{
			name: "owner inventory",
			path: func(_ string, environment []string) string {
				return filepath.Join(
					environmentValue(environment, "SUBYARD_HOME"),
					"owner-inventory",
					"registration.json",
				)
			},
		},
		{
			name: "HostID rename",
			path: func(configHome string, _ []string) string {
				return configsync.HostIDRenameTransactionPath(configHome)
			},
		},
	} {
		for _, invocation := range []struct {
			name      string
			arguments []string
			wantCode  int
			wantText  string
		}{
			{
				name: "status", arguments: []string{"config", "sync", "status", "--offline"},
				wantCode: 1, wantText: "Versioned configuration sync status",
			},
			{
				name: "check", arguments: []string{"config", "sync", "--check"},
				wantCode: 2, wantText: "no checkout was provided or registered",
			},
		} {
			t.Run(recovery.name+"/"+invocation.name, func(t *testing.T) {
				root, _, configHome, environment := configCommandFixture(t)
				environment = append(environment, "SUBYARD_HOST_ID=owner-a")
				journalPath := recovery.path(configHome, environment)
				journal := []byte("{invalid-owner-recovery\n")
				writeConfigCommandFile(t, journalPath, string(journal))

				var stdout, stderr bytes.Buffer
				program, err := New(Options{
					RepositoryRoot: root, Program: "yard", Arguments: invocation.arguments,
					Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr,
				})
				if err != nil {
					t.Fatal(err)
				}
				code := program.Run(context.Background())
				output := stdout.String() + stderr.String()
				if code != invocation.wantCode || !strings.Contains(output, invocation.wantText) {
					t.Fatalf("config sync read-only mode: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
				}
				got, err := os.ReadFile(journalPath)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, journal) {
					t.Fatalf("config sync read-only mode changed recovery journal: got %q, want %q", got, journal)
				}
			})
		}
	}
}

func TestConfigSyncRemoteRoutingIgnoresLocalRecoveryJournal(t *testing.T) {
	for _, test := range []struct {
		name      string
		arguments []string
	}{
		{name: "mutation", arguments: []string{"sync", "--yes"}},
		{name: "check", arguments: []string{"sync", "--check"}},
		{name: "status", arguments: []string{"sync", "status", "--offline"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, home, configHome, environment := configCommandFixture(t)
			target, transaction := seedConfigSyncRecoveryJournal(t, configHome)
			writeConfigCommandFile(t,
				filepath.Join(configHome, "yards", "remote", "config.env"),
				"YARD_TYPE=remote\nREMOTE_DEST=owner.example\nREMOTE_YARD=inner\nSSH_PORT=4444\n")
			fakeBin := filepath.Join(home, "fake-bin")
			logPath := filepath.Join(home, "ssh-arguments")
			writeConfigCommandFile(t, filepath.Join(fakeBin, "ssh"), `#!/bin/sh
printf '%s\n' "$@" >"$SUBYARD_TEST_SSH_LOG"
`, 0o700)
			t.Setenv("PATH", fakeBin+":"+os.Getenv("PATH"))
			environment = append(environment,
				"PATH="+os.Getenv("PATH"),
				"SUBYARD_TEST_SSH_LOG="+logPath)
			arguments := append([]string{"-Y", "remote", "config"}, test.arguments...)
			var stdout, stderr bytes.Buffer
			program, err := New(Options{
				RepositoryRoot: root, Program: "yard", Arguments: arguments,
				Environment: environment, WorkingDir: root,
				Stdout: &stdout, Stderr: &stderr,
			})
			if err != nil {
				t.Fatal(err)
			}
			if code := program.Run(context.Background()); code != 0 {
				t.Fatalf("remote %s: code=%d stdout=%s stderr=%s",
					test.name, code, stdout.String(), stderr.String())
			}
			if _, err := os.Lstat(logPath); err != nil {
				t.Fatalf("remote %s was not forwarded: %v", test.name, err)
			}
			if _, err := os.Lstat(transaction); err != nil {
				t.Fatalf("remote %s consumed local recovery journal: %v", test.name, err)
			}
			if content, err := os.ReadFile(target); err != nil || string(content) != "SSH_PORT=2999\n" {
				t.Fatalf("remote %s recovered local target: %q err=%v",
					test.name, content, err)
			}
		})
	}
}

func TestConfigSourceConnectClonesRegistersAndAppliesWithOnePrompt(t *testing.T) {
	root, home, configHome, environment := configCommandFixture(t)
	source := configSourceGitRepository(t, "owner-a", "SSH_PORT=2292\n")
	checkout := filepath.Join(home, ".local", "share", "subyard-config")
	prompt := &testkit.Prompt{Answers: []bool{true}}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments: []string{
			"config", "sync", "connect", source,
			"--host-id", "owner-a",
		},
		Environment: environment, WorkingDir: root, Prompt: prompt,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("source connect: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	if len(prompt.Requests) != 1 ||
		prompt.Requests[0].Summary != "Connect versioned configuration source" ||
		prompt.Requests[0].Default != domain.ConfirmationDefaultYes ||
		!strings.Contains(strings.Join(prompt.Requests[0].Consequences, "\n"),
			"update versioned configuration manifest metadata") {
		t.Fatalf("source connect requests=%#v", prompt.Requests)
	}
	for _, expected := range []string{
		"Configuration source onboarding",
		"checkout: " + checkout,
		"owner-host: owner-a",
		"config sync: connected " + checkout,
		"config sync: applied generation 1",
	} {
		if !strings.Contains(stdout.String(), expected) {
			t.Fatalf("source connect omitted %q:\n%s", expected, stdout.String())
		}
	}
	content, err := os.ReadFile(filepath.Join(configHome, "config.env"))
	if err != nil || string(content) != "SSH_PORT=2292\n" {
		t.Fatalf("source connect did not apply host config: %q %v", content, err)
	}
	record, exists, err := configsync.ReadSourceRecord(configHome)
	if err != nil || !exists || record.Checkout != checkout {
		t.Fatalf("source record: %#v exists=%v err=%v", record, exists, err)
	}
	origin := strings.TrimSpace(configSourceGitOutput(
		t, checkout, "remote", "get-url", "origin",
	))
	if origin != source {
		t.Fatalf("cloned origin = %q, want %q", origin, source)
	}

	stdout.Reset()
	stderr.Reset()
	pathPrompt := &testkit.Prompt{}
	program, err = New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"config", "sync", "path"},
		Environment: environment, WorkingDir: root, Prompt: pathPrompt,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 ||
		strings.TrimSpace(stdout.String()) != checkout {
		t.Fatalf("source path: code=%d stdout=%q stderr=%q",
			code, stdout.String(), stderr.String())
	}
	if len(pathPrompt.Requests) != 0 {
		t.Fatalf("read-only path prompted: %#v", pathPrompt.Requests)
	}

	stdout.Reset()
	stderr.Reset()
	program, err = New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"config", "sync"},
		Environment: environment, WorkingDir: root, Prompt: &testkit.Prompt{},
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 ||
		!strings.Contains(stdout.String(), "already converged") {
		t.Fatalf("registered source sync: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}

	idempotentPrompt := &testkit.Prompt{}
	stdout.Reset()
	stderr.Reset()
	program, err = New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments: []string{
			"config", "sync", "connect", source,
			"--host-id", "owner-a", "--checkout", checkout,
		},
		Environment: environment, WorkingDir: root, Prompt: idempotentPrompt,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 ||
		!strings.Contains(stdout.String(), "already connected and converged") {
		t.Fatalf("idempotent connect: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	if len(idempotentPrompt.Seen) != 0 {
		t.Fatalf("idempotent connect prompted: %#v", idempotentPrompt.Seen)
	}
}

func TestConfigSourceConnectOnboardsSharedOnlySource(t *testing.T) {
	root, home, configHome, environment := configCommandFixture(t)
	source := configSharedOnlyGitRepository(t)
	checkout := filepath.Join(home, ".local", "share", "subyard-config")
	prompt := &testkit.Prompt{Answers: []bool{true}}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments: []string{
			"config", "sync", "connect", source, "--host-id", "owner-a",
		},
		Environment: environment, WorkingDir: root, Prompt: prompt,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("shared-only source connect: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	if len(prompt.Seen) != 1 {
		t.Fatalf("shared-only source connect prompted %d times: %#v",
			len(prompt.Seen), prompt.Seen)
	}
	for _, expected := range []string{
		"checkout: " + checkout,
		"owner-host: owner-a",
		"initialize: host-id",
		"add",
		"overrides/shared/config.env",
		"config sync: connected " + checkout,
	} {
		if !strings.Contains(stdout.String(), expected) {
			t.Fatalf("shared-only source connect omitted %q:\n%s", expected, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "images:debian/13") {
		t.Fatalf("source connect printed a configuration value:\n%s", stdout.String())
	}
	content, err := os.ReadFile(
		filepath.Join(configHome, "overrides", "shared", "config.env"),
	)
	if err != nil || string(content) != "YARD_IMAGE=images:debian/13\n" {
		t.Fatalf("source connect did not apply shared settings: %q %v", content, err)
	}
	content, err = os.ReadFile(filepath.Join(configHome, "config.env"))
	if err != nil || len(content) != 0 {
		t.Fatalf("shared-only connect changed unmanaged host settings: %q %v", content, err)
	}
	content, err = os.ReadFile(configsync.HostIDPath(configHome))
	if err != nil || string(content) != "owner-a\n" {
		t.Fatalf("shared-only connect did not save local host ID: %q %v", content, err)
	}
	if _, err := os.Lstat(filepath.Join(checkout, "hosts")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source connect created a hosts tree in Git checkout: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	program, err = New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"config", "sync", "--check"},
		Environment: environment, WorkingDir: root, Prompt: &testkit.Prompt{},
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 ||
		!strings.Contains(stdout.String(), "config sync check: converged") {
		t.Fatalf("shared-only repeated check: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
}

func TestConfigSyncTransitionsBetweenSharedOnlyAndHostOverlay(t *testing.T) {
	root, home, configHome, environment := configCommandFixture(t)
	source := configSharedOnlyGitRepository(t)
	checkout := filepath.Join(home, ".local", "share", "subyard-config")
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments: []string{
			"config", "sync", "connect", source, "--host-id", "owner-a", "--yes",
		},
		Environment: environment, WorkingDir: root,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("initial shared-only connect: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}

	writeConfigCommandFile(
		t, filepath.Join(source, "hosts", "owner-a", "config.env"), "SSH_PORT=2294\n",
	)
	commitConfigSource(t, source, "add owner-a overlay")
	runConfigSyncGit(t, checkout, "pull", "--ff-only")
	for _, directory := range []string{
		filepath.Join(checkout, "hosts"),
		filepath.Join(checkout, "hosts", "owner-a"),
	} {
		if err := os.Chmod(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(
		filepath.Join(checkout, "hosts", "owner-a", "config.env"), 0o600,
	); err != nil {
		t.Fatal(err)
	}

	stdout.Reset()
	stderr.Reset()
	checkPrompt := &testkit.Prompt{}
	program, err = New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"config", "sync", "--check", "--adopt"},
		Environment: environment, WorkingDir: root, Prompt: checkPrompt,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 ||
		!strings.Contains(stdout.String(), "config sync check: changes required") ||
		!strings.Contains(stdout.String(), "config.env") {
		t.Fatalf("host overlay check: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	if len(checkPrompt.Seen) != 0 {
		t.Fatalf("host overlay --check prompted: %#v", checkPrompt.Seen)
	}

	declinePrompt := &testkit.Prompt{Answers: []bool{false}}
	stdout.Reset()
	stderr.Reset()
	program, err = New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"config", "sync", "--adopt"},
		Environment: environment, WorkingDir: root, Prompt: declinePrompt,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 ||
		!strings.Contains(stderr.String(), "operation declined") {
		t.Fatalf("declined host overlay: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	content, err := os.ReadFile(filepath.Join(configHome, "config.env"))
	if err != nil || len(content) != 0 {
		t.Fatalf("declined host overlay changed live settings: %q %v", content, err)
	}

	stdout.Reset()
	stderr.Reset()
	program, err = New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"config", "sync", "--adopt"},
		Environment: environment, WorkingDir: root,
		Prompt: &testkit.Prompt{Answers: []bool{true}},
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("apply host overlay: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	content, err = os.ReadFile(filepath.Join(configHome, "config.env"))
	if err != nil || string(content) != "SSH_PORT=2294\n" {
		t.Fatalf("host overlay was not applied: %q %v", content, err)
	}

	if err := os.RemoveAll(filepath.Join(source, "hosts", "owner-a")); err != nil {
		t.Fatal(err)
	}
	commitConfigSource(t, source, "remove owner-a overlay")
	runConfigSyncGit(t, checkout, "pull", "--ff-only")
	stdout.Reset()
	stderr.Reset()
	program, err = New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"config", "sync", "--check"},
		Environment: environment, WorkingDir: root, Prompt: &testkit.Prompt{},
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 ||
		!strings.Contains(stdout.String(), "delete") ||
		!strings.Contains(stdout.String(), "config.env") {
		t.Fatalf("host overlay removal check: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	program, err = New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"config", "sync", "--yes"},
		Environment: environment, WorkingDir: root,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("remove host overlay: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	if _, err := os.Lstat(filepath.Join(configHome, "config.env")); !errors.Is(
		err, os.ErrNotExist,
	) {
		t.Fatalf("managed host overlay survived removal: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	program, err = New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"config", "sync"},
		Environment: environment, WorkingDir: root, Prompt: &testkit.Prompt{},
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 ||
		!strings.Contains(stdout.String(), "already converged") {
		t.Fatalf("shared-only repeat sync: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
}

func TestConfigSourceConnectDeclineLeavesNoCheckoutOrRegistration(t *testing.T) {
	root, home, configHome, environment := configCommandFixture(t)
	source := configSourceGitRepository(t, "owner-a", "SSH_PORT=2293\n")
	checkout := filepath.Join(home, "declined-source")
	prompt := &testkit.Prompt{Answers: []bool{false}}
	applier := &recordingConfigApplier{}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments: []string{
			"config", "sync", "connect", source,
			"--host-id", "owner-a", "--checkout", checkout, "--apply",
		},
		Environment: environment, WorkingDir: root, Prompt: prompt,
		Config: applier,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 ||
		!strings.Contains(stderr.String(), "operation declined") {
		t.Fatalf("declined source connect: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	if _, err := os.Lstat(checkout); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("declined connect left checkout: %v", err)
	}
	if _, exists, err := configsync.ReadSourceRecord(configHome); err != nil || exists {
		t.Fatalf("declined connect registered source: exists=%v err=%v", exists, err)
	}
	if len(applier.yards) != 0 {
		t.Fatalf("declined connect materialized yards: %#v", applier.yards)
	}
	stages, err := filepath.Glob(filepath.Join(home, sourceStagePrefix+"*"))
	if err != nil || len(stages) != 0 {
		t.Fatalf("declined connect left stages: %#v err=%v", stages, err)
	}
	content, err := os.ReadFile(filepath.Join(configHome, "config.env"))
	if err != nil || len(content) != 0 {
		t.Fatalf("declined connect changed live config: %q %v", content, err)
	}
}

func TestConfigSourceConnectRejectsEmbeddedCredentialsAndRemoteForwards(t *testing.T) {
	root, home, configHome, environment := configCommandFixture(t)
	preconditionPrompt := &testkit.Prompt{}
	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments: []string{
			"config", "sync", "connect",
			"https://token@example.invalid/private.git", "--yes",
		},
		Environment: environment, WorkingDir: root, Prompt: preconditionPrompt,
		Stdout: &bytes.Buffer{}, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 ||
		!strings.Contains(stderr.String(), "credentials must not be embedded") {
		t.Fatalf("credential URL: code=%d stderr=%s", code, stderr.String())
	}
	if len(preconditionPrompt.Requests) != 0 {
		t.Fatalf("credential precondition prompted: %#v", preconditionPrompt.Requests)
	}

	writeConfigCommandFile(t,
		filepath.Join(configHome, "yards", "remote", "config.env"),
		"ACCESS_KIND=remote\nREMOTE_DEST=owner.example\nREMOTE_YARD=inner\nSSH_PORT=4444\n")
	fakeBin := filepath.Join(home, "fake-bin")
	logPath := filepath.Join(home, "ssh-arguments")
	writeConfigCommandFile(t, filepath.Join(fakeBin, "ssh"), `#!/bin/sh
printf '%s\n' "$@" >"$SUBYARD_TEST_SSH_LOG"
`, 0o700)
	t.Setenv("PATH", fakeBin+":"+os.Getenv("PATH"))
	environment = append(environment,
		"PATH="+os.Getenv("PATH"),
		"SUBYARD_TEST_SSH_LOG="+logPath)
	stderr.Reset()
	program, err = New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments: []string{
			"-Y", "remote", "config", "sync", "connect",
			"git@example.invalid:private/config.git",
			"--host-id", "owner-b", "--yes",
		},
		Environment: environment, WorkingDir: root,
		Stdout: &bytes.Buffer{}, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("remote source forwarding: code=%d stderr=%s", code, stderr.String())
	}
	forwarded, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	output := string(forwarded)
	for _, expected := range []string{
		"-t\nowner.example\n--\nbash\n-lc\n",
		`'\''yard'\'' '\''-Y'\'' '\''inner'\'' '\''config'\'' '\''sync'\'' '\''connect'\''`,
		"git@example.invalid:private/config.git",
		"--host-id",
		"owner-b",
		"--yes",
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("remote source forwarding omitted %q:\n%s", expected, output)
		}
	}
}

func TestConfigSyncStatusNotConfiguredAndOldSourceCommandsAreRemoved(t *testing.T) {
	root, _, _, environment := configCommandFixture(t)
	readOnlyPrompt := &testkit.Prompt{}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"config", "sync", "status", "--offline"},
		Environment: environment, WorkingDir: root, Prompt: readOnlyPrompt,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 {
		t.Fatalf("unconfigured status: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	for _, expected := range []string{
		"versioned: no",
		"automation: manual",
		"registration: not configured",
		"yard config sync connect <git-url>",
	} {
		if !strings.Contains(stdout.String(), expected) {
			t.Fatalf("unconfigured status omitted %q:\n%s", expected, stdout.String())
		}
	}
	if len(readOnlyPrompt.Requests) != 0 {
		t.Fatalf("read-only status prompted: %#v", readOnlyPrompt.Requests)
	}

	stdout.Reset()
	stderr.Reset()
	program, err = New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"config", "source", "path"},
		Environment: environment, WorkingDir: root,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 2 {
		t.Fatalf("removed config source command: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	program, err = New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"config", "sync", "help"},
		Environment: environment, WorkingDir: root, Prompt: readOnlyPrompt,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("sync help: code=%d stderr=%s", code, stderr.String())
	}
	for _, expected := range []string{
		"config sync connect <git-url>",
		"config sync status",
		"config sync pull --apply",
		"config sync push -m",
		"no background pull or push",
		"never reads",
	} {
		if !strings.Contains(stdout.String(), expected) {
			t.Fatalf("sync help omitted %q:\n%s", expected, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "config source") {
		t.Fatalf("sync help retained removed source command:\n%s", stdout.String())
	}
	if len(readOnlyPrompt.Requests) != 0 {
		t.Fatalf("read-only sync help prompted: %#v", readOnlyPrompt.Requests)
	}
}

func TestConfigSyncConnectInitCreatesInitialCommit(t *testing.T) {
	root, home, _, environment := configCommandFixture(t)
	remote := filepath.Join(t.TempDir(), "empty.git")
	runConfigSyncGit(t, filepath.Dir(remote), "init", "--bare", "-q", remote)
	environment = append(environment,
		"GIT_AUTHOR_NAME=Subyard Test",
		"GIT_AUTHOR_EMAIL=test@invalid",
		"GIT_COMMITTER_NAME=Subyard Test",
		"GIT_COMMITTER_EMAIL=test@invalid",
	)
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments: []string{
			"config", "sync", "connect", remote,
			"--host-id", "owner-a", "--init", "--yes",
		},
		Environment: environment, WorkingDir: root,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("init connect: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	manifest := configSourceGitOutput(
		t, remote, "show", "HEAD:subyard-config.json",
	)
	if !strings.Contains(manifest, `"schemaVersion": 1`) {
		t.Fatalf("initial manifest = %q", manifest)
	}
	checkout := filepath.Join(home, ".local", "share", "subyard-config")
	upstream := strings.TrimSpace(configSourceGitOutput(
		t, checkout, "rev-parse", "--abbrev-ref", "@{upstream}",
	))
	if upstream == "" {
		t.Fatal("initialized checkout has no upstream")
	}
}

func TestConfigSyncConnectInitDeclineLeavesExistingCheckoutUnborn(t *testing.T) {
	root, home, configHome, environment := configCommandFixture(t)
	remote := filepath.Join(t.TempDir(), "empty.git")
	runConfigSyncGit(t, filepath.Dir(remote), "init", "--bare", "-q", remote)
	checkout := filepath.Join(home, "existing-empty")
	runConfigSyncGit(t, home, "clone", "-q", remote, checkout)
	if err := hardenClonedConfigSource(checkout); err != nil {
		t.Fatal(err)
	}
	environment = append(environment,
		"GIT_AUTHOR_NAME=Subyard Test",
		"GIT_AUTHOR_EMAIL=test@invalid",
		"GIT_COMMITTER_NAME=Subyard Test",
		"GIT_COMMITTER_EMAIL=test@invalid",
	)
	before := snapshotConfigCheckout(t, checkout)
	prompt := &testkit.Prompt{Answers: []bool{false}}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments: []string{
			"config", "sync", "connect", remote, "--checkout", checkout,
			"--host-id", "owner-a", "--init",
		},
		Environment: environment, WorkingDir: root, Prompt: prompt,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 ||
		!strings.Contains(stderr.String(), "operation declined") {
		t.Fatalf("declined existing init: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	if after := snapshotConfigCheckout(t, checkout); !reflect.DeepEqual(after, before) {
		t.Fatalf("declined existing init changed checkout:\nbefore=%#v\nafter=%#v", before, after)
	}
	if _, exists, err := configsync.ReadSourceRecord(configHome); err != nil || exists {
		t.Fatalf("declined existing init registered source: exists=%v err=%v", exists, err)
	}
}

func TestConfigSyncConnectInitChangesExistingCheckoutAfterConsent(t *testing.T) {
	root, home, _, environment := configCommandFixture(t)
	remote := filepath.Join(t.TempDir(), "empty.git")
	runConfigSyncGit(t, filepath.Dir(remote), "init", "--bare", "-q", remote)
	checkout := filepath.Join(home, "existing-empty")
	runConfigSyncGit(t, home, "clone", "-q", remote, checkout)
	if err := hardenClonedConfigSource(checkout); err != nil {
		t.Fatal(err)
	}
	environment = append(environment,
		"GIT_AUTHOR_NAME=Subyard Test",
		"GIT_AUTHOR_EMAIL=test@invalid",
		"GIT_COMMITTER_NAME=Subyard Test",
		"GIT_COMMITTER_EMAIL=test@invalid",
	)
	before := snapshotConfigCheckout(t, checkout)
	prompt := &callbackPrompt{callback: func() {
		if during := snapshotConfigCheckout(t, checkout); !reflect.DeepEqual(during, before) {
			t.Fatalf("existing checkout changed before consent:\nbefore=%#v\nduring=%#v", before, during)
		}
	}}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments: []string{
			"config", "sync", "connect", remote, "--checkout", checkout,
			"--host-id", "owner-a", "--init",
		},
		Environment: environment, WorkingDir: root, Prompt: prompt,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("accepted existing init: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	manifest := configSourceGitOutput(t, checkout, "show", "HEAD:subyard-config.json")
	if !strings.Contains(manifest, `"schemaVersion": 1`) {
		t.Fatalf("accepted existing init manifest = %q", manifest)
	}
}

func TestConfigSyncConnectApplyRefreshesAffectedRunningYardWithOnePrompt(t *testing.T) {
	root, _, configHome, environment := configCommandFixture(t)
	source := filepath.Join(t.TempDir(), "source")
	writeConfigCommandFile(t, filepath.Join(source, "subyard-config.json"),
		"{\n  \"schemaVersion\": 1\n}\n")
	relative := filepath.Join("codex", "config.toml")
	content := "model = \"test\"\n"
	writeConfigCommandFile(t,
		filepath.Join(source, "hosts", "owner-a", "overrides", "agents", relative),
		content)
	runConfigSyncGit(t, source, "init", "-q")
	commitConfigSource(t, source, "host file setting")
	writeConfigCommandFile(t,
		filepath.Join(configHome, "overrides", "host", "agents", relative),
		content, 0o600)

	loaded := loadConfigCommandContext(t, root, environment, "default")
	fake := &testkit.Incus{Instances: map[string]ports.InstanceInfo{
		loaded.Context.IncusProject + "/" + loaded.Context.YardInstanceName: {
			Name:    loaded.Context.YardInstanceName,
			Project: loaded.Context.IncusProject,
			Status:  "Running",
		},
	}}
	appendHashSteps(t, fake, loaded)
	prompt := &testkit.Prompt{Answers: []bool{true}}
	applier := &recordingConfigApplier{}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments: []string{
			"config", "sync", "connect", source,
			"--host-id", "owner-a", "--apply",
		},
		Environment: environment, WorkingDir: root, Prompt: prompt,
		Incus: fake, Executor: fake, Config: applier,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("connect --apply: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	if len(prompt.Seen) != 1 {
		t.Fatalf("connect --apply prompted %d times: %#v", len(prompt.Seen), prompt.Seen)
	}
	if len(applier.yards) != 0 {
		t.Fatalf("connect --apply refreshed converged yards %#v", applier.yards)
	}
}

func TestConfigSyncPullFetchesFastForwardsAndImports(t *testing.T) {
	root, home, configHome, environment := configCommandFixture(t)
	remote, publisher := configBareSourceRepository(
		t, "owner-a", "SSH_PORT=2295\n",
	)
	checkout := filepath.Join(home, ".local", "share", "subyard-config")
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments: []string{
			"config", "sync", "connect", remote,
			"--host-id", "owner-a", "--yes",
		},
		Environment: environment, WorkingDir: root,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("connect: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}

	writeConfigCommandFile(t,
		filepath.Join(publisher, "hosts", "owner-a", "config.env"),
		"SSH_PORT=2296\n")
	commitConfigSource(t, publisher, "remote update")
	runConfigSyncGit(t, publisher, "push", "-q")
	checkoutBeforeDecline := strings.TrimSpace(configSourceGitOutput(
		t, checkout, "rev-parse", "HEAD",
	))
	checkoutStateBeforeDecline := snapshotConfigCheckout(t, checkout)
	declinePrompt := &testkit.Prompt{Answers: []bool{false}}
	stdout.Reset()
	stderr.Reset()
	program, err = New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"config", "sync", "pull"},
		Environment: environment, WorkingDir: root, Prompt: declinePrompt,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 ||
		!strings.Contains(stderr.String(), "operation declined") {
		t.Fatalf("declined pull: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	if len(declinePrompt.Requests) != 1 ||
		declinePrompt.Requests[0].Default != domain.ConfirmationDefaultYes {
		t.Fatalf("declined pull requests=%#v", declinePrompt.Requests)
	}
	if head := strings.TrimSpace(configSourceGitOutput(t, checkout, "rev-parse", "HEAD")); head != checkoutBeforeDecline {
		t.Fatalf("declined pull advanced checkout: before=%s after=%s",
			checkoutBeforeDecline, head)
	}
	if after := snapshotConfigCheckout(t, checkout); !reflect.DeepEqual(after, checkoutStateBeforeDecline) {
		t.Fatalf("declined pull changed registered checkout:\nbefore=%#v\nafter=%#v",
			checkoutStateBeforeDecline, after)
	}
	if content, err := os.ReadFile(filepath.Join(configHome, "config.env")); err != nil ||
		strings.TrimSpace(string(content)) != "SSH_PORT=2295" {
		t.Fatalf("declined pull changed live settings: %q err=%v", content, err)
	}

	stdout.Reset()
	stderr.Reset()
	program, err = New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"config", "sync", "status"},
		Environment: environment, WorkingDir: root,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 ||
		!strings.Contains(stdout.String(), "relation: behind 1") {
		t.Fatalf("behind status: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}

	prompt := &testkit.Prompt{Answers: []bool{true}}
	stdout.Reset()
	stderr.Reset()
	program, err = New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"config", "sync", "pull"},
		Environment: environment, WorkingDir: root, Prompt: prompt,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("pull: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	if len(prompt.Requests) != 1 ||
		prompt.Requests[0].Summary != "Pull versioned configuration" ||
		prompt.Requests[0].Default != domain.ConfirmationDefaultYes {
		t.Fatalf("pull requests=%#v", prompt.Requests)
	}
	content, err := os.ReadFile(filepath.Join(configHome, "config.env"))
	if err != nil || strings.TrimSpace(string(content)) != "SSH_PORT=2296" {
		t.Fatalf("pull did not import remote settings: %q %v", content, err)
	}
	head := strings.TrimSpace(configSourceGitOutput(
		t, checkout, "rev-parse", "HEAD",
	))
	upstream := strings.TrimSpace(configSourceGitOutput(
		t, checkout, "rev-parse", "@{upstream}",
	))
	if head != upstream {
		t.Fatalf("pull did not fast-forward: head=%s upstream=%s", head, upstream)
	}
}

func TestConfigSyncPullPromptsForManifestOnlyApplyWithoutFastForward(t *testing.T) {
	root, home, configHome, environment := configCommandFixture(t)
	remote, publisher := configBareSourceRepository(
		t, "owner-a", "SSH_PORT=2296\n",
	)
	checkout := filepath.Join(home, ".local", "share", "subyard-config")
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments: []string{
			"config", "sync", "connect", remote,
			"--host-id", "owner-a", "--yes",
		},
		Environment: environment, WorkingDir: root,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("connect: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	manifestBefore, err := os.ReadFile(configsync.ManifestPath(configHome))
	if err != nil {
		t.Fatal(err)
	}

	writeConfigCommandFile(t, filepath.Join(publisher, "subyard-config.json"),
		"{\"schemaVersion\":1}\n")
	commitConfigSource(t, publisher, "reformat source manifest")
	runConfigSyncGit(t, publisher, "push", "-q")
	runConfigSyncGit(t, checkout, "pull", "--ff-only", "-q")
	if err := os.Chmod(filepath.Join(checkout, "subyard-config.json"), 0o600); err != nil {
		t.Fatal(err)
	}

	prompt := &testkit.Prompt{Answers: []bool{false}}
	stdout.Reset()
	stderr.Reset()
	program, err = New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"config", "sync", "pull"},
		Environment: environment, WorkingDir: root, Prompt: prompt,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 ||
		!strings.Contains(stderr.String(), "operation declined") {
		t.Fatalf("manifest-only pull decline: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	if len(prompt.Requests) != 1 ||
		prompt.Requests[0].Summary != "Pull versioned configuration" ||
		prompt.Requests[0].Default != domain.ConfirmationDefaultYes ||
		!reflect.DeepEqual(prompt.Requests[0].Consequences,
			[]string{"update versioned configuration manifest metadata"}) {
		t.Fatalf("manifest-only pull requests=%#v", prompt.Requests)
	}
	manifestAfter, err := os.ReadFile(configsync.ManifestPath(configHome))
	if err != nil || !bytes.Equal(manifestAfter, manifestBefore) {
		t.Fatalf("declined manifest-only pull changed manifest: equal=%v err=%v",
			bytes.Equal(manifestAfter, manifestBefore), err)
	}
}

func TestConfigSyncPullAndPushNoOpsDoNotPromptOrMutate(t *testing.T) {
	root, home, configHome, environment := configCommandFixture(t)
	remote, _ := configBareSourceRepository(
		t, "owner-a", "SSH_PORT='2297'\n",
	)
	environment = append(environment,
		"GIT_AUTHOR_NAME=Subyard Test",
		"GIT_AUTHOR_EMAIL=test@invalid",
		"GIT_COMMITTER_NAME=Subyard Test",
		"GIT_COMMITTER_EMAIL=test@invalid",
	)
	checkout := filepath.Join(home, ".local", "share", "subyard-config")
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments: []string{
			"config", "sync", "connect", remote,
			"--host-id", "owner-a", "--yes",
		},
		Environment: environment, WorkingDir: root,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("connect: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	manifestBefore, err := os.ReadFile(configsync.ManifestPath(configHome))
	if err != nil {
		t.Fatal(err)
	}
	checkoutBefore := strings.TrimSpace(configSourceGitOutput(t, checkout, "rev-parse", "HEAD"))
	remoteBefore := strings.TrimSpace(configSourceGitOutput(t, remote, "rev-parse", "HEAD"))

	for _, test := range []struct {
		name      string
		arguments []string
	}{
		{name: "pull", arguments: []string{"config", "sync", "pull"}},
		{name: "push", arguments: []string{"config", "sync", "push", "-m", "No changes"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			checkoutStateBefore := snapshotConfigCheckout(t, checkout)
			prompt := &testkit.Prompt{}
			stdout.Reset()
			stderr.Reset()
			program, err := New(Options{
				RepositoryRoot: root, Program: "yard", Arguments: test.arguments,
				Environment: environment, WorkingDir: root, Prompt: prompt,
				Stdout: &stdout, Stderr: &stderr,
			})
			if err != nil {
				t.Fatal(err)
			}
			if code := program.Run(context.Background()); code != 0 ||
				!strings.Contains(stdout.String(), "already converged") {
				t.Fatalf("no-op %s: code=%d stdout=%s stderr=%s",
					test.name, code, stdout.String(), stderr.String())
			}
			if len(prompt.Requests) != 0 {
				t.Fatalf("no-op %s prompted: %#v", test.name, prompt.Requests)
			}
			manifestAfter, err := os.ReadFile(configsync.ManifestPath(configHome))
			if err != nil || !bytes.Equal(manifestAfter, manifestBefore) {
				t.Fatalf("no-op %s rewrote manifest: equal=%v err=%v",
					test.name, bytes.Equal(manifestAfter, manifestBefore), err)
			}
			if head := strings.TrimSpace(configSourceGitOutput(t, checkout, "rev-parse", "HEAD")); head != checkoutBefore {
				t.Fatalf("no-op %s changed checkout: before=%s after=%s",
					test.name, checkoutBefore, head)
			}
			if head := strings.TrimSpace(configSourceGitOutput(t, remote, "rev-parse", "HEAD")); head != remoteBefore {
				t.Fatalf("no-op %s changed upstream: before=%s after=%s",
					test.name, remoteBefore, head)
			}
			if after := snapshotConfigCheckout(t, checkout); !reflect.DeepEqual(after, checkoutStateBefore) {
				t.Fatalf("no-op %s changed registered checkout:\nbefore=%#v\nafter=%#v",
					test.name, checkoutStateBefore, after)
			}
		})
	}
}

func TestConfigSyncNoOpPreflightDoesNotRefreshRegisteredIndex(t *testing.T) {
	for _, test := range []struct {
		name             string
		arguments        func(remote, checkout string) []string
		clonesRegistered bool
	}{
		{
			name: "plain sync",
			arguments: func(_, _ string) []string {
				return []string{"config", "sync"}
			},
		},
		{
			name: "connect",
			arguments: func(remote, checkout string) []string {
				return []string{
					"config", "sync", "connect", remote,
					"--host-id", "owner-a", "--checkout", checkout,
				}
			},
		},
		{
			name:             "pull",
			clonesRegistered: true,
			arguments: func(_, _ string) []string {
				return []string{"config", "sync", "pull"}
			},
		},
		{
			name:             "push",
			clonesRegistered: true,
			arguments: func(_, _ string) []string {
				return []string{"config", "sync", "push", "-m", "No changes"}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("GIT_OPTIONAL_LOCKS", "1")
			root, home, configHome, environment := configCommandFixture(t)
			remote, _ := configBareSourceRepository(
				t, "owner-a", "SSH_PORT='2297'\n",
			)
			environment = append(environment,
				"GIT_AUTHOR_NAME=Subyard Test",
				"GIT_AUTHOR_EMAIL=test@invalid",
				"GIT_COMMITTER_NAME=Subyard Test",
				"GIT_COMMITTER_EMAIL=test@invalid",
				"GIT_OPTIONAL_LOCKS=1",
			)
			checkout := filepath.Join(home, ".local", "share", "subyard-config")
			var stdout, stderr bytes.Buffer
			program, err := New(Options{
				RepositoryRoot: root, Program: "yard",
				Arguments: []string{
					"config", "sync", "connect", remote,
					"--host-id", "owner-a", "--yes",
				},
				Environment: environment, WorkingDir: root,
				Stdout: &stdout, Stderr: &stderr,
			})
			if err != nil {
				t.Fatal(err)
			}
			if code := program.Run(context.Background()); code != 0 {
				t.Fatalf("connect: code=%d stdout=%s stderr=%s",
					code, stdout.String(), stderr.String())
			}
			realGit, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			fakeBin := t.TempDir()
			gitLog := filepath.Join(fakeBin, "registered-checkout-git.log")
			writeConfigCommandFile(t, filepath.Join(fakeBin, "git"), `#!/bin/sh
set -eu
for argument do
	if [ "$argument" = "$SUBYARD_TEST_REGISTERED_CHECKOUT" ]; then
		printf '%s\t%s\n' "${GIT_OPTIONAL_LOCKS-unset}" "$*" >>"$SUBYARD_TEST_GIT_INSPECT_LOG"
		break
	fi
done
exec "$SUBYARD_TEST_REAL_GIT" "$@"
`, 0o700)
			pathEnv := fakeBin + string(os.PathListSeparator) + os.Getenv("PATH")
			for key, value := range map[string]string{
				"PATH":                             pathEnv,
				"SUBYARD_TEST_REAL_GIT":            realGit,
				"SUBYARD_TEST_REGISTERED_CHECKOUT": checkout,
				"SUBYARD_TEST_GIT_INSPECT_LOG":     gitLog,
			} {
				t.Setenv(key, value)
				environment = append(environment, key+"="+value)
			}

			tracked := filepath.Join(checkout, "subyard-config.json")
			staleTime := time.Unix(978307200, 0)
			if err := os.Chtimes(tracked, staleTime, staleTime); err != nil {
				t.Fatal(err)
			}
			before := snapshotConfigCheckoutRaw(t, checkout)
			registrationBefore, err := os.ReadFile(configsync.SourceRecordPath(configHome))
			if err != nil {
				t.Fatal(err)
			}
			manifestBefore, err := os.ReadFile(configsync.ManifestPath(configHome))
			if err != nil {
				t.Fatal(err)
			}
			liveBefore, err := os.ReadFile(filepath.Join(configHome, "config.env"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(gitLog, nil, 0o600); err != nil {
				t.Fatal(err)
			}

			prompt := &testkit.Prompt{}
			stdout.Reset()
			stderr.Reset()
			program, err = New(Options{
				RepositoryRoot: root, Program: "yard",
				Arguments:   test.arguments(remote, checkout),
				Environment: environment, WorkingDir: root, Prompt: prompt,
				Stdout: &stdout, Stderr: &stderr,
			})
			if err != nil {
				t.Fatal(err)
			}
			if code := program.Run(context.Background()); code != 0 {
				t.Fatalf("no-op %s: code=%d stdout=%s stderr=%s",
					test.name, code, stdout.String(), stderr.String())
			}
			if len(prompt.Requests) != 0 {
				t.Fatalf("no-op %s prompted: %#v", test.name, prompt.Requests)
			}
			gitLogBytes, err := os.ReadFile(gitLog)
			if err != nil {
				t.Fatalf("read registered-checkout Git log for %s: %v", test.name, err)
			}
			sawRegisteredClone := false
			for _, line := range strings.Split(strings.TrimSpace(string(gitLogBytes)), "\n") {
				if !strings.HasPrefix(line, "0\t") {
					t.Fatalf("no-op %s ran registered-checkout Git without GIT_OPTIONAL_LOCKS=0: %q",
						test.name, line)
				}
				if strings.Contains(line, "\tclone --quiet -- ") {
					sawRegisteredClone = true
				}
			}
			if test.clonesRegistered && !sawRegisteredClone {
				t.Fatalf("no-op %s did not audit the registered checkout as a clone source:\n%s",
					test.name, gitLogBytes)
			}
			if after := snapshotConfigCheckoutRaw(t, checkout); !reflect.DeepEqual(after, before) {
				t.Fatalf("no-op %s changed registered checkout:\nbefore=%#v\nafter=%#v",
					test.name, before, after)
			}
			for path, expected := range map[string][]byte{
				configsync.SourceRecordPath(configHome): registrationBefore,
				configsync.ManifestPath(configHome):     manifestBefore,
				filepath.Join(configHome, "config.env"): liveBefore,
			} {
				actual, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(actual, expected) {
					t.Fatalf("no-op %s changed %s: equal=%v err=%v",
						test.name, path, bytes.Equal(actual, expected), err)
				}
			}
		})
	}
}

func TestConfigSyncOnlineStatusFetchesWithoutMutatingRegisteredCheckout(t *testing.T) {
	root, home, configHome, environment := configCommandFixture(t)
	remote, publisher := configBareSourceRepository(
		t, "owner-a", "SSH_PORT=2297\n",
	)
	checkout := filepath.Join(home, ".local", "share", "subyard-config")
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments: []string{
			"config", "sync", "connect", remote,
			"--host-id", "owner-a", "--yes",
		},
		Environment: environment, WorkingDir: root,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("connect: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}

	writeConfigCommandFile(t,
		filepath.Join(publisher, "hosts", "owner-a", "config.env"),
		"SSH_PORT=2298\n")
	commitConfigSource(t, publisher, "remote status update")
	runConfigSyncGit(t, publisher, "push", "-q")

	before := snapshotConfigCheckoutRaw(t, checkout)
	registrationBefore, err := os.ReadFile(configsync.SourceRecordPath(configHome))
	if err != nil {
		t.Fatal(err)
	}
	manifestBefore, err := os.ReadFile(configsync.ManifestPath(configHome))
	if err != nil {
		t.Fatal(err)
	}
	liveBefore, err := os.ReadFile(filepath.Join(configHome, "config.env"))
	if err != nil {
		t.Fatal(err)
	}

	prompt := &testkit.Prompt{}
	stdout.Reset()
	stderr.Reset()
	program, err = New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"config", "sync", "status"},
		Environment: environment, WorkingDir: root, Prompt: prompt,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 ||
		!strings.Contains(stdout.String(), "fetch: fresh") ||
		!strings.Contains(stdout.String(), "relation: behind 1") {
		t.Fatalf("online status: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	if len(prompt.Requests) != 0 {
		t.Fatalf("online status prompted: %#v", prompt.Requests)
	}
	if after := snapshotConfigCheckoutRaw(t, checkout); !reflect.DeepEqual(after, before) {
		t.Fatalf("online status changed registered checkout:\nbefore=%#v\nafter=%#v",
			before, after)
	}
	for path, expected := range map[string][]byte{
		configsync.SourceRecordPath(configHome): registrationBefore,
		configsync.ManifestPath(configHome):     manifestBefore,
		filepath.Join(configHome, "config.env"): liveBefore,
	} {
		actual, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(actual, expected) {
			t.Fatalf("online status changed %s: equal=%v err=%v",
				path, bytes.Equal(actual, expected), err)
		}
	}
}

func TestConfigSetAndSyncPushCommitPersistentHostSettings(t *testing.T) {
	root, home, configHome, environment := configCommandFixture(t)
	remote, _ := configBareSourceRepository(
		t, "owner-a", "SSH_PORT=2297\n",
	)
	environment = append(environment,
		"GIT_AUTHOR_NAME=Subyard Test",
		"GIT_AUTHOR_EMAIL=test@invalid",
		"GIT_COMMITTER_NAME=Subyard Test",
		"GIT_COMMITTER_EMAIL=test@invalid",
	)
	var stdout, stderr bytes.Buffer
	run := func(arguments ...string) int {
		t.Helper()
		stdout.Reset()
		stderr.Reset()
		program, err := New(Options{
			RepositoryRoot: root, Program: "yard",
			Arguments: arguments, Environment: environment, WorkingDir: root,
			Stdout: &stdout, Stderr: &stderr,
		})
		if err != nil {
			t.Fatal(err)
		}
		return program.Run(context.Background())
	}
	if code := run(
		"config", "sync", "connect", remote,
		"--host-id", "owner-a", "--yes",
	); code != 0 {
		t.Fatalf("connect: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	if code := run(
		"config", "set", "SSH_PORT", "2298", "--scope", "host", "--yes",
	); code != 0 {
		t.Fatalf("config set: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	if code := run(
		"config", "set", "E2E_VM_CPU", "3", "--scope", "shared", "--yes",
	); code != 0 {
		t.Fatalf("config set shared: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	checkout := filepath.Join(home, ".local", "share", "subyard-config")
	checkoutBeforeDecline := strings.TrimSpace(configSourceGitOutput(
		t, checkout, "rev-parse", "HEAD",
	))
	remoteBeforeDecline := strings.TrimSpace(configSourceGitOutput(
		t, remote, "rev-parse", "HEAD",
	))
	manifestBeforeDecline, err := os.ReadFile(configsync.ManifestPath(configHome))
	if err != nil {
		t.Fatal(err)
	}
	checkoutStateBeforeDecline := snapshotConfigCheckout(t, checkout)
	declinePrompt := &testkit.Prompt{Answers: []bool{false}}
	stdout.Reset()
	stderr.Reset()
	declineProgram, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"config", "sync", "push", "-m", "Declined update"},
		Environment: environment, WorkingDir: root, Prompt: declinePrompt,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := declineProgram.Run(context.Background()); code != 1 ||
		!strings.Contains(stderr.String(), "operation declined") {
		t.Fatalf("declined push: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	if len(declinePrompt.Requests) != 1 ||
		declinePrompt.Requests[0].Summary != "Push versioned configuration" ||
		declinePrompt.Requests[0].Default != domain.ConfirmationDefaultYes {
		t.Fatalf("declined push requests=%#v", declinePrompt.Requests)
	}
	if head := strings.TrimSpace(configSourceGitOutput(t, checkout, "rev-parse", "HEAD")); head != checkoutBeforeDecline {
		t.Fatalf("declined push advanced checkout: before=%s after=%s",
			checkoutBeforeDecline, head)
	}
	if after := snapshotConfigCheckout(t, checkout); !reflect.DeepEqual(after, checkoutStateBeforeDecline) {
		t.Fatalf("declined push changed registered checkout:\nbefore=%#v\nafter=%#v",
			checkoutStateBeforeDecline, after)
	}
	if head := strings.TrimSpace(configSourceGitOutput(t, remote, "rev-parse", "HEAD")); head != remoteBeforeDecline {
		t.Fatalf("declined push advanced upstream: before=%s after=%s",
			remoteBeforeDecline, head)
	}
	if manifestAfterDecline, err := os.ReadFile(configsync.ManifestPath(configHome)); err != nil ||
		!bytes.Equal(manifestAfterDecline, manifestBeforeDecline) {
		t.Fatalf("declined push applied preview: equal=%v err=%v",
			bytes.Equal(manifestAfterDecline, manifestBeforeDecline), err)
	}
	writeConfigCommandFile(t,
		filepath.Join(configHome, "projects", "must-not-export"), "runtime\n")
	writeConfigCommandFile(t,
		filepath.Join(configHome, "secrets", "must-not-export"), "credential\n")
	if code := run(
		"config", "sync", "push", "-m", "Update SSH port", "--yes",
	); code != 0 {
		t.Fatalf("config push: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	versioned := configSourceGitOutput(
		t, remote, "show", "HEAD:hosts/owner-a/config.env",
	)
	if strings.TrimSpace(versioned) != "SSH_PORT='2298'" {
		t.Fatalf("pushed host config = %q", versioned)
	}
	shared := configSourceGitOutput(
		t, remote, "show", "HEAD:shared/config.env",
	)
	if strings.TrimSpace(shared) != "E2E_VM_CPU='3'" {
		t.Fatalf("pushed shared config = %q", shared)
	}
	tree := configSourceGitOutput(t, remote, "ls-tree", "-r", "--name-only", "HEAD")
	if strings.Contains(tree, "must-not-export") ||
		strings.Contains(tree, "projects/") || strings.Contains(tree, "secrets/") {
		t.Fatalf("push exported runtime or secret paths:\n%s", tree)
	}
	status := strings.TrimSpace(configSourceGitOutput(
		t, checkout, "status", "--short",
	))
	if status != "" {
		t.Fatalf("push left dirty checkout: %s", status)
	}
	if code := run("config", "sync", "status", "--offline"); code != 0 {
		t.Fatalf("post-push status: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	gone := remote + ".temporarily-unavailable"
	if err := os.Rename(remote, gone); err != nil {
		t.Fatal(err)
	}
	if code := run("config", "sync", "status"); code != 1 ||
		!strings.Contains(stdout.String(), "fetch: failed") ||
		!strings.Contains(stdout.String(), "head:") {
		t.Fatalf("fetch failure status: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	if err := os.Rename(gone, remote); err != nil {
		t.Fatal(err)
	}
	runConfigSyncGit(t, checkout, "remote", "set-url", "origin",
		filepath.Join(t.TempDir(), "changed.git"))
	if code := run("config", "sync", "status", "--offline"); code != 1 ||
		!strings.Contains(stdout.String(), "remote identity changed") {
		t.Fatalf("changed origin status: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
}

func TestConfigGitStatusParsingAndURLRedaction(t *testing.T) {
	state := configGitState{}
	parseConfigGitPorcelain(&state, []byte(
		"M  hosts/owner/config.env\x00"+
			" M shared/config.env\x00"+
			"?? scratch\x00"+
			"UU hosts/owner/conflict\x00",
	))
	if state.Worktree != "conflicted" || state.Staged != 2 ||
		state.Unstaged != 2 || state.Untracked != 1 || state.Conflicts != 1 {
		t.Fatalf("parsed Git state: %#v", state)
	}
	for input, expected := range map[string]string{
		"https://user:secret@example.invalid/private.git?token=secret#fragment": "https://example.invalid/private.git",
		"ssh://git@example.invalid/private.git":                                 "ssh://git@example.invalid/private.git",
		"git@example.invalid:private/config.git":                                "git@example.invalid:private/config.git",
	} {
		if actual := sanitizeConfigGitURL(input); actual != expected {
			t.Fatalf("sanitize %q = %q, want %q", input, actual, expected)
		}
	}
}

func TestConfigSyncPushRejectsConcurrentRemoteAdvanceWithoutForce(t *testing.T) {
	root, _, configHome, environment := configCommandFixture(t)
	remote, publisher := configBareSourceRepository(
		t, "owner-a", "SSH_PORT=2301\n",
	)
	environment = append(environment,
		"GIT_AUTHOR_NAME=Subyard Test",
		"GIT_AUTHOR_EMAIL=test@invalid",
		"GIT_COMMITTER_NAME=Subyard Test",
		"GIT_COMMITTER_EMAIL=test@invalid",
	)
	run := func(options Options) int {
		t.Helper()
		options.RepositoryRoot = root
		options.Program = "yard"
		options.Environment = environment
		options.WorkingDir = root
		program, err := New(options)
		if err != nil {
			t.Fatal(err)
		}
		return program.Run(context.Background())
	}
	var stdout, stderr bytes.Buffer
	if code := run(Options{
		Arguments: []string{
			"config", "sync", "connect", remote,
			"--host-id", "owner-a", "--yes",
		},
		Stdout: &stdout, Stderr: &stderr,
	}); code != 0 {
		t.Fatalf("connect: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run(Options{
		Arguments: []string{
			"config", "set", "SSH_PORT", "2302", "--scope", "host", "--yes",
		},
		Stdout: &stdout, Stderr: &stderr,
	}); code != 0 {
		t.Fatalf("set: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	record, exists, err := configsync.ReadSourceRecord(configHome)
	if err != nil || !exists {
		t.Fatalf("read source record: record=%#v exists=%v err=%v", record, exists, err)
	}
	checkout := record.Checkout
	beforePush := snapshotConfigCheckout(t, checkout)
	prompt := &callbackPrompt{callback: func() {
		writeConfigCommandFile(t,
			filepath.Join(publisher, "shared", "config.env"),
			"YARD_IMAGE=images:debian/13\n")
		commitConfigSource(t, publisher, "concurrent remote change")
		runConfigSyncGit(t, publisher, "push", "-q")
	}}
	stdout.Reset()
	stderr.Reset()
	if code := run(Options{
		Arguments: []string{
			"config", "sync", "push", "-m", "Local host update",
		},
		Prompt: prompt, Stdout: &stdout, Stderr: &stderr,
	}); code != 1 ||
		!strings.Contains(stderr.String(), "upstream changed after preview; rerun operation") {
		t.Fatalf("concurrent push: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	afterPush := snapshotConfigCheckout(t, checkout)
	if afterPush.head != beforePush.head || afterPush.index != beforePush.index ||
		afterPush.status != beforePush.status {
		t.Fatalf("stale push changed registered checkout before rejection:\nbefore=%#v\nafter=%#v",
			beforePush, afterPush)
	}
	if len(prompt.requests) != 1 ||
		prompt.requests[0].Summary != "Push versioned configuration" ||
		prompt.requests[0].Default != domain.ConfirmationDefaultYes ||
		!strings.Contains(strings.Join(prompt.requests[0].Consequences, "\n"),
			"update versioned configuration manifest metadata") {
		t.Fatalf("concurrent push requests=%#v", prompt.requests)
	}
	versioned := configSourceGitOutput(
		t, remote, "show", "HEAD:hosts/owner-a/config.env",
	)
	if strings.TrimSpace(versioned) != "SSH_PORT=2301" {
		t.Fatalf("failed push changed remote host config: %q", versioned)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run(Options{
		Arguments: []string{"config", "sync", "status"},
		Stdout:    &stdout, Stderr: &stderr,
	}); code != 1 || !strings.Contains(stdout.String(), "relation: behind 1") {
		t.Fatalf("post-rejection status: code=%d stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
}

type callbackPrompt struct {
	callback func()
	requests []domain.ConfirmationRequest
}

func (prompt *callbackPrompt) Confirm(
	_ context.Context,
	request domain.ConfirmationRequest,
) (bool, error) {
	prompt.requests = append(prompt.requests, request)
	if prompt.callback != nil {
		prompt.callback()
	}
	return true, nil
}

type recordingConfigApplier struct {
	yards []string
}

func (applier *recordingConfigApplier) ApplyConfig(_ context.Context, yard string) error {
	applier.yards = append(applier.yards, yard)
	return nil
}

func configCommandFixture(t *testing.T) (string, string, string, []string) {
	t.Helper()
	root := repositoryRoot(t)
	temp := t.TempDir()
	home := filepath.Join(temp, "home")
	configHome := filepath.Join(home, ".config", "subyard")
	for _, directory := range []string{home, configHome} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeConfigCommandFile(t, filepath.Join(configHome, "config.env"), "")
	environment := []string{
		"HOME=" + home,
		"SUBYARD_OPERATOR_HOME=" + home,
		"SUBYARD_CONFIG_HOME=" + configHome,
		"SUBYARD_HOME=" + filepath.Join(home, ".subyard"),
		"SUBYARD_NO_AUDIT=1",
	}
	return root, home, configHome, environment
}

func writeConfigCommandFile(t *testing.T, path, contents string, modes ...os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	mode := os.FileMode(0o600)
	if len(modes) != 0 {
		mode = modes[0]
	}
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
}

func writeConfigAuthoringEditor(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "editor")
	writeConfigCommandFile(t, path, "#!/bin/sh\nset -eu\n"+body, 0o700)
	return path
}

func loadConfigCommandContext(t *testing.T, root string, environment []string, yard string) config.Loaded {
	t.Helper()
	values := map[string]string{}
	for _, assignment := range environment {
		name, value, _ := strings.Cut(assignment, "=")
		values[name] = value
	}
	loaded, err := config.Load(config.LoadOptions{
		RepositoryRoot: root, OperatorHome: values["SUBYARD_OPERATOR_HOME"],
		YardName: yard, Environment: values, DisablePrivate: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

func appendHashSteps(t *testing.T, fake *testkit.Incus, loaded config.Loaded) {
	t.Helper()
	assets, err := effectiveConfigAssets(loaded)
	if err != nil {
		t.Fatal(err)
	}
	for _, asset := range assets {
		hash, err := hashRegularFile(asset.Source)
		if err != nil {
			t.Fatal(err)
		}
		fake.ExecSteps = append(fake.ExecSteps, testkit.IncusExecStep{
			Result: ports.InstanceExecResult{
				Stdout: []byte(fmt.Sprintf("%s  %s\n", hash, asset.Destination)), ExitCode: 0,
			},
		})
	}
}

func appendMismatchedHashSteps(
	t *testing.T,
	fake *testkit.Incus,
	loaded config.Loaded,
	digit string,
) {
	t.Helper()
	assets, err := effectiveConfigAssets(loaded)
	if err != nil {
		t.Fatal(err)
	}
	for _, asset := range assets {
		fake.ExecSteps = append(fake.ExecSteps, testkit.IncusExecStep{
			Result: ports.InstanceExecResult{
				Stdout:   []byte(strings.Repeat(digit, 64) + "  " + asset.Destination + "\n"),
				ExitCode: 0,
			},
		})
	}
}

func appendUnavailableHashSteps(
	t *testing.T,
	fake *testkit.Incus,
	loaded config.Loaded,
) {
	t.Helper()
	assets, err := effectiveConfigAssets(loaded)
	if err != nil {
		t.Fatal(err)
	}
	for range assets {
		fake.ExecSteps = append(fake.ExecSteps, testkit.IncusExecStep{
			Result: ports.InstanceExecResult{ExitCode: 1},
			Err:    errors.New("instance command exited with status 1"),
		})
	}
}

func runConfigSyncGit(t *testing.T, directory string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, arguments...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(arguments, " "), err, output)
	}
}

func configSourceGitRepository(
	t *testing.T,
	hostID string,
	hostConfig string,
) string {
	t.Helper()
	source := filepath.Join(t.TempDir(), "source")
	writeConfigCommandFile(t, filepath.Join(source, "subyard-config.json"),
		"{\n  \"schemaVersion\": 1\n}\n")
	writeConfigCommandFile(t,
		filepath.Join(source, "hosts", hostID, "config.env"), hostConfig)
	runConfigSyncGit(t, source, "init", "-q")
	runConfigSyncGit(t, source, "add", "-A")
	runConfigSyncGit(t, source,
		"-c", "user.name=Subyard Test", "-c", "user.email=test@invalid",
		"commit", "-q", "-m", "initial")
	return source
}

func configSharedOnlyGitRepository(t *testing.T) string {
	t.Helper()
	source := filepath.Join(t.TempDir(), "source")
	writeConfigCommandFile(t, filepath.Join(source, "subyard-config.json"),
		"{\n  \"schemaVersion\": 1\n}\n")
	writeConfigCommandFile(t, filepath.Join(source, "shared", "config.env"),
		"YARD_IMAGE=images:debian/13\n")
	runConfigSyncGit(t, source, "init", "-q")
	commitConfigSource(t, source, "shared only")
	return source
}

func configBareSourceRepository(
	t *testing.T,
	hostID string,
	hostConfig string,
) (string, string) {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	publisher := filepath.Join(root, "publisher")
	if err := os.MkdirAll(publisher, 0o700); err != nil {
		t.Fatal(err)
	}
	runConfigSyncGit(t, root, "init", "--bare", "-q", remote)
	runConfigSyncGit(t, publisher, "init", "-q", "-b", "main")
	writeConfigCommandFile(t, filepath.Join(publisher, "subyard-config.json"),
		"{\n  \"schemaVersion\": 1\n}\n")
	writeConfigCommandFile(t,
		filepath.Join(publisher, "hosts", hostID, "config.env"), hostConfig)
	runConfigSyncGit(t, publisher, "add", "-A")
	runConfigSyncGit(t, publisher,
		"-c", "user.name=Subyard Test", "-c", "user.email=test@invalid",
		"commit", "-q", "-m", "initial")
	runConfigSyncGit(t, publisher, "remote", "add", "origin", remote)
	runConfigSyncGit(t, publisher, "push", "-q", "-u", "origin", "main")
	runConfigSyncGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	return remote, publisher
}

func commitConfigSource(t *testing.T, source, message string) {
	t.Helper()
	runConfigSyncGit(t, source, "add", "-A")
	runConfigSyncGit(t, source,
		"-c", "user.name=Subyard Test", "-c", "user.email=test@invalid",
		"commit", "-q", "-m", message)
}

func configSourceGitOutput(
	t *testing.T,
	directory string,
	arguments ...string,
) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, arguments...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(arguments, " "), err, output)
	}
	return string(output)
}

type configCheckoutSnapshot struct {
	head      string
	index     string
	refs      string
	fetchHead string
	status    string
}

type configCheckoutRawSnapshot struct {
	head      string
	index     string
	refs      string
	fetchHead string
	worktree  map[string]configCheckoutPathSnapshot
}

type configCheckoutPathSnapshot struct {
	mode    os.FileMode
	content string
}

func snapshotConfigCheckoutRaw(t *testing.T, checkout string) configCheckoutRawSnapshot {
	t.Helper()
	read := func(path string) string {
		content, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return "<absent>"
		}
		if err != nil {
			t.Fatal(err)
		}
		return string(content)
	}
	digest := func(path string) string {
		content, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return "<absent>"
		}
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%x", sha256.Sum256(content))
	}
	worktree := map[string]configCheckoutPathSnapshot{}
	err := filepath.Walk(checkout, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == filepath.Join(checkout, ".git") {
			return filepath.SkipDir
		}
		relative, err := filepath.Rel(checkout, path)
		if err != nil {
			return err
		}
		snapshot := configCheckoutPathSnapshot{mode: info.Mode()}
		if info.Mode()&os.ModeSymlink != 0 {
			snapshot.content, err = os.Readlink(path)
		} else if info.Mode().IsRegular() {
			var content []byte
			content, err = os.ReadFile(path)
			snapshot.content = string(content)
		}
		if err != nil {
			return err
		}
		worktree[relative] = snapshot
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return configCheckoutRawSnapshot{
		head:      read(filepath.Join(checkout, ".git", "HEAD")),
		index:     digest(filepath.Join(checkout, ".git", "index")),
		refs:      configSourceGitOutput(t, checkout, "for-each-ref", "--format=%(refname):%(objectname)"),
		fetchHead: read(filepath.Join(checkout, ".git", "FETCH_HEAD")),
		worktree:  worktree,
	}
}

func snapshotConfigCheckout(t *testing.T, checkout string) configCheckoutSnapshot {
	t.Helper()
	read := func(path string) string {
		content, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return "<absent>"
		}
		if err != nil {
			t.Fatal(err)
		}
		return string(content)
	}
	status := configSourceGitOutput(t, checkout, "status", "--porcelain=v1", "--untracked-files=all")
	return configCheckoutSnapshot{
		head:      read(filepath.Join(checkout, ".git", "HEAD")),
		index:     read(filepath.Join(checkout, ".git", "index")),
		refs:      configSourceGitOutput(t, checkout, "for-each-ref", "--format=%(refname):%(objectname)"),
		fetchHead: read(filepath.Join(checkout, ".git", "FETCH_HEAD")),
		status:    status,
	}
}

func seedConfigSyncRecoveryJournal(t *testing.T, configHome string) (string, string) {
	t.Helper()
	transactionID := "1-ffffffffffffffff"
	transactionRoot := filepath.Join(configHome, ".sync", "transactions", transactionID)
	if err := os.MkdirAll(transactionRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	before := []byte("SSH_PORT=2291\n")
	after := []byte("SSH_PORT=2999\n")
	target := filepath.Join(configHome, "config.env")
	writeConfigCommandFile(t, target, string(after))
	writeConfigCommandFile(t, filepath.Join(transactionRoot, "backup", "config.env"), string(before))
	transaction := configsync.TransactionPath(configHome)
	writeConfigCommandFile(t, transaction, fmt.Sprintf(`{
  "schemaVersion": 1,
  "id": %q,
  "phase": "applying",
  "planDigest": %q,
  "newManifestDigest": %q,
  "applied": 1,
  "entries": [{
    "path": "config.env",
    "action": "update",
    "existed": true,
    "beforeDigest": %q,
    "afterDigest": %q,
    "beforeMode": 384,
    "afterMode": 384
  }]
}
`, transactionID, strings.Repeat("a", 64), strings.Repeat("b", 64),
		fmt.Sprintf("%x", sha256.Sum256(before)), fmt.Sprintf("%x", sha256.Sum256(after))))
	return target, transaction
}
