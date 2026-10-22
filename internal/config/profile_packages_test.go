package config

import (
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
