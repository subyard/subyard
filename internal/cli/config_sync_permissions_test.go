package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestConfigSyncRepairsRegisteredCheckoutPermissions(t *testing.T) {
	for _, test := range []struct {
		name           string
		arguments      []string
		externalUpdate bool
	}{
		{"pull after external fast-forward", []string{"config", "sync", "pull"}, true},
		{"pull with converged content", []string{"config", "sync", "pull"}, false},
		{"push with converged content", []string{"config", "sync", "push", "-m", "Keep configuration permissions safe"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, home, configHome, environment := configCommandFixture(t)
			localPath := filepath.Join(configHome, "config.env")
			localContent := "SSH_PORT='2299'\n"
			writeConfigCommandFile(t, localPath, localContent)
			cachePath := filepath.Join(configHome, config.GitSettingsRelativePath, "config.env")
			remote, publisher := configBareSourceRepository(t, "owner-a", "SSH_PORT='2295'\n")
			checkout := filepath.Join(home, ".local", "share", "subyard-config")
			run := func(arguments []string, prompt *testkit.Prompt) (int, string, string) {
				t.Helper()
				var stdout, stderr bytes.Buffer
				program, err := New(Options{
					RepositoryRoot: root, Program: "yard", Arguments: arguments,
					Environment: environment, WorkingDir: root, Prompt: prompt,
					Stdout: &stdout, Stderr: &stderr,
				})
				if err != nil {
					t.Fatal(err)
				}
				code := program.Run(context.Background())
				return code, stdout.String(), stderr.String()
			}
			if code, stdout, stderr := run([]string{
				"config", "sync", "connect", remote, "--host-id", "owner-a", "--yes",
			}, &testkit.Prompt{}); code != 0 {
				t.Fatalf("connect: code=%d stdout=%s stderr=%s", code, stdout, stderr)
			}
			runConfigSyncGit(t, checkout, "config", "user.name", "Subyard Test")
			runConfigSyncGit(t, checkout, "config", "user.email", "test@invalid")
			content := "SSH_PORT='2295'\n"
			if test.externalUpdate {
				content = "SSH_PORT='2296'\n"
				writeConfigCommandFile(t, filepath.Join(publisher, "hosts", "owner-a", "config.env"), content)
				commitConfigSource(t, publisher, "update configuration")
				runConfigSyncGit(t, publisher, "push", "-q")
				runConfigSyncGit(t, checkout, "fetch", "-q", "origin")
				runConfigSyncGit(t, checkout, "merge", "--ff-only", "--no-edit", "origin/main")
			}
			path := filepath.Join(checkout, "hosts", "owner-a", "config.env")
			testkit.WriteFile(t, path, []byte(content), 0o664)
			if err := os.Chmod(filepath.Dir(path), 0o775); err != nil {
				t.Fatal(err)
			}
			sourceCommit := strings.TrimSpace(configSourceGitOutput(t, checkout, "rev-parse", "HEAD"))
			before := snapshotConfigCheckoutRaw(t, checkout)
			checkPrompt := &testkit.Prompt{}
			if code, stdout, stderr := run([]string{"config", "sync", "--check"}, checkPrompt); code != 1 ||
				!strings.Contains(stderr, "group/world writable") {
				t.Fatalf("unsafe source check: code=%d stdout=%s stderr=%s", code, stdout, stderr)
			}
			if len(checkPrompt.Requests) != 0 || !reflect.DeepEqual(before, snapshotConfigCheckoutRaw(t, checkout)) {
				t.Fatal("read-only source check prompted or changed the checkout")
			}
			decline := &testkit.Prompt{Answers: []bool{false}}
			if code, stdout, stderr := run(test.arguments, decline); code != 1 ||
				!strings.Contains(stderr, "operation declined") {
				t.Fatalf("declined repair: code=%d stdout=%s stderr=%s", code, stdout, stderr)
			}
			if !reflect.DeepEqual(before, snapshotConfigCheckoutRaw(t, checkout)) {
				t.Fatal("declined permission repair changed the checkout")
			}
			if cached, err := os.ReadFile(cachePath); err != nil || string(cached) != "SSH_PORT='2295'\n" {
				t.Fatalf("declined permission repair changed Git fallback: %q %v", cached, err)
			}
			if local, err := os.ReadFile(localPath); err != nil || string(local) != localContent {
				t.Fatalf("declined permission repair changed local configuration: %q %v", local, err)
			}
			accept := &testkit.Prompt{Answers: []bool{true}}
			if code, stdout, stderr := run(test.arguments, accept); code != 0 {
				t.Fatalf("permission repair: code=%d stdout=%s stderr=%s", code, stdout, stderr)
			}
			if len(accept.Requests) != 1 {
				t.Fatalf("permission repair prompted %d times", len(accept.Requests))
			}
			if !strings.Contains(strings.Join(accept.Requests[0].Consequences, "\n"), "write permissions") {
				t.Fatal("permission repair confirmation omitted the permission change")
			}
			for _, target := range []string{path, filepath.Dir(path)} {
				info, err := os.Stat(target)
				if err != nil || info.Mode().Perm()&0o022 != 0 {
					t.Fatalf("source path remains unsafe: %s %v", target, err)
				}
			}
			if cached, err := os.ReadFile(cachePath); err != nil || string(cached) != content {
				t.Fatalf("permission repair did not import Git fallback: %q %v", cached, err)
			}
			if local, err := os.ReadFile(localPath); err != nil || string(local) != localContent {
				t.Fatalf("permission repair changed local configuration: %q %v", local, err)
			}
			if loaded := loadConfigCommandContext(t, root, environment, "default"); loaded.Environment["SSH_PORT"] != "2299" {
				t.Fatalf("permission repair changed effective local setting: %q", loaded.Environment["SSH_PORT"])
			}
			if head := strings.TrimSpace(configSourceGitOutput(t, checkout, "rev-parse", "HEAD")); head != sourceCommit {
				t.Fatal("permission repair created or advanced a configuration commit")
			}
			if code, stdout, stderr := run([]string{"config", "sync", "--check"}, &testkit.Prompt{}); code != 0 {
				t.Fatalf("repaired source check: code=%d stdout=%s stderr=%s", code, stdout, stderr)
			}
		})
	}
}
