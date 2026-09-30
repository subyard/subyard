package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestWebPreviewHostPortSelection(t *testing.T) {
	for _, test := range []struct {
		name, sshPort, port, want string
	}{
		{"default", "2222", "", "32222"},
		{"another yard", "2223", "", "32223"},
		{"last port before wrapping", "35535", "", "65535"},
		{"wrapped port", "35536", "", "1024"},
		{"high SSH port", "60000", "", "25488"},
		{"explicit override", "2222", "18080", "18080"},
	} {
		t.Run(test.name, func(t *testing.T) {
			loaded, err := Load(LoadOptions{
				RepositoryRoot: filepath.Join("..", ".."), OperatorHome: testkit.TempDir(t), DisablePrivate: true,
				Environment: map[string]string{"SSH_PORT": test.sshPort, "WEB_PREVIEW_HOST_PORT": test.port},
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := loaded.Environment["WEB_PREVIEW_HOST_PORT"]; got != test.want {
				t.Fatalf("preview port = %q, want %q", got, test.want)
			}
		})
	}
}

func TestWebPreviewRejectsInvalidAndCollidingHostPort(t *testing.T) {
	for _, port := range []string{"0", "80", "1023", "65536", "oops", "032222", "+32222", "2222", "22222"} {
		t.Run(port, func(t *testing.T) {
			_, err := Load(LoadOptions{
				RepositoryRoot: filepath.Join("..", ".."), OperatorHome: testkit.TempDir(t), DisablePrivate: true,
				Environment: map[string]string{
					"SSH_PORT": "2222", "WEB_PREVIEW_HOST_PORT": port, "CODING_TOOL_INTEGRATIONS": "aiobserver",
				},
			})
			if err == nil {
				t.Fatal("invalid or colliding preview port accepted")
			}
			if !strings.Contains(err.Error(), "WEB_PREVIEW_HOST_PORT") {
				t.Fatalf("preview setting missing from diagnostic: %v", err)
			}
		})
	}
	_, err := Load(LoadOptions{
		RepositoryRoot: filepath.Join("..", ".."), OperatorHome: testkit.TempDir(t), DisablePrivate: true,
		Environment: map[string]string{
			"SSH_PORT": "2222", "AI_OBSERVER_HOST_PORT": "32222", "CODING_TOOL_INTEGRATIONS": "aiobserver",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "WEB_PREVIEW_HOST_PORT collides with AI_OBSERVER_HOST_PORT") {
		t.Fatalf("derived preview collision error = %v", err)
	}
	_, err = Load(LoadOptions{
		RepositoryRoot: filepath.Join("..", ".."), OperatorHome: testkit.TempDir(t), DisablePrivate: true,
		Environment: map[string]string{
			"SSH_PORT": "2222", "WEB_PREVIEW_HOST_PORT": "22222", "CODING_TOOL_INTEGRATIONS": "codex",
		},
	})
	if err != nil {
		t.Fatalf("unused Observer port was reserved: %v", err)
	}
}

func TestWebPreviewHostPortReservesSelectedProfileListeners(t *testing.T) {
	root := syntheticResourceRoot(t)
	for _, selected := range []string{"", "fixture"} {
		t.Run("selected="+selected, func(t *testing.T) {
			_, err := Load(LoadOptions{
				RepositoryRoot: root, OperatorHome: testkit.TempDir(t), DisablePrivate: true,
				Environment: map[string]string{
					"WEB_PREVIEW_HOST_PORT": "18080", "ENVIRONMENT_PROFILES": selected, "FIXTURE_HOST_PORT": "18080",
				},
			})
			if (err != nil) != (selected != "") {
				t.Fatalf("listener collision error = %v", err)
			}
			if err != nil && !strings.Contains(err.Error(), "FIXTURE_HOST_PORT") {
				t.Fatalf("listener missing from diagnostic: %v", err)
			}
		})
	}
}

func TestWebPreviewHostPortPreservesNamedYardOverrides(t *testing.T) {
	home := testkit.TempDir(t)
	configHome := filepath.Join(home, ".config", "subyard")
	for _, yard := range []string{"inherited", "overridden"} {
		if err := os.MkdirAll(filepath.Join(configHome, "yards", yard), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	testkit.WriteFile(t, filepath.Join(configHome, "config.env"), []byte("WEB_PREVIEW_HOST_PORT=18080\n"), 0o600)
	testkit.WriteFile(t, filepath.Join(configHome, "yards", "inherited", "config.env"), []byte("SSH_PORT=2223\n"), 0o600)
	testkit.WriteFile(t, filepath.Join(configHome, "yards", "overridden", "config.env"), []byte("SSH_PORT=2224\nWEB_PREVIEW_HOST_PORT=18081\n"), 0o600)
	for _, test := range []struct{ yard, port string }{{"inherited", "18080"}, {"overridden", "18081"}} {
		t.Run(test.yard, func(t *testing.T) {
			loaded, err := Load(LoadOptions{
				RepositoryRoot: filepath.Join("..", ".."), OperatorHome: home, YardName: test.yard, DisablePrivate: true,
				Environment: map[string]string{"SUBYARD_OPERATOR_HOME": home},
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := loaded.Environment["WEB_PREVIEW_HOST_PORT"]; got != test.port {
				t.Fatalf("preview port = %q, want explicit %q", got, test.port)
			}
		})
	}
}
