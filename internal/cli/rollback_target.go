package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"

	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

func (cli *CLI) runRollbackTargetProcess(ctx context.Context, payload []byte, output io.Writer) int {
	request, err := releasetransition.ParseRollbackTargetRequest(payload)
	if err != nil || request.Mode != "activate-under-v2" {
		cli.errorf("rollback target request is invalid")
		return 2
	}
	var lock *os.File
	if request.Execution != nil {
		fd, err := strconv.Atoi(os.Getenv("SUBYARD_RELEASE_TRANSITION_LOCK_FD"))
		if err != nil || fd < 3 || fd > 1<<31-1 {
			cli.errorf("rollback target transition lock descriptor is invalid")
			return 2
		}
		unix.CloseOnExec(fd)
		lock = os.NewFile(uintptr(fd), "inherited release transition lock")
		defer lock.Close()
	}
	registry, version, err := releaseruntime.VerifyRollbackTargetAssets(ctx, cli.options.RepositoryRoot, request)
	if err != nil {
		cli.errorf("rollback target assets: %v", err)
		return 1
	}
	if !domain.SafeID(cli.ensureOperationID()) {
		cli.errorf("rollback target operation context is invalid")
		return 2
	}
	actorRequest := releasetransition.ProcessRequest{SchemaVersion: releasetransition.ProcessProtocolSchemaV1, Mode: releasetransition.ProcessInspect, RuntimeRoot: request.RuntimeRoot, ConfigHome: request.ConfigHome, Yard: request.Yard, Target: request.Releases.Target, Direction: releasetransition.DirectionActivatePrevious, ArtifactDigest: request.TargetArtifact, RegistryDigest: request.TargetRegistry, InheritedSettingIDs: request.InheritedSettingIDs}
	if lock != nil {
		actorRequest.Mode = releasetransition.ProcessConverge
		if err := releaseruntime.CheckSourcePolicy(ctx, actorRequest); err != nil {
			cli.errorf("rollback target source policy: %v", err)
			return 1
		}
	}
	copy := *cli
	copy.activation = nil
	operation := copy.releaseActivationOperation(actorRequest)
	// Only this verified target operation may use proposed native completion.
	// Actual native history is checked again before any activation effects.
	operation.activation.rollbackPreview = true
	reconcilers := operation.bindServiceRegistrationActivation(actorRequest, operation.activationReconcilers(actorRequest))
	registration := operation.ownerRegistrationTransition(actorRequest)
	links, err := releasetransition.NewRuntimeLinkStore(request.RuntimeRoot)
	if err != nil {
		cli.errorf("rollback target links: %v", err)
		return 1
	}
	options := releasetransition.V2Options{ConfigHome: request.ConfigHome, Releases: request.Releases, Direction: releasetransition.DirectionActivatePrevious,
		OperatorHome: os.Getenv("SUBYARD_OPERATOR_HOME"), CandidateConfigDir: filepath.Join(cli.options.RepositoryRoot, "config"), CandidateVersion: Version,
		RegistryPayload: registry, ArtifactDigest: request.TargetArtifact, RollbackTarget: &releasetransition.RollbackTarget{Version: version, RegistryDigest: request.TargetRegistry},
		Reconcilers: reconcilers, OwnerRegistration: registration, InheritedSettingIDs: request.InheritedSettingIDs,
		ObserveLinks:        func(context.Context) (releasetransition.ReleaseLinks, error) { return links.Observe() },
		VerifyAuthorization: func(releasetransition.PlanToken, releasetransition.Authorization) bool { return false },
	}
	if owner, ok := registration.(interface {
		registeredMigrationOwners() ([]releasetransition.V2ActivationReconciler, []releasetransition.V2ActivationReconciler, []string)
	}); ok {
		options.RegisteredYards, options.GuestDiskTmp, options.RegisteredEndpointSettings = owner.registeredMigrationOwners()
	}
	transition, err := releasetransition.NewV2Transition(options)
	if err != nil {
		cli.errorf("rollback target construction: %v", err)
		return 1
	}
	ctx = releasetransition.WithMigrationDiagnosticWriter(ctx, cli.options.Stderr)
	var response releasetransition.RollbackTargetResponse
	if request.Execution == nil {
		inspection, inspectErr := transition.InspectRollbackTargetActivation(ctx, request)
		if inspectErr != nil {
			cli.errorf("rollback target inspection: %v", inspectErr)
			return 1
		}
		response = releasetransition.RollbackTargetResponse{SchemaVersion: releasetransition.RollbackTargetProcessSchemaV1, Inspection: &inspection}
	} else {
		response, err = transition.ConvergeRollbackTargetActivation(ctx, request, lock)
		if err != nil {
			cli.errorf("rollback target activation: %v", err)
			return 1
		}
	}
	if err := response.Validate(); err != nil {
		cli.errorf("rollback target response is invalid")
		return 1
	}
	if err := json.NewEncoder(output).Encode(response); err != nil {
		return 1
	}
	return 0
}
