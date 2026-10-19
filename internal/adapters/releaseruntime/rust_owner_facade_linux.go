//go:build linux

package releaseruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/uninstall"
)

// Temporary integration hold. Remove this constant and its guards when the
// coordinated functional prerequisites authorize the final owner cutover.
const releaseOwnerRoutingEnabled = false

type rustOwnerSessions struct {
	mu       sync.Mutex
	closed   bool
	sessions map[string]*rustOwnerSession
}

func (owners *rustOwnerSessions) Close() error {
	owners.mu.Lock()
	owners.closed = true
	sessions := owners.sessions
	owners.sessions = nil
	owners.mu.Unlock()
	var err error
	for _, session := range sessions {
		err = errors.Join(err, session.Close())
	}
	return err
}

func rustOwnerSessionKey(owner releasetransition.Fingerprint, request releasetransition.ProcessRequest, recovery *releasetransition.RecoveryProcessRequest) (string, error) {
	request.Mode, request.Execution = releasetransition.ProcessInspect, nil
	// The exact owner manifest already binds this registry; callers may omit
	// the redundant digest before the verified dispatch fills it.
	request.RegistryDigest = ""
	var normalizedRecovery *releasetransition.RecoveryProcessRequest
	if recovery != nil {
		copy := *recovery
		copy.Mode, copy.Execution = releasetransition.RecoveryProcessInspect, nil
		copy.RegistryDigest = ""
		normalizedRecovery = &copy
	}
	raw, err := json.Marshal(struct {
		Owner          releasetransition.Fingerprint
		Request        releasetransition.ProcessRequest
		Contract       string
		MigrationOwner *releasetransition.MigrationOwnerReplacementRequest
		Recovery       *releasetransition.RecoveryProcessRequest
	}{owner, request, request.RecoveryContract, request.MigrationOwnerReplacement, normalizedRecovery})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

func (runtime *Runtime) findRustOwnerSession(owner releasetransition.Fingerprint, request releasetransition.ProcessRequest, recovery *releasetransition.RecoveryProcessRequest) *rustOwnerSession {
	if runtime.rustOwners == nil {
		return nil
	}
	key, err := rustOwnerSessionKey(owner, request, recovery)
	if err != nil {
		return nil
	}
	runtime.rustOwners.mu.Lock()
	defer runtime.rustOwners.mu.Unlock()
	return runtime.rustOwners.sessions[key]
}

// This typed boundary is deliberately callable before product dispatch changes.
// A retained preparation is reused only for the exact owner/request/recovery key.
func (runtime *Runtime) invokeRustOwnerTransition(ctx context.Context, assets, engine, target *verifiedPublishedCandidate, request releasetransition.ProcessRequest, recovery *releasetransition.RecoveryProcessRequest, grant releasetransition.Authorization) (result releasetransition.ProcessResponse, resultErr error) {
	defer func() {
		if engine != nil && engine.releaseOwner != nil {
			resultErr = markSelectedRustOwnerError(resultErr)
		}
	}()
	if assets == nil || engine == nil || target == nil || engine.releaseOwner == nil ||
		assets.manifestDigest != engine.manifestDigest || assets.registryDigest != engine.registryDigest ||
		target.manifestDigest != request.ArtifactDigest || target.candidate.release != request.Target ||
		request.RegistryDigest != "" && request.RegistryDigest != assets.registryDigest {
		return releasetransition.ProcessResponse{}, errors.New("verified Rust transition owner is unavailable")
	}
	if runtime.rustOwners == nil {
		return releasetransition.ProcessResponse{}, errors.New("release runtime is unavailable")
	}
	key, err := rustOwnerSessionKey(engine.manifestDigest, request, recovery)
	if err != nil {
		return releasetransition.ProcessResponse{}, err
	}
	runtime.rustOwners.mu.Lock()
	if runtime.rustOwners.closed {
		runtime.rustOwners.mu.Unlock()
		return releasetransition.ProcessResponse{}, domain.ErrPlanStale
	}
	session := runtime.rustOwners.sessions[key]
	if session != nil && request.Mode == releasetransition.ProcessInspect {
		session.mu.Lock()
		finished := session.closed || session.used
		session.mu.Unlock()
		if finished {
			session = nil
		}
	}
	if session == nil && request.Mode == releasetransition.ProcessInspect {
		session, err = runtime.startRustOwner(ctx, assets, engine, target, request, grant)
		if err == nil {
			runtime.rustOwners.sessions[key] = session
		}
	}
	runtime.rustOwners.mu.Unlock()
	if err != nil {
		return releasetransition.ProcessResponse{}, err
	}
	if session == nil {
		return releasetransition.ProcessResponse{}, errors.New("Rust execution has no retained original preparation")
	}
	response := releasetransition.ProcessResponse{SchemaVersion: releasetransition.ProcessProtocolSchemaV1}
	switch request.Mode {
	case releasetransition.ProcessInspect:
		inspection, err := session.Inspect(ctx, recovery)
		if err != nil {
			session.Close()
			return response, err
		}
		response.Inspection = &inspection
	case releasetransition.ProcessConverge:
		if request.Execution == nil {
			return response, errors.New("Rust execution is missing its exact plan")
		}
		outcome, err := session.Execute(ctx, request.Execution.Plan, runtime.config.Inputs.value("SUBYARD_OWNER_LOCK_FD", runtime.config.Context))
		if err != nil {
			session.Close()
			return response, err
		}
		response.Outcome = &outcome
	default:
		return response, errors.New("unsupported Rust transition mode")
	}
	session.mu.Lock()
	response.ActivationReconciliationOwned = session.activationOwned
	session.mu.Unlock()
	return response, nil
}

// Verify the currently executing helper against the declared owner's manifest.
// The frozen ingress cannot reinterpret old assets through this current helper.
func (runtime *Runtime) verifyFrozenRustOwner(ctx context.Context, repositoryRoot string) (*verifiedPublishedCandidate, string, error) {
	root, err := filepath.EvalSymlinks(repositoryRoot)
	if err != nil || !filepath.IsAbs(root) {
		return nil, "", errors.New("frozen Rust owner root is unavailable")
	}
	runtimeRoot := filepath.Dir(filepath.Dir(root))
	release := releasetransition.ReleaseID(filepath.Base(root))
	if root != filepath.Join(runtimeRoot, "releases", string(release)) {
		return nil, "", errors.New("frozen Rust owner must be a verified published runtime")
	}
	verified, err := runtime.verifyPublishedCandidate(ctx, publishedCandidate{root: root, release: release}, runtimeRoot, nil)
	if err != nil {
		return nil, "", err
	}
	fail := func(err error) (*verifiedPublishedCandidate, string, error) { verified.Close(); return nil, "", err }
	if verified.releaseOwner == nil {
		return fail(errors.New("current release tooling requires its declared Rust owner; run the exact verified historical Go owner for historical assets"))
	}
	image, err := os.Open("/proc/self/exe")
	if err != nil {
		return fail(err)
	}
	digest := sha256.New()
	_, copyErr := io.Copy(digest, image)
	closeErr := image.Close()
	if copyErr != nil || closeErr != nil || hex.EncodeToString(digest.Sum(nil)) != verified.producerEvidence.EngineSHA256 {
		return fail(errors.New("executing native helper does not match the declared Rust owner's exact helper image"))
	}
	return verified, runtimeRoot, nil
}

// InvokeFrozenRustTransition retains the frozen PV1/schema2 codecs while keeping
// inspection and convergence policy entirely in the selected Rust owner.
func (runtime *Runtime) InvokeFrozenRustTransition(ctx context.Context, repositoryRoot string, request releasetransition.ProcessRequest, recovery *releasetransition.RecoveryProcessRequest, expectedGrant releasetransition.Authorization) (releasetransition.ProcessResponse, error) {
	owner, _, err := runtime.verifyFrozenRustOwner(ctx, repositoryRoot)
	if err != nil {
		return releasetransition.ProcessResponse{}, err
	}
	defer owner.Close()
	if request.RegistryDigest != "" && request.RegistryDigest != owner.registryDigest {
		return releasetransition.ProcessResponse{}, errors.New("frozen registry does not match its selected Rust owner")
	}
	target := owner
	if owner.candidate.release != request.Target || owner.manifestDigest != request.ArtifactDigest {
		target, err = runtime.verifyPublishedCandidate(ctx, publishedCandidate{release: request.Target, root: filepath.Join(request.RuntimeRoot, "releases", string(request.Target))}, request.RuntimeRoot, &request.ArtifactDigest)
		if err != nil {
			return releasetransition.ProcessResponse{}, err
		}
		defer target.Close()
	}
	inspectedRequest := request
	inspectedRequest.Mode, inspectedRequest.Execution = releasetransition.ProcessInspect, nil
	response, err := runtime.invokeRustOwnerTransition(ctx, owner, owner, target, inspectedRequest, recovery, expectedGrant)
	if err != nil || request.Mode == releasetransition.ProcessInspect {
		return response, err
	}
	if request.Mode != releasetransition.ProcessConverge || request.Execution == nil || response.Inspection == nil || request.Execution.Plan != response.Inspection.Plan {
		return releasetransition.ProcessResponse{}, domain.ErrPlanStale
	}
	session := runtime.findRustOwnerSession(owner.manifestDigest, inspectedRequest, recovery)
	if session == nil {
		return releasetransition.ProcessResponse{}, domain.ErrPlanStale
	}
	session.mu.Lock()
	durableAdmission := session.admission != nil
	session.mu.Unlock()
	if !durableAdmission && response.Inspection.Resume == nil && response.Inspection.Assessment.Changed {
		if expectedGrant == "" || request.Execution.Authorization != expectedGrant {
			return releasetransition.ProcessResponse{}, errors.New("frozen Rust convergence is missing its original private parent authorization")
		}
	} else if request.Execution.Authorization != "" && request.Execution.Authorization != expectedGrant {
		return releasetransition.ProcessResponse{}, errors.New("frozen Rust convergence authorization differs from its parent")
	}
	// Historical PV1 callers have the real private grant but no account-barrier
	// descriptor role. Only this frozen bridge may establish the missing barrier,
	// after the exact inspected plan and inherited authority have been checked.
	// A supplied invalid descriptor always refuses; it is never replaced.
	if durableAdmission || response.Inspection.Resume != nil || response.Inspection.Assessment.Changed {
		barrier, barrierErr := borrowRustOwnerBarrier(session.options.DataHome, runtime.config.Inputs.value("SUBYARD_OWNER_LOCK_FD", runtime.config.Context))
		if errors.Is(barrierErr, errRustOwnerBarrierAbsent) {
			_, release, err := uninstall.OwnerLock(ctx, session.options.DataHome, nil)
			if err != nil {
				return releasetransition.ProcessResponse{}, err
			}
			defer release()
		} else if barrierErr != nil {
			return releasetransition.ProcessResponse{}, barrierErr
		} else {
			barrier.Close()
		}
	}
	return runtime.invokeRustOwnerTransition(ctx, owner, owner, target, request, recovery, expectedGrant)
}

func (session *rustOwnerSession) Capabilities(ctx context.Context, kind releasetransition.RecoveryProcessMode) (releasetransition.RecoveryProcessResponse, error) {
	reply, err := session.exchange(ctx, map[string]any{"op": "capabilities", "kind": kind}, rustOwnerExchangeLimit)
	if err != nil {
		return releasetransition.RecoveryProcessResponse{}, err
	}
	if reply.Custody != nil || reply.Inspection != nil || reply.Outcome != nil || reply.Admission != nil || reply.RetainedReleases != nil {
		return releasetransition.RecoveryProcessResponse{}, errors.New("Rust owner returned an invalid capability response")
	}
	response := releasetransition.RecoveryProcessResponse{SchemaVersion: releasetransition.ProcessRecoverySchemaV2,
		ActivationReconciliationOwned: reply.ActivationReconciliationOwned, Capabilities: reply.Capabilities, ActivationOwners: reply.ActivationOwners}
	raw, err := releasetransition.MarshalRecoveryProcessResponse(response)
	if err != nil {
		return releasetransition.RecoveryProcessResponse{}, err
	}
	return releasetransition.ParseRecoveryProcessResponse(raw)
}

func (runtime *Runtime) InvokeFrozenRustCapabilities(ctx context.Context, repositoryRoot string, kind releasetransition.RecoveryProcessMode) (releasetransition.RecoveryProcessResponse, error) {
	owner, root, err := runtime.verifyFrozenRustOwner(ctx, repositoryRoot)
	if err != nil {
		return releasetransition.RecoveryProcessResponse{}, err
	}
	defer owner.Close()
	configHome := runtime.config.Inputs.value("SUBYARD_CONFIG_HOME", runtime.config.Context)
	if !filepath.IsAbs(configHome) {
		return releasetransition.RecoveryProcessResponse{}, errors.New("Rust capability inspection requires its pinned configuration scope")
	}
	request := releasetransition.ProcessRequest{SchemaVersion: 1, Mode: releasetransition.ProcessInspect,
		RuntimeRoot: root, ConfigHome: configHome, Yard: "default", Target: owner.candidate.release,
		Direction: releasetransition.DirectionActivateTarget, ArtifactDigest: owner.manifestDigest, RegistryDigest: owner.registryDigest}
	session, err := runtime.startRustOwner(ctx, owner, owner, owner, request, "")
	if err != nil {
		return releasetransition.RecoveryProcessResponse{}, fmt.Errorf("open Rust capability owner: %w", err)
	}
	defer session.Close()
	return session.Capabilities(ctx, kind)
}

// Attach a prepared operation to the original process, even if a later check
// opens another read-only session for the same release scope.
func (runtime *Runtime) retainRustPreparation(prepared Prepared, session *rustOwnerSession, parsed options, request releasetransition.ProcessRequest, inspection releasetransition.Inspection) Prepared {
	prepared.Close = session.Close
	prepared.run = func(ctx context.Context) error {
		defer session.Close()
		if err := requirePreparedReleaseRoots(parsed, false); err != nil {
			return err
		}
		if parsed.expectedLinks != nil {
			links, err := runtime.inspectRuntimeLinks(parsed.root)
			if err != nil || links != *parsed.expectedLinks {
				return fmt.Errorf("%w: runtime links changed after inspection", domain.ErrPlanStale)
			}
		}
		outcome, err := session.Execute(ctx, inspection.Plan, runtime.config.Inputs.value("SUBYARD_OWNER_LOCK_FD", runtime.config.Context))
		if err != nil {
			return err
		}
		if err := releasetransition.ValidateProcessConvergence(releasetransition.Goal{Target: request.Target, Direction: request.Direction}, inspection, outcome); err != nil {
			return err
		}
		if outcome.Status != releasetransition.StatusReady {
			return transitionOutcomeError(outcome)
		}
		return nil
	}
	return prepared
}

func (runtime *Runtime) invokeRustRecoveryTransition(ctx context.Context, owner *verifiedPublishedCandidate, root string, request releasetransition.RecoveryProcessRequest, grant releasetransition.Authorization) (releasetransition.RecoveryProcessResponse, error) {
	capability := request.Mode == releasetransition.RecoveryProcessCapabilities || request.Mode == releasetransition.RecoveryProcessLifecycleCapabilities || request.Mode == releasetransition.RecoveryProcessRuntimeCapabilities || request.Mode == releasetransition.RecoveryProcessMigrationOwnerCapabilities
	if capability {
		configHome := runtime.config.Inputs.value("SUBYARD_CONFIG_HOME", runtime.config.Context)
		if !filepath.IsAbs(configHome) {
			return releasetransition.RecoveryProcessResponse{}, errors.New("Rust capability inspection requires its pinned configuration scope")
		}
		scope := releasetransition.ProcessRequest{SchemaVersion: 1, Mode: releasetransition.ProcessInspect, RuntimeRoot: root, ConfigHome: configHome, Yard: "default", Target: owner.candidate.release, Direction: releasetransition.DirectionActivateTarget, ArtifactDigest: owner.manifestDigest, RegistryDigest: owner.registryDigest}
		session, err := runtime.startRustOwner(ctx, owner, owner, owner, scope, "")
		if err != nil {
			return releasetransition.RecoveryProcessResponse{}, err
		}
		defer session.Close()
		return session.Capabilities(ctx, request.Mode)
	}
	response, err := runtime.invokeRustOwnerTransition(ctx, owner, owner, owner, request.TransitionRequest(), &request, grant)
	if err != nil {
		return releasetransition.RecoveryProcessResponse{}, err
	}
	return releasetransition.RecoveryProcessResponse{SchemaVersion: releasetransition.ProcessRecoverySchemaV2, ActivationReconciliationOwned: response.ActivationReconciliationOwned, Inspection: response.Inspection, Outcome: response.Outcome}, nil
}
