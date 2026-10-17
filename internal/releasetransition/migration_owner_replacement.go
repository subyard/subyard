package releasetransition

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/blang/semver/v4"
)

const (
	MigrationOwnerReplacementContractV1 = "migration-ledger-owner-replacement-v1"
	MigrationOwnerReceiptSchemaV1       = 1
	MigrationOwnerTransactionPrefixV1   = "migration-owner-v1-"
	// Initial support is deliberately an exact published artifact, not a version range.
	AffectedMigrationOwnerManifest = Fingerprint("1db8e08c693bc9e507988576ebef7e3fb958c7c03a667e9f5a68719a49aa5ac7")
	AffectedMigrationOwnerRegistry = Fingerprint("e4d70df389a0eaac35bd309f864f69c5609a9e3b395e69334441187a96d49d20")
)

var migrationOwnerReceiptParts = []string{"release-transition", "migration-owner", "v1", "transactions"}

type MigrationOwnerAssetsProof struct {
	Binding        Fingerprint
	SourceRegistry []byte
	PreviousSource *MigrationOwnerPreviousSource
}

// MigrationOwnerPreviousSource binds only the exact earlier release named by
// the defective journal. Its independently sealed registry must equal the source
// registry; matching registry contents alone never authorize another release.
type MigrationOwnerPreviousSource struct {
	Release  ReleaseID   `json:"release"`
	Artifact Fingerprint `json:"artifact"`
	Registry Fingerprint `json:"registry"`
	Inputs   Fingerprint `json:"inputs"`
}

type MigrationOwnerReplacementRequest struct {
	Transaction TransactionID `json:"transaction"`
	Fingerprint Fingerprint   `json:"fingerprint"`
}

func (request MigrationOwnerReplacementRequest) Validate() error {
	if err := validateTransactionID(request.Transaction); err != nil {
		return err
	}
	if strings.HasPrefix(string(request.Transaction), MigrationOwnerTransactionPrefixV1) || IsRecoveryTransaction(request.Transaction) {
		return invalid("migration owner replacement excludes replacement chains")
	}
	return validateFingerprint(request.Fingerprint, "migration owner predecessor")
}

type MigrationOwnerReceiptV1 struct {
	Registry             []byte                           `json:"registry"`
	SourceRegistry       []byte                           `json:"sourceRegistry"`
	PreviousSource       *MigrationOwnerPreviousSource    `json:"previousSource,omitempty"`
	RetainedTransactions []TransactionID                  `json:"retainedTransactions,omitempty"`
	ProjectionPayload    []byte                           `json:"projection"`
	CheckpointPayload    []byte                           `json:"checkpoint"`
	SchemaVersion        int                              `json:"schemaVersion"`
	Contract             string                           `json:"contract"`
	Replacement          MigrationOwnerReplacementRequest `json:"replacement"`
	Predecessor          JournalRecord                    `json:"predecessor"`
	Successor            JournalRecord                    `json:"successor"`
	Projection           Fingerprint                      `json:"projectionFingerprint"`
	Authority            Fingerprint                      `json:"authorityFingerprint"`
	ProtectedInputs      Fingerprint                      `json:"protectedInputs"`
	Assets               Fingerprint                      `json:"assets"`
	Links                ReleaseLinks                     `json:"links"`
}

