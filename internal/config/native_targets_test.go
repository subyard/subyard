package config

import (
	"errors"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
)

func TestNativeTargetCollisions(t *testing.T) {
	yard := func(name, instance, volume string) Loaded {
		return Loaded{Context: domain.Context{YardName: name, IncusProject: "shared", YardInstanceName: instance, Paths: domain.RuntimePaths{ConfigHome: "/config"}}, Environment: map[string]string{"SRV_VOLUME": volume}, Settings: map[string]SettingTrace{"INCUS_PROJECT": {Resolutions: []SettingResolution{{Status: "effective", Scope: "default", Path: "/runtime/defaults.env"}}}, "YARD_INSTANCE_NAME": {Resolutions: []SettingResolution{{Status: "effective", Scope: "yard", Path: "/config/yard.env"}}}, "SRV_VOLUME": {Resolutions: []SettingResolution{{Status: "effective", Scope: "host", Path: "/config/config.env"}}}}}
	}
	for _, tc := range []struct {
		name, instance, volume, owner, selected string
		want                                    []string
	}{
		{"instance-known-owner", "yard", "other", "named", "", []string{"share native target shared/yard", "yard scope for default", `"yard.env" (yard scope)`}},
		{"volume-unknown-owner", "other", "data", "", "named", []string{"share /srv volume shared/data", "one of default or named (the non-owner)", `"config.env" (host scope)`}},
		{"both-kinds", "yard", "data", "default", "", []string{"share native target", "share /srv volume", "yard scope for named"}},
		{"distinct", "other", "other", "", "", nil},
		{"different-project", "yard", "data", "", "", nil},
		{"selected-pairs", "yard", "", "", "default", []string{"default and named", "default and zeta"}},
		{"unrelated-selection", "yard", "data", "", "unrelated", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// default is implicit: no registration file is required to count it.
			yards := []Loaded{yard("named", tc.instance, tc.volume), yard("default", "yard", "data"), yard("remote", "yard", "data")}
			yards[2].Context.AccessKind = domain.AccessRemote
			if tc.name == "different-project" {
				yards[0].Context.IncusProject = "other"
			}
			if tc.name == "both-kinds" || tc.name == "selected-pairs" {
				yards = append(yards, yard("zeta", "yard", ""))
			}
			owners := map[string]string{"shared/yard": tc.owner}
			err := NativeTargetCollisions(yards, owners, tc.selected)
			if len(tc.want) == 0 {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil {
				t.Fatal("collision accepted")
			}
			for _, want := range append(tc.want, `"shipped defaults" (default scope)`) {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("missing %q: %v", want, err)
				}
			}
			forbiddenPhrases := []string{"remote", "remove", "/runtime/", "/config/"}
			if tc.name == "selected-pairs" {
				forbiddenPhrases = append(forbiddenPhrases, "named and zeta")
			}
			for _, forbidden := range forbiddenPhrases {
				if strings.Contains(err.Error(), forbidden) {
					t.Fatalf("unsafe diagnostic: %v", err)
				}
			}
			yards[0], yards[1] = yards[1], yards[0]
			if again := NativeTargetCollisions(yards, owners, tc.selected); again.Error() != err.Error() {
				t.Fatal("unstable order")
			}
			var findings *domain.Refusals
			if tc.name == "both-kinds" && (!errors.As(err, &findings) || len(findings.Unwrap()) != 4 || !strings.Contains(err.Error(), "default and zeta") || !strings.Contains(err.Error(), "named and zeta")) {
				t.Fatal("lost kind order", err)
			}
		})
	}
	a, b := strings.Repeat("a", 63), strings.Repeat("b", 63)
	err := NativeTargetCollisions([]Loaded{yard(a, strings.Repeat("i", 63), ""), yard(b, strings.Repeat("i", 63), "")}, nil, "")
	var diagnostic interface{ ActivationDiagnostic() (string, string) }
	if !errors.As(err, &diagnostic) {
		t.Fatal(err)
	}
	message, _ := diagnostic.ActivationDiagnostic()
	if len(message) > 512 || !strings.Contains(message, a) || !strings.Contains(message, b) || !strings.Contains(message, "INCUS_PROJECT/YARD_INSTANCE_NAME") || !strings.Contains(message, "set INCUS_PROJECT") {
		t.Fatal(message)
	}
}
