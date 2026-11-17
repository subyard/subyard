package testvmsruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestImageSelectionProtocol(t *testing.T) {
	key := strings.Fields(fixturePublicKey(t))
	for _, selection := range []struct{ suffix, image, disk, code string }{
		{"", "", "20GiB", ""},
		{" 1", "", "20GiB", ""},
		{" 1 50GiB", "", "50GiB", ""},
		{" 1 - debian-13", DefaultImage, "20GiB", ""},
		{" 1 - ubuntu-24.04", "ubuntu-24.04", "20GiB", ""},
		{" 1 50GiB ubuntu-24.04", "ubuntu-24.04", "50GiB", ""},
		{" 1 - images:ubuntu/24.04/cloud", "", "", "unsupported_image"},
		{" 1 - ubuntu-26.04", "ubuntu-26.04", "20GiB", ""},
		{" 1 -", "", "", "invalid_disk_size"},
		{" 1 60GiB ubuntu-24.04", "", "", "invalid_disk_size"},
	} {
		t.Run(selection.suffix, func(t *testing.T) {
			cfg := fixtureConfig(t)
			cfg.Disk = "20GiB"
			store := LeaseStore{Path: filepath.Join(t.TempDir(), "leases.json"), SlotCount: 1}
			var output bytes.Buffer
			facade := Facade{Store: store, Output: &output, EnvironmentSpec: cfg.EnvironmentSpec}
			command := "acquire-v3 subyard-pair client SHA256:key default Project run tests " + key[0] + " " + key[1] + " slot-001" + selection.suffix
			if err := facade.Run(command); err != nil {
				t.Fatal(err)
			}
			var response facadeResponse
			if err := json.Unmarshal(output.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if selection.code != "" {
				if response.Reason != selection.code || response.Grant != nil {
					t.Fatalf("unexpected refusal: %s", output.String())
				}
				if _, err := os.Stat(store.Path); !os.IsNotExist(err) {
					t.Fatal("invalid image mutated lease state")
				}
				return
			}
			if response.Grant == nil {
				t.Fatalf("missing grant: %s", output.String())
			}
			spec := response.Grant.Environment
			if spec.Image != selection.image || spec.Disk != selection.disk {
				t.Fatalf("wrong environment: %+v", spec)
			}
			// A fresh reader preserves explicit images and accepts image-less persisted leases.
			pool, err := store.Inspect()
			if err != nil || pool.Slots[0].Environment.Image != selection.image {
				t.Fatalf("persisted image: %+v %v", pool, err)
			}
		})
	}
}

func TestImageSelectionKeepsCurrentBasePerOSAndType(t *testing.T) {
	cfg := fixtureConfig(t)
	rt := Runtime{Config: cfg}
	now := time.Now()
	registry := ImageRegistry{SchemaVersion: 1, Owner: strings.Repeat("a", 32)}
	for i, spec := range []struct{ name, image string }{
		{EnvironmentPair, ""}, {EnvironmentPair, "ubuntu-24.04"},
		{EnvironmentAndroid, DefaultImage}, {EnvironmentAndroid, "ubuntu-24.04"},
		{EnvironmentPair, "ubuntu-26.04"}, {EnvironmentAndroid, "ubuntu-26.04"},
	} {
		registry.Bases = append(registry.Bases, BaseImage{Environment: spec.name, Image: spec.image, Key: fmt.Sprintf("%064x", i+1), Fingerprint: fmt.Sprintf("%064x", i+10), CreatedAt: now.Add(time.Duration(i) * time.Minute)})
	}
	store := LeaseStore{Path: filepath.Join(t.TempDir(), "leases.json"), SlotCount: 1}
	rt.Runner = &fakeRunner{handler: func(_ string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
		return nil, nil, fmt.Errorf("current base must not be pruned: %v", args)
	}}
	if err := rt.pruneImages(context.Background(), store, &registry, true); err != nil {
		t.Fatal(err)
	}
	if len(registry.Bases) != 6 {
		t.Fatalf("lost OS base: %+v", registry.Bases)
	}
}

func TestImageSelectionRecipeIdentity(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.RecipeRoot = t.TempDir()
	if err := os.WriteFile(filepath.Join(cfg.RecipeRoot, "manifest.sha256"), []byte("recipe"), 0600); err != nil {
		t.Fatal(err)
	}
	// Pinned from master 8c18dbf7, before image selection, with manifest bytes "recipe".
	const legacy = "c755d79f5eaacebe9feac8231a59e5a87da00336b72665693b20b04a0f6e2e1d"
	seen := map[string]bool{}
	for _, image := range []string{DefaultImage, "ubuntu-24.04", "ubuntu-26.04"} {
		selected, err := cfg.withImage(image)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{EnvironmentPair, EnvironmentAndroid} {
			digest, err := (&Runtime{Config: selected}).recipeDigest(name)
			if err != nil || seen[digest] {
				t.Fatalf("shared recipe identity: %s %s %v", image, name, err)
			}
			seen[digest] = true
			if image == DefaultImage && name == EnvironmentPair && digest != legacy {
				t.Fatal("legacy Debian identity changed")
			}
		}
	}
	if _, err := cfg.withImage("images:arbitrary"); err == nil {
		t.Fatal("unallowlisted image accepted")
	}
}

func TestImageSelectionReportsIndependentEnvironmentRefusals(t *testing.T) {
	err := (EnvironmentSpec{Image: "unsupported"}).Validate()
	if err == nil {
		t.Fatal("invalid environment accepted")
	}
	for _, finding := range []string{"unsupported VM image", "lifecycle or CPU", "composition", "memory limit", "disk must"} {
		if !strings.Contains(err.Error(), finding) {
			t.Fatalf("missing %s: %v", finding, err)
		}
	}
}