func (receipt MigrationOwnerReceiptV1) Validate() error {
	if receipt.SchemaVersion != MigrationOwnerReceiptSchemaV1 || receipt.Contract != MigrationOwnerReplacementContractV1 {
		return invalid("unsupported migration owner receipt")
	}
	if err := receipt.Replacement.Validate(); err != nil {
		return err
	}
	before, after := receipt.Predecessor, receipt.Successor
	if err := before.Validate(); err != nil {
		return err
	}
	if err := after.Validate(); err != nil {
		return err
	}
	payload, err := MarshalJournal(before)
	if err != nil {
		return err
	}
	if before.Transaction != receipt.Replacement.Transaction || fingerprintPayload(payload) != receipt.Replacement.Fingerprint ||
		before.ArtifactDigest != AffectedMigrationOwnerManifest || before.RegistryDigest != AffectedMigrationOwnerRegistry ||
		before.Checkpoint != JournalReconciling || before.Goal.Direction != DirectionActivateTarget || before.SourceIngress != nil || len(before.Steps) != 2 {
		return invalid("unsupported migration owner predecessor")
	}
	if err := receipt.Links.Validate(); err != nil {
		return err
	}
	if !activatedReleaseLinks(receipt.Links, before.Releases) || !initialReleaseLinks(receipt.Links, after.Releases) ||
		after.Checkpoint != JournalAuthorized || after.SourceIngress != nil || len(after.Steps) != 0 || after.Goal.Direction != DirectionActivateTarget ||
		after.Goal.Target == before.Goal.Target || after.RegistryDigest != before.RegistryDigest ||
		!strings.HasPrefix(string(after.Transaction), MigrationOwnerTransactionPrefixV1) || after.AuthorizationPlan == before.AuthorizationPlan ||
		after.ResumePlan == before.ResumePlan || after.AuthorizationDigest == before.AuthorizationDigest {
		return invalid("migration owner successor does not bind a fresh forward grant")
	}
	for _, fingerprint := range []Fingerprint{receipt.Projection, receipt.Authority, receipt.ProtectedInputs, receipt.Assets} {
		if err := validateFingerprint(fingerprint, "migration owner receipt binding"); err != nil {
			return err
		}
	}
	state, err := validateMigrationOwnerLedger(before, receipt.Registry, receipt.ProjectionPayload, receipt.CheckpointPayload)
	if err != nil {
		return err
	}
	if _, err := migrationOwnerSourceRegistry(receipt.SourceRegistry, state.ProjectionSnapshot); err != nil {
		return err
	}
	if err := validateMigrationOwnerPreviousSource(before.Releases, receipt.SourceRegistry, receipt.PreviousSource); err != nil {
		return err
	}
	if len(receipt.RetainedTransactions) > maxTransactionGraphEntries || !slices.IsSorted(receipt.RetainedTransactions) {
		return invalid("migration owner retained history is not bounded and canonical")
	}
	for i, transaction := range receipt.RetainedTransactions {
		if err := validateTransactionID(transaction); err != nil || transaction == before.Transaction || transaction == after.Transaction || i > 0 && transaction == receipt.RetainedTransactions[i-1] {
			return invalid("migration owner retained history is unbound")
		}
	}
	if state.ProjectionSnapshot.Fingerprint != receipt.Projection || state.AuthoritySnapshot.Fingerprint != receipt.Authority {
		return invalid("migration owner receipt authority binding differs")
	}
	return nil
}

