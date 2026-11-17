package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestRollbackNativePreviewDoesNotRelaxOrdinaryHistoryAdmission(t *testing.T) {
	home := testkit.TempDir(t)
	directory := filepath.Join(home, "release-transition", "v2")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "journal.json")
	contents := []byte("foreign or damaged journal\n")
	testkit.WriteFile(t, path, contents, 0o600)
	program := &CLI{}
	source := &releaseConfiguration{rollbackPreview: true}
	pair := releasetransition.ReleasePair{From: "release-s", Target: "release-r"}
	goal := releasetransition.Goal{Target: pair.Target, Direction: releasetransition.DirectionActivatePrevious}
	if wide, err := source.materializedScope(program, pair, home, goal, "", "", false); err != nil || !wide {
		t.Fatalf("validated preview did not use native target scope: %t, %v", wide, err)
	}
	source.rollbackPreview = false
	if _, err := source.materializedScope(program, pair, home, goal, "", "", false); err == nil {
		t.Fatal("ordinary activation silently accepted foreign history")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, contents) {
		t.Fatal("preview changed protected history")
	}
	owner := &profileRuntimeActivationReconciler{cli: &CLI{activation: &releaseConfiguration{rollbackPreview: true}}}
	if complete, err := owner.sourceMigrationsComplete(); err != nil || !complete {
		t.Fatalf("profile preview lost native completion: %t, %v", complete, err)
	}
	owner.cli.activation = nil
	if _, err := owner.sourceMigrationsComplete(); err == nil {
		t.Fatal("ordinary profile owner bypassed its history reader")
	}
}

func TestRollbackTargetProcessRejectsUnadvertisedProbeAndMalformedInput(t *testing.T) {
	for _, payload := range []string{`{"schemaVersion":3,"mode":"capabilities"}`, `{"schemaVersion":3,"mode":"activate-under-v2"}`, `{"schemaVersion":3,"mode":"activate-under-v2","script":"arbitrary"}`} {
		t.Run(payload, func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			var output, diagnostics bytes.Buffer
			program, err := New(Options{RepositoryRoot: root, Environment: environment, Stdin: strings.NewReader(payload), Stdout: &output, Stderr: &diagnostics})
			if err != nil {
				t.Fatal(err)
			}
			if code := program.runReleaseTransition(context.Background(), nil); code != 2 || output.Len() != 0 {
				t.Fatalf("incomplete target contract became available: code=%d output=%q diagnostics=%s", code, output.String(), diagnostics.String())
			}
		})
	}
}
