package releasetransition

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func shippedHistoryRegistry(t *testing.T) (RegistryV2, Fingerprint, Fingerprint) {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join("..", "..", "config", "release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	registry, digest, err := ParseRegistryV2(payload, BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := registry.CanonicalDigest()
	if err != nil {
		t.Fatal(err)
	}
	return registry, digest, canonical
}

// olderHistoryRegistry drops the newest migration: a predecessor that differs
// in more than retired prefixes.
func olderHistoryRegistry(registry RegistryV2) RegistryV2 {
	last := registry.Migrations[len(registry.Migrations)-1]
	older := registry
	older.Migrations = slices.Clone(registry.Migrations[:len(registry.Migrations)-1])
	older.CurrentEpochs = maps.Clone(registry.CurrentEpochs)
	older.CurrentEpochs[last.Domain] = last.FromEpoch
	return older
}

// historySnapshots returns the authoritative ledger and checkpoint for one shape.
func historySnapshots(t *testing.T, registry RegistryV2, digest Fingerprint, shape string) (ProtectedSnapshot, ProtectedSnapshot) {
	t.Helper()
	store, err := NewPOSIXV2Store(testkit.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	read := func() (ProtectedSnapshot, ProtectedSnapshot) {
		ledger, err := store.ReadLedger()
		if err != nil {
			t.Fatal(err)
		}
		checkpoint, err := store.ReadMigrationCheckpoint()
		if err != nil {
			t.Fatal(err)
		}
		return ledger, checkpoint
	}
	if shape == "missing" {
		return read()
	}
	complete := BaselineLedgerV2(registry)
	for _, migration := range registry.Migrations {
		if complete, err = complete.Advance(registry, migration); err != nil {
			t.Fatal(err)
		}
	}
	projected := complete
	if shape == "advanced" {
		projected = BaselineLedgerV2(registry)
	}
	payload, _, err := MarshalLedgerV2(projected, registry)
	if err != nil {
		t.Fatal(err)
	}
	missing, _ := read()
	if err := store.CompareAndSwapLedger(missing, payload); err != nil {
		t.Fatal(err)
	}
	if shape == "ledger" {
		return read()
	}
	state, err := store.ReadMigrationLedger(registry, digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConvertMigrationLedger(state, registry, digest, registry, digest); err != nil {
		t.Fatal(err)
	}
	if shape == "advanced" {
		state, err = store.ReadMigrationLedger(registry, digest)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.CompareAndSwapMigrationLedger(state, complete, registry, digest); err != nil {
			t.Fatal(err)
		}
	}
	return read()
}

func TestHistoryCompatibilityVerdicts(t *testing.T) {
	registry, digest, canonical := shippedHistoryRegistry(t)
	other := olderHistoryRegistry(registry)
	evidence := func(shape string, reader bool, readerErr error) *HistoryEvidence {
		ledger, checkpoint := historySnapshots(t, registry, digest, shape)
		return &HistoryEvidence{Registry: registry, RegistryDigest: digest, Ledger: ledger, Checkpoint: checkpoint,
			CheckpointReader: reader, CheckpointReaderErr: readerErr}
	}
	corrupt := ProtectedSnapshot{Exists: true, Payload: []byte("{}"), Fingerprint: fingerprintPayload([]byte("{}"))}
	readerErr := errors.New("marker changed")
	for _, test := range []struct {
		name        string
		input       HistoryCompatibilityInput
		kind        HistoryCompatibilityKind
		reason      HistoryRefusalReason
		message     string
		cause       error
		predecessor bool
		registry    HistoryRegistryRelation
	}{
		{name: "registryless pair", input: HistoryCompatibilityInput{}, kind: HistoryIdentity, registry: RegistryAbsent},
		{name: "identity without history", input: HistoryCompatibilityInput{Active: &registry, Target: &registry}, kind: HistoryIdentity},
		{name: "identity ledger", input: HistoryCompatibilityInput{Active: &registry, Target: &registry, History: evidence("ledger", false, nil)}, kind: HistoryIdentity},
		{name: "identity checkpoint reader", input: HistoryCompatibilityInput{Active: &registry, Target: &registry, History: evidence("advanced", true, nil)}, kind: HistoryIdentity},
		{name: "identity matching projection", input: HistoryCompatibilityInput{Active: &registry, Target: &registry, History: evidence("checkpoint", false, nil)}, kind: HistoryIdentity},
		{name: "identity declared predecessor", input: HistoryCompatibilityInput{Active: &registry, Target: &registry, Predecessor: canonical}, kind: HistoryIdentity, predecessor: true},
		{
			name:  "registry differs skips history",
			input: HistoryCompatibilityInput{Active: &other, Target: &registry, History: &HistoryEvidence{Registry: registry, RegistryDigest: digest, Ledger: corrupt}},
			kind:  HistoryRefuse, reason: HistoryRegistryDiffers, registry: RegistryDiffers,
			message: "the selected release's migration registry differs from the active release's registry",
		},
		{
			name:  "registryless target differs",
			input: HistoryCompatibilityInput{Active: &registry},
			kind:  HistoryRefuse, reason: HistoryRegistryDiffers, registry: RegistryDiffers,
			message: "the selected release's migration registry differs from the active release's registry",
		},
		{
			name:  "declared predecessor is not admitted yet",
			input: HistoryCompatibilityInput{Active: &other, Target: &registry, Predecessor: canonical},
			kind:  HistoryRefuse, reason: HistoryRegistryDiffers, predecessor: true, registry: RegistryDiffers,
			message: "the selected release's migration registry differs from the active release's registry",
		},
		{
			name:  "unverifiable history",
			input: HistoryCompatibilityInput{Active: &registry, Target: &registry, History: &HistoryEvidence{Registry: registry, RegistryDigest: digest, Ledger: corrupt}},
			kind:  HistoryIdentity, reason: HistoryUnverifiable,
			message: "the selected release cannot verify the authoritative active migration history",
		},
		{
			name:  "missing history",
			input: HistoryCompatibilityInput{Active: &registry, Target: &registry, History: evidence("missing", true, readerErr)},
			kind:  HistoryIdentity, reason: HistoryMissing,
			message: "the authoritative active migration history is missing",
		},
		{
			name:  "unverifiable checkpoint reader",
			input: HistoryCompatibilityInput{Active: &registry, Target: &registry, History: evidence("checkpoint", false, readerErr)},
			kind:  HistoryIdentity, reason: HistoryCheckpointReaderUnverifiable, cause: readerErr,
			message: "the selected release's migration checkpoint capability cannot be verified",
		},
		{
			name:  "ledger reader ignores the checkpoint marker",
			input: HistoryCompatibilityInput{Active: &registry, Target: &registry, History: evidence("ledger", false, readerErr)},
			kind:  HistoryIdentity,
		},
		{
			name:  "projection behind checkpoint",
			input: HistoryCompatibilityInput{Active: &registry, Target: &registry, History: evidence("advanced", false, nil)},
			kind:  HistoryIdentity, reason: HistoryProjectionDiffers,
			message: "the retained release requires a legacy ledger that matches the authoritative migration checkpoint",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			verdict := HistoryCompatibility(test.input)
			if test.registry == "" {
				test.registry = RegistryIdentical
			}
			if verdict.Kind != test.kind || verdict.Registry != test.registry || verdict.DeclaredPredecessor != test.predecessor {
				t.Fatalf("verdict = %#v", verdict)
			}
			message, cause := verdict.Refusal()
			if test.reason == "" {
				if !verdict.Admitted() || len(verdict.Refusals) != 0 || message != "" || cause != nil {
					t.Fatalf("admissible history refused: %#v", verdict)
				}
				return
			}
			if verdict.Admitted() || len(verdict.Refusals) != 1 || verdict.Refusals[0].Reason != test.reason || message != test.message {
				t.Fatalf("refusal = %#v, message %q", verdict, message)
			}
			if test.cause != nil && !errors.Is(cause, test.cause) {
				t.Fatalf("refusal cause = %v", cause)
			}
			if test.reason == HistoryUnverifiable && cause == nil {
				t.Fatal("unverifiable history lost its cause")
			}
		})
	}
}

func TestHistoryCompatibilityAdmitsIdentityAndDeclaredEquivalent(t *testing.T) {
	for _, verdict := range []HistoryCompatibilityVerdict{
		{Kind: HistoryEquivalent}, {Kind: HistorySteps, DeclaredPredecessor: true}, {Kind: HistoryRefuse, DeclaredPredecessor: true},
		// Each direction of an Equivalent pair needs its own declaration.
		{Kind: HistoryEquivalent, DeclaredSuccessor: true}, {Kind: HistoryEquivalent, Successor: true, DeclaredPredecessor: true},
	} {
		kind := verdict.Kind
		verdict.Registry = RegistryDiffers
		verdict = verdict.sealed()
		message, _ := verdict.Refusal()
		// Callers that read only the refusal text must still see a refusal.
		if verdict.Admitted() || len(verdict.Refusals) != 1 || verdict.Refusals[0].Reason != HistoryRegistryDiffers ||
			message != "the selected release's migration registry differs from the active release's registry" {
			t.Fatalf("%s is admitted or unrefused before its slice: %#v", kind, verdict)
		}
	}
	if verdict := (HistoryCompatibilityVerdict{Kind: HistoryIdentity, Registry: RegistryIdentical}).sealed(); !verdict.Admitted() {
		t.Fatalf("identity gained a refusal: %#v", verdict)
	}
	if verdict := (HistoryCompatibilityVerdict{Kind: HistoryEquivalent, Registry: RegistryDiffers, DeclaredPredecessor: true}).sealed(); !verdict.Admitted() {
		t.Fatalf("declared equivalent predecessor gained a refusal: %#v", verdict)
	}
	if verdict := (HistoryCompatibilityVerdict{Kind: HistoryEquivalent, Registry: RegistryDiffers, Successor: true, DeclaredSuccessor: true}).sealed(); !verdict.Admitted() {
		t.Fatalf("declaring equivalent successor gained a refusal: %#v", verdict)
	}
	first, second := errors.New("first cause"), errors.New("second cause")
	verdict := HistoryCompatibilityVerdict{Kind: HistoryIdentity, Refusals: []HistoryRefusal{
		{Reason: HistoryMissing, Message: "first", Cause: first},
		{Reason: HistoryProjectionDiffers, Message: "second", Cause: second},
	}}
	message, cause := verdict.Refusal()
	if verdict.Admitted() || message != "first; second" || !errors.Is(cause, first) || !errors.Is(cause, second) {
		t.Fatalf("collected refusal = %q, %v", message, cause)
	}
}

func TestRollbackDeclarationContract(t *testing.T) {
	shipped, err := os.ReadFile(filepath.Join("..", "..", "config", "release-rollback.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseRollbackDeclaration(shipped); err != nil {
		t.Fatalf("shipped rollback declaration: %v", err)
	}
	valid := `{"schemaVersion":1,"predecessorRegistryDigest":"` + strings.Repeat("a", 64) + `","migrations":[],"profileFormats":[]}`
	if _, err := ParseRollbackDeclaration([]byte(valid)); err != nil {
		t.Fatalf("valid declaration: %v", err)
	}
	for name, payload := range map[string]string{
		"empty":            "",
		"schema":           strings.Replace(valid, `"schemaVersion":1`, `"schemaVersion":2`, 1),
		"short digest":     strings.Replace(valid, strings.Repeat("a", 64), "aaaa", 1),
		"uppercase digest": strings.Replace(valid, strings.Repeat("a", 64), strings.Repeat("A", 64), 1),
		"missing lists":    `{"schemaVersion":1,"predecessorRegistryDigest":"` + strings.Repeat("a", 64) + `"}`,
		"null list":        strings.Replace(valid, `"migrations":[]`, `"migrations":null`, 1),
		"migration entry":  strings.Replace(valid, `"migrations":[]`, `"migrations":[{"id":"x"}]`, 1),
		"profile entry":    strings.Replace(valid, `"profileFormats":[]`, `"profileFormats":[{}]`, 1),
		"unknown field":    strings.Replace(valid, `{`, `{"extra":true,`, 1),
		"trailing":         valid + "{}",
		"oversize":         valid + strings.Repeat(" ", MaxRollbackDeclarationBytes),
	} {
		if _, err := ParseRollbackDeclaration([]byte(payload)); err == nil {
			t.Errorf("%s declaration was accepted", name)
		}
	}
}
