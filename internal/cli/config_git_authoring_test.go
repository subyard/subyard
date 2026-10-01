package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/testkit"
)

type configGitAuthoringFixture struct {
	t                              *testing.T
	root, home, configHome, remote string
	environment                    []string
	stdout, stderr                 bytes.Buffer
}

func newConfigGitAuthoringFixture(t *testing.T) *configGitAuthoringFixture {
	t.Helper()
	root, home, configHome, environment := configCommandFixture(t)
	remote, _ := configBareSourceRepository(t, "owner-a", "# Preserve source records\nSSH_PORT=2297\nDEV_SUDO=0\n")
	f := &configGitAuthoringFixture{t: t, root: root, home: home, configHome: configHome, remote: remote,
		environment: append(environment, "GIT_AUTHOR_NAME=Subyard Test", "GIT_AUTHOR_EMAIL=test@invalid",
			"GIT_COMMITTER_NAME=Subyard Test", "GIT_COMMITTER_EMAIL=test@invalid")}
	f.run(0, "config", "sync", "connect", remote, "--host-id", "owner-a", "--yes")
	return f
}

func (f *configGitAuthoringFixture) run(want int, arguments ...string) {
	f.t.Helper()
	f.stdout.Reset()
	f.stderr.Reset()
	program, err := New(Options{RepositoryRoot: f.root, Program: "yard", Arguments: arguments,
		Environment: f.environment, WorkingDir: f.root, Stdout: &f.stdout, Stderr: &f.stderr})
	if err != nil {
		f.t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != want {
		f.t.Fatalf("%v: code=%d want=%d stdout=%s stderr=%s", arguments, code, want, f.stdout.String(), f.stderr.String())
	}
}

func (f *configGitAuthoringFixture) assertSetting(name, value string) {
	f.t.Helper()
	loaded := loadConfigCommandContext(f.t, f.root, f.environment, "default")
	if loaded.Environment[name] != value {
		f.t.Fatalf("%s = %q, want %q", name, loaded.Environment[name], value)
	}
}

func TestConfigGitAuthoringPreservesLocalOverridesAndUnrelatedSourceRecords(t *testing.T) {
	f := newConfigGitAuthoringFixture(t)
	f.run(0, "config", "set", "SSH_PORT", "2298", "--scope", "host", "--local", "--yes")
	f.run(0, "config", "set", "E2E_VM_CPU", "3", "--scope", "shared", "--yes")
	localPath := filepath.Join(f.configHome, "config.env")
	local, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatal(err)
	}
	f.run(0, "config", "set", "SSH_PORT", "2299", "--scope", "host", "--git", "--yes")
	f.assertSetting("SSH_PORT", "2298")
	if after, err := os.ReadFile(localPath); err != nil || !bytes.Equal(local, after) {
		t.Fatalf("Git save changed local settings: %q, %v", after, err)
	}
	versioned := configSourceGitOutput(t, f.remote, "show", "HEAD:hosts/owner-a/config.env")
	if versioned != "# Preserve source records\nSSH_PORT='2299'\nDEV_SUDO=0\n" {
		t.Fatalf("selected save changed unrelated records: %q", versioned)
	}
	changed := strings.TrimSpace(configSourceGitOutput(t, f.remote, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD"))
	if changed != "hosts/owner-a/config.env" {
		t.Fatalf("changed paths: %q", changed)
	}
	f.run(0, "config", "sync", "pull", "--yes")
	f.assertSetting("SSH_PORT", "2298")
	f.run(0, "config", "unset", "SSH_PORT", "--scope", "host", "--yes")
	f.assertSetting("SSH_PORT", "2299")
	f.run(0, "config", "unset", "SSH_PORT", "--scope", "host", "--git", "--yes")
	versioned = configSourceGitOutput(t, f.remote, "show", "HEAD:hosts/owner-a/config.env")
	if versioned != "# Preserve source records\nDEV_SUDO=0\n" {
		t.Fatalf("Git unset: %q", versioned)
	}
	f.assertSetting("SSH_PORT", "2222")
	f.run(0, "config", "set", "CODING_TOOL_INTEGRATIONS", "codex", "--scope", "host", "--git", "--yes")
	f.run(0, "config", "set", "CODING_TOOL_INTEGRATIONS", "", "--scope", "host", "--local", "--yes")
	f.assertSetting("CODING_TOOL_INTEGRATIONS", "")
	f.run(0, "config", "unset", "CODING_TOOL_INTEGRATIONS", "--scope", "host", "--yes")
	f.assertSetting("CODING_TOOL_INTEGRATIONS", "codex")
	// A transport-only retry cannot silently export the remaining shared override.
	f.run(0, "config", "sync", "push", "--yes")
	tree := configSourceGitOutput(t, f.remote, "ls-tree", "-r", "--name-only", "HEAD")
	if strings.Contains(tree, "shared/config.env") {
		t.Fatalf("push exported a local override: %s", tree)
	}
}

func TestConfigGitFileAuthoringKeepsLocalFileAndCachesFallback(t *testing.T) {
	f := newConfigGitAuthoringFixture(t)
	input := filepath.Join(f.home, "input.rules")
	writeConfigCommandFile(t, input, "local-rule\n", 0o600)
	f.run(0, "config", "import", "AGENT_codex_RULES", input, "--scope", "host", "--yes")
	local := filepath.Join(f.configHome, "overrides", "host", "agents", "codex", "rules", "repo.rules")
	writeConfigCommandFile(t, input, "git-rule\n", 0o600)
	f.run(0, "config", "import", "AGENT_codex_RULES", input, "--scope", "host", "--git", "--yes")
	f.assertSetting("AGENT_codex_RULES", local)
	if content, err := os.ReadFile(local); err != nil || string(content) != "local-rule\n" {
		t.Fatalf("Git import changed local file: %q, %v", content, err)
	}
	f.run(0, "config", "sync", "pull", "--yes")
	if err := os.Remove(local); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(f.configHome, config.GitSettingsRelativePath, "overrides", "host", "agents", "codex", "rules", "repo.rules")
	f.assertSetting("AGENT_codex_RULES", cache)
	if content, err := os.ReadFile(cache); err != nil || string(content) != "git-rule\n" {
		t.Fatalf("Git fallback: %q, %v", content, err)
	}
}

func TestConfigGitAuthoringRetriesRejectedPushWithoutDuplicateCommit(t *testing.T) {
	f := newConfigGitAuthoringFixture(t)
	hook := filepath.Join(f.remote, "hooks", "pre-receive")
	testkit.WriteFile(t, hook, []byte("#!/bin/sh\nexit 1\n"), 0o700)
	before := strings.TrimSpace(configSourceGitOutput(t, f.remote, "rev-parse", "HEAD"))
	f.run(1, "config", "set", "SSH_PORT", "2301", "--scope", "host", "--git", "--yes")
	if !strings.Contains(f.stderr.String(), "local checkout remains ahead and can be retried") {
		t.Fatalf("failed push diagnostic: %s", f.stderr.String())
	}
	f.assertSetting("SSH_PORT", "2301")
	checkout := filepath.Join(f.home, ".local", "share", "subyard-config")
	ahead := strings.TrimSpace(configSourceGitOutput(t, checkout, "rev-parse", "HEAD"))
	if actual := strings.TrimSpace(configSourceGitOutput(t, f.remote, "rev-parse", "HEAD")); actual != before || ahead == before {
		t.Fatalf("failed push remote=%s before=%s local=%s", actual, before, ahead)
	}
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	f.run(0, "config", "set", "SSH_PORT", "2301", "--scope", "host", "--git", "--yes")
	if actual := strings.TrimSpace(configSourceGitOutput(t, f.remote, "rev-parse", "HEAD")); actual != ahead {
		t.Fatalf("retry created a duplicate commit: remote=%s existing=%s", actual, ahead)
	}
}

func TestConfigAuthoringStorageChoiceAndNoSource(t *testing.T) {
	root, _, _, environment := configCommandFixture(t)
	for _, arguments := range [][]string{
		{"config", "set", "SSH_PORT", "2301", "--scope", "host", "--local", "--git", "--yes"},
		{"config", "set", "SSH_PORT", "2301", "--scope", "host", "--git", "--yes"},
	} {
		var stderr bytes.Buffer
		program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: arguments, Environment: environment, WorkingDir: root, Stderr: &stderr})
		if err != nil {
			t.Fatal(err)
		}
		if code := program.Run(context.Background()); code == 0 {
			t.Fatalf("accepted %v", arguments)
		}
	}
}

