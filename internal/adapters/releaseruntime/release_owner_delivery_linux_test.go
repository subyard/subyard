//go:build linux

package releaseruntime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func rewriteReleaseOwnerManifest(t *testing.T, candidate publishedCandidate) {
	t.Helper()
	var manifest strings.Builder
	err := filepath.WalkDir(candidate.root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || path == filepath.Join(candidate.root, "runtime-files.sha256") {
			return err
		}
		payload, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(candidate.root, path)
		if err != nil {
			return err
		}
		fmt.Fprintf(&manifest, "%x  ./%s\n", sha256.Sum256(payload), relative)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(candidate.root, "runtime-files.sha256"), []byte(manifest.String()), 0o600)
}

// All experimental metadata, including partial declarations, is rejected before
// any engine probe. It cannot acquire the historical completed-metadata exception.
func TestGoRefusesExperimentalOwnerBeforeExecution(t *testing.T) {
	for _, asset := range []string{"config/release-owner.json", "config/release-owner.schema.json", "config/release-owner-build.json", "bin/subyard-release-owner"} {
		for _, shape := range []string{"listed", "unlisted", "symlink"} {
			t.Run(asset+"/"+shape, func(t *testing.T) {
				root := testkit.TempDir(t)
				effect := filepath.Join(root, "effect")
				candidate := writeDelegateRuntimeFixture(t, root, "1.2.3-owner", "owner", []byte(sourceAdmissionRegistry), fmt.Sprintf("#!/bin/sh\n: > %q\nprintf 'yard-engine 1.2.3\\n'\n", effect))
				path := filepath.Join(candidate.root, asset)
				if shape == "symlink" {
					if err := os.Symlink(effect, path); err != nil {
						t.Fatal(err)
					}
				} else {
					testkit.WriteFile(t, path, []byte("unsupported experimental asset"), 0600)
				}
				if shape == "listed" {
					rewriteReleaseOwnerManifest(t, candidate)
				}
				runtime := New(Config{})
				defer runtime.Close()
				verified, err := runtime.verifyPublishedCandidate(context.Background(), candidate, root, nil)
				if verified != nil {
					verified.Close()
					t.Fatal("experimental owner was admitted")
				}
				var unsupported unsupportedReleaseOwnerError
				if !errors.As(err, &unsupported) || allowsCompletedMetadataAdmission(nil, err) {
					t.Fatal("unsupported metadata acquired historical authority", err)
				}
				if _, err := os.Stat(effect); !os.IsNotExist(err) {
					t.Fatal("executable ran before refusal", err)
				}
				if err := os.Symlink("releases/"+string(candidate.release), filepath.Join(root, "current")); err != nil {
					t.Fatal(err)
				}
				proof, err := runtime.InspectSourceAdmission(context.Background(), root, testkit.TempDir(t))
				if err == nil || proof.Binding != "" || !errors.As(err, &unsupported) {
					t.Fatal("Go source admission adopted experimental owner", proof, err)
				}
			})
		}
	}
}
