package releasetransition

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"github.com/Subyard/Subyard/internal/testkit"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestLegacyCodecFrames(t *testing.T) {
	frame := func(seq uint64, raw []byte) []byte {
		h := make([]byte, 52)
		copy(h, "SYC1")
		h[4] = 1
		h[5] = 1
		binary.BigEndian.PutUint64(h[8:], seq)
		hash := sha256.Sum256(raw)
		copy(h[16:], hash[:])
		binary.BigEndian.PutUint32(h[48:], uint32(len(raw)))
		return append(h, raw...)
	}
	input := append(frame(1, []byte(validRegistryV2)), frame(2, []byte(validRegistryV2))...)
	var output bytes.Buffer
	if err := RunLegacyCodec(bytes.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	for seq := uint64(1); seq <= 2; seq++ {
		h := output.Next(52)
		if len(h) != 52 || h[6] != 0 || binary.BigEndian.Uint64(h[8:]) != seq {
			t.Fatal("reply correlation")
		}
		if len(output.Next(int(binary.BigEndian.Uint32(h[48:])))) == 0 {
			t.Fatal("missing projection")
		}
	}
	for _, bad := range [][]byte{frame(2, []byte(validRegistryV2)), input[:20], append(frame(1, []byte(validRegistryV2)), 1), frame(1, []byte("{} {}"))} {
		var out bytes.Buffer
		err := RunLegacyCodec(bytes.NewReader(bad), &out)
		if err == nil && (out.Len() < 52 || out.Bytes()[6] != 1) {
			t.Fatal("malformed frame accepted")
		}
	}
}
func TestRustOwnerLegacyCorpus(t *testing.T) {
	destination := os.Getenv("SUBYARD_RUST_LEGACY_CORPUS")
	if destination == "" {
		t.Skip("opt-in frozen reader comparison")
	}
	type vector struct {
		Name       string `json:"name"`
		Kind       byte   `json:"kind"`
		Raw        string `json:"raw"`
		Projection string `json:"projection"`
		Flags      byte   `json:"flags"`
		Syntax     bool   `json:"syntax"`
		Accepted   bool   `json:"accepted"`
	}
	vectors := []vector{}
	catalog := registryV2TestCatalog(t)
	var owningRegistry, owningLedgerRegistry RegistryV2
	var owningDigest Fingerprint
	var owningProjection []byte
	// Accepted projections come from the existing owning parsers, independently
	// of normalizeLegacy's implementation. This remains the one Go oracle.
	owningCanonical := func(kind byte, raw []byte) ([]byte, error) {
		var value any
		var err error
		switch kind {
		case 1:
			value, _, err = ParseRegistryV2(raw, catalog)
		case 2:
			var descriptors []CapabilityDescriptor
			err = decodeBoundedRecord(raw, LegacyCodecMaxRaw, &descriptors)
			value = descriptors
		case 3:
			value, _, err = ParseLedgerV2(raw, owningLedgerRegistry)
		case 4:
			value, _, err = ParseLedgerCheckpointV1(raw, owningRegistry, owningDigest, protectedSnapshotFromPayload(owningProjection))
		case 5:
			value, err = ParseJournal(raw)
		case 6:
			value, err = ParseEvidence(raw)
		case 7:
			value, err = ParseSupersededJournal(raw)
		case 8:
			value, err = ParseCompatibilityEvidence(raw)
		case 9:
			value, err = ParseRecoveryReceipt(raw)
		case 10:
			value, err = ParseRecoveryLifecycleRecord(raw)
		case 11:
			value, err = ParseRecoveryFrontier(raw)
		case 12:
			value, err = ParseMigrationOwnerReceipt(raw)
		case 13:
			var recovery settingsRecoveryV1
			err = decodeBoundedRecord(raw, 4096, &recovery)
			value = recovery
		case 14:
			var recovery ownerRegistrationRecoveryV1
			err = decodeBoundedRecord(raw, 4096, &recovery)
			value = recovery
		default:
			t.Fatal("closed owning oracle kind")
		}
		if err != nil {
			return nil, err
		}
		return json.Marshal(value)
	}
	add := func(name string, kind byte, raw []byte, validate func([]byte) error) {
		projection, flags, err := normalizeLegacy(kind, raw)
		accepted := validate(raw) == nil
		if accepted {
			var owningErr error
			projection, owningErr = owningCanonical(kind, raw)
			if owningErr != nil {
				t.Fatalf("owning projection %s: %v", name, owningErr)
			}
		}
		vectors = append(vectors, vector{name, kind, base64.StdEncoding.EncodeToString(raw), string(projection), flags, err == nil, accepted})
	}
	validateRegistry := func(raw []byte) error { _, _, e := ParseRegistryV2(raw, catalog); return e }
	raw := []byte(validRegistryV2)
	add("registry-fold", 1, bytes.ReplaceAll(raw, []byte("schemaVersion"), []byte("SCHEMAVERSION")), validateRegistry)
	add("registry-duplicate", 1, append([]byte(`{"schemaVersion":0,`), bytes.TrimSpace(raw)[1:]...), validateRegistry)
	add("registry-null", 1, []byte(`null`), validateRegistry)
	add("registry-scalar", 1, []byte(`1`), validateRegistry)
	add("registry-trailing", 1, append(raw, []byte(`{}`)...), validateRegistry)
	add("registry-unknown", 1, []byte(`{"foreign":1}`), validateRegistry)
	add("registry-invalid-utf8", 1, []byte("{\"schemaVersion\":2,\"minimumEpochs\":{\"\xff\":1},\"currentEpochs\":{\"\xff\":1},\"migrations\":[]}"), validateRegistry)
	add("registry-int-exact", 1, []byte(`{"schemaVersion":2,"minimumEpochs":{"settings":9007199254740993},"currentEpochs":{"settings":9007199254740993},"migrations":[]}`), validateRegistry)
	add("registry-int-overflow", 1, []byte(`{"schemaVersion":9223372036854775808}`), validateRegistry)
	catalogRaw, _ := json.Marshal(catalog.descriptors)
	add("catalog", 2, catalogRaw, func(raw []byte) error {
		var d []CapabilityDescriptor
		if e := decodeBoundedRecord(raw, LegacyCodecMaxRaw, &d); e != nil {
			return e
		}
		_, e := NewCapabilityCatalog(d)
		return e
	})
	baseRegistry, baseDigest, e := ParseRegistryV2([]byte(validRegistryV2), catalog)
	if e != nil {
		t.Fatal(e)
	}
	baseProjection, _, e := MarshalLedgerV2(BaselineLedgerV2(baseRegistry), baseRegistry)
	if e != nil {
		t.Fatal(e)
	}
	owningRegistry, owningDigest, owningProjection = baseRegistry, baseDigest, baseProjection
	checkpoint, e := newLedgerCheckpointV1(BaselineLedgerV2(baseRegistry), baseRegistry, baseDigest, protectedSnapshotFromPayload(baseProjection), baseRegistry.MinimumEpochs)
	if e != nil {
		t.Fatal(e)
	}
	checkpointRaw, _, e := MarshalLedgerCheckpointV1(checkpoint, baseRegistry, baseDigest, protectedSnapshotFromPayload(baseProjection))
	if e != nil {
		t.Fatal(e)
	}
	add("checkpoint", 4, checkpointRaw, func(raw []byte) error {
		_, _, e := ParseLedgerCheckpointV1(raw, baseRegistry, baseDigest, protectedSnapshotFromPayload(baseProjection))
		return e
	})
	journal := validJournal(releasePair())
	evidence := EvidenceRecord{SchemaVersion: 2, Transaction: journal.Transaction, Releases: journal.Releases, Step: journal.Steps[0].ID, Expected: digestA, Desired: digestB, Observed: digestA, Checkpoint: EvidenceCaptured}
	evidenceRaw, _ := MarshalEvidence(evidence)
	add("evidence", 6, evidenceRaw, func(raw []byte) error { _, e := ParseEvidence(raw); return e })
	compatibilityRaw, _ := MarshalCompatibilityEvidence(CompatibilityEvidence{SchemaVersion: 1, Kind: V2LegacyV1Import, Identity: digestA, From: "old", Previous: "previous"})
	add("compatibility", 8, compatibilityRaw, func(raw []byte) error { _, e := ParseCompatibilityEvidence(raw); return e })
	lifecycle := lifecycleReceiptFixture(t)
	complete := lifecycle.Successor
	complete.Checkpoint = JournalComplete
	terminalRaw, e := MarshalRecoveryLifecycleRecord(RecoveryLifecycleRecord{SchemaVersion: 1, Receipt: lifecycle, Completed: &complete})
	if e != nil {
		t.Fatal(e)
	}
	add("lifecycle", 10, terminalRaw, func(raw []byte) error { _, e := ParseRecoveryLifecycleRecord(raw); return e })
	frontierRaw, e := MarshalRecoveryFrontier(RecoveryFrontier{SchemaVersion: 1, Replacement: lifecycle.Replacement, Generation: 1})
	if e != nil {
		t.Fatal(e)
	}
	add("frontier", 11, frontierRaw, func(raw []byte) error { _, e := ParseRecoveryFrontier(raw); return e })
	r := recoveryReceiptFixture(t)
	receiptRaw, e := MarshalRecoveryReceipt(r)
	if e != nil {
		t.Fatal(e)
	}
	validateReceipt := func(raw []byte) error { _, e := ParseRecoveryReceipt(raw); return e }
	for _, field := range []string{`"basePlan":null,`, `"BasePlan":null,`, `"generation":0,`, `"Generation":0,`} {
		add("receipt-presence-"+field, 9, append(append([]byte{'{'}, field...), receiptRaw[1:]...), validateReceipt)
	}
	add("receipt-valid", 9, receiptRaw, validateReceipt)
	add("nested-journal-invalid-first", 9, append([]byte(`{"predecessor":{},`), receiptRaw[1:]...), validateReceipt)
	add("nested-journal-partial-replacement", 9, append(bytes.TrimSpace(receiptRaw)[:len(bytes.TrimSpace(receiptRaw))-1], []byte(`,"predecessor":{"checkpoint":"complete"}}`)...), validateReceipt)
	candidate, _, _ := migrationOwnerFixture(t)
	inspection, e := candidate.Inspect(context.Background(), Goal{Target: "release-c", Direction: DirectionActivateTarget})
	if e != nil {
		t.Fatal(e)
	}
	candidate.options.fault = func(point string) error {
		if point == "after-migration-owner-receipt" {
			return errors.New("legacy receipt cut")
		}
		return nil
	}
	candidate.Converge(context.Background(), Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
	receipts, e := candidate.store.migrationOwnerReceipts()
	if e != nil {
		t.Fatal(e)
	}
	for _, owner := range receipts {
		raw, e := MarshalMigrationOwnerReceipt(owner)
		if e != nil {
			t.Fatal(e)
		}
		validate := func(raw []byte) error { _, e := ParseMigrationOwnerReceipt(raw); return e }
		add("owner-base64", 12, raw, validate)
		var object map[string]json.RawMessage
		if e := json.Unmarshal(raw, &object); e != nil {
			t.Fatal(e)
		}
		values := make([]int, len(owner.Registry))
		for i, b := range owner.Registry {
			values[i] = int(b)
		}
		object["registry"], _ = json.Marshal(values)
		array, _ := json.Marshal(object)
		add("owner-byte-array", 12, array, validate)
		object["registry"] = json.RawMessage(`null`)
		null, _ := json.Marshal(object)
		add("owner-null-bytes", 12, null, validate)
		object["registry"] = json.RawMessage(`""`)
		empty, _ := json.Marshal(object)
		add("owner-empty-bytes", 12, empty, validate)
	}
	// A real valid owning composite exceeds the original 1MiB limit after defaults
	// are restored. All domains are baseline, so no invented registry capabilities.
	minimum := map[string]int{}
	domains := map[string]any{}
	for i := 0; i < 30000; i++ {
		name := fmtLegacyDomain(i)
		minimum[name] = 1
		domains[name] = map[string]int{"epoch": 1}
	}
	registry := RegistryV2{SchemaVersion: 2, MinimumEpochs: minimum, CurrentEpochs: minimum, Migrations: []MigrationDefinitionV2{}}
	registryRaw, _ := json.Marshal(registry)
	registryRaw = bytes.ReplaceAll(registryRaw, []byte("schemaVersion"), []byte("SCHEMAVERSION"))
	registryValue, registryDigest, e := ParseRegistryV2(registryRaw, catalog)
	if e != nil || len(registryRaw) > LegacyCodecMaxRaw {
		t.Fatalf("expansion registry: %v %d", e, len(registryRaw))
	}
	ledgerRaw, _ := json.Marshal(map[string]any{"schemaVersion": 2, "domains": domains})
	ledgerRaw = bytes.ReplaceAll(ledgerRaw, []byte("schemaVersion"), []byte("SCHEMAVERSION"))
	owningLedgerRegistry = registryValue
	ledgerValidate := func(raw []byte) error { _, _, e := ParseLedgerV2(raw, registryValue); return e }
	add("valid-ledger-output-expansion", 3, ledgerRaw, ledgerValidate)
	last := vectors[len(vectors)-1]
	if !last.Accepted || !last.Syntax || len(last.Projection) <= LegacyCodecMaxRaw {
		t.Fatalf("not valid expansion: %+v", struct {
			Accepted bool
			Size     int
		}{last.Accepted, len(last.Projection)})
	}
	type storeCase struct {
		Name, Operation, Registry, RegistryDigest string
		Files                                     map[string]string
		Accepted                                  bool
	}
	stores := []storeCase{}
	typedTransition, typedStore := completedSourceMetadataFixtureWithOwner(t, &v2TestOwnerRegistration{state: OwnerRegistrationLegacyDirectory})
	typedCurrent, e := typedStore.ReadCurrentJournal()
	if e != nil {
		t.Fatal(e)
	}
	typedJournal, e := ParseJournal(typedCurrent.Payload)
	if e != nil {
		t.Fatal(e)
	}
	for _, step := range typedJournal.Steps {
		if step.Evidence == nil || step.Evidence.Recovery == "" {
			continue
		}
		snapshot, e := typedStore.ReadRecovery(typedJournal.Transaction, step.ID)
		if e != nil {
			t.Fatal(e)
		}
		if step.Resource == ownerRegistrationResource {
			add("owner-sidecar", 14, snapshot.Payload, func(raw []byte) error {
				var value ownerRegistrationRecoveryV1
				if e := decodeBoundedRecord(raw, 4096, &value); e != nil {
					return e
				}
				return validateOwnerRegistrationRecovery(value, typedJournal, step)
			})
		} else {
			add("settings-sidecar", 13, snapshot.Payload, func(raw []byte) error {
				var value settingsRecoveryV1
				if e := decodeBoundedRecord(raw, 4096, &value); e != nil {
					return e
				}
				return validateSettingsRecovery(value, typedJournal, step)
			})
		}
	}
	typedRegistryRaw, _ := json.Marshal(typedTransition.registry)
	typedFiles := map[string]string{}
	if e := filepath.WalkDir(typedStore.configHome, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			p, e := os.ReadFile(path)
			if e != nil {
				return e
			}
			rel, _ := filepath.Rel(typedStore.configHome, path)
			typedFiles[filepath.ToSlash(rel)] = base64.StdEncoding.EncodeToString(p)
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	_, e = typedStore.InspectSourceMetadata(typedTransition.registry, typedTransition.registryDigest)
	stores = append(stores, storeCase{"typed-source", "source", base64.StdEncoding.EncodeToString(typedRegistryRaw), string(typedTransition.registryDigest), typedFiles, e == nil})

	// Real source admission on alternate registry/ledger spelling, retaining raw pins.
	home := protectedConfigHome(t)
	st, _ := NewPOSIXV2Store(home)
	if e := rustPrivateDirs(filepath.Join(home, "release-transition/v2"), 0700); e != nil {
		t.Fatal(e)
	}
	testkit.WriteFile(t, filepath.Join(home, "release-transition/v2/ledger.json"), ledgerRaw, 0600)
	_, e = st.InspectSourceMetadata(registryValue, registryDigest)
	stores = append(stores, storeCase{"expanded-source", "source", base64.StdEncoding.EncodeToString(registryRaw), string(registryDigest), map[string]string{"release-transition/v2/ledger.json": base64.StdEncoding.EncodeToString(ledgerRaw)}, e == nil})
	tr, st := completedSourceMetadataFixture(t)
	snap, e := st.ReadCurrentJournal()
	if e != nil {
		t.Fatal(e)
	}
	alternate := bytes.ReplaceAll(snap.Payload, []byte("schemaVersion"), []byte("SCHEMAVERSION"))
	testkit.WriteFile(t, filepath.Join(st.configHome, "release-transition/v2/journal.json"), alternate, 0600)
	_, e = st.InspectSourceMetadata(tr.registry, tr.registryDigest)
	if e == nil {
		t.Fatal("canonical source accepted alternate journal")
	}
	regRaw, _ := json.Marshal(tr.registry)
	files := map[string]string{}
	filepath.WalkDir(st.configHome, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			p, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(st.configHome, path)
			files[filepath.ToSlash(rel)] = base64.StdEncoding.EncodeToString(p)
		}
		return nil
	})
	stores = append(stores, storeCase{"canonical-source-refusal", "source", base64.StdEncoding.EncodeToString(regRaw), string(tr.registryDigest), files, false})
	// Ordinary graph parsing preserves the frozen raw decoder acceptance set.
	graphHome := protectedConfigHome(t)
	graphStore, _ := NewPOSIXV2Store(graphHome)
	prior := lifecycleReceiptFixture(t).Predecessor
	prior.Transaction = "tx-legacy-before"
	prior.Checkpoint = JournalComplete
	priorRaw := mustJournalPayload(t, prior)
	alternateJournal := bytes.ReplaceAll(priorRaw, []byte("schemaVersion"), []byte("SCHEMAVERSION"))
	add("journal-alternate", 5, alternateJournal, func(raw []byte) error { _, e := ParseJournal(raw); return e })
	currentHome := protectedConfigHome(t)
	currentStore, _ := NewPOSIXV2Store(currentHome)
	if e := rustPrivateDirs(filepath.Join(currentHome, "release-transition/v2"), 0700); e != nil {
		t.Fatal(e)
	}
	testkit.WriteFile(t, filepath.Join(currentHome, "release-transition/v2/journal.json"), alternateJournal, 0600)
	currentSnapshot, e := currentStore.ReadCurrentJournal()
	if e != nil {
		t.Fatal(e)
	}
	_, e = ParseJournal(currentSnapshot.Payload)
	stores = append(stores, storeCase{"alternate-current-ingress", "journal", "", "", map[string]string{"release-transition/v2/journal.json": base64.StdEncoding.EncodeToString(alternateJournal)}, e == nil})
	archive := SupersededJournalRecord{SchemaVersion: 1, AuthorizationPlan: prior.AuthorizationPlan, Replacement: JournalReplacement{Transaction: prior.Transaction, Fingerprint: fingerprintPayload(priorRaw), Reason: JournalReplacementPostActivationScopeV0111, SourceVersion: "0.11.1"}, Journal: prior}
	archiveRaw, e := MarshalSupersededJournal(archive)
	if e != nil {
		t.Fatal(e)
	}
	archiveRaw = bytes.ReplaceAll(archiveRaw, []byte("schemaVersion"), []byte("SCHEMAVERSION"))
	add("archive-alternate-root", 7, archiveRaw, func(raw []byte) error { _, e := ParseSupersededJournal(raw); return e })
	for _, id := range []string{"tx-legacy-before", "tx-legacy-after"} {
		if e := rustPrivateDirs(filepath.Join(graphHome, "release-transition/v2/transactions", id), 0700); e != nil {
			t.Fatal(e)
		}
	}
	archivePath := "release-transition/v2/transactions/tx-legacy-after/superseded-journal.json"
	testkit.WriteFile(t, filepath.Join(graphHome, archivePath), archiveRaw, 0600)
	placeholder := "release-transition/v2/transactions/tx-legacy-before/evidence/placeholder.json"
	if e := rustPrivateDirs(filepath.Dir(filepath.Join(graphHome, placeholder)), 0700); e != nil {
		t.Fatal(e)
	}
	testkit.WriteFile(t, filepath.Join(graphHome, placeholder), []byte("closed"), 0600)
	_, e = graphStore.readTransactionGraph()
	stores = append(stores, storeCase{"alternate-archive", "graph", "", "", map[string]string{archivePath: base64.StdEncoding.EncodeToString(archiveRaw), "release-transition/v2/transactions/tx-legacy-before/evidence/placeholder.json": base64.StdEncoding.EncodeToString([]byte("closed"))}, e == nil})
	payload, e := json.Marshal(struct {
		Catalog           []CapabilityDescriptor `json:"catalog"`
		Registry          string                 `json:"registry"`
		RegistryDigest    string                 `json:"registryDigest"`
		Projection        string                 `json:"projection"`
		ExpansionRegistry string                 `json:"expansionRegistry"`
		Vectors           []vector               `json:"vectors"`
		Stores            []storeCase            `json:"stores"`
	}{catalog.descriptors, base64.StdEncoding.EncodeToString([]byte(validRegistryV2)), string(baseDigest), base64.StdEncoding.EncodeToString(baseProjection), base64.StdEncoding.EncodeToString(registryRaw), vectors, stores})
	if e != nil {
		t.Fatal(e)
	}
	covered := map[byte]bool{}
	for _, v := range vectors {
		if v.Accepted && v.Syntax {
			covered[v.Kind] = true
		}
	}
	if len(covered) != 14 {
		t.Fatalf("valid owning-kind coverage: %v", covered)
	}
	if len(payload) > 16<<20 {
		t.Fatal("bounded legacy corpus")
	}
	testkit.WriteFile(t, destination, append(payload, '\n'), 0600)
	t.Logf("exported %d frozen decoder probes and %d actual source outcomes; expansion raw=%d normalized=%d", len(vectors), len(stores), len(ledgerRaw), len(last.Projection))
}
func fmtLegacyDomain(i int) string {
	return "d" + strings.Repeat("0", 5-len(strconv.Itoa(i))) + strconv.Itoa(i)
}

func TestLegacyCodecProjectionBudget(t *testing.T) {
	// Compact invalid DTO arrays must be refused before Marshal constructs their
	// expanded default-field output. This is below the original input bound.
	raw := []byte("[" + strings.Repeat("{},", 249999) + "{}]")
	if len(raw) > LegacyCodecMaxRaw {
		t.Fatal("fixture bound")
	}
	if _, _, err := normalizeLegacy(2, raw); err == nil {
		t.Fatal("expanded invalid DTO array was not bounded")
	}
	if _, _, err := normalizeLegacy(1, make([]byte, LegacyCodecMaxRaw+1)); err == nil {
		t.Fatal("original input bound")
	}
}
