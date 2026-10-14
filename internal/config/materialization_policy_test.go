package config

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
)

func TestMaterializationScopeSelectsOnlyDeclaredConsumers(t *testing.T) {
	values := map[string]string{"CODING_TOOL_INTEGRATIONS": "sample other", "AGENT_sample_CONFIG": "source-label", "AGENT_sample_CONFIG_DEST": "config.json", "AGENT_sample_RULES": "rules-label", "AGENT_sample_RULES_DEST": "rules"}
	instruction := MaterializationSource{Integration: "sample", Label: "Synthetic instructions", PreviewInstructions: true, Asset: MaterializedAsset{Name: "sample.instructions", Source: "instruction-label", Destination: "/home/dev/instructions", FollowSymlinks: true}}
	for _, test := range []struct {
		name   string
		scope  MaterializationScope
		allows string
		want   []string
	}{
		{"status", MaterializationConfigAssets, "", []string{"sample.config", "sample.rules"}},
		{"runtime", MaterializationRuntime, "", []string{"sample.instructions", "sample.config", "sample.rules"}},
		{"disallowed instructions", MaterializationRuntime, "false", []string{"sample.config", "sample.rules"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			values["ALLOWS_CODING_TOOLS"] = test.allows
			selected, err := SelectMaterializationSources(values, "dev", test.scope, []MaterializationSource{instruction})
			if err != nil {
				t.Fatal(err)
			}
			names := []string{}
			for _, source := range selected {
				names = append(names, source.Asset.Name)
			}
			if !reflect.DeepEqual(names, test.want) {
				t.Fatalf("selection=%v", names)
			}
			if test.name == "runtime" && (!selected[0].Asset.FollowSymlinks || selected[1].Asset.FollowSymlinks || selected[1].Asset.OwnedFormat != "json") {
				t.Fatal("physical source and owned-format policies changed")
			}
		})
	}
	values["CODING_TOOL_INTEGRATIONS"] = "other"
	selected, err := SelectMaterializationSources(values, "dev", MaterializationRuntime, []MaterializationSource{instruction})
	if err != nil || len(selected) != 0 {
		t.Fatal("unselected consumer escaped scope")
	}
}

func TestMaterializationContentPreservesMissingAndPreviewPolicy(t *testing.T) {
	preview := []byte("<!-- subyard-preview -->\nCurrent.\n<!-- /subyard-preview -->\n")
	host := []byte("Host text.")
	legacy := "Before.\n<!-- subyard-preview -->\nOld.\n<!-- /subyard-preview -->\nAfter.\n"
	for _, test := range []struct {
		name                  string
		scope                 MaterializationScope
		instructions, present bool
		source, want          string
		include               bool
		fail                  bool
	}{
		{name: "status missing", scope: MaterializationConfigAssets, fail: true},
		{name: "runtime missing", scope: MaterializationRuntime},
		{name: "raw present", scope: MaterializationRuntime, present: true, source: string(host), want: string(host), include: true},
		{name: "present empty", scope: MaterializationConfigAssets, present: true, include: true},
		{name: "instruction missing", scope: MaterializationRuntime, instructions: true, want: string(preview), include: true},
		{name: "instruction empty", scope: MaterializationRuntime, instructions: true, present: true, want: string(preview), include: true},
		{name: "instruction append", scope: MaterializationRuntime, instructions: true, present: true, source: string(host), want: string(host) + "\n\n" + string(preview), include: true},
		{name: "instruction refresh", scope: MaterializationRuntime, instructions: true, present: true, source: legacy, want: "Before.\n" + string(preview) + "After.\n", include: true},
		{name: "ambiguous", scope: MaterializationRuntime, instructions: true, present: true, source: legacy + legacy, fail: true},
		{name: "unterminated", scope: MaterializationRuntime, instructions: true, present: true, source: "<!-- subyard-preview -->", fail: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := []byte(test.source)
			content, include, err := PrepareMaterializationContent(test.scope, test.instructions, MaterializationContent{Present: test.present, Payload: source}, preview)
			if (err != nil) != test.fail || err == nil && (include != test.include || string(content) != test.want) {
				t.Fatalf("content=%q include=%v error=%v", content, include, err)
			}
			if test.name == "status missing" && !errors.Is(err, ErrMaterializationSourceMissing) {
				t.Fatal("missing source lost classification")
			}
			if len(content) > 0 {
				content[0] = 'X'
			}
			if string(source) != test.source || !bytes.HasPrefix(preview, []byte("<!--")) {
				t.Fatal("composition changed supplied bytes")
			}
		})
	}
}

func TestMaterializationArtifactAssessmentUsesRawAndOwnedFacts(t *testing.T) {
	for _, test := range []struct {
		name, format string
		observation  MaterializationObservation
		output       string
		converged    bool
	}{
		{"raw current", "", MaterializationObservation{Output: []byte("digest  /home/dev/file\n")}, "sha256", true},
		{"raw drift", "", MaterializationObservation{Output: []byte("changed  /home/dev/file\n")}, "sha256", false},
		{"raw absent", "", MaterializationObservation{ExitCode: 1}, "unavailable", false},
		{"raw malformed", "", MaterializationObservation{}, "malformed", false},
		{"JSON current", "json", MaterializationObservation{Fingerprint: "owned", OwnedConverged: true}, "owned-json", true},
		{"JSON drift", "json", MaterializationObservation{Fingerprint: "owned"}, "owned-json", false},
		{"TOML current", "toml", MaterializationObservation{Fingerprint: "owned", OwnedConverged: true}, "owned-toml", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := AssessMaterializedArtifact(DesiredMaterializedArtifact{Name: "sample.config", DesiredHash: "digest", OwnedFormat: test.format}, test.observation)
			if err != nil || result.Converged != test.converged || result.Snapshot.GuestOutput != test.output {
				t.Fatalf("assessment=%+v err=%v", result, err)
			}
		})
	}
}

