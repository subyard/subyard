package config

import (
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
)

func TestComponentSelectionExplanationKeepsStableShippedProvenance(t *testing.T) {
	for _, source := range []SettingResolution{
		{Scope: "default", Role: "shipped defaults", Path: "/proc/101/fd/7/config/agents.env", Line: 45, Status: "effective"},
		{Scope: "host", Role: "scalar settings", Path: "/proc/101/fd/7/state/config.env", Line: 3, Status: "effective"},
		{Scope: "default", Role: "shipped defaults", Path: "/proc/101/fd/70/config/agents.env", Line: 8, Status: "effective"},
	} {
		loaded := Loaded{Context: domain.Context{YardName: "default", Paths: domain.RuntimePaths{RepositoryRoot: "/proc/101/fd/7"}},
			Settings: map[string]SettingTrace{"CODING_TOOL_INTEGRATIONS": {Resolutions: []SettingResolution{source}}}}
		text := loaded.IntegrationSelectionExplanation()
		if source.Scope == "default" && source.Line == 45 {
			if !strings.Contains(text, "config/agents.env:45)") || strings.Contains(text, "/proc/") {
				t.Fatalf("shipped identity depends on the read descriptor: %q", text)
			}
		} else if !strings.Contains(text, source.Path+":") {
			t.Fatalf("local or foreign source identity was rewritten: %q", text)
		}
		if loaded.Settings["CODING_TOOL_INTEGRATIONS"].Resolutions[0] != source {
			t.Fatal("rendering rewrote the raw source trace")
		}
	}
}
