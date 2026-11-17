package releaseruntime

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

// VerifyRollbackTargetAssets checks both sealed endpoints before R constructs
// activation adapters. Structural source provenance grants no execution support.
func VerifyRollbackTargetAssets(ctx context.Context, repositoryRoot string, request releasetransition.RollbackTargetRequest) (targetRegistry []byte, targetVersion string, err error) {
	if err := request.Validate(); err != nil {
		return nil, "", err
	}
	if request.Mode != "activate-under-v2" {
		return nil, "", errors.New("rollback target assets require an activation request")
	}
	runtime := New(Config{})
	defer runtime.Close()
	source, sourceErr := runtime.verifyPublishedCandidate(ctx, publishedCandidate{release: request.Releases.From, root: filepath.Join(request.RuntimeRoot, "releases", string(request.Releases.From))}, request.RuntimeRoot, &request.SourceArtifact)
	target, targetErr := runtime.verifyPublishedCandidate(ctx, publishedCandidate{release: request.Releases.Target, root: filepath.Join(request.RuntimeRoot, "releases", string(request.Releases.Target))}, request.RuntimeRoot, &request.TargetArtifact)
	refusals := []error{sourceErr, targetErr}
	if source != nil {
		defer source.Close()
		if !strings.HasPrefix(string(source.candidate.release), source.version+"-") {
			refusals = append(refusals, errors.New("rollback source identity differs from its sealed version"))
		}
		if source.registryDigest != request.SourceRegistry {
			refusals = append(refusals, errors.New("rollback source registry binding changed"))
		}
		payload, readErr := readRollbackRegistryPayload(source)
		if readErr != nil {
			refusals = append(refusals, readErr)
		} else if _, digest, parseErr := releasetransition.ParseRegistryV2Provenance(payload); parseErr != nil || digest != source.registryDigest {
			refusals = append(refusals, errors.New("rollback source registry has invalid structural provenance"))
		}
	}
	if target != nil {
		defer target.Close()
		targetVersion = target.version
		if !strings.HasPrefix(string(target.candidate.release), target.version+"-") {
			refusals = append(refusals, errors.New("rollback target identity differs from its sealed version"))
		}
		actual, statErr := os.Stat(repositoryRoot)
		expected, targetStatErr := target.root.Stat()
		if statErr != nil || targetStatErr != nil || !os.SameFile(actual, expected) {
			refusals = append(refusals, errors.New("rollback activation is not running from its sealed target assets"))
		}
		if target.registryDigest != request.TargetRegistry {
			refusals = append(refusals, errors.New("rollback target registry binding changed"))
		}
		var readErr error
		targetRegistry, readErr = readRollbackRegistryPayload(target)
		if readErr != nil {
			refusals = append(refusals, readErr)
		} else {
			registry, digest, parseErr := releasetransition.ParseRegistryV2(targetRegistry, releasetransition.BuiltinCapabilityCatalog())
			if parseErr != nil || digest != target.registryDigest {
				refusals = append(refusals, errors.New("rollback target registry is not executable by this target"))
			} else {
				pending, historyErr := registry.PendingPath(*request.NativeHistory)
				if historyErr != nil || len(pending) != 0 {
					refusals = append(refusals, errors.New("rollback target preview is not complete native history"))
				}
			}
		}
	}
	store, storeErr := releasetransition.NewPOSIXV2Store(request.ConfigHome)
	if storeErr != nil {
		refusals = append(refusals, storeErr)
	} else {
		projection, readErr := store.ReadLedger()
		if readErr != nil {
			refusals = append(refusals, readErr)
		} else if projection.Fingerprint != request.Projection {
			refusals = append(refusals, errors.New("rollback target projection changed"))
		}
	}
	if err := domain.CollectRefusals(refusals...); err != nil {
		return nil, "", err
	}
	return targetRegistry, targetVersion, nil
}

func readRollbackRegistryPayload(candidate *verifiedPublishedCandidate) ([]byte, error) {
	if candidate.registryDigest == "" {
		// Historical completion may predate V2. Native contract admission
		// separately requires a nonempty, valid sealed registry payload.
		return nil, nil
	}
	file, err := openCandidateFile(int(candidate.root.Fd()), "config/release-transition.json")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	payload, err := io.ReadAll(io.LimitReader(file, releasetransition.MaxRegistryV2Bytes+1))
	if err != nil {
		return nil, err
	}
	if len(payload) > releasetransition.MaxRegistryV2Bytes {
		return nil, errors.New("rollback registry exceeds its bound")
	}
	return payload, nil
}
