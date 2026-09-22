package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"sync"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/transport"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/rpc"
)

const exactPlanCapability = "operation-exact-plan-v1"
const exactPlanLifetime = 5 * time.Minute

// The additive capability leaves legacy callers unchanged. Exact clients must
// retain this envelope and execute its digest over the same negotiated session.
type exactOperationPlan struct {
	Schema    int                  `json:"schema"`
	Plan      domain.OperationPlan `json:"plan"`
	Digest    string               `json:"digest"`
	ExpiresAt time.Time            `json:"expiresAt"`
}

func operationStateDigest(value any) string {
	payload, err := json.Marshal(value)
	if err != nil {
		panic("operation binding must be JSON encodable")
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func exactOperationDigest(prepared *preparedCommand, plan exactOperationPlan) string {
	return operationStateDigest(struct {
		Schema    int
		Plan      domain.OperationPlan
		ExpiresAt time.Time
		Context   domain.Context
		Arguments []string
		State     string
	}{plan.Schema, plan.Plan, plan.ExpiresAt, prepared.Loaded.Context, prepared.Arguments, prepared.exactState})
}

func bindExactOperationPlan(prepared *preparedCommand, now time.Time) exactOperationPlan {
	plan := exactOperationPlan{Schema: 1, Plan: prepared.Plan, ExpiresAt: now.Add(exactPlanLifetime)}
	plan.Digest = exactOperationDigest(prepared, plan)
	return plan
}

func validateExactOperationPlan(plan exactOperationPlan, prepared *preparedCommand, digest string, now time.Time) error {
	if !now.Before(plan.ExpiresAt) {
		return fmt.Errorf("%w: operation plan expired", domain.ErrPlanStale)
	}
	if plan.Schema != 1 || digest == "" || digest != plan.Digest || digest != exactOperationDigest(prepared, plan) || operationStateDigest(plan.Plan) != operationStateDigest(prepared.Plan) {
		return errors.New("operation plan digest does not match the stored plan")
	}
	return nil
}

// ownerRPCSession owns one SSH process for negotiation, preview, confirmation,
// and execution. Closing it discards owner-side plans; there is no replay path.
type ownerRPCSession struct {
	codec       *rpc.Codec
	close       func() error
	diagnostics func() []byte
}

func (cli *CLI) openOwnerRPC(ctx context.Context, yard domain.Context) (*ownerRPCSession, error) {
	ownerYard := yard.OwnerYardName
	if ownerYard == "" {
		ownerYard = "default"
	}
	process, err := transport.SSHYard("ssh", yard.OwnerEndpoint, ownerYard, 3*time.Second)
	if err != nil {
		return nil, err
	}
	options, err := transport.SSHOptions(ctx, process.Program, yard.OwnerEndpoint)
	if err != nil {
		return nil, err
	}
	process.Arguments = append(options, process.Arguments...)
	sessionContext, cancel := context.WithCancel(ctx)
	command := exec.CommandContext(sessionContext, process.Program, process.Arguments...)
	command.Env = environmentList(cli.env, nil)
	command.Dir = cli.options.WorkingDir
	stderr := &boundedResourceBuffer{limit: rpc.MaxFrameSize}
	command.Stderr = stderr
	configureResourceProcess(command)
	input, err := command.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	// Own the read end separately: Cmd.Wait must not close unread final frames.
	output, remoteOutput, err := os.Pipe()
	if err != nil {
		cancel()
		_ = input.Close()
		return nil, err
	}
	command.Stdout = remoteOutput
	if err = command.Start(); err != nil {
		cancel()
		_ = input.Close()
		_ = output.Close()
		_ = remoteOutput.Close()
		return nil, err
	}
	_ = remoteOutput.Close()
	done := make(chan struct{})
	go func() { _ = command.Wait(); close(done) }()
	session := &ownerRPCSession{codec: rpc.NewCodec(output, input), diagnostics: stderr.Bytes}
	var once sync.Once
	session.close = func() error {
		once.Do(func() { _ = input.Close(); cancel(); _ = output.Close(); <-done })
		return nil
	}
	return session, nil
}

func (session *ownerRPCSession) call(ctx context.Context, method, operationID string, params any, result any) error {
	stop := context.AfterFunc(ctx, func() { _ = session.close() })
	defer stop()
	payload, err := json.Marshal(params)
	if err != nil {
		return err
	}
	request := rpc.Request{Version: rpc.ProtocolVersion, Type: "request", ID: method, Method: method, OperationID: operationID, Params: payload}
	// Enforce controller deadlines through session cancellation, without sending
	// an absolute timestamp to an owner whose wall clock may differ.
	if err = session.codec.Write(request); err != nil {
		return fmt.Errorf("owner RPC disconnected: %w", err)
	}
	for events := 0; events < 4096; events++ {
		response, err := session.codec.ReadResponse()
		if err != nil {
			return fmt.Errorf("owner RPC disconnected: %w", err)
		}
		if response.Version != rpc.ProtocolVersion || response.OperationID != operationID {
			return errors.New("owner RPC response identity mismatch")
		}
		if response.Type == "event" {
			continue
		}
		if response.Type != "response" || response.ID != request.ID {
			return errors.New("owner RPC response identity mismatch")
		}
		if response.Error != nil {
			return response.Error
		}
		encoded, err := json.Marshal(response.Result)
		if err != nil {
			return err
		}
		return json.Unmarshal(encoded, result)
	}
	return errors.New("owner RPC event limit exceeded")
}

func (session *ownerRPCSession) negotiate(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var response struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := session.call(ctx, "rpc.negotiate", "", struct{}{}, &response); err != nil {
		return err
	}
	if !slices.Contains(response.Capabilities, exactPlanCapability) {
		return errors.New("owner does not support exact operation plans; update Subyard on the owner host")
	}
	return nil
}

func (session *ownerRPCSession) integrationStatus(ctx context.Context, yard domain.Context, operationID, id string) (integrationStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var status integrationStatus
	if err := session.call(ctx, "integration.status", operationID, struct {
		ID        string `json:"id"`
		OwnerOnly bool   `json:"ownerOnly"`
	}{id, true}, &status); err != nil {
		return status, err
	}
	ownerYard := yard.OwnerYardName
	if ownerYard == "" {
		ownerYard = "default"
	}
	if status.Yard != ownerYard || status.ID != id ||
		!slices.Contains([]string{"ready", "pending", "conflict", "stopped", "missing", "unknown"}, status.Observed) {
		return integrationStatus{}, errors.New("owner returned an invalid integration status")
	}
	return status, nil
}

func (prepared *preparedCommand) prepareRemoteOperation(ctx context.Context) error {
	cli := prepared.CLI
	request, err := parseIntegrationArguments(prepared.Arguments)
	if err != nil {
		return err
	}
	session, err := cli.openOwnerRPC(ctx, prepared.Loaded.Context)
	if err != nil {
		return err
	}
	prepared.closeResource = func() error {
		err := session.close()
		// Join the SSH writer before reporting diagnostics through caller-owned
		// streams, which can share a non-concurrent writer with the preview.
		_, _ = cli.options.Stderr.Write(session.diagnostics())
		return err
	}
	if err = session.negotiate(ctx); err != nil {
		return err
	}
	planContext, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if request.verb == "status" {
		status, err := session.integrationStatus(planContext, prepared.Loaded.Context, cli.ensureOperationID(), request.id)
		if err != nil {
			return err
		}
		prepared.displayOnly = func() { cli.printIntegrationStatus(status, request.json) }
		return nil
	}
	var exact exactOperationPlan
	params := struct {
		Command   string   `json:"command"`
		Arguments []string `json:"arguments"`
		Exact     bool     `json:"exact"`
	}{prepared.Definition.Name, keysWithoutConsent(prepared.Arguments), true}
	if err = session.call(planContext, "operation.plan", cli.ensureOperationID(), params, &exact); err != nil {
		return err
	}
	digest, decodeErr := hex.DecodeString(exact.Digest)
	// Native preparation pre-confirms actions whose concrete policy is never,
	// including converged no-ops. Prompting actions still require this controller.
	preconfirmedPrompt := exact.Plan.Confirmed && exact.Plan.Confirmation != domain.ConfirmationNever
	if exact.Schema != 1 || exact.Plan.OperationID != cli.ensureOperationID() || exact.Plan.Command != prepared.Definition.Name || exact.Plan.Effect != domain.CommandMutate || exact.Plan.Target != domain.TargetLocalOwner || preconfirmedPrompt || decodeErr != nil || len(digest) != sha256.Size || exact.ExpiresAt.IsZero() {
		return errors.New("owner returned an invalid exact operation plan")
	}
	prepared.Plan = exact.Plan
	prepared.ownerPlan = true
	prepared.executeNoOp = true // Even no-op plans recheck owner live preconditions.
	prepared.preview = func() {
		for _, step := range exact.Plan.Consequences {
			fmt.Fprintln(cli.options.Stdout, "  "+step)
		}
	}
	prepared.execute = func(ctx context.Context, _ *application.Orchestrator, _ io.Writer) (domain.AdapterResult, error) {
		// The owner validates expiry against the same clock that issued the plan.
		var response struct {
			Plan   domain.OperationPlan `json:"plan"`
			Result domain.AdapterResult `json:"result"`
		}
		err := session.call(ctx, "operation.execute", exact.Plan.OperationID, struct {
			Confirmed bool   `json:"confirmed"`
			Digest    string `json:"digest"`
		}{true, exact.Digest}, &response)
		if err != nil {
			return domain.AdapterResult{}, err
		}
		if response.Plan.OperationID != exact.Plan.OperationID || response.Result.OperationID != exact.Plan.OperationID {
			return domain.AdapterResult{}, errors.New("owner execution response identity mismatch")
		}
		return response.Result, nil
	}
	return nil
}