func MarshalMigrationOwnerReceipt(receipt MigrationOwnerReceiptV1) ([]byte, error) {
	if err := receipt.Validate(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(receipt)
	if len(payload)+1 > MaxProtectedRecordBytes {
		return nil, invalid("migration owner receipt is too large")
	}
	return append(payload, '\n'), err
}

func ParseMigrationOwnerReceipt(payload []byte) (MigrationOwnerReceiptV1, error) {
	var receipt MigrationOwnerReceiptV1
	if err := decodeBoundedRecord(payload, MaxProtectedRecordBytes, &receipt); err != nil {
		return receipt, err
	}
	return receipt, receipt.Validate()
}

// The proof reads the canonical predecessor and every durable evidence triple.
// It replays only the two completed ledger transitions from the frozen projection;
// it never observes or reruns the historical migration's operational resources.
func (transition *V2Transition) observeMigrationOwnerReplacement(ctx context.Context, observation *v2Observation) (bool, error) {
	request := transition.options.MigrationOwnerReplacement
	if request == nil {
		return false, nil
	}
	if err := request.Validate(); err != nil {
		return false, err
	}
	journal := observation.journal
	if journal != nil && strings.HasPrefix(string(journal.Transaction), MigrationOwnerTransactionPrefixV1) {
		if err := transition.store.ValidateMigrationOwnerSuccessor(*journal, *request); err != nil {
			return false, err
		}
		return false, nil
	}
	if journal == nil || journal.Transaction != request.Transaction || observation.journalSnapshot.Fingerprint != request.Fingerprint {
		return false, invalid("migration owner predecessor changed")
	}
	canonical, err := MarshalJournal(*journal)
	if err != nil || !bytes.Equal(canonical, observation.journalSnapshot.Payload) {
		return false, invalid("migration owner predecessor is not canonical")
	}
	candidateVersion, err := semver.Parse(transition.options.CandidateVersion)
	if err != nil || candidateVersion.String() != transition.options.CandidateVersion || !candidateVersion.GT(semver.MustParse("0.21.0")) ||
		!transition.options.MigrationCheckpoint || !observation.checkpointAuthority || transition.options.Replacement != nil ||
		transition.options.SourceIngress != nil || transition.options.Direction != DirectionActivateTarget ||
		journal.ArtifactDigest != AffectedMigrationOwnerManifest || journal.RegistryDigest != AffectedMigrationOwnerRegistry ||
		transition.registryDigest != journal.RegistryDigest || journal.Checkpoint != JournalReconciling || journal.SourceIngress != nil ||
		journal.Goal.Direction != DirectionActivateTarget || len(journal.Steps) != 2 ||
		transition.options.Releases.From != journal.Goal.Target || transition.options.Releases.Target == journal.Goal.Target ||
		!activatedReleaseLinks(observation.links, journal.Releases) || !initialReleaseLinks(observation.links, transition.options.Releases) {
		return false, invalid("unsupported migration owner replacement")
	}
	state, err := validateMigrationOwnerLedger(*journal, transition.options.RegistryPayload, observation.migrationLedgerSnapshot.ProjectionSnapshot.Payload, observation.checkpointSnapshot.Payload)
	if err != nil {
		return false, err
	}
	projection := state.ProjectionSnapshot
	if err := transition.store.migrationOwnerEvidence(*journal); err != nil {
		return false, err
	}
	if transition.options.MigrationOwnerAssets == nil {
		return false, invalid("migration owner protected assets verifier is required")
	}
	assets, err := transition.options.MigrationOwnerAssets(ctx)
	if err != nil {
		return false, err
	}
	if err := validateFingerprint(assets.Binding, "migration owner assets"); err != nil {
		return false, err
	}
	inputs, retained, err := transition.store.migrationOwnerInputs(*journal, assets.SourceRegistry, assets.PreviousSource, projection)
	if err != nil {
		return false, err
	}
	receipt := &MigrationOwnerReceiptV1{SchemaVersion: MigrationOwnerReceiptSchemaV1, Contract: MigrationOwnerReplacementContractV1,
		Registry: transition.options.RegistryPayload, SourceRegistry: assets.SourceRegistry, ProjectionPayload: projection.Payload, CheckpointPayload: observation.checkpointSnapshot.Payload,
		PreviousSource: assets.PreviousSource, RetainedTransactions: retained,
		Replacement: *request, Predecessor: *journal, Projection: projection.Fingerprint, Authority: observation.migrationLedgerSnapshot.AuthoritySnapshot.Fingerprint,
		ProtectedInputs: inputs, Assets: assets.Binding, Links: observation.links}
	candidate := v2Observation{goal: observation.goal, links: observation.links, ledger: observation.ledger, ledgerSnapshot: observation.ledgerSnapshot,
		migrationLedgerSnapshot: observation.migrationLedgerSnapshot, checkpointSnapshot: observation.checkpointSnapshot, checkpointAuthority: true,
		journalSnapshot: observation.journalSnapshot, migrationOwnerReceipt: receipt}
	for _, binding := range []struct {
		resource    string
		fingerprint Fingerprint
	}{{"predecessor", request.Fingerprint}, {"authority", receipt.Authority}, {"projection", receipt.Projection}, {"protected-inputs", inputs}, {"assets", assets.Binding}} {
		candidate.observations = append(candidate.observations, ResourceObservation{Resource: "migration-owner." + binding.resource, Class: MigrationOwnerReplacementContractV1, Fingerprint: binding.fingerprint})
	}
	if err := transition.observeActivation(ctx, &candidate); err != nil {
		return false, err
	}
	candidate.activationConsequences = append(candidate.activationConsequences, "preserve the proved defective owner's unfinished activation as superseded evidence and activate the standalone candidate")
	candidate.observationScope, candidate.checkpointConsent, err = transition.currentObservationScope(candidate.activationScope, nil, true, receipt.Authority)
	if err != nil {
		return false, err
	}
	*observation = candidate
	return true, nil
}

func validateMigrationOwnerLedger(journal JournalRecord, registryPayload, projectionPayload, checkpointPayload []byte) (MigrationLedgerSnapshot, error) {
	registry, digest, err := ParseRegistryV2(registryPayload, BuiltinCapabilityCatalog())
	if err != nil || digest != AffectedMigrationOwnerRegistry || journal.RegistryDigest != digest {
		return MigrationLedgerSnapshot{}, invalid("unsupported migration owner registry")
	}
	original, supported := SupportedBuiltinCatalog(journal.CatalogDigest)
	if !supported {
		return MigrationLedgerSnapshot{}, invalid("unsupported migration owner catalog")
	}
	for _, migration := range registry.Migrations {
		if !original.Supports(migration.Kind, migration.Domain) {
			return MigrationLedgerSnapshot{}, invalid("migration exceeds predecessor catalog")
		}
	}
	projection := protectedSnapshotFromPayload(projectionPayload)
	state, err := ParseMigrationLedger(projection, protectedSnapshotFromPayload(checkpointPayload), registry, digest)
	if err != nil || !state.Checkpointed {
		return state, invalid("migration owner requires valid checkpoint authority")
	}
	canonical, _, err := MarshalLedgerCheckpointV1(state.Checkpoint, registry, digest, projection)
	if err != nil || !bytes.Equal(canonical, checkpointPayload) {
		return state, invalid("migration owner checkpoint is not canonical")
	}
	ledger, _, err := ParseLedgerV2(projectionPayload, registry)
	if err != nil {
		return state, err
	}
	canonical, _, err = MarshalLedgerV2(ledger, registry)
	if err != nil || !bytes.Equal(canonical, projectionPayload) {
		return state, invalid("migration owner projection is not canonical")
	}
	epochs := map[string]int{"owner-registration": 2, "power-metadata": 1, "project-state": 1, "settings": 2}
	if len(ledger.Domains) != len(epochs) {
		return state, invalid("unsupported migration owner domains")
	}
	for domain, epoch := range epochs {
		domainState, exists := ledger.Domains[domain]
		checkpoint, present := state.Checkpoint.Domains[domain]
		if !exists || !present || domainState.Epoch != epoch || checkpoint.CompactedThrough != epoch {
			return state, invalid("unsupported migration owner projection prefix")
		}
	}
	if len(journal.Steps) != 2 {
		return state, invalid("unsupported migration owner steps")
	}
	reconstructed := projection
	for index, id := range []string{"introduce-owner-publication-state-v1", "localize-ssh-port-settings-v3"} {
		step := journal.Steps[index]
		var migration MigrationDefinitionV2
		for _, definition := range registry.Migrations {
			if definition.ID == id {
				migration = definition
			}
		}
		if migration.ID == "" || step.Migration != id || step.ID != id+".ledger" || step.Resource != "ledger."+migration.Domain || step.Decision != DecisionTransform || step.Checkpoint != StepVerified || step.Expected != reconstructed.Fingerprint || step.Evidence == nil || !postActivationEvidenceMatches(journal, step, EvidenceVerified, *step.Evidence) {
			return state, invalid("unsupported completed migration owner ledger step")
		}
		advanced, err := ledger.Advance(registry, migration)
		if err != nil {
			return state, err
		}
		payload, desired, err := MarshalLedgerV2(advanced, registry)
		if err != nil || desired != step.Desired {
			return state, invalid("migration owner ledger replay differs")
		}
		ledger, reconstructed = advanced, protectedSnapshotFromPayload(payload)
	}
	pending, err := registry.PendingPath(ledger)
	authority, _, marshalErr := MarshalLedgerV2(state.Ledger, registry)
	if err != nil || marshalErr != nil || len(pending) != 0 || !bytes.Equal(reconstructed.Payload, authority) {
		return state, invalid("migration owner authority does not match complete replay")
	}
	return state, nil
}

func (store *POSIXV2Store) migrationOwnerEvidence(journal JournalRecord) error {
	matched, err := store.postActivationJournalArtifactsMatch(journal)
	if err != nil || !matched {
		return invalid("migration owner protected artifacts differ")
	}
	for _, step := range journal.Steps {
		for _, checkpoint := range []EvidenceCheckpoint{EvidenceCaptured, EvidenceApplied, EvidenceVerified} {
			snapshot, err := store.ReadCheckpointEvidence(journal.Transaction, step.ID, checkpoint)
			if err != nil || !snapshot.Exists {
				return invalid("migration owner evidence is missing")
			}
			evidence, err := ParseEvidence(snapshot.Payload)
			canonical, marshalErr := MarshalEvidence(evidence)
			if err != nil || marshalErr != nil || !bytes.Equal(canonical, snapshot.Payload) || !postActivationEvidenceMatches(journal, step, checkpoint, evidence) || checkpoint == EvidenceVerified && !evidenceRecordsEqual(evidence, *step.Evidence) {
				return invalid("migration owner evidence differs from completed ledger step")
			}
		}
	}
	return nil
}

// Bind the finite existing protected graph, excluding only this contract's
// immutable reservation. Its contents are independently validated on every read.
func migrationOwnerSourceRegistry(sourceRegistry []byte, projection ProtectedSnapshot) (RegistryV2, error) {
	original, _, err := ParseRegistryV2(sourceRegistry, BuiltinCapabilityCatalog())
	if err != nil {
		return RegistryV2{}, err
	}
	historical := BaselineLedgerV2(original)
	path, err := original.PendingPath(historical)
	if err != nil {
		return RegistryV2{}, err
	}
	for _, migration := range path {
		historical, err = historical.Advance(original, migration)
		if err != nil {
			return RegistryV2{}, err
		}
	}
	payload, _, err := MarshalLedgerV2(historical, original)
	if err != nil || !bytes.Equal(payload, projection.Payload) {
		return RegistryV2{}, invalid("retained source registry does not prove the frozen projection")
	}
	return original, nil
}

func validateMigrationOwnerPreviousSource(pair ReleasePair, registry []byte, previous *MigrationOwnerPreviousSource) error {
	if pair.Previous == nil {
		if previous != nil {
			return invalid("migration owner previous source is not journal-bound")
		}
		return nil
	}
	if previous == nil || previous.Release != *pair.Previous || previous.Registry != fingerprintPayload(registry) {
		return invalid("migration owner previous source differs from its sealed registry or journal")
	}
	for _, value := range []Fingerprint{previous.Artifact, previous.Registry, previous.Inputs} {
		if err := validateFingerprint(value, "migration owner previous source"); err != nil {
			return err
		}
	}
	return nil
}

func migrationOwnerRetainedTarget(pair ReleasePair, previous *MigrationOwnerPreviousSource, target ReleaseID) bool {
	return target == pair.From || previous != nil && pair.Previous != nil && previous.Release == *pair.Previous && target == previous.Release
}

func (store *POSIXV2Store) migrationOwnerInputs(predecessor JournalRecord, sourceRegistry []byte, previous *MigrationOwnerPreviousSource, projection ProtectedSnapshot) (Fingerprint, []TransactionID, error) {
	if err := validateMigrationOwnerPreviousSource(predecessor.Releases, sourceRegistry, previous); err != nil {
		return "", nil, err
	}
	inputs, err := store.sourceMetadataInventoryWithoutMigrationOwner()
	if err != nil {
		return "", nil, err
	}
	graph, err := store.readTransactionGraph()
	if err != nil {
		return "", nil, err
	}
	if graph == nil || len(graph.pending) != 0 || len(graph.archives) != 0 {
		return "", nil, invalid("migration owner replacement requires an original finite transaction graph")
	}
	if _, exists := graph.directories[predecessor.Transaction]; !exists {
		return "", nil, invalid("migration owner predecessor artifacts are missing")
	}
	original, err := migrationOwnerSourceRegistry(sourceRegistry, projection)
	if err != nil {
		return "", nil, err
	}
	sourceLedger, _, err := ParseLedgerV2(projection.Payload, original)
	if err != nil {
		return "", nil, err
	}
	state := MigrationLedgerSnapshot{Ledger: sourceLedger, ProjectionSnapshot: projection}
	retained := []TransactionID{}
	for transaction := range graph.directories {
		if transaction == predecessor.Transaction {
			continue
		}
		bound, err := store.sourceRetainedTerminalEvidence(transaction, original)
		if err != nil {
			return "", nil, err
		}
		if !migrationOwnerRetainedTarget(predecessor.Releases, previous, bound.Releases.Target) {
			return "", nil, invalid("retained transaction does not belong to an exact source release")
		}
		if err := store.sourceTerminalBindings(bound, original, projection.Fingerprint, state); err != nil {
			return "", nil, err
		}
		retained = append(retained, transaction)
	}
	for _, parts := range [][]string{recoveryReceiptParts, recoveryLifecycleParts, recoveryArchiveParts, recoveryFrontierParts, {"release-transition", "v2", "compatibility"}} {
		entries, _, err := store.readDirectoryEntries(parts, 0)
		if err != nil || len(entries) != 0 {
			return "", nil, invalid("migration owner replacement excludes foreign recovery and ingress evidence")
		}
	}
	slices.Sort(retained)
	return inputs, retained, nil
}

func (store *POSIXV2Store) migrationOwnerRetainedHistory(receipt MigrationOwnerReceiptV1) error {
	original, err := migrationOwnerSourceRegistry(receipt.SourceRegistry, ProtectedSnapshot{Fingerprint: receipt.Projection, Payload: receipt.ProjectionPayload, Exists: true})
	if err != nil {
		return err
	}
	sourceLedger, _, err := ParseLedgerV2(receipt.ProjectionPayload, original)
	if err != nil {
		return err
	}
	state := MigrationLedgerSnapshot{Ledger: sourceLedger, ProjectionSnapshot: ProtectedSnapshot{Exists: true, Fingerprint: receipt.Projection, Payload: receipt.ProjectionPayload}}
	for _, transaction := range receipt.RetainedTransactions {
		bound, err := store.sourceRetainedTerminalEvidence(transaction, original)
		if err != nil {
			return err
		}
		if !migrationOwnerRetainedTarget(receipt.Predecessor.Releases, receipt.PreviousSource, bound.Releases.Target) {
			return invalid("migration owner retained history has a foreign target")
		}
		if err := store.sourceTerminalBindings(bound, original, receipt.Projection, state); err != nil {
			return err
		}
	}
	return nil
}

func (store *POSIXV2Store) migrationOwnerReceipts() (map[TransactionID]MigrationOwnerReceiptV1, error) {
	for _, branch := range []struct {
		parts []string
		names []string
	}{
		{migrationOwnerReceiptParts[:2], []string{"v1"}},
		{migrationOwnerReceiptParts[:3], []string{"transactions", "completed"}},
	} {
		entries, _, err := store.readDirectoryEntries(branch.parts, len(branch.names))
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if !entry.IsDir() || !slices.Contains(branch.names, entry.Name()) {
				return nil, invalid("unsupported migration owner namespace")
			}
		}
	}
	entries, _, err := store.recoveryEntries(migrationOwnerReceiptParts)
	if err != nil {
		return nil, err
	}
	receipts := map[TransactionID]MigrationOwnerReceiptV1{}
	predecessors := map[TransactionID]TransactionID{}
	for _, entry := range entries {
		name := entry.Name()
		transaction := TransactionID(strings.TrimSuffix(name, ".json"))
		if strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".json.pending") {
			transaction = TransactionID(strings.TrimSuffix(strings.TrimPrefix(name, "."), ".json.pending"))
		}
		snapshot, err := store.readRecord(migrationOwnerReceiptParts, name)
		if err != nil || !snapshot.Exists {
			return nil, invalid("migration owner receipt disappeared")
		}
		receipt, err := ParseMigrationOwnerReceipt(snapshot.Payload)
		canonical, marshalErr := MarshalMigrationOwnerReceipt(receipt)
		if err != nil || marshalErr != nil || !bytes.Equal(canonical, snapshot.Payload) || receipt.Successor.Transaction != transaction {
			return nil, invalid("invalid migration owner receipt entry")
		}
		if prior, exists := receipts[transaction]; exists {
			payload, _ := MarshalMigrationOwnerReceipt(prior)
			if !bytes.Equal(payload, canonical) {
				return nil, invalid("conflicting migration owner receipt publication")
			}
		}
		if successor, exists := predecessors[receipt.Predecessor.Transaction]; exists && successor != transaction {
			return nil, invalid("migration owner predecessor has multiple successors")
		}
		receipts[transaction], predecessors[receipt.Predecessor.Transaction] = receipt, transaction
	}
	if len(receipts) > 1 {
		return nil, invalid("migration owner replacement horizon is full")
	}
	completed, _, err := store.recoveryEntries(migrationOwnerCompletedParts)
	if err != nil {
		return nil, err
	}
	if len(completed) != 0 {
		if len(receipts) == 0 {
			return nil, invalid("unbound migration owner completion record")
		}
		for _, receipt := range receipts {
			if _, err := store.migrationOwnerCompletion(receipt, nil); err != nil {
				return nil, err
			}
		}
	}
	return receipts, nil
}

