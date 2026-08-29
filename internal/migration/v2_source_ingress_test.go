package migration

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
)

func TestV2SourceIngressPlansProspectiveSettingsAndResumesItsTwoLeafSteps(t *testing.T) {
	requireJQ(t)
	fixture := newSourceInstallFixture(t,
		"# Stable launcher for a release-installed native Go control-plane engine.")
	writeTestFile(t, filepath.Join(fixture.source, "private/config.env"), 0o600, "DEV_SUDO=1\n")
	writeTestFile(t, filepath.Join(fixture.data, "config.env"), 0o600, "DEV_SUDO=1\n")
	descriptor := sourceIngressDescriptor(fixture)
	candidateRoot := filepath.Join(fixture.data, "runtime/releases/release-current")
	ingress, err := newV2SourceIngress(V2SourceIngressOptions{
		Descriptor: descriptor, RepositoryRoot: candidateRoot,
		RuntimeRoot: filepath.Join(fixture.data, "runtime"), ConfigHome: fixture.config,
		Environment: append(os.Environ(),
			"HOME=/untrusted",
			"TEST_DATA_HOME="+fixture.data,
			"TEST_CONFIG_HOME="+fixture.config,
			"TEST_SOURCE_ROOT="+fixture.source,
		),
	}, fixture.home, nil)
	if err != nil {
		t.Fatal(err)
	}

	planned, err := ingress.Inspect(context.Background(), nil)
	if err != nil || len(planned.Operations) != 2 || planned.Prospective == nil {
		t.Fatalf("source ingress plan = %#v, err=%v", planned, err)
	}
	if planned.Operations[0].Kind != releasetransition.V2SourceImport ||
		planned.Operations[1].Kind != releasetransition.V2SourceEntrypoints {
		t.Fatalf("source ingress order = %#v", planned.Operations)
	}
	snapshot, err := planned.Prospective.ReadSnapshot(
		filepath.Join(fixture.config, "yards/named/config.env"),
	)
	if err != nil || !snapshot.Exists ||
		!strings.Contains(string(snapshot.Content), "YARD_TEMPLATE=test-vms") {
		t.Fatalf("prospective source settings = %#v, err=%v", snapshot, err)
	}

	binding := releasetransition.V2IngressBinding{
		Transaction: "tx-source-test-001",
		Plan:        releasetransition.PlanToken("plan-v1-" + strings.Repeat("a", 64)),
		Releases:    releasetransition.ReleasePair{From: "release-old", Target: "release-current"},
	}
	for _, operation := range planned.Operations {
		step := releasetransition.V2IngressStep{
			Binding: binding, Kind: operation.Kind,
			Expected: operation.Expected, Desired: operation.Desired,
		}
		if err := ingress.Apply(context.Background(), step); err != nil {
			t.Fatalf("apply %s: %v", operation.Kind, err)
		}
		if err := ingress.Verify(context.Background(), step); err != nil {
			t.Fatalf("verify %s: %v", operation.Kind, err)
		}
	}
	if final, err := ingress.Inspect(context.Background(), nil); err != nil || len(final.Operations) != 0 {
		t.Fatalf("completed source ingress = %#v, err=%v", final, err)
	}
	assertRuntimeEntrypoints(t, fixture)
}

