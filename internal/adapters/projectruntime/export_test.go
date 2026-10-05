package projectruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestPatchStorePublishesProtectedArtifact(t *testing.T) {
	root := filepath.Join(t.TempDir(), "exports")
	store := PatchStore{Directory: root, Now: func() time.Time {
		return time.Date(2026, 7, 22, 8, 39, 0, 0, time.UTC)
	}}
	path, err := store.Publish(context.Background(), "demo-12345678", []byte("patch"))
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(root, "demo-12345678-20260722T083900.000000000Z.patch") {
		t.Fatalf("unexpected export path: %s", path)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("export is not protected: info=%v err=%v", info, err)
	}
	directory, err := os.Stat(root)
	if err != nil || directory.Mode().Perm() != 0o700 {
		t.Fatalf("export directory is not protected: info=%v err=%v", directory, err)
	}
}

func TestPreparedPatchStoreRetainsDestinationWithoutPublishing(t *testing.T) {
	root := filepath.Join(testkit.TempDir(t), "exports")
	clock := time.Date(2026, 7, 22, 8, 39, 0, 0, time.UTC)
	prepared, err := (PatchStore{Directory: root, Now: func() time.Time { return clock }}).Prepare("Demo")
	if err != nil {
		t.Fatal(err)
	}
	path, binding := prepared.Path(), prepared.Binding()
	clock = clock.Add(time.Hour)
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preparation created destination: %v", err)
	}
	actual, err := prepared.Publish(context.Background(), "Demo", []byte("approved patch"))
	if err != nil || actual != path || binding != prepared.Binding() {
		t.Fatalf("destination changed: %s %v", actual, err)
	}
	if _, err := prepared.Publish(context.Background(), "Demo", []byte("replay")); err == nil {
		t.Fatal("destination replay accepted")
	}
}

func TestPreparedPatchStoreRejectsForeignCollisionAndDirectoryDrift(t *testing.T) {
	for _, change := range []string{"collision", "replace", "mode", "symlink-prefix"} {
		t.Run(change, func(t *testing.T) {
			parent := testkit.TempDir(t)
			root := filepath.Join(parent, "exports")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(root, 0o700); err != nil {
				t.Fatal(err)
			}
			prepared, err := (PatchStore{Directory: root}).Prepare("Demo")
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "collision":
				testkit.WriteFile(t, prepared.Path(), []byte("foreign"), 0o600)
			case "mode":
				if err := os.Chmod(root, 0o755); err != nil {
					t.Fatal(err)
				}
			case "replace":
				if err := os.Rename(root, root+".previous"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(root, 0o700); err != nil {
					t.Fatal(err)
				}
			case "symlink-prefix":
				if err := os.Rename(root, root+".previous"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(root+".previous", root); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := prepared.Publish(context.Background(), "Demo", []byte("approved")); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatalf("drift not refused: %v", err)
			}
			if change == "collision" {
				payload, err := os.ReadFile(prepared.Path())
				if err != nil || string(payload) != "foreign" {
					t.Fatal("foreign collision changed")
				}
			}
		})
	}
}
