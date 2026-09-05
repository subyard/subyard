package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/shelladapter"
	"github.com/Subyard/Subyard/internal/adapters/testvmsruntime"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

func testVMLogsInvocation(arguments []string) bool {
	for _, argument := range arguments {
		if argument == "-y" || argument == "--yes" {
			continue
		}
		return argument == "logs"
	}
	return false
}

func testVMStatusInvocation(arguments []string) bool {
	for _, argument := range arguments {
		if argument == "-y" || argument == "--yes" {
			continue
		}
		return argument == "status"
	}
	return false
}

func (cli *CLI) runTestVMLogs(ctx context.Context, arguments []string) int {
	lines := 200
	follow := false
	slotID := ""
	actionSeen := false
	for index := 0; index < len(arguments); index++ {
		switch argument := arguments[index]; argument {
		case "logs":
			if actionSeen {
				cli.errorf("test-vms logs: accepts one command")
				return 2
			}
			actionSeen = true
		case "-n":
			index++
			if index >= len(arguments) {
				cli.errorf("test-vms logs: -n needs a positive number")
				return 2
			}
			value, err := strconv.Atoi(arguments[index])
			if err != nil || value < 1 || value > 100000 {
				cli.errorf("test-vms logs: -n needs a number from 1 to 100000")
				return 2
			}
			lines = value
		case "-f":
			follow = true
		case "--slot":
			index++
			if index >= len(arguments) {
				cli.errorf("test-vms logs: --slot needs a number")
				return 2
			}
			value, err := strconv.Atoi(arguments[index])
			if err != nil || value < 1 || value > 999 {
				cli.errorf("test-vms logs: --slot needs a number from 1 to 999")
				return 2
			}
			slotID = fmt.Sprintf("slot-%03d", value)
		case "-y", "--yes":
		case "-h", "--help":
			fmt.Fprintf(
				cli.options.Stdout,
				"Usage: %s test-vms logs [-n N] [-f] [--slot N]\n",
				cli.options.Program,
			)
			return 0
		default:
			cli.errorf("test-vms logs: unknown option %q", argument)
			return 2
		}
	}
	if !actionSeen {
		cli.errorf("test-vms logs: logs command is required")
		return 2
	}
	dataHome := cli.env["SUBYARD_HOME"]
	if dataHome == "" {
		operatorHome := cli.env["SUBYARD_OPERATOR_HOME"]
		if operatorHome == "" {
			operatorHome = cli.env["HOME"]
		}
		if operatorHome == "" || !filepath.IsAbs(operatorHome) {
			cli.errorf("test-vms logs: operator home is unavailable")
			return 1
		}
		dataHome = filepath.Join(operatorHome, ".subyard")
	}
	if !filepath.IsAbs(dataHome) {
		cli.errorf("test-vms logs: Subyard data home must be absolute")
		return 1
	}
	path := filepath.Join(dataHome, "logs", testvmsruntime.HostEventLogName)
	seen := map[string]bool{}
	printCurrent := func(limit int) error {
		events, err := testvmsruntime.ReadHostBrokerEvents(path, limit, slotID)
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(cli.options.Stdout)
		encoder.SetEscapeHTML(false)
		for _, event := range events {
			if seen[event.EventID] {
				continue
			}
			if err := encoder.Encode(event); err != nil {
				return err
			}
			seen[event.EventID] = true
		}
		return nil
	}
	if err := printCurrent(lines); err != nil {
		cli.errorf("test-vms logs: %v", err)
		return 1
	}
	if !follow {
		return 0
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return 0
		case <-ticker.C:
			if _, err := os.Stat(path); err != nil && !os.IsNotExist(err) {
				cli.errorf("test-vms logs: %v", err)
				return 1
			}
			if err := printCurrent(100000); err != nil {
				cli.errorf("test-vms logs: %v", err)
				return 1
			}
		}
	}
}

type testVMExecution struct {
	action      string
	slot        int
	identity    testvmsruntime.LeaseIdentity
	hasSnapshot bool
	noOp        bool
}

const testVMStatusMaxBytes = 64 << 10

type testVMStatusResponse struct {
	SchemaVersion int                       `json:"schema_version"`
	Status        string                    `json:"status"`
	Code          string                    `json:"code"`
	Message       string                    `json:"message"`
	Pool          *testvmsruntime.LeasePool `json:"pool"`
}

