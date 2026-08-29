package migration

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
)

func TestV1ImportReaderSelectsPublished041WithoutMutatingLegacyInput(t *testing.T) {
	options, journalPath, _ := publishedV1CompatibilityFixture(t, "0.4.1")
	before, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(options.ConfigHome, "migrations", "state.json")
	stateBefore, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	stateBeforeInfo, err := os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}

	var observed V1ImportFactExpectation
	reader, err := NewV1ImportReader(V1ImportOptions{
		ConfigHome:  options.ConfigHome,
		RuntimeRoot: options.RuntimeRoot,
		Facts: V1ImportFactObserverFunc(func(
			_ context.Context,
			expectation V1ImportFactExpectation,
		) (releasetransition.Fingerprint, error) {
			observed = expectation
			return releasetransition.Fingerprint(
				"cc3eb4b78d8df266ad9aa0d4b1714f5f510466d22aa00653e3c54d8c1ee3a20e",
			), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	result, found, err := reader.InspectFresh(context.Background(), V1ImportRuntimePair{
		Current:  testPublishedV1041Runtime,
		Previous: testPublishedV1PreviousRuntime,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("InspectFresh() did not select the published 0.4.1 journal")
	}
	if result.Release != V1ImportRelease041 ||
		result.StaticDigest == "" || result.FactDigest == "" ||
		result.StaticDigest == result.FactDigest {
		t.Fatalf("InspectFresh() = %#v", result)
	}
	if observed.Release != V1ImportRelease041 ||
		observed.Checkpoint != V1ImportCheckpoint041OwnerRollingBack ||
		observed.Transaction != result.Transaction {
		t.Fatalf("fact expectation = %#v, result = %#v", observed, result)
	}

	after, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) || afterInfo.Mode() != beforeInfo.Mode() ||
		!os.SameFile(beforeInfo, afterInfo) {
		t.Fatal("InspectFresh() mutated the legacy journal")
	}
	stateAfter, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	stateAfterInfo, err := os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stateAfter, stateBefore) || stateAfterInfo.Mode() != stateBeforeInfo.Mode() ||
		!os.SameFile(stateBeforeInfo, stateAfterInfo) {
		t.Fatal("InspectFresh() mutated legacy global state")
	}
}

func TestV1ImportProductionFactObserverAcceptsPublishedFacts(t *testing.T) {
	options, _, _ := publishedV1CompatibilityFixture(t, "0.4.2")
	observer, err := NewV1ImportFactObserver(options)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := NewV1ImportReader(V1ImportOptions{
		ConfigHome: options.ConfigHome, RuntimeRoot: options.RuntimeRoot, Facts: observer,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, found, err := reader.InspectFresh(context.Background(), V1ImportRuntimePair{
		Current: testPublishedV1042Runtime, Previous: testPublishedV1PreviousRuntime,
	})
	if err != nil || !found || !validV1ImportFingerprint(result.FactDigest) {
		t.Fatalf("production fact observation = %#v, found=%v, err=%v", result, found, err)
	}
}

func TestV1ImportReaderRejectsExpandedRetainedPublishedRegistry(t *testing.T) {
	options, _, _ := publishedV1CompatibilityFixture(t, "0.4.1")
	registryPath := filepath.Join(
		options.RuntimeRoot, testPublishedV1041Runtime, "config", "migrations.json",
	)
	registry, err := LoadRegistry(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	registry.CurrentLayout = 3
	registry.Migrations = append(registry.Migrations, Definition{
		ID: "refresh-test-vm-broker", FromLayout: 2, ToLayout: 3,
		Resources:      []string{"test-vm-broker-runtime"},
		FinalizePolicy: orderedFinalizePolicy, RollbackPolicy: orderedRollbackPolicy,
		Operations: []Operation{{
			ID: "test-vm-broker-runtime", Kind: OperationKindTestVMBrokerRuntimeV1,
		}},
	})
	writeRegistryFixture(t, registryPath, registry)

	reader := newV1ImportReaderFixture(t, options)
	_, _, err = reader.InspectFresh(context.Background(), V1ImportRuntimePair{
		Current: testPublishedV1041Runtime, Previous: testPublishedV1PreviousRuntime,
	})
	if err == nil {
		t.Fatal("InspectFresh() accepted an expanded retained 0.4.1 registry")
	}
}

func newV1ImportReaderFixture(t *testing.T, options ReleaseOptions) *V1ImportReader {
	t.Helper()
	reader, err := NewV1ImportReader(V1ImportOptions{
		ConfigHome: options.ConfigHome, RuntimeRoot: options.RuntimeRoot,
		Facts: V1ImportFactObserverFunc(func(
			context.Context,
			V1ImportFactExpectation,
		) (releasetransition.Fingerprint, error) {
			return releasetransition.Fingerprint(
				"cc3eb4b78d8df266ad9aa0d4b1714f5f510466d22aa00653e3c54d8c1ee3a20e",
			), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return reader
}

func TestV1ImportReaderAcceptsOnlyPublishedCheckpointVectors(t *testing.T) {
	tests := []struct {
		name       string
		version    string
		phases     []string
		terminal   bool
		checkpoint V1ImportCheckpoint
	}{
		{
			name: "041 owner rolling back", version: "0.4.1",
			phases:     []string{operationRollingBack, operationRolledBack},
			checkpoint: V1ImportCheckpoint041OwnerRollingBack,
		},
		{
			name: "041 owner rolled back", version: "0.4.1",
			phases:     []string{operationRolledBack, operationRolledBack},
			checkpoint: V1ImportCheckpoint041OwnerRolledBack,
		},
		{
			name: "041 terminal", version: "0.4.1", terminal: true,
			phases:     []string{operationRolledBack, operationRolledBack},
			checkpoint: V1ImportCheckpoint041Terminal,
		},
		{
			name: "042 broker rolling back", version: "0.4.2",
			phases:     []string{operationCommitted, operationCommitted, operationRollingBack},
			checkpoint: V1ImportCheckpoint042BrokerRollingBack,
		},
		{
			name: "042 broker rolled back", version: "0.4.2",
			phases:     []string{operationCommitted, operationCommitted, operationRolledBack},
			checkpoint: V1ImportCheckpoint042BrokerRolledBack,
		},
		{
			name: "042 routes rolling back", version: "0.4.2",
			phases:     []string{operationCommitted, operationRollingBack, operationRolledBack},
			checkpoint: V1ImportCheckpoint042RoutesRollingBack,
		},
		{
			name: "042 routes rolled back", version: "0.4.2",
			phases:     []string{operationCommitted, operationRolledBack, operationRolledBack},
			checkpoint: V1ImportCheckpoint042RoutesRolledBack,
		},
		{
			name: "042 owner rolling back", version: "0.4.2",
			phases:     []string{operationRollingBack, operationRolledBack, operationRolledBack},
			checkpoint: V1ImportCheckpoint042OwnerRollingBack,
		},
		{
			name: "042 owner rolled back", version: "0.4.2",
			phases:     []string{operationRolledBack, operationRolledBack, operationRolledBack},
			checkpoint: V1ImportCheckpoint042OwnerRolledBack,
		},
		{
			name: "042 terminal", version: "0.4.2", terminal: true,
			phases:     []string{operationRolledBack, operationRolledBack, operationRolledBack},
			checkpoint: V1ImportCheckpoint042Terminal,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options, journalPath, _ := publishedV1CompatibilityFixture(t, test.version)
			tx := readV1CompatibilityTransaction(t, journalPath)
			for index, phase := range test.phases {
				tx.Operations[index].Phase = phase
			}
			if test.terminal {
				tx.Phase = "rolled-back"
			}
			writeV1CompatibilityTransaction(t, journalPath, tx)

			result, found, err := newV1ImportReaderFixture(t, options).InspectFresh(
				context.Background(),
				V1ImportRuntimePair{Current: tx.ToRuntime, Previous: tx.FromRuntime},
			)
			if err != nil || !found {
				t.Fatalf("InspectFresh() found=%t err=%v", found, err)
			}
			if result.Checkpoint != test.checkpoint {
				t.Fatalf("checkpoint = %q, want %q", result.Checkpoint, test.checkpoint)
			}
		})
	}
}

func TestV1ImportReaderBoundResumeUsesJournaledPairAfterActivation(t *testing.T) {
	options, _, _ := publishedV1CompatibilityFixture(t, "0.4.2")
	reader := newV1ImportReaderFixture(t, options)
	pair := V1ImportRuntimePair{
		Current: testPublishedV1042Runtime, Previous: testPublishedV1PreviousRuntime,
	}
	fresh, found, err := reader.InspectFresh(context.Background(), pair)
	if err != nil || !found {
		t.Fatalf("InspectFresh() found=%t err=%v", found, err)
	}

	activatedCurrent := "releases/0.5.0-test-release"
	replaceV1CompatibilityRuntimeLink(t, options.RuntimeRoot, "current", activatedCurrent)
	replaceV1CompatibilityRuntimeLink(t, options.RuntimeRoot, "previous", testPublishedV1042Runtime)
	if result, found, err := reader.InspectFresh(context.Background(), V1ImportRuntimePair{
		Current: activatedCurrent, Previous: testPublishedV1042Runtime,
	}); err != nil || found || result.Release != "" || result.Transaction != "" ||
		result.StaticDigest != "" || result.FactDigest != "" {
		t.Fatalf("candidate InspectFresh() result=%#v found=%t err=%v", result, found, err)
	}

	bound, err := reader.InspectBound(context.Background(), pair)
	if err != nil {
		t.Fatal(err)
	}
	if bound.StaticDigest != fresh.StaticDigest || bound.FactDigest != fresh.FactDigest {
		t.Fatalf("InspectBound() = %#v, fresh = %#v", bound, fresh)
	}
}

func TestV1ImportReaderBoundStaticResumeDoesNotObserveMutableFacts(t *testing.T) {
	options, _, _ := publishedV1CompatibilityFixture(t, "0.4.2")
	pair := V1ImportRuntimePair{
		Current: testPublishedV1042Runtime, Previous: testPublishedV1PreviousRuntime,
	}
	full := newV1ImportReaderFixture(t, options)
	before, found, err := full.InspectFresh(context.Background(), pair)
	if err != nil || !found {
		t.Fatalf("InspectFresh() found=%t err=%v", found, err)
	}

	reader, err := NewV1ImportReader(V1ImportOptions{
		ConfigHome: options.ConfigHome, RuntimeRoot: options.RuntimeRoot,
		Facts: V1ImportFactObserverFunc(func(
			context.Context,
			V1ImportFactExpectation,
		) (releasetransition.Fingerprint, error) {
			return "", os.ErrPermission
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	static, err := reader.InspectBoundStatic(context.Background(), pair)
	if err != nil {
		t.Fatal(err)
	}
	if static.StaticDigest != before.StaticDigest || static.FactDigest != "" {
		t.Fatalf("static result=%#v, full result=%#v", static, before)
	}
}

func TestV1ImportReaderBoundStaticResumeReportsJournalAndRegistryDrift(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*testing.T, ReleaseOptions, string)
		wantError bool
	}{
		{
			name: "journal",
			mutate: func(t *testing.T, _ ReleaseOptions, journalPath string) {
				t.Helper()
				tx := readV1CompatibilityTransaction(t, journalPath)
				tx.Operations[0].Phase = operationRolledBack
				writeV1CompatibilityTransaction(t, journalPath, tx)
			},
		},
		{
			name:      "registry",
			wantError: true,
			mutate: func(t *testing.T, options ReleaseOptions, _ string) {
				t.Helper()
				registryPath := filepath.Join(
					options.RuntimeRoot, testPublishedV1041Runtime, "config", "migrations.json",
				)
				registry, err := LoadRegistry(registryPath)
				if err != nil {
					t.Fatal(err)
				}
				registry.Migrations[0].Resources = []string{"test-yard-owner"}
				writeRegistryFixture(t, registryPath, registry)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options, journalPath, _ := publishedV1CompatibilityFixture(t, "0.4.1")
			reader := newV1ImportReaderFixture(t, options)
			pair := V1ImportRuntimePair{
				Current: testPublishedV1041Runtime, Previous: testPublishedV1PreviousRuntime,
			}
			original, found, err := reader.InspectFresh(context.Background(), pair)
			if err != nil || !found {
				t.Fatalf("InspectFresh() found=%t err=%v", found, err)
			}
			test.mutate(t, options, journalPath)
			observed, err := reader.InspectBoundStatic(context.Background(), pair)
			if test.wantError {
				if err == nil {
					t.Fatal("InspectBoundStatic() accepted a changed retained registry")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if observed.StaticDigest == original.StaticDigest || observed.FactDigest != "" {
				t.Fatalf("observed=%#v original=%#v", observed, original)
			}
		})
	}
}

func TestV1ImportReaderRejectsMalformedReorderedMultipleAndUnsafeInputs(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*testing.T, ReleaseOptions, string)
	}{
		{
			name: "malformed journal",
			prepare: func(t *testing.T, _ ReleaseOptions, journalPath string) {
				t.Helper()
				if err := os.WriteFile(journalPath, []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "reordered operations",
			prepare: func(t *testing.T, _ ReleaseOptions, journalPath string) {
				t.Helper()
				tx := readV1CompatibilityTransaction(t, journalPath)
				tx.Operations[0], tx.Operations[1] = tx.Operations[1], tx.Operations[0]
				writeV1CompatibilityTransaction(t, journalPath, tx)
			},
		},
		{
			name:    "multiple published journals",
			prepare: prepareMultiplePublishedV1ImportJournals,
		},
		{
			name: "unsafe journal symlink",
			prepare: func(t *testing.T, _ ReleaseOptions, journalPath string) {
				t.Helper()
				if err := os.Remove(journalPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("foreign.json", journalPath); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "unsafe retained registry symlink",
			prepare: func(t *testing.T, options ReleaseOptions, _ string) {
				t.Helper()
				registryPath := filepath.Join(
					options.RuntimeRoot, testPublishedV1042Runtime, "config", "migrations.json",
				)
				if err := os.Remove(registryPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("foreign.json", registryPath); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options, journalPath, _ := publishedV1CompatibilityFixture(t, "0.4.2")
			test.prepare(t, options, journalPath)
			_, _, err := newV1ImportReaderFixture(t, options).InspectFresh(
				context.Background(),
				V1ImportRuntimePair{
					Current: testPublishedV1042Runtime, Previous: testPublishedV1PreviousRuntime,
				},
			)
			if err == nil {
				t.Fatal("InspectFresh() accepted unsafe or ambiguous legacy input")
			}
		})
	}
}

func TestV1ImportReaderSeparatesStaticAndFactDigests(t *testing.T) {
	options, _, _ := publishedV1CompatibilityFixture(t, "0.4.1")
	pair := V1ImportRuntimePair{
		Current: testPublishedV1041Runtime, Previous: testPublishedV1PreviousRuntime,
	}
	first := newV1ImportReaderWithFactsFixture(t, options,
		"cc3eb4b78d8df266ad9aa0d4b1714f5f510466d22aa00653e3c54d8c1ee3a20e")
	second := newV1ImportReaderWithFactsFixture(t, options,
		"09b65312a9065eead61e23d6e62599a71f90a3e916903cf268bd7100e93c18a7")
	left, found, err := first.InspectFresh(context.Background(), pair)
	if err != nil || !found {
		t.Fatalf("first InspectFresh() found=%t err=%v", found, err)
	}
	right, found, err := second.InspectFresh(context.Background(), pair)
	if err != nil || !found {
		t.Fatalf("second InspectFresh() found=%t err=%v", found, err)
	}
	if left.StaticDigest != right.StaticDigest || left.FactDigest == right.FactDigest {
		t.Fatalf("left=%#v right=%#v", left, right)
	}
}

func prepareMultiplePublishedV1ImportJournals(
	t *testing.T,
	options ReleaseOptions,
	journalPath string,
) {
	t.Helper()
	tx := readV1CompatibilityTransaction(t, journalPath)
	tx.ToRelease = "0.4.1"
	tx.ToRuntime = testPublishedV1041Runtime
	tx.ToLayout = 2
	tx.Migrations = []string{"migrate-test-yard-owner"}
	tx.Operations = tx.Operations[:2]
	tx.Operations[0].Phase = operationRollingBack
	tx.Operations[1].Phase = operationRolledBack
	registryPath := filepath.Join(options.RuntimeRoot, tx.ToRuntime, "config", "migrations.json")
	if err := os.MkdirAll(filepath.Dir(registryPath), 0o700); err != nil {
		t.Fatal(err)
	}
	registry, err := LoadRegistry(filepath.Join(
		options.RuntimeRoot, testPublishedV1042Runtime, "config", "migrations.json",
	))
	if err != nil {
		t.Fatal(err)
	}
	registry.CurrentLayout = 2
	registry.Migrations = registry.Migrations[:1]
	writeRegistryFixture(t, registryPath, registry)
	writeV1CompatibilityTransaction(t, transactionPath(options.ConfigHome, tx.ToRelease), tx)
}

func newV1ImportReaderWithFactsFixture(
	t *testing.T,
	options ReleaseOptions,
	facts releasetransition.Fingerprint,
) *V1ImportReader {
	t.Helper()
	reader, err := NewV1ImportReader(V1ImportOptions{
		ConfigHome: options.ConfigHome, RuntimeRoot: options.RuntimeRoot,
		Facts: V1ImportFactObserverFunc(func(
			context.Context,
			V1ImportFactExpectation,
		) (releasetransition.Fingerprint, error) {
			return facts, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return reader
}
