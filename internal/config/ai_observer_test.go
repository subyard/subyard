package config

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestAIObserverDefaultAndHostPortSelection(t *testing.T) {
	for _, test := range []struct {
		name, sshPort, port, agents, wantPort string
		wantAgent                             bool
	}{
		{name: "default", sshPort: "2222", wantPort: "22222", wantAgent: true},
		{name: "another yard", sshPort: "2223", wantPort: "22223", wantAgent: true},
		{name: "high SSH port", sshPort: "60000", wantPort: "15488", wantAgent: true},
		{name: "explicit port", sshPort: "2222", port: "18080", wantPort: "18080", wantAgent: true},
		{name: "explicit agents", sshPort: "2222", agents: "codex", wantPort: "22222"},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			env := map[string]string{"SSH_PORT": test.sshPort}
			if test.port != "" {
				env["AI_OBSERVER_HOST_PORT"] = test.port
			}
			if test.agents != "" {
				env["CODING_TOOL_INTEGRATIONS"] = test.agents
			}
			loaded, err := Load(LoadOptions{RepositoryRoot: filepath.Join("..", ".."), OperatorHome: home, DisablePrivate: true, Environment: env})
			if err != nil {
				t.Fatal(err)
			}
			if got := loaded.Environment["AI_OBSERVER_HOST_PORT"]; got != test.wantPort {
				t.Errorf("dashboard port = %q, want %q", got, test.wantPort)
			}
			if got := slices.Contains(strings.Fields(loaded.Environment["CODING_TOOL_INTEGRATIONS"]), "aiobserver"); got != test.wantAgent {
				t.Errorf("observer selected = %v, want %v", got, test.wantAgent)
			}
		})
	}
}

func TestAIObserverRejectsInvalidAndCollidingHostPort(t *testing.T) {
	for _, port := range []string{"0", "65536", "oops", "2222", "15555"} {
		t.Run(port, func(t *testing.T) {
			_, err := Load(LoadOptions{RepositoryRoot: filepath.Join("..", ".."), OperatorHome: t.TempDir(), DisablePrivate: true,
				Environment: map[string]string{"SSH_PORT": "2222", "AI_OBSERVER_HOST_PORT": port, "CODING_TOOL_INTEGRATIONS": "aiobserver"}})
			if err == nil {
				t.Fatal("invalid or colliding dashboard port accepted")
			}
		})
	}
}
