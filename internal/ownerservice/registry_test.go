package ownerservice

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
)

func fixture(t *testing.T) (Registry, Registration, string) {
	t.Helper()
	root := testkit.TempDir(t)
	generation, err := NewGeneration()
	if err != nil {
		t.Fatal(err)
	}
	return Registry{Root: filepath.Join(root, "registry")}, Registration{Public: Public{SchemaVersion: 1, Owner: "owner-a", Service: "fixture", Pool: "main", Yard: "service-container", Project: "owned-project", Instance: "owned-instance", UUID: "original-uuid", Generation: generation}, Ownership: strings.Repeat("a", 64)}, filepath.Join(root, "public")
}

func TestRegistryExactResolutionAndAmbiguity(t *testing.T) {
	registry, record, _ := fixture(t)
	_, err := registry.Resolve(record.Owner, record.Service, "")
	var failure *ResolutionError
	if !errors.As(err, &failure) || failure.Code != "service_absent" {
		t.Fatalf("absent: %v", err)
	}
	if err := registry.Save(record); err != nil {
		t.Fatal(err)
	}
	resolved, err := registry.Resolve(record.Owner, record.Service, "")
	if err != nil || resolved != record {
		t.Fatalf("resolve: %#v %v", resolved, err)
	}
	second := record
	second.Pool = "second"
	second.UUID = "another-uuid"
	if err := registry.Save(second); err != nil {
		t.Fatal(err)
	}
	_, err = registry.Resolve(record.Owner, record.Service, "")
	if !errors.As(err, &failure) || failure.Code != "service_ambiguous" || len(failure.Candidates) != 2 {
		t.Fatalf("ambiguity: %v", err)
	}
	resolved, err = registry.Resolve(record.Owner, record.Service, "second")
	if err != nil || resolved != second {
		t.Fatalf("exact: %#v %v", resolved, err)
	}
	if _, err := registry.Resolve("another-owner", record.Service, "main"); err == nil {
		t.Fatal("foreign owner accepted")
	}
}

func TestRegistryInterruptedPublicationAndGenerationCAS(t *testing.T) {
	registry, record, public := fixture(t)
	if err := registry.Save(record); err != nil {
		t.Fatal(err)
	}
	if err := registry.Published(record, public); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unpublished registration accepted: %v", err)
	}
	if err := registry.Save(record); err != nil {
		t.Fatal(err)
	}
	if err := registry.Publish(record, public); err != nil {
		t.Fatal(err)
	}
	if err := registry.Published(record, public); err != nil {
		t.Fatal(err)
	}
	replacement := record
	replacement.UUID = "replacement-uuid"
	if err := registry.Save(replacement); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("runtime replaced: %v", err)
	}
	replacement = record
	replacement.Generation = strings.Repeat("b", 64)
	if err := registry.Publish(replacement, public); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("stale publication: %v", err)
	}
	if err := registry.Remove(replacement, public); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("stale removal: %v", err)
	}
	if err := registry.Remove(record, public); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(public, record.Service, record.Pool+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("public binding survived removal")
	}
}

func TestRegistryRejectsUnsafeModeLinkAndPublicReplacement(t *testing.T) {
	for _, kind := range []string{"mode", "symlink", "hardlink", "public"} {
		t.Run(kind, func(t *testing.T) {
			registry, record, public := fixture(t)
			if err := registry.Save(record); err != nil {
				t.Fatal(err)
			}
			if err := registry.Publish(record, public); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(registry.Root, record.Service, record.Pool+".json")
			switch kind {
			case "mode":
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(path, path+".other"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".other", path); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(path, path+".other"); err != nil {
					t.Fatal(err)
				}
			case "public":
				testkit.WriteFile(t, filepath.Join(public, record.Service, record.Pool+".json"), []byte(`{}`), 0o644)
			}
			if kind == "public" {
				if err := registry.Published(record, public); err == nil {
					t.Fatal("public replacement accepted")
				}
			} else {
				if _, err := registry.List(record.Service); err == nil {
					t.Fatal("unsafe private record accepted")
				}
			}
		})
	}
}
