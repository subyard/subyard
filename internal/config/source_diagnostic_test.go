package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestSourceDiagnosticDoesNotRenderMalformedInput(t *testing.T) {
	const secret = "SYNTHETIC_PRIVATE_DIAGNOSTIC"
	for _, fixture := range []struct {
		name, content, key, reason string
	}{
		{"required parameter", "SSH_PORT=${SSH_PORT:?" + secret + "}\n", "SSH_PORT", "required parameter is unset"},
		{"invalid parameter", "SSH_PORT=${" + secret + "!}\n", "SSH_PORT", "invalid parameter name"},
		{"invalid variable", secret + "!=value\n", "", "invalid variable name"},
		{"unknown setting", secret + "=value\n", "", "unknown setting"},
		{"invalid mount", "HOST_MOUNTS=" + secret + "\n", "HOST_MOUNTS", "invalid mount list"},
		{"unterminated value", "SSH_PORT='" + secret + "\n", "SSH_PORT", "unterminated quoted assignment"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			root := testkit.TempDir(t)
			path := filepath.Join(root, "config.env")
			testkit.WriteFile(t, path, []byte("# source location\n"+fixture.content), 0o600)
			values := environment{}
			err := applyEnvFileValidated(path, values, ScopeHost, false, nil)
			if err == nil {
				t.Fatal("malformed configuration was accepted")
			}
			diagnostic, ok := SourceDiagnostic(root, fmt.Errorf("outer context: %w", err))
			if !ok || !strings.Contains(diagnostic, "config.env:2") ||
				!strings.Contains(diagnostic, "local scalar settings") || !strings.Contains(diagnostic, fixture.reason) {
				t.Fatalf("source diagnostic = %q, recognized=%v", diagnostic, ok)
			}
			if strings.Contains(diagnostic, secret) || strings.Contains(err.Error(), secret) || strings.Contains(diagnostic, root) {
				t.Fatalf("source diagnostic exposed private input: %q", diagnostic)
			}
			if fixture.key != "" && !strings.Contains(diagnostic, fixture.key) {
				t.Fatalf("canonical key missing from diagnostic: %q", diagnostic)
			}
			if fixture.name == "required parameter" {
				var source *SourceError
				if !errors.As(err, &source) || source.Unwrap() == nil {
					t.Fatal("source error lost its wrapped cause")
				}
			}
		})
	}
}

func TestLoadSourceDiagnosticLocalizesValidation(t *testing.T) {
	home := testkit.TempDir(t)
	configHome := filepath.Join(home, "config")
	if err := os.Mkdir(configHome, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(configHome, "config.env")
	testkit.WriteFile(t, path, []byte("# invalid setting\nSSH_PORT=70000\n"), 0o600)
	_, err := Load(LoadOptions{
		RepositoryRoot: filepath.Join("..", ".."), OperatorHome: home, DisablePrivate: true,
		Environment: map[string]string{"SUBYARD_CONFIG_HOME": configHome},
	})
	diagnostic, ok := SourceDiagnostic(configHome, err)
	if !ok || !strings.Contains(diagnostic, "config.env:2: SSH_PORT") ||
		!strings.Contains(diagnostic, "port value") || strings.Contains(diagnostic, "70000") {
		t.Fatalf("loaded source diagnostic = %q, recognized=%v", diagnostic, ok)
	}
	if _, ok := SourceDiagnostic(configHome, errors.New("arbitrary private cause")); ok {
		t.Fatal("arbitrary error was accepted as safe context")
	}
}

func TestPersistentParserSourceDiagnosticCanonicalizesAndRedacts(t *testing.T) {
	root := testkit.TempDir(t)
	path := filepath.Join(root, GitSettingsRelativePath, "yards", "fixture", "config.env")
	_, err := ParsePersistentAssignments(path, []byte("# source\nAGENTS=${MISSING:?synthetic-private-error}\n"))
	diagnostic, ok := SourceDiagnostic(root, err)
	if !ok || !strings.Contains(diagnostic, ".sync/settings/yards/fixture/config.env:2") ||
		!strings.Contains(diagnostic, "CODING_TOOL_INTEGRATIONS") ||
		!strings.Contains(diagnostic, "Git persistent settings") || strings.Contains(diagnostic, "synthetic-private-error") {
		t.Fatalf("persistent parser source diagnostic = %q, recognized=%v", diagnostic, ok)
	}
	outside := filepath.Join(filepath.Dir(root), "synthetic-private-path", "config.env")
	_, err = ParsePersistentAssignments(outside, []byte("SSH_PORT='unterminated\n"))
	diagnostic, ok = SourceDiagnostic(root, err)
	if !ok || strings.Contains(diagnostic, "synthetic-private-path") ||
		!strings.Contains(diagnostic, "configuration source outside config home:1") {
		t.Fatalf("external source diagnostic = %q, recognized=%v", diagnostic, ok)
	}
}

func TestPersistentSourceDiagnosticsDistinguishSafetyChecks(t *testing.T) {
	for _, fixture := range []struct {
		name, reason string
		prepare      func(*testing.T, string)
	}{
		{"mode", "target is group/world writable", func(t *testing.T, path string) {
			testkit.WriteFile(t, path, []byte("SSH_PORT=2222\n"), 0o622)
		}},
		{"type", "regular non-symlink file", func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink", "unsafe file type", func(t *testing.T, path string) {
			testkit.WriteFile(t, path+".target", nil, 0o600)
			if err := os.Symlink(path+".target", path); err != nil {
				t.Fatal(err)
			}
		}},
		{"ownership", "target is not operator-owned", func(t *testing.T, path string) {
			if os.Geteuid() != 0 {
				t.Skip("ownership fixture requires root")
			}
			testkit.WriteFile(t, path, []byte("SSH_PORT=2222\n"), 0o600)
			if err := os.Chown(path, 1, -1); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			root := testkit.TempDir(t)
			path := filepath.Join(root, "config.env")
			fixture.prepare(t, path)
			_, err := ReadPersistentFileSnapshot(root, path)
			diagnostic, ok := SourceDiagnostic(root, err)
			if !ok || !strings.Contains(diagnostic, "config.env") || !strings.Contains(diagnostic, fixture.reason) {
				t.Fatalf("persistent source diagnostic = %q, recognized=%v", diagnostic, ok)
			}
		})
	}
}