func TestV2SourceIngressReportsPendingOperationsMissingFromOuterBinding(t *testing.T) {
	requireJQ(t)
	fixture := newSourceInstallFixture(t,
		"# Stable launcher for a release-installed native Go control-plane engine.")
	writeTestFile(t, filepath.Join(fixture.source, "private/config.env"), 0o600, "DEV_SUDO=1\n")
	writeTestFile(t, filepath.Join(fixture.data, "config.env"), 0o600, "DEV_SUDO=1\n")
	ingress, err := newV2SourceIngress(V2SourceIngressOptions{
		Descriptor: sourceIngressDescriptor(fixture),
		RepositoryRoot: filepath.Join(
			fixture.data, "runtime/releases/release-current",
		),
		RuntimeRoot: filepath.Join(fixture.data, "runtime"), ConfigHome: fixture.config,
		Environment: append(os.Environ(),
			"HOME=/untrusted",
			"TEST_DATA_HOME="+fixture.data,
			"TEST_CONFIG_HOME="+fixture.config,
			"TEST_SOURCE_ROOT="+fixture.source,
		),
	}, fixture.home, func(context.Context, releasetransition.V2IngressStep) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := ingress.Inspect(context.Background(), nil)
	if err != nil || len(fresh.Operations) != 2 {
		t.Fatalf("fresh source inspection = %#v, err=%v", fresh, err)
	}
	binding := &releasetransition.V2IngressBinding{
		Transaction: "tx-source-test-001",
		Plan:        releasetransition.PlanToken("plan-v1-" + strings.Repeat("a", 64)),
		Releases:    releasetransition.ReleasePair{From: "release-old", Target: "release-current"},
		Steps: []releasetransition.V2IngressStepBinding{{
			Kind:     releasetransition.V2SourceImport,
			Expected: fresh.Operations[0].Expected, Desired: fresh.Operations[0].Desired,
		}},
	}
	inspection, err := ingress.Inspect(context.Background(), binding)
	if err != nil || len(inspection.Operations) != 2 ||
		inspection.Operations[0].Kind != releasetransition.V2SourceImport ||
		inspection.Operations[1].Kind != releasetransition.V2SourceEntrypoints {
		t.Fatalf("bound source inspection = %#v, err=%v", inspection, err)
	}

	compatibility := &V2CompatibilityIngress{Source: ingress}
	binding.Steps = nil
	inspection, err = compatibility.Inspect(context.Background(), binding)
	if err != nil || len(inspection.Operations) != 2 {
		t.Fatalf("compatibility source inspection = %#v, err=%v", inspection, err)
	}
}

func TestV2SourceIngressObserveRejectsDriftFromJournaledResults(t *testing.T) {
	requireJQ(t)
	for _, test := range []struct {
		name  string
		apply int
		drift func(t *testing.T, fixture sourceInstallFixture)
	}{
		{
			name:  "imported config",
			apply: 1,
			drift: func(t *testing.T, fixture sourceInstallFixture) {
				writeTestFile(t, filepath.Join(fixture.config, "config.env"), 0o600, "drift\n")
			},
		},
		{
			name:  "shell integration",
			apply: 2,
			drift: func(t *testing.T, fixture sourceInstallFixture) {
				writeTestFile(t, fixture.rc, 0o600, "drift\n")
			},
		},
		{
			name:  "runtime entrypoint",
			apply: 2,
			drift: func(t *testing.T, fixture sourceInstallFixture) {
				path := filepath.Join(fixture.bin, "yard")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(fixture.source, "bin/yard"), path); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSourceInstallFixture(t,
				"# Stable launcher for a release-installed native Go control-plane engine.")
			writeTestFile(t, filepath.Join(fixture.source, "private/config.env"), 0o600, "DEV_SUDO=1\n")
			writeTestFile(t, filepath.Join(fixture.data, "config.env"), 0o600, "DEV_SUDO=1\n")
			ingress, err := newV2SourceIngress(V2SourceIngressOptions{
				Descriptor: sourceIngressDescriptor(fixture),
				RepositoryRoot: filepath.Join(
					fixture.data, "runtime/releases/release-current",
				),
				RuntimeRoot: filepath.Join(fixture.data, "runtime"), ConfigHome: fixture.config,
				Environment: append(os.Environ(),
					"HOME=/untrusted",
					"TEST_DATA_HOME="+fixture.data,
					"TEST_CONFIG_HOME="+fixture.config,
					"TEST_SOURCE_ROOT="+fixture.source,
				),
			}, fixture.home, nil)
			if err != nil {
				t.Fatal(err)
			}
			planned, err := ingress.Inspect(context.Background(), nil)
			if err != nil || len(planned.Operations) != 2 {
				t.Fatalf("source ingress plan = %#v, err=%v", planned, err)
			}
			binding := releasetransition.V2IngressBinding{
				Transaction: "tx-source-test-001",
				Plan:        releasetransition.PlanToken("plan-v1-" + strings.Repeat("a", 64)),
				Releases:    releasetransition.ReleasePair{From: "release-old", Target: "release-current"},
			}
			steps := make([]releasetransition.V2IngressStep, len(planned.Operations))
			for index, operation := range planned.Operations {
				steps[index] = releasetransition.V2IngressStep{
					Binding: binding, Kind: operation.Kind,
					Expected: operation.Expected, Desired: operation.Desired,
				}
			}
			for _, step := range steps[:test.apply] {
				if err := ingress.Apply(context.Background(), step); err != nil {
					t.Fatalf("apply %s: %v", step.Kind, err)
				}
			}
			test.drift(t, fixture)
			if _, err := ingress.Observe(context.Background(), steps[test.apply-1]); err == nil {
				t.Fatal("source ingress accepted drift from its journaled result")
			}
		})
	}
}