func (store *POSIXV2Store) validateMigrationOwnerCurrent(current JournalRecord) error {
	receipts, err := store.migrationOwnerReceipts()
	if err != nil {
		return err
	}
	if !strings.HasPrefix(string(current.Transaction), MigrationOwnerTransactionPrefixV1) {
		return nil
	}
	receipt, exists := receipts[current.Transaction]
	if !exists {
		return invalid("migration owner successor has no protected receipt")
	}
	if err := store.migrationOwnerEvidence(receipt.Predecessor); err != nil {
		return err
	}
	if err := store.migrationOwnerRetainedHistory(receipt); err != nil {
		return err
	}
	before, after := receipt.Successor, current
	before.Checkpoint, after.Checkpoint = JournalAuthorized, JournalAuthorized
	beforePayload, _ := MarshalJournal(before)
	afterPayload, err := MarshalJournal(after)
	if err != nil || !bytes.Equal(beforePayload, afterPayload) {
		return invalid("migration owner current journal differs from its receipt")
	}
	snapshot, err := store.ReadLedger()
	if err != nil || !snapshot.Exists || snapshot.Fingerprint != receipt.Projection {
		return invalid("migration owner frozen projection changed")
	}
	return nil
}

func (transition *V2Transition) migrationOwnerNewJournal(observation v2Observation, transaction TransactionID, plan PlanToken) (TransactionID, error) {
	receipts, err := transition.store.migrationOwnerReceipts()
	if err != nil {
		return "", err
	}
	for id, receipt := range receipts {
		wanted := *observation.migrationOwnerReceipt
		wanted.Successor = receipt.Successor
		payload, _ := MarshalMigrationOwnerReceipt(wanted)
		actual, _ := MarshalMigrationOwnerReceipt(receipt)
		if receipt.Successor.AuthorizationPlan != plan || !bytes.Equal(payload, actual) {
			return "", invalid("migration owner predecessor already has a different reserved successor")
		}
		return id, nil
	}
	return TransactionID(MigrationOwnerTransactionPrefixV1 + string(transaction)), nil
}

