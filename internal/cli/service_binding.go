package cli

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/domain"
)

type serviceOperationBinding struct {
	Schema  int      `json:"schema_version"`
	Binding string   `json:"binding"`
	Facts   []string `json:"facts"`
}

// Observation and mutation execute the same opened, bounded handler bytes.
func (cli *CLI) runServiceBinding(ctx context.Context, selection *serviceSelection, expectedSource string, output io.Writer, arguments ...string) (string, error) {
	handler := selection.definition.InfrastructureService.BindingHandler
	if handler == "" {
		return "", nil
	}
	file, err := os.OpenFile(filepath.Join(selection.definition.Root, handler), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", err
	}
	defer file.Close()
	source, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return "", err
	}
	if len(source) > 1<<20 {
		return "", errors.New("service binding source exceeds limit")
	}
	if _, err := file.Seek(0, 0); err != nil {
		return "", err
	}
	digest := operationStateDigest(source)
	if expectedSource != "" && digest != expectedSource {
		return "", domain.ErrPlanStale
	}
	deadline, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	command := exec.CommandContext(deadline, "/proc/self/fd/3", append([]string{"--project", selection.record.Project, "--instance", selection.record.Instance}, arguments...)...)
	command.ExtraFiles, command.Env = []*os.File{file}, environmentList(cli.baseEnv, nil)
	command.Stdout, command.Stderr = output, io.Discard
	if err := command.Run(); err != nil {
		if len(arguments) == 0 {
			return "", errors.New("service operation binding unavailable")
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 75 {
			return "", domain.ErrPlanStale
		}
		return "", fmt.Errorf("service operation fencing unavailable; run yard service update %s before retrying", selection.record.Service)
	}
	return digest, nil
}

func (cli *CLI) observeServiceBinding(ctx context.Context, selection *serviceSelection) (serviceOperationBinding, string, error) {
	var binding serviceOperationBinding
	output := &boundedResourceBuffer{limit: 16384}
	source, err := cli.runServiceBinding(ctx, selection, "", output)
	if err != nil || source == "" {
		return binding, "", err
	}
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&binding) != nil || decoder.Decode(new(any)) != io.EOF || binding.Schema != 1 || len(binding.Binding) != 64 || strings.ToLower(binding.Binding) != binding.Binding || len(binding.Facts) > 128 {
		return binding, "", errors.New("invalid service operation binding")
	}
	if _, err := hex.DecodeString(binding.Binding); err != nil {
		return binding, "", errors.New("invalid service operation binding")
	}
	for _, fact := range binding.Facts {
		if len(fact) > 512 || strings.ContainsAny(fact, "\x00\x1b\r\n") {
			return binding, "", errors.New("unsafe service operation fact")
		}
	}
	return binding, source, nil
}

func (cli *CLI) applyServiceBinding(ctx context.Context, selection *serviceSelection, source, binding string, resume bool) error {
	arguments := []string{"--apply-binding", binding}
	if resume {
		arguments = []string{"--resume"}
	}
	_, err := cli.runServiceBinding(ctx, selection, source, io.Discard, arguments...)
	return err
}

func (prepared *preparedCommand) bindServiceOperation(ctx context.Context, selection *serviceSelection) error {
	incus, _ := prepared.CLI.statusPorts()
	instance, err := incus.Instance(ctx, selection.record.Project, selection.record.Instance)
	if err != nil {
		return err
	}
	if !strings.EqualFold(instance.Status, "running") {
		return nil
	}
	binding, source, err := prepared.CLI.observeServiceBinding(ctx, selection)
	if err != nil {
		return err
	}
	if source == "" {
		return nil
	}
	prepared.exactState = operationStateDigest(struct {
		Inputs, Source string
		Binding        serviceOperationBinding
	}{prepared.exactState, source, binding})
	steps, execute, refresh := prepared.steps, prepared.execute, prepared.refresh
	guard := func(ctx context.Context) error {
		current, currentSource, err := prepared.CLI.observeServiceBinding(ctx, selection)
		if err != nil {
			return err
		}
		if currentSource != source || operationStateDigest(current) != operationStateDigest(binding) {
			return errors.Join(domain.ErrPlanStale, errors.New("service operation scope changed"))
		}
		return nil
	}
	prepared.steps = func() []domain.OperationStep {
		result := steps()
		for index, fact := range binding.Facts {
			result = append(result, domain.OperationStep{ID: fmt.Sprintf("service-allocation-%d", index), Target: selection.record.Service + "/" + selection.record.Pool, Observed: fact, Desired: "only the captured service allocation may be drained", Decision: domain.StepConditional, Preconditions: []string{"profile allocation binding is unchanged"}, Verify: "compare the profile allocation digest before native effects", Consequence: fact})
		}
		return result
	}
	if refresh != nil {
		prepared.refresh = func(ctx context.Context) (domain.ActionID, domain.ActionDelta, error) {
			if err := guard(ctx); err != nil {
				return "", domain.ActionDelta{}, err
			}
			return refresh(ctx)
		}
	}
	prepared.execute = func(ctx context.Context, orchestrator *application.Orchestrator, diagnostics io.Writer) (domain.AdapterResult, error) {
		if err := guard(ctx); err != nil {
			return domain.AdapterResult{}, err
		}
		if err := prepared.CLI.applyServiceBinding(ctx, selection, source, binding.Binding, false); err != nil {
			return domain.AdapterResult{}, err
		}
		return execute(ctx, orchestrator, diagnostics)
	}
	return nil
}
