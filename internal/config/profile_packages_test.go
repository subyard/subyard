package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestProfileProvisionBindingEvaluatesOnlyDeclaredPublicKeys(t *testing.T) {
	root := testkit.TempDir(t)
	directory := filepath.Join(root, "config/profiles/sample")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(directory, "profile.conf"), []byte("PROFILE_NAME=sample\nVALUE=\"${BASE:-fallback}\"\nSEQUENTIAL=\"$VALUE/suffix\"\n"), 0o644)
	testkit.WriteFile(t, filepath.Join(directory, "provision.sh"), []byte("#!/bin/sh\n# subyard-provision-check-v1\ntrue\n"), 0o755)
	pkg, err := profile.ResolvePackage(root, "sample")
	if err != nil {
		t.Fatal(err)
	}
	binding, err := ProfileProvisionBinding(pkg, map[string]string{"BASE": "inherited", "UNDECLARED": "exclude"})
	if err != nil {
		t.Fatal(err)
	}
	if len(binding.Environment) != 3 || binding.Environment["VALUE"] != "inherited" || binding.Environment["SEQUENTIAL"] != "inherited/suffix" {
		t.Fatalf("environment=%v", binding.Environment)
	}
}

func TestProfileProvisionBindingCapturesSharedDeliverySource(t *testing.T) {
	root := testkit.TempDir(t)
	directory := filepath.Join(root, "config/profiles/sample")
	helper := filepath.Join(root, "config/skills/deliver.py")
	for _, path := range []string{directory, filepath.Dir(helper)} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	testkit.WriteFile(t, filepath.Join(directory, "profile.conf"), nil, 0o644)
	testkit.WriteFile(t, filepath.Join(directory, "provision.sh"), []byte("#!/bin/sh\n# subyard-provision-check-v1\ntrue\n"), 0o755)
	pkg, err := profile.ResolvePackage(root, "sample")
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := ProfileProvisionBinding(pkg, nil)
	if err != nil || len(legacy.Sources) != 0 {
		t.Fatalf("historical sources=%v err=%v", legacy.Sources, err)
	}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	var projection map[string]any
	if err := json.Unmarshal(raw, &projection); err != nil {
		t.Fatal(err)
	}
	if _, exists := projection["sources"]; exists {
		t.Fatal("historical projection gained absent optional sources")
	}
	testkit.WriteFile(t, helper, []byte("shared delivery\n"), 0o644)
	binding, err := ProfileProvisionBinding(pkg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(binding.Sources) != 1 || binding.Sources["agent-skills.py"].Path != helper {
		t.Fatal("missing shared delivery source")
	}
	// Historical transport does not discover a newly available helper.
	if err := legacy.Check(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(helper, helper+".old"); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, helper, []byte("shared delivery\n"), 0o644)
	if err := binding.Check(); err == nil {
		t.Fatal("same-byte helper replacement accepted")
	}
	// The shared alias cannot replace a package-owned source.
	testkit.WriteFile(t, filepath.Join(directory, "agent-skills.py"), []byte("foreign"), 0o644)
	pkg, err = profile.ResolvePackage(root, "sample")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ProfileProvisionBinding(pkg, nil); err == nil {
		t.Fatal("shared source collision accepted")
	}
}
