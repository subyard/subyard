package cli

import (
	"context"
	"encoding/json"
	"io"

	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

type releaseOwnerFacadeInputError string

func (failure releaseOwnerFacadeInputError) Error() string { return string(failure) }

// The frozen ingress owns codecs and inherited authority; the selected Rust
// process owns inspection, eligibility, preparation, and convergence.
func (cli *CLI) invokeRustReleaseTransitionFacade(ctx context.Context, payload []byte, output io.Writer) error {
	var envelope struct {
		SchemaVersion int `json:"schemaVersion"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return releaseOwnerFacadeInputError("release transition request is invalid")
	}
	var request releasetransition.ProcessRequest
	var recovery *releasetransition.RecoveryProcessRequest
	if envelope.SchemaVersion == releasetransition.ProcessRecoverySchemaV2 {
		parsed, err := releasetransition.ParseRecoveryProcessRequest(payload)
		if err != nil {
			return releaseOwnerFacadeInputError("release recovery request is invalid")
		}
		recovery = &parsed
		request = parsed.TransitionRequest()
	} else if envelope.SchemaVersion != releasetransition.ProcessProtocolSchemaV1 || json.Unmarshal(payload, &request) != nil {
		return releaseOwnerFacadeInputError("release transition request is invalid")
	}
	if !domain.SafeID(cli.ensureOperationID()) {
		return releaseOwnerFacadeInputError("release transition operation context is invalid")
	}
	runtime := releaseruntime.New(releaseruntime.Config{
		Inputs: cli.commandInputs, RepositoryRoot: cli.options.RepositoryRoot, Stderr: cli.options.Stderr,
		Context: releaseruntime.RuntimeContext{OperatorHome: firstNonempty(cli.env["SUBYARD_OPERATOR_HOME"], cli.env["HOME"]),
			DataHome: cli.env["SUBYARD_HOME"], ConfigHome: firstNonempty(request.ConfigHome, cli.env["SUBYARD_CONFIG_HOME"]),
			RuntimeRoot: request.RuntimeRoot, OperationID: cli.env["SUBYARD_OPERATION_ID"]},
	})
	defer runtime.Close()
	if recovery != nil && (recovery.Mode == releasetransition.RecoveryProcessCapabilities ||
		recovery.Mode == releasetransition.RecoveryProcessLifecycleCapabilities ||
		recovery.Mode == releasetransition.RecoveryProcessRuntimeCapabilities ||
		recovery.Mode == releasetransition.RecoveryProcessMigrationOwnerCapabilities) {
		response, err := runtime.InvokeFrozenRustCapabilities(ctx, cli.options.RepositoryRoot, recovery.Mode)
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(response)
	}
	grant, err := readReleaseTransitionGrant()
	if err != nil {
		return releaseOwnerFacadeInputError("release transition authorization channel is invalid")
	}
	response, err := runtime.InvokeFrozenRustTransition(ctx, cli.options.RepositoryRoot, request, recovery, grant)
	if err != nil {
		return err
	}
	if recovery != nil {
		return json.NewEncoder(output).Encode(releasetransition.RecoveryProcessResponse{
			SchemaVersion: releasetransition.ProcessRecoverySchemaV2, ActivationReconciliationOwned: response.ActivationReconciliationOwned,
			Inspection: response.Inspection, Outcome: response.Outcome})
	}
	return json.NewEncoder(output).Encode(response)
}
