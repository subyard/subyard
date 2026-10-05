package configsync

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sort"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

// ScopeDigest binds the candidate and all unmanaged inputs without exposing
// settings content. Managed targets may converge independently after review.
func (plan Plan) ScopeDigest() string {
	content, _ := json.Marshal(struct {
		SourceID, Commit, SourceDigest, HostID, ConfigHome string
		Adopt                                              bool
		Desired                                            map[string]candidateFile
		LocalInputs                                        []localInputObservation
	}{plan.SourceID, plan.SourceCommit, plan.SourceDigest, plan.HostID, plan.options.ConfigHome,
		plan.Adopt, plan.desired, plan.localInputs})
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// Refresh repeats native validation before any outer checkout or registration
// writes. The retained candidate remains authoritative; only convergence can
// shrink the approved work.
func (plan Plan) Refresh() (Plan, error) {
	options := plan.options
	options.ConfigLocked = false
	fresh, err := BuildPlan(options)
	if err != nil {
		return Plan{}, err
	}
	if err := plan.CheckFresh(fresh); err != nil {
		return Plan{}, err
	}
	return fresh, nil
}

func (plan Plan) VerifyPublished() error {
	options := plan.options
	options.ConfigLocked = false
	root := plan.SourceRoot
	if options.SourceIdentityRoot != "" {
		root = options.SourceIdentityRoot
	}
	options.SourceRoot, options.SourceIdentityRoot = root, root
	verified, err := BuildPlan(options)
	if err != nil {
		return err
	}
	if verified.SourceID != plan.SourceID || verified.SourceCommit != plan.SourceCommit ||
		verified.SourceDigest != plan.SourceDigest || verified.HostID != plan.HostID {
		return fmt.Errorf("configuration sync verification: approved source or owner identity changed")
	}
	if verified.NeedsApply() {
		return fmt.Errorf("configuration sync verification: native configuration did not converge")
	}
	return nil
}

func (plan Plan) CheckFresh(fresh Plan) error {
	if plan.ScopeDigest() != fresh.ScopeDigest() {
		return fmt.Errorf("%w: configuration source or local inputs changed after preview", ErrPlanStale)
	}
	if (!plan.InitializeHostID && fresh.InitializeHostID) || (!plan.ManifestChanged && fresh.ManifestChanged) ||
		(fresh.ManifestChanged && plan.PreviousGeneration != fresh.PreviousGeneration) {
		return fmt.Errorf("%w: configuration ownership or manifest changed after preview", ErrPlanStale)
	}
	for _, change := range fresh.Changes {
		if !slices.ContainsFunc(plan.Changes, func(before Change) bool {
			if before.Path == change.Path && before.AfterDigest == change.AfterDigest && before.Mode == change.Mode &&
				(change.Action == "record-converged" || (change.Action == "record-deleted" && before.Action == "delete")) {
				return true
			}
			return before.Path == change.Path && before.Action == change.Action && before.BeforeDigest == change.BeforeDigest &&
				before.AfterDigest == change.AfterDigest && before.Mode == change.Mode && slices.Equal(before.Applications, change.Applications)
		}) {
			return fmt.Errorf("%w: managed target %s changed after preview", ErrPlanStale, change.Path)
		}
	}
	return nil
}

// OperationSteps uses the approved target set even when a fresh native plan
// has fewer changes. CheckFresh must establish native convergence first.
func (plan Plan) OperationSteps(approved Plan) []domain.OperationStep {
	paths := map[string]bool{}
	for path := range approved.desired {
		paths[path] = true
	}
	for _, change := range approved.Changes {
		paths[change.Path] = true
	}
	names := make([]string, 0, len(paths))
	for path := range paths {
		names = append(names, path)
	}
	sort.Strings(names)
	changes := map[string]Change{}
	for _, change := range plan.Changes {
		changes[change.Path] = change
	}
	var steps []domain.OperationStep
	for index, path := range names {
		desired := "absent"
		if file, exists := approved.desired[path]; exists {
			desired = "fingerprint:" + file.Digest + fmt.Sprintf("; mode:%o", file.Mode)
		}
		for _, change := range approved.Changes {
			if change.Path == path && change.Action == "retain-local" {
				desired = "fingerprint:" + change.AfterDigest + fmt.Sprintf("; mode:%o", change.Mode)
			}
		}
		observed, decision := desired, domain.StepSkip
		if change, changed := changes[path]; changed && change.Action != "record-converged" && change.Action != "record-deleted" {
			observed = "absent"
			if change.BeforeDigest != "" {
				observed = "fingerprint:" + change.BeforeDigest
			}
			decision = domain.StepApply
		}
		consequence := "publish managed setting " + path
		for _, before := range approved.Changes {
			if before.Path == path {
				consequence = before.Action + " " + path
			}
		}
		steps = append(steps, domain.OperationStep{ID: fmt.Sprintf("config.sync.file.%d", index), Target: "managed setting " + path,
			Observed: observed, Desired: desired, Decision: decision,
			Preconditions: []string{"native source, unmanaged inputs, ownership and target baseline remain approved"},
			Verify:        "native rebuild proves the approved content and ownership are converged", Consequence: consequence})
	}
	for _, metadata := range []struct {
		id, target, desired string
		changed             bool
	}{
		{"host-id", "owner host identity", approved.HostID, plan.InitializeHostID},
		{"manifest", "managed configuration manifest", "fingerprint:" + approved.SourceDigest, plan.ManifestChanged},
	} {
		decision, observed := domain.StepSkip, metadata.desired
		if metadata.changed {
			decision, observed = domain.StepApply, "native metadata update required"
		}
		consequence := "record owner host ID " + approved.HostID
		if metadata.id == "manifest" {
			consequence = "update versioned configuration manifest metadata"
		}
		steps = append(steps, domain.OperationStep{ID: "config.sync." + metadata.id, Target: metadata.target, Observed: observed,
			Desired: metadata.desired, Decision: decision, Preconditions: []string{"native identity and manifest generation are unchanged or converged"},
			Verify: "native rebuild proves metadata converged", Consequence: consequence})
	}
	return steps
}

// CandidateConfigs reuses the native validation layers so downstream consumers
// can bind their desired inputs before those settings are published.
func (plan Plan) CandidateConfigs() ([]config.Loaded, error) {
	unlock, err := config.LockRoot(plan.options.ConfigHome, false)
	if err != nil {
		return nil, err
	}
	defer unlock()
	catalog, err := config.LoadCatalog(plan.options.RepositoryRoot)
	if err != nil {
		return nil, err
	}
	source, err := readSource(plan.options, plan.HostID, catalog)
	if err != nil {
		return nil, err
	}
	if source.digest != plan.SourceDigest || source.commit != plan.SourceCommit || source.id != plan.SourceID {
		return nil, ErrPlanStale
	}
	var contexts []config.Loaded
	if err := validateCandidate(plan.options, source, plan.previous, catalog, &contexts); err != nil {
		return nil, err
	}
	return contexts, nil
}