// These digests were characterized from the preceding CLI snapshot declarations:
// struct field order, sorted assets, omission rules and ordinary Go JSON/SHA256.
func TestMaterializationTargetRetainsLegacyFingerprintBytes(t *testing.T) {
	target := MaterializationTarget{Name: "alpha", AccessKind: domain.AccessLocal, YardKind: domain.YardContainer, IncusProject: "subyard", YardInstanceName: "yard-alpha", DevUser: "dev", DevUID: 1001,
		Assets: []DesiredMaterializedArtifact{
			{Name: "z.rules", Destination: "/home/dev/rules", DesiredHash: strings.Repeat("b", 64)},
			{Name: "a.config", Destination: "/home/dev/a.json", Scope: "yard", Role: "scalar settings", DesiredHash: strings.Repeat("a", 64), OwnedFormat: "json"},
		},
	}
	facts := []MaterializedArtifactAssessment{
		{Snapshot: MaterializedArtifactSnapshot{Name: "z.rules", GuestHash: strings.Repeat("c", 64), GuestOutput: "sha256"}},
		{Snapshot: MaterializedArtifactSnapshot{Name: "a.config", GuestHash: strings.Repeat("d", 64), GuestOutput: "owned-json"}, Converged: true},
	}
	result, err := AssessMaterialization(target, "running", true, facts)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != "drift" || !result.Changed || result.DesiredFingerprint != "adb480d5bd11e95c9a48b5c16d1533afb5455374bb92876e4027841bf9f61b2e" || result.MaterializedFingerprint != "7337ed5f9f1f84e6fb400fd01ae830da6da524324681e7c4216b86ade21b4036" {
		t.Fatalf("fingerprint contract=%+v", result)
	}
	if target.Assets[0].Name != "z.rules" || facts[0].Snapshot.Name != "z.rules" {
		t.Fatal("assessment reordered caller data")
	}
	facts[0].Converged = true
	facts[0].Snapshot.GuestHash = strings.Repeat("b", 64)
	result, err = AssessMaterialization(target, "running", true, facts)
	if err != nil || result.State != "converged" || result.Changed {
		t.Fatalf("convergence=%+v %v", result, err)
	}
	for _, state := range []string{"absent", "stopped"} {
		result, err := AssessMaterialization(target, state, true, nil)
		if err != nil || result.State != state || result.Changed || result.DesiredFingerprint != "adb480d5bd11e95c9a48b5c16d1533afb5455374bb92876e4027841bf9f61b2e" {
			t.Fatalf("inactive target=%+v %v", result, err)
		}
	}
	result, err = AssessMaterialization(target, "running", false, nil)
	if err != nil || result.State != "running" || result.Changed || result.DesiredFingerprint != "abeabf573d60732f491260cc0b0c7513efee98d6e5020c21561c9653b0e5f704" {
		t.Fatal("unchecked target changed scope or fingerprint")
	}
	target.AccessKind = domain.AccessRemote
	result, err = AssessMaterialization(target, "unobserved", true, nil)
	if err != nil || result.State != "remote (settings only; consumers not checked)" || result.Changed || result.DesiredFingerprint != "1b25c3bafed0d798c01836852272e6715136eec83172960740c549c98201968f" {
		t.Fatal("remote target changed scope or fingerprint")
	}
}

func TestMaterializationActivationRetainsLegacyTargetComposition(t *testing.T) {
	targets := []MaterializationActivationTarget{
		{Name: "zeta", Assessment: MaterializationAssessment{State: "drift", Changed: true, DesiredFingerprint: strings.Repeat("e", 64), MaterializedFingerprint: strings.Repeat("f", 64)}, IntegrationScope: "scope-z", IntegrationFingerprint: strings.Repeat("a", 64)},
		{Name: "alpha", Assessment: MaterializationAssessment{State: "stopped", DesiredFingerprint: strings.Repeat("b", 64), MaterializedFingerprint: strings.Repeat("c", 64)}},
	}
	result := AssessMaterializationActivation(targets)
	if result.Converged || result.Desired != "c1414adc78f8c4da53d46adcf74deb32b71df4cb7d2363019dee7bc1618bf40f" || result.Actual != "86bcde3de187ad1ebb87212cbb19bec87e99ad84baba92eb6aaeb02cf98d07d7" {
		t.Fatalf("activation contract=%+v", result)
	}
	targets[0].Assessment.Changed = false
	result = AssessMaterializationActivation(targets)
	if !result.Converged || result.Actual != result.Desired || result.Desired != "c1414adc78f8c4da53d46adcf74deb32b71df4cb7d2363019dee7bc1618bf40f" {
		t.Fatal("converged actual stopped aliasing desired")
	}
	targets[0].IntegrationChanged = true
	result = AssessMaterializationActivation(targets)
	if result.Converged || result.Actual == result.Desired {
		t.Fatal("integration drift was omitted")
	}
}