func TestConfigLocalSaveRequiresMigrationEvenForAnIdenticalLegacyValue(t *testing.T) {
	root, _, configHome, environment := configCommandFixture(t)
	path := filepath.Join(configHome, "config.env")
	writeConfigCommandFile(t, path, "SSH_PORT=2301\n", 0o600)
	writeConfigCommandFile(t, filepath.Join(configHome, ".sync", "manifest.json"),
		`{"schemaVersion":1,"files":[{"path":"config.env"}]}`, 0o600)
	prompt := &testkit.Prompt{}
	var stderr bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment,
		Arguments:  []string{"config", "set", "SSH_PORT", "2301", "--scope", "host", "--local"},
		WorkingDir: root, Prompt: prompt, Stderr: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 || !strings.Contains(stderr.String(), "migrate Git settings") {
		t.Fatalf("identical legacy save: code=%d stderr=%s", code, stderr.String())
	}
	if len(prompt.Requests) != 0 {
		t.Fatalf("migration precondition prompted: %#v", prompt.Requests)
	}
	if content, err := os.ReadFile(path); err != nil || string(content) != "SSH_PORT=2301\n" {
		t.Fatalf("migration precondition changed local settings: %q, %v", content, err)
	}
}

func TestConfigGitAuthoringRejectsSourceSymlinksBeforeWriting(t *testing.T) {
	for _, kind := range []string{"file", "scalar ancestor"} {
		t.Run(kind, func(t *testing.T) {
			f := newConfigGitAuthoringFixture(t)
			checkout := filepath.Join(f.home, ".local", "share", "subyard-config")
			outside := testkit.TempDir(t)
			canary := filepath.Join(outside, "config.env")
			writeConfigCommandFile(t, canary, "SSH_PORT=2297\n", 0o600)
			input := filepath.Join(f.home, "input.rules")
			writeConfigCommandFile(t, input, "new-rule\n", 0o600)
			arguments := []string{"config", "import", "AGENT_codex_RULES", input, "--scope", "host", "--git"}
			if kind == "file" {
				path := filepath.Join(checkout, "hosts", "owner-a", "overrides", "agents", "codex", "rules", "repo.rules")
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(canary, path); err != nil {
					t.Fatal(err)
				}
			} else {
				path := filepath.Join(checkout, "hosts", "owner-a")
				if err := os.Remove(filepath.Join(path, "config.env")); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
				arguments = []string{"config", "set", "SSH_PORT", "2301", "--scope", "host", "--git"}
			}
			commitConfigSource(t, checkout, "Invalid source boundary")
			before := configSourceGitOutput(t, checkout, "rev-parse", "HEAD")
			prompt := &testkit.Prompt{}
			program, err := New(Options{RepositoryRoot: f.root, Program: "yard", Arguments: arguments,
				Environment: f.environment, WorkingDir: f.root, Prompt: prompt, Stderr: &f.stderr})
			if err != nil {
				t.Fatal(err)
			}
			if code := program.Run(context.Background()); code != 1 || !strings.Contains(f.stderr.String(), "validate configuration before selected change") {
				t.Fatalf("source boundary: code=%d stderr=%s", code, f.stderr.String())
			}
			if len(prompt.Requests) != 0 {
				t.Fatalf("unsafe source prompted: %#v", prompt.Requests)
			}
			if content, err := os.ReadFile(canary); err != nil || string(content) != "SSH_PORT=2297\n" {
				t.Fatalf("candidate writer escaped source boundary: %q, %v", content, err)
			}
			if after := configSourceGitOutput(t, checkout, "rev-parse", "HEAD"); after != before {
				t.Fatalf("unsafe source advanced: before=%s after=%s", before, after)
			}
		})
	}
}