func (transition *V2Transition) publishMigrationOwnerReceipt(observation v2Observation, created *JournalRecord) error {
	receipt := *observation.migrationOwnerReceipt
	receipts, err := transition.store.migrationOwnerReceipts()
	if err != nil {
		return err
	}
	if existing, found := receipts[created.Transaction]; found {
		// Resume the exact initial successor and its original durable grant after a
		// receipt-only crash. A newly confirmed grant authorizes this same plan.
		receipt.Successor = existing.Successor
		*created = existing.Successor
	} else {
		receipt.Successor = *created
	}
	payload, err := MarshalMigrationOwnerReceipt(receipt)
	if err != nil {
		return err
	}
	return transition.store.createImmutable(migrationOwnerReceiptParts, string(created.Transaction)+".json", payload)
}

func migrationOwnerProtectedTransactions(receipts map[TransactionID]MigrationOwnerReceiptV1) []TransactionID {
	ids := []TransactionID{}
	for transaction, receipt := range receipts {
		ids = append(ids, transaction, receipt.Predecessor.Transaction)
		ids = append(ids, receipt.RetainedTransactions...)
	}
	return slices.Compact(ids)
}

// ValidateMigrationOwnerSuccessor binds process results to their typed immutable
// predecessor receipt rather than accepting a reserved transaction name alone.
func (store *POSIXV2Store) ValidateMigrationOwnerSuccessor(current JournalRecord, request MigrationOwnerReplacementRequest) error {
	if err := store.validateMigrationOwnerCurrent(current); err != nil {
		return err
	}
	receipts, err := store.migrationOwnerReceipts()
	if err != nil {
		return err
	}
	receipt, found := receipts[current.Transaction]
	if !found || receipt.Replacement != request {
		return invalid("migration owner successor does not bind selected predecessor")
	}
	return nil
}