func (cli *CLI) prepareTestVMExecution(
	ctx context.Context,
	loaded config.Loaded,
	arguments []string,
) (*testVMExecution, error) {
	if !loaded.Context.NestedE2EVMs {
		return nil, errors.New("nested E2E VMs are disabled for this yard")
	}
	if loaded.Context.YardKind != domain.YardContainer {
		return nil, errors.New("nested E2E VMs require a container yard")
	}
	action := ""
	slot := 0
	var expectedGeneration, expectedEpoch uint64
	var expectedGenerationSet, expectedEpochSet bool
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		switch argument {
		case "-y", "--yes":
		case "-h", "--help":
			return nil, errors.New("help is not an executable test-vms operation")
		case "--slot":
			index++
			if index >= len(arguments) {
				return nil, errors.New("--slot requires a number")
			}
			var err error
			slot, err = strconv.Atoi(arguments[index])
			if err != nil || slot < 1 {
				return nil, errors.New("--slot requires a positive integer")
			}
		case "--expect-resource-generation", "--expect-lease-epoch":
			option := argument
			index++
			if index >= len(arguments) {
				return nil, fmt.Errorf("%s requires a non-negative integer", option)
			}
			value, err := strconv.ParseUint(arguments[index], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("%s requires a non-negative integer", option)
			}
			if option == "--expect-resource-generation" {
				if expectedGenerationSet {
					return nil, fmt.Errorf("%s may be specified only once", option)
				}
				expectedGeneration, expectedGenerationSet = value, true
			} else {
				if expectedEpochSet {
					return nil, fmt.Errorf("%s may be specified only once", option)
				}
				expectedEpoch, expectedEpochSet = value, true
			}
		default:
			if strings.HasPrefix(argument, "-") {
				return nil, fmt.Errorf("unknown test-vms option %q", argument)
			}
			if action != "" {
				return nil, errors.New("test-vms accepts one command")
			}
			action = argument
		}
	}
	switch action {
	case "status":
		if slot != 0 {
			return nil, errors.New("--slot is not valid for status")
		}
	case "revoke", "recover":
		if slot == 0 {
			return nil, fmt.Errorf("%s requires --slot N", action)
		}
	default:
		return nil, fmt.Errorf("unknown test-vms command %q", action)
	}
	if expectedGenerationSet != expectedEpochSet {
		return nil, errors.New("forwarded test VM target identity requires generation and epoch")
	}
	if expectedGenerationSet && action != "revoke" && action != "recover" {
		return nil, errors.New("forwarded test VM target identity requires revoke or recover")
	}
	// A remote invocation is preflighted by the owner after forwarding. A local
	// invocation must prove the yard can run the broker operation before any
	// confirmation is requested.
	if loaded.Context.AccessKind != domain.AccessRemote {
		incusPort, _ := cli.statusPorts()
		instance, err := incusPort.Instance(
			ctx, loaded.Context.IncusProject, loaded.Context.YardInstanceName,
		)
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(instance.Status, "running") {
			return nil, fmt.Errorf("yard %q must be running", loaded.Context.YardInstanceName)
		}
	}
	execution := &testVMExecution{action: action, slot: slot}
	if action == "revoke" || action == "recover" {
		slotSnapshot, err := cli.probeTestVMSlot(ctx, loaded, slot)
		if err != nil {
			return nil, err
		}
		slotState := slotSnapshot.State
		execution.identity = testvmsruntime.LeaseIdentity{
			SlotID:             slotSnapshot.SlotID,
			ResourceGeneration: slotSnapshot.ResourceGeneration,
			LeaseEpoch:         slotSnapshot.LeaseEpoch,
		}
		execution.hasSnapshot = true
		if expectedGenerationSet &&
			(slotSnapshot.ResourceGeneration != expectedGeneration ||
				slotSnapshot.LeaseEpoch != expectedEpoch) {
			return nil, fmt.Errorf(
				"%w: test VM lease target changed after remote confirmation",
				domain.ErrPlanStale,
			)
		}
		switch action {
		case "revoke":
			switch slotState {
			case testvmsruntime.SlotAvailable:
				execution.noOp = true
			case testvmsruntime.SlotHeld, testvmsruntime.SlotProvisioning, testvmsruntime.SlotDraining:
			default:
				return nil, fmt.Errorf("test VM slot %d cannot be revoked while %s", slot, slotState)
			}
		case "recover":
			switch slotState {
			case testvmsruntime.SlotAvailable, testvmsruntime.SlotRecovering:
				execution.noOp = true
			case testvmsruntime.SlotQuarantined:
			default:
				return nil, fmt.Errorf("test VM slot %d cannot be recovered while %s", slot, slotState)
			}
		}
		if !execution.noOp {
			if slotSnapshot.ResourceGeneration == 0 || slotSnapshot.LeaseEpoch == 0 {
				return nil, errors.New("test VM broker returned an incomplete lease target identity")
			}
		}
	}
	return execution, nil
}

