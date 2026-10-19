//go:build linux

package releaseruntime

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestV0173RustStandaloneRequiresHistoricalGoBridge(t *testing.T) {
	fixture := newV0173RecoveryFixture(t)
	declared := writeReleaseOwnerCandidate(t, testkit.TempDir(t), "#!/bin/sh\nprintf 'yard-engine 1.2.3\\n'\n")
	for _, path := range []string{releaseOwnerDeclarationPath, releaseOwnerSchemaPath, releaseOwnerBuildPath, releaseOwnerImagePath} {
		testkit.WriteFile(t, filepath.Join(fixture.candidate.root, path), mustReadFile(t, filepath.Join(declared.root, path)), 0o700)
	}
	rewriteReleaseOwnerManifest(t, fixture.candidate)
	runtime := New(Config{RepositoryRoot: fixture.candidate.root, Inputs: fixtureCommandInputs(fixture.environment()), Stderr: &bytes.Buffer{}})
	defer runtime.Close()
	protected, err := runtime.inspectProtectedTransition(context.Background(), fixture.runtimeRoot, fixture.configHome, "default", nil)
	if err != nil || !isV0173ActivationOnlyBlocker(protected) {
		t.Fatal("original historical owner prerequisite", err)
	}
	runtime.setV0173StandaloneRetry(context.Background(), fixture.runtimeRoot, protected)
	if !strings.Contains(protected.inspection.Outcome.Retry, "historical Go standalone bridge") || strings.Contains(protected.inspection.Outcome.Retry, filepath.Join(fixture.candidate.root, "bin/yard")) {
		t.Fatal("Rust tooling inherited historical Go delegation", protected.inspection.Outcome.Retry)
	}
	store, _ := releasetransition.NewPOSIXV2Store(fixture.configHome)
	after, _ := store.ReadCurrentJournal()
	if !sameProtectedSnapshot(fixture.journal, after) {
		t.Fatal("bridge refusal changed original journal")
	}
}