var migrationOwnerCompletedParts = []string{"release-transition", "migration-owner", "v1", "completed"}

func migrationOwnerSuccessorMatches(initial, current JournalRecord) bool {
	initial.Checkpoint, current.Checkpoint = JournalAuthorized, JournalAuthorized
	left, _ := MarshalJournal(initial)
	right, err := MarshalJournal(current)
	return err == nil && bytes.Equal(left, right)
}

func (store *POSIXV2Store) recordCompletedMigrationOwner(current JournalRecord) error {
	if !strings.HasPrefix(string(current.Transaction), MigrationOwnerTransactionPrefixV1) {
		return nil
	}
	if current.Checkpoint != JournalComplete {
		return invalid("migration owner closure requires completed successor")
	}
	if err := store.validateMigrationOwnerCurrent(current); err != nil {
		return err
	}
	payload, err := MarshalJournal(current)
	if err != nil {
		return err
	}
	return store.createImmutable(migrationOwnerCompletedParts, string(current.Transaction)+".json", payload)
}

func (store *POSIXV2Store) migrationOwnerCompletion(receipt MigrationOwnerReceiptV1, current *JournalRecord) (JournalRecord, error) {
	entries, _, err := store.recoveryEntries(migrationOwnerCompletedParts)
	if err != nil {
		return JournalRecord{}, err
	}
	var terminal *JournalRecord
	for _, entry := range entries {
		name := entry.Name()
		if name != string(receipt.Successor.Transaction)+".json" && name != "."+string(receipt.Successor.Transaction)+".json.pending" {
			return JournalRecord{}, invalid("unbound migration owner completion record")
		}
		snapshot, err := store.readRecord(migrationOwnerCompletedParts, name)
		if err != nil || !snapshot.Exists {
			return JournalRecord{}, invalid("migration owner completion disappeared")
		}
		journal, err := ParseJournal(snapshot.Payload)
		canonical, marshalErr := MarshalJournal(journal)
		if err != nil || marshalErr != nil || !bytes.Equal(canonical, snapshot.Payload) || journal.Checkpoint != JournalComplete || !migrationOwnerSuccessorMatches(receipt.Successor, journal) {
			return JournalRecord{}, invalid("migration owner completion differs from receipt")
		}
		terminal = &journal
	}
	if current != nil && current.Checkpoint == JournalComplete && migrationOwnerSuccessorMatches(receipt.Successor, *current) {
		return *current, nil
	}
	if terminal == nil {
		return JournalRecord{}, invalid("migration owner supersession has no terminal successor evidence")
	}
	return *terminal, nil
}

func (store *POSIXV2Store) validateMigrationOwnerPreparedJournal() error {
	receipts, err := store.migrationOwnerReceipts()
	if err != nil || len(receipts) != 1 {
		return invalid("unbound prepared migration owner journal")
	}
	pending, err := store.readRecord([]string{"release-transition", "v2"}, ".journal.json.pending")
	if err != nil || !pending.Exists {
		return invalid("prepared migration owner journal is unavailable")
	}
	current, err := store.ReadCurrentJournal()
	if err != nil {
		return err
	}
	for _, receipt := range receipts {
		expected, _ := MarshalJournal(receipt.Successor)
		predecessor, _ := MarshalJournal(receipt.Predecessor)
		if !bytes.Equal(pending.Payload, expected) || !bytes.Equal(current.Payload, predecessor) {
			return invalid("prepared migration owner journal conflicts with its immutable receipt")
		}
	}
	return nil
}