func TestV2SourceIngressResumesPredecessorStateMigrationJournal(t *testing.T) {
	requireJQ(t)
	fixture := newSourceInstallFixture(t,
		"# Stable launcher for a release-installed native Go control-plane engine.")
	writeTestFile(t, filepath.Join(fixture.source, "private/config.env"), 0o600, "DEV_SUDO=1\n")
	writeTestFile(t, filepath.Join(fixture.data, "config.env"), 0o600, "DEV_SUDO=1\n")
	if output, err := fixture.runSourceOperation("import", "source-import-ready"); err == nil {
		t.Fatalf("source import fault did not interrupt the fixture: %s", output)
	}
	recovery := filepath.Join(fixture.data, "recovery/pre-go-source")
	for _, name := range []string{
		"outer-transaction", "outer-plan", "source-plan", "source-import.state",
	} {
		if err := os.Remove(filepath.Join(recovery, name)); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, filepath.Join(recovery, "transaction"), 0o600,
		"schema=1\nphase=applying\nstep=state-migration\n")

	ingress, err := newV2SourceIngress(V2SourceIngressOptions{
		Descriptor: sourceIngressDescriptor(fixture),
		RepositoryRoot: filepath.Join(
			fixture.data, "runtime/releases/release-current",
		),
		RuntimeRoot: filepath.Join(fixture.data, "runtime"), ConfigHome: fixture.config,
		Environment: append(os.Environ(),
			"HOME=/untrusted",
			"TEST_DATA_HOME="+fixture.data,
			"TEST_CONFIG_HOME="+fixture.config,
			"TEST_SOURCE_ROOT="+fixture.source,
		),
	}, fixture.home, nil)
	if err != nil {
		t.Fatal(err)
	}
	planned, err := ingress.Inspect(context.Background(), nil)
	if err != nil || len(planned.Operations) != 2 {
		t.Fatalf("predecessor source plan = %#v, err=%v", planned, err)
	}
	binding := releasetransition.V2IngressBinding{
		Transaction: "tx-source-test-002",
		Plan:        releasetransition.PlanToken("plan-v1-" + strings.Repeat("b", 64)),
		Releases:    releasetransition.ReleasePair{From: "release-old", Target: "release-current"},
	}
	for _, operation := range planned.Operations {
		step := releasetransition.V2IngressStep{
			Binding: binding, Kind: operation.Kind,
			Expected: operation.Expected, Desired: operation.Desired,
		}
		if err := ingress.Apply(context.Background(), step); err != nil {
			t.Fatalf("resume predecessor %s: %v", operation.Kind, err)
		}
		if err := ingress.Verify(context.Background(), step); err != nil {
			t.Fatalf("verify predecessor %s: %v", operation.Kind, err)
		}
	}
	assertRuntimeEntrypoints(t, fixture)
}

