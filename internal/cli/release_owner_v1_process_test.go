//go:build linux

package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/releaseowner"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

// The owning migration fixture keeps its original physical roots alive while
// invoking this test executable. Only paths and process inputs cross that seam;
// Rust must select the original journal and request its physical two-FD proof.
func TestRustOwnerOriginalV1WholeEntryRefusesRetiredSource(t *testing.T) {
	path := os.Getenv("SUBYARD_RUST_V1_WHOLE_FIXTURE")
	if path == "" {
		t.Skip("explicit original 0.4.2 physical fixture and normal runtime required")
	}
	var fixture struct {
		ConfigHome, DataHome, RuntimeRoot, JournalPath string
		Environment                                    []string
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(fixture.JournalPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		after, err := os.ReadFile(fixture.JournalPath)
		if err != nil || !bytes.Equal(original, after) {
			t.Error("original V1 journal changed", err)
		}
	}()
	repository := sourceAdmissionToolingFixture(t, fixture.RuntimeRoot)
	versionRaw, err := exec.Command(filepath.Join(repository, "bin/yard-engine"), "--version").Output()
	if err != nil || len(strings.Fields(string(versionRaw))) != 2 {
		t.Fatal("normal helper version", err)
	}
	version := strings.Fields(string(versionRaw))[1]
	targetID := version + "-aaaaaaaaaaaa"
	target := filepath.Join(fixture.RuntimeRoot, "releases", targetID)
	if repository != target {
		if err := os.Rename(repository, target); err != nil {
			t.Fatal(err)
		}
	}
	// Original fixture files stay original Go-owned data. Their manifest binds
	// the exact fixture image and retained registry; no Rust declaration or
	// native bundle is added to a historical source.
	proof := func(id, version string, historical bool) map[string]any {
		t.Helper()
		root := filepath.Join(fixture.RuntimeRoot, "releases", id)
		if historical {
			var lines []string
			err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
				if err != nil || entry.IsDir() {
					return err
				}
				rel, err := filepath.Rel(root, path)
				if err != nil || rel == "runtime-files.sha256" {
					return err
				}
				body, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				lines = append(lines, fmt.Sprintf("%x  ./%s", sha256.Sum256(body), filepath.ToSlash(rel)))
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			sort.Strings(lines)
			testkit.WriteFile(t, filepath.Join(root, "runtime-files.sha256"), []byte(strings.Join(lines, "\n")+"\n"), 0644)
		}
		fd, err := os.Open(root)
		if err != nil {
			t.Fatal(err)
		}
		identity, err := releaseowner.IdentityOf(fd)
		fd.Close()
		if err != nil {
			t.Fatal(err)
		}
		digest := func(name string) string {
			t.Helper()
			body, err := os.ReadFile(filepath.Join(root, name))
			if err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256(body)
			return hex.EncodeToString(hash[:])
		}
		return map[string]any{"release": id, "version": version, "root": identity, "artifactDigest": digest("runtime-files.sha256"), "engineDigest": digest("bin/yard-engine")}
	}
	const currentID, previousID = "0.4.2-17608894ab09", "0.4.0-68b9925f6880"
	current, previous, candidate := proof(currentID, "0.4.2", true), proof(previousID, "0.4.0", true), proof(targetID, version, false)
	registry, err := os.ReadFile(filepath.Join(target, "config/release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	options := map[string]any{"dataHome": fixture.DataHome, "configHome": fixture.ConfigHome, "runtimeRoot": fixture.RuntimeRoot, "repositoryRoot": target, "yard": "default", "releases": map[string]any{"from": currentID, "previous": previousID, "target": targetID}, "artifactDigest": candidate["artifactDigest"], "registryRaw": registry, "catalog": releasetransition.BuiltinCapabilityCatalog().Descriptors(), "inheritedSettingIds": []string{}, "rollbackTarget": nil, "migrationCheckpoint": true, "sourceIngress": nil, "sourceArtifact": current, "previousArtifact": previous, "targetArtifact": candidate, "candidateVersion": version}
	protected := func() map[string]string {
		t.Helper()
		result := map[string]string{}
		for _, root := range []string{fixture.ConfigHome, fixture.DataHome} {
			err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				info, err := entry.Info()
				if err != nil {
					return err
				}
				value := fmt.Sprintf("%o", info.Mode())
				if info.Mode().IsRegular() {
					raw, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					value += fmt.Sprintf(":%x", sha256.Sum256(raw))
				} else if info.Mode()&os.ModeSymlink != 0 {
					target, err := os.Readlink(path)
					if err != nil {
						return err
					}
					value += ":" + target
				}
				result[path] = value
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		for _, name := range []string{"current", "previous"} {
			path := filepath.Join(fixture.RuntimeRoot, name)
			target, err := os.Readlink(path)
			if err != nil {
				t.Fatal(err)
			}
			result[path] = target
		}
		return result
	}
	before := protected()
	defer func() {
		if after := protected(); !reflect.DeepEqual(before, after) {
			t.Error("retired-source inspection changed protected paths, bytes, modes, or runtime links")
		}
	}()
	process := startRustOwnerWithBudget(t, filepath.Join(target, "bin/subyard-release-owner"), filepath.Join(target, "bin/yard-engine"), fixture.Environment, target, false, time.Minute)
	defer process.close()
	if reply := process.exchange(map[string]any{"op": "open", "options": options}); reply.Error != "" {
		t.Fatal("normal Rust V1 open", reply.Error)
	}
	review := process.exchange(map[string]any{"op": "inspect", "goal": releasetransition.Goal{Direction: releasetransition.DirectionActivateTarget, Target: releasetransition.ReleaseID(targetID)}})
	inspectionRaw, err := json.MarshalIndent(review, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(filepath.Dir(path), "whole-entry-inspection.json"), inspectionRaw, 0600)
	if review.Error != "" || review.Inspection == nil || len(review.Inspection.Blockers) != 1 {
		t.Fatalf("retired source refusal: error=%q inspection=%+v", review.Error, review.Inspection)
	}
	blocker := review.Inspection.Blockers[0]
	if blocker.Code != releasetransition.CodePreconditionBlocked || blocker.Resource != "transition.source" {
		t.Fatalf("retired source refused for a different reason: %+v", blocker)
	}
	if _, err := os.Lstat(filepath.Join(fixture.ConfigHome, "release-transition/v2/journal.json")); !os.IsNotExist(err) {
		t.Fatal("refused source published current history", err)
	}
	if _, err := os.Lstat(filepath.Join(fixture.ConfigHome, "migrations/update.lock")); !os.IsNotExist(err) {
		t.Fatal("refused source acquired update custody", err)
	}
	if process.admitted {
		t.Fatal("refused source admitted execution custody")
	}
	t.Log("actual normal Rust inspect refused retired 0.4.2 source without execute or custody admission")
}