func (execution *testVMExecution) remoteArguments(arguments []string) ([]string, error) {
	if execution == nil || execution.action == "status" {
		return append([]string(nil), arguments...), nil
	}
	if !execution.hasSnapshot {
		return nil, errors.New("test VM target snapshot is required for remote forwarding")
	}
	forwarded := make([]string, 0, len(arguments)+4)
	for index := 0; index < len(arguments); index++ {
		switch arguments[index] {
		case "--expect-resource-generation", "--expect-lease-epoch":
			index++
		default:
			forwarded = append(forwarded, arguments[index])
		}
	}
	forwarded = append(forwarded,
		"--expect-resource-generation", strconv.FormatUint(execution.identity.ResourceGeneration, 10),
		"--expect-lease-epoch", strconv.FormatUint(execution.identity.LeaseEpoch, 10),
	)
	return forwarded, nil
}

func (cli *CLI) probeTestVMSlot(
	ctx context.Context,
	loaded config.Loaded,
	slot int,
) (testvmsruntime.LeaseSlot, error) {
	probeContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	result, err := cli.projectDataPlane().Execute(probeContext, loaded.Context, ports.InstanceExecRequest{
		Command: []string{testvmsruntime.DefaultInstalledPath, "_test-vms-worker", "status"},
	})
	if err != nil {
		return testvmsruntime.LeaseSlot{}, fmt.Errorf("read test VM broker status: %w", err)
	}
	if result.ExitCode != 0 {
		return testvmsruntime.LeaseSlot{}, fmt.Errorf("read test VM broker status: exit status %d", result.ExitCode)
	}
	if len(result.Stdout) > testVMStatusMaxBytes {
		return testvmsruntime.LeaseSlot{}, errors.New("test VM broker status exceeds the safe probe limit")
	}
	var response testVMStatusResponse
	if err := json.Unmarshal(result.Stdout, &response); err != nil {
		return testvmsruntime.LeaseSlot{}, fmt.Errorf("decode test VM broker status: %w", err)
	}
	if response.SchemaVersion != testvmsruntime.LeaseSchemaVersion || response.Status != "ok" ||
		response.Pool == nil || response.Pool.SchemaVersion != testvmsruntime.LeaseSchemaVersion ||
		response.Pool.ResourceType != "agent-e2e" || response.Pool.ResourceID != "test-vms" {
		return testvmsruntime.LeaseSlot{}, errors.New("test VM broker returned an invalid status response")
	}
	if err := validateTestVMStatusSlots(response.Pool.Slots); err != nil {
		return testvmsruntime.LeaseSlot{}, errors.New("test VM broker returned an invalid status response")
	}
	wanted := fmt.Sprintf("slot-%03d", slot)
	for _, candidate := range response.Pool.Slots {
		if candidate.SlotID == wanted {
			return candidate, nil
		}
	}
	return testvmsruntime.LeaseSlot{}, fmt.Errorf("test VM slot %d is not configured", slot)
}

