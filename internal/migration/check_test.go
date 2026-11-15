package migration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestLegacyGitConfigReadinessRefusesWithoutChangingOperatorFile(t *testing.T) {
	for _, content := range []string{"", "[user]\nname = Yard Developer\nemail = yard@example.test\n[core]\neditor = private-editor-value\n[include]\npath = absent-file\n"} {
		yard := domain.Context{YardName: "sample", Paths: domain.RuntimePaths{DataHome: testkit.TempDir(t)}}
		path := filepath.Join(yard.Paths.DataHome, "gitconfig")
		if err := CheckLegacyGitConfig(context.Background(), yard); err != nil {
			t.Fatalf("absent drop-in: %v", err)
		}
		testkit.WriteFile(t, path, []byte(content), 0600)
		before, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		err = CheckLegacyGitConfig(context.Background(), yard)
		if err == nil || !strings.Contains(err.Error(), "move or delete") || !strings.Contains(err.Error(), path) {
			t.Fatalf("legacy file did not refuse with a preservation route: %v", err)
		}
		if content != "" {
			for _, expected := range []string{
				"yard -Y sample config set GIT_USER_NAME 'Yard Developer'", "yard -Y sample config set GIT_USER_EMAIL 'yard@example.test'",
				"--scope yard", "core.editor", "include.path", "no longer managed by Subyard",
			} {
				if !strings.Contains(err.Error(), expected) {
					t.Errorf("missing %q in %v", expected, err)
				}
			}
			if strings.Contains(err.Error(), "private-editor-value") {
				t.Fatal("diagnostic exposed an unrelated Git value")
			}
			var refusals *domain.Refusals
			if !errors.As(err, &refusals) || len(refusals.Unwrap()) != 5 {
				t.Fatalf("independent legacy findings lost: %v", err)
			}
			for _, finding := range refusals.Unwrap() {
				var diagnostic interface{ ActivationDiagnostic() (string, string) }
				if !errors.As(finding, &diagnostic) {
					t.Fatalf("missing public diagnostic: %v", finding)
				}
				message, next := diagnostic.ActivationDiagnostic()
				if !strings.Contains(message, "yard sample") || next == "" || len(message) > 512 || strings.ContainsAny(message, "\n\r") {
					t.Fatalf("invalid activation diagnostic: %q / %q", message, next)
				}
			}
		}
		again := CheckLegacyGitConfig(context.Background(), yard)
		if again == nil || again.Error() != err.Error() {
			t.Fatal("legacy findings are unstable")
		}
		after, readErr := os.ReadFile(path)
		info, statErr := os.Stat(path)
		if readErr != nil || statErr != nil || string(after) != content || !os.SameFile(before, info) || before.Mode() != info.Mode() || before.ModTime() != info.ModTime() {
			t.Fatal("readiness modified the legacy file")
		}
	}
}

func TestLegacyGitConfigReadinessRefusesUnreadableConfiguration(t *testing.T) {
	for _, kind := range []string{"malformed", "directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			yard := domain.Context{YardName: "sample", Paths: domain.RuntimePaths{DataHome: testkit.TempDir(t)}}
			path := filepath.Join(yard.Paths.DataHome, "gitconfig")
			switch kind {
			case "malformed":
				testkit.WriteFile(t, path, []byte("[invalid\n"), 0600)
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink("absent", path); err != nil {
					t.Fatal(err)
				}
			}
			if err := CheckLegacyGitConfig(context.Background(), yard); err == nil || !strings.Contains(err.Error(), "move or delete") {
				t.Fatalf("unsafe legacy drop-in was accepted: %v", err)
			}
		})
	}
}