func TestV2SourceIngressRejectsInconsistentRecoveryTransaction(t *testing.T) {
	recovery := t.TempDir()
	writeTestFile(t, filepath.Join(recovery, "transaction"), 0o600,
		"schema=1\nphase=complete\nstep=source-import-ready\n")
	ingress := V2SourceIngress{recoveryRoot: recovery}
	if _, err := ingress.readRecoveryPhase(); err == nil || !strings.Contains(err.Error(), "transaction") {
		t.Fatalf("inconsistent recovery transaction error = %v", err)
	}
}

func TestV2SourceIngressRejectsPathsOutsideTheAccountHome(t *testing.T) {
	fixture := newSourceInstallFixture(t,
		"# Stable launcher for a release-installed native Go control-plane engine.")
	descriptor := sourceIngressDescriptor(fixture)
	descriptor.RC = filepath.Join(filepath.Dir(fixture.home), "foreign.rc")
	_, err := newV2SourceIngress(V2SourceIngressOptions{
		Descriptor:     descriptor,
		RepositoryRoot: filepath.Join(fixture.data, "runtime/releases/release-current"),
		RuntimeRoot:    filepath.Join(fixture.data, "runtime"), ConfigHome: fixture.config,
	}, fixture.home, func(context.Context, releasetransition.V2IngressStep) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "operating-system account home") {
		t.Fatalf("outside-home source ingress error = %v", err)
	}
}

func TestV2SourceIngressAcceptsPinnedCandidateRoot(t *testing.T) {
	fixture := newSourceInstallFixture(t,
		"# Stable launcher for a release-installed native Go control-plane engine.")
	candidateRoot := filepath.Join(fixture.data, "runtime/releases/release-current")
	candidate, err := os.Open(candidateRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Close()
	pinnedRoot := "/proc/self/fd/" + strconv.FormatUint(uint64(candidate.Fd()), 10)
	if _, err := os.Stat(pinnedRoot); err != nil {
		t.Skipf("descriptor paths are unavailable: %v", err)
	}

	_, err = newV2SourceIngress(V2SourceIngressOptions{
		Descriptor: sourceIngressDescriptor(fixture), RepositoryRoot: pinnedRoot,
		RuntimeRoot: filepath.Join(fixture.data, "runtime"), ConfigHome: fixture.config,
	}, fixture.home, func(context.Context, releasetransition.V2IngressStep) error { return nil })
	if err != nil {
		t.Fatalf("pinned candidate root was rejected: %v", err)
	}
	ordinaryLink := filepath.Join(fixture.home, "candidate-link")
	if err := os.Symlink(candidateRoot, ordinaryLink); err != nil {
		t.Fatal(err)
	}
	_, err = newV2SourceIngress(V2SourceIngressOptions{
		Descriptor: sourceIngressDescriptor(fixture), RepositoryRoot: ordinaryLink,
		RuntimeRoot: filepath.Join(fixture.data, "runtime"), ConfigHome: fixture.config,
	}, fixture.home, func(context.Context, releasetransition.V2IngressStep) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "managed release store") {
		t.Fatalf("ordinary candidate symlink error = %v", err)
	}
	insideLink := filepath.Join(fixture.data, "runtime/releases/candidate-link")
	if err := os.Symlink(fixture.source, insideLink); err != nil {
		t.Fatal(err)
	}
	_, err = newV2SourceIngress(V2SourceIngressOptions{
		Descriptor: sourceIngressDescriptor(fixture), RepositoryRoot: insideLink,
		RuntimeRoot: filepath.Join(fixture.data, "runtime"), ConfigHome: fixture.config,
	}, fixture.home, func(context.Context, releasetransition.V2IngressStep) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "managed release store") {
		t.Fatalf("in-store escaping candidate symlink error = %v", err)
	}
}

func sourceIngressDescriptor(fixture sourceInstallFixture) releasetransition.SourceIngressRequest {
	return releasetransition.SourceIngressRequest{
		SchemaVersion: releasetransition.SourceIngressRequestSchemaV1,
		Kind:          releasetransition.SourceIngressPreGoV1,
		SourceRoot:    fixture.source,
		DataHome:      fixture.data,
		BinDir:        fixture.bin,
		RC:            fixture.rc,
		LoginRC:       fixture.login,
	}
}