func validateTestVMStatusSlots(slots []testvmsruntime.LeaseSlot) error {
	seen := make(map[int]struct{}, len(slots))
	for _, slot := range slots {
		const prefix = "slot-"
		if !strings.HasPrefix(slot.SlotID, prefix) {
			return errors.New("invalid slot id")
		}
		number, err := strconv.Atoi(strings.TrimPrefix(slot.SlotID, prefix))
		if err != nil || number < 1 || number > len(slots) ||
			slot.SlotID != fmt.Sprintf("slot-%03d", number) {
			return errors.New("invalid slot id")
		}
		if _, duplicate := seen[number]; duplicate {
			return errors.New("duplicate slot id")
		}
		seen[number] = struct{}{}
		switch slot.State {
		case testvmsruntime.SlotAvailable, testvmsruntime.SlotProvisioning,
			testvmsruntime.SlotHeld, testvmsruntime.SlotDraining,
			testvmsruntime.SlotQuarantined, testvmsruntime.SlotRecovering,
			testvmsruntime.SlotUnavailable:
		default:
			return errors.New("unknown slot state")
		}
	}
	return nil
}

func (execution *testVMExecution) actionPlan() (domain.ActionID, domain.ActionDelta, error) {
	if execution == nil {
		return "", domain.ActionDelta{}, errors.New("test-vms execution is required")
	}
	consequences := []string{"read the configured test VM lease pool"}
	switch execution.action {
	case "status":
		return "test-vms.status", domain.ActionDelta{Consequences: consequences}, nil
	case "revoke":
		consequences = []string{
			fmt.Sprintf("fence and stop active lease slot %d", execution.slot),
			"retain both VM disks and the slot network/project",
		}
		return "test-vms.revoke", domain.ActionDelta{Changed: !execution.noOp, Consequences: consequences}, nil
	case "recover":
		consequences = []string{
			fmt.Sprintf("immediately recover quarantined lease slot %d", execution.slot),
			"save incident evidence, then delete both marker-owned disposable VM disks",
			"provision and verify a clean two-VM pair before publishing the slot as available",
		}
		return "test-vms.recover", domain.ActionDelta{Changed: !execution.noOp, Consequences: consequences}, nil
	default:
		return "", domain.ActionDelta{}, fmt.Errorf(
			"%w: unknown test-vms action %q", domain.ErrActionPolicyInvalid, execution.action,
		)
	}
}

func (cli *CLI) executeTestVMs(
	ctx context.Context,
	orchestrator *application.Orchestrator,
	loaded config.Loaded,
	plan domain.OperationPlan,
	execution *testVMExecution,
	diagnostics io.Writer,
) (domain.AdapterResult, error) {
	if execution == nil {
		return domain.AdapterResult{}, errors.New("test-vms execution is required")
	}
	if execution.noOp {
		return domain.AdapterResult{
			Schema: shelladapter.ProtocolSchema, OperationID: plan.OperationID, Status: "ok",
		}, nil
	}
	incusPort, _ := cli.statusPorts()
	instance, err := incusPort.Instance(ctx, loaded.Context.IncusProject, loaded.Context.YardInstanceName)
	if err != nil {
		return domain.AdapterResult{}, err
	}
	if !strings.EqualFold(instance.Status, "running") {
		return domain.AdapterResult{}, fmt.Errorf("yard %q must be running", loaded.Context.YardInstanceName)
	}
	argument := execution.action
	if execution.action == "revoke" {
		argument = fmt.Sprintf("revoke-slot-%d", execution.slot)
	} else if execution.action == "recover" {
		argument = fmt.Sprintf("recover-slot-%d", execution.slot)
	}
	arguments := []string{argument}
	if execution.action != "status" {
		arguments = append(arguments,
			"--expect-resource-generation", strconv.FormatUint(execution.identity.ResourceGeneration, 10),
			"--expect-lease-epoch", strconv.FormatUint(execution.identity.LeaseEpoch, 10),
			"--yes",
		)
	}
	request := domain.AdapterRequest{
		Schema: shelladapter.ProtocolSchema, OperationID: plan.OperationID,
		Adapter: "test-vms", Action: execution.action, Arguments: arguments,
		Context: structuredCommandContext(loaded),
	}
	result, stderr, err := orchestrator.RunAdapter(ctx, plan, request, nil)
	writeAdapterDiagnostics(diagnostics, stderr)
	var exitErr *exec.ExitError
	if err != nil && errors.As(err, &exitErr) &&
		exitErr.ExitCode() == testvmsruntime.LeaseTargetStaleExitCode {
		return result, fmt.Errorf(
			"%w: test VM lease target changed after confirmation",
			domain.ErrPlanStale,
		)
	}
	return result, err
}