func TestConfigGitAuthoringRejectsUnpinnedPushURLs(t *testing.T) {
	for _, kind := range []string{"different", "multiple"} {
		t.Run(kind, func(t *testing.T) {
			f := newConfigGitAuthoringFixture(t)
			checkout := filepath.Join(f.home, ".local", "share", "subyard-config")
			pushURL := f.remote
			if kind == "different" {
				pushURL = filepath.Join(testkit.TempDir(t), "alternate.git")
				runConfigSyncGit(t, f.home, "clone", "--bare", f.remote, pushURL)
			}
			runConfigSyncGit(t, checkout, "config", "--add", "remote.origin.pushurl", pushURL)
			if kind == "multiple" {
				runConfigSyncGit(t, checkout, "config", "--add", "remote.origin.pushurl", pushURL)
			}
			before := configSourceGitOutput(t, checkout, "rev-parse", "HEAD")
			f.run(1, "config", "set", "SSH_PORT", "2301", "--scope", "host", "--git", "--yes")
			if !strings.Contains(f.stderr.String(), "one push URL matching the registered configuration source") {
				t.Fatalf("unbounded push target: %s", f.stderr.String())
			}
			for _, repository := range []string{checkout, f.remote, pushURL} {
				if after := configSourceGitOutput(t, repository, "rev-parse", "HEAD"); after != before {
					t.Fatalf("invalid push target advanced %s", repository)
				}
			}
			f.assertSetting("SSH_PORT", "2297")
		})
	}
}

func TestConfigSyncPushDoesNotRequireCommitIdentity(t *testing.T) {
	f := newConfigGitAuthoringFixture(t)
	checkout := filepath.Join(f.home, ".local", "share", "subyard-config")
	writeConfigCommandFile(t, filepath.Join(checkout, "hosts", "owner-a", "config.env"), "SSH_PORT=2302\n", 0o600)
	commitConfigSource(t, checkout, "Already committed settings")
	before := configSourceGitOutput(t, checkout, "rev-parse", "HEAD")
	f.environment = append(f.environment, "GIT_AUTHOR_NAME=", "GIT_AUTHOR_EMAIL=", "GIT_COMMITTER_NAME=", "GIT_COMMITTER_EMAIL=")
	f.run(0, "config", "sync", "push", "--yes")
	if after := configSourceGitOutput(t, f.remote, "rev-parse", "HEAD"); after != before {
		t.Fatalf("transport-only push changed the commit: %s", after)
	}
	f.assertSetting("SSH_PORT", "2302")
	f.run(0, "config", "sync", "push", "--yes")
}
