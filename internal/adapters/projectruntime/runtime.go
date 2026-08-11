package projectruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/transport"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/shellquote"
)

var metadataCommand = []string{
	"find", "/srv/workspaces", "-mindepth", "2", "-maxdepth", "2",
	"-type", "f", "-name", ".subyard-meta.json", "-exec", "cat", "{}", "+",
}

type Runtime struct {
	Incus       ports.Incus
	Executor    ports.InstanceExecutor
	Streamer    ports.InstanceStreamExecutor
	SSHBinary   string
	Environment []string
	Timeout     time.Duration
	MaxBytes    int64
}

func (runtime Runtime) Execute(
	ctx context.Context,
	yard domain.Context,
	request ports.InstanceExecRequest,
) (ports.InstanceExecResult, error) {
	if yard.AccessKind != domain.AccessRemote {
		if runtime.Executor == nil {
			return ports.InstanceExecResult{}, errors.New("Incus executor is required")
		}
		return runtime.Executor.Exec(ctx, yard.IncusProject, yard.YardInstanceName, request)
	}
	return runtime.executeSSH(ctx, yard.SSHHost, request)
}

func (runtime Runtime) Stream(
	ctx context.Context,
	yard domain.Context,
	request ports.InstanceExecRequest,
	stdin io.Reader,
) (ports.InstanceExecResult, error) {
	if yard.AccessKind != domain.AccessRemote {
		if runtime.Streamer == nil {
			return ports.InstanceExecResult{}, errors.New("Incus stream executor is required")
		}
		return runtime.Streamer.StreamExec(ctx, yard.IncusProject, yard.YardInstanceName, request, stdin)
	}
	return runtime.executeSSHReader(ctx, yard.SSHHost, request, stdin)
}

func (runtime Runtime) executeSSH(
	ctx context.Context,
	host string,
	request ports.InstanceExecRequest,
) (ports.InstanceExecResult, error) {
	return runtime.executeSSHReader(ctx, host, request, bytes.NewReader(request.Stdin))
}

func (runtime Runtime) executeSSHReader(
	ctx context.Context,
	host string,
	request ports.InstanceExecRequest,
	stdin io.Reader,
) (ports.InstanceExecResult, error) {
	if !domain.SafeSSHTarget(host) {
		return ports.InstanceExecResult{}, errors.New("invalid yard SSH target")
	}
	command := make([]string, 0, len(request.Command)+len(request.Environment)+1)
	if len(request.Environment) != 0 {
		command = append(command, "env")
		keys := make([]string, 0, len(request.Environment))
		for key := range request.Environment {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if !config.ValidVariable(key) {
				return ports.InstanceExecResult{}, fmt.Errorf("invalid environment key %q", key)
			}
			command = append(command, key+"="+request.Environment[key])
		}
	}
	command = append(command, request.Command...)
	binary := runtime.SSHBinary
	if binary == "" {
		binary = "ssh"
	}
	process := transport.Process{
		Program: binary, Env: runtime.Environment, Timeout: runtime.Timeout, MaxBytes: runtime.MaxBytes,
		Arguments: []string{
			"-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "-o", "StrictHostKeyChecking=yes",
			host, "--", shellquote.Command(command),
		},
	}
	stdout, err := process.CallReader(ctx, stdin)
	result := ports.InstanceExecResult{Stdout: stdout}
	if err != nil {
		result.ExitCode = 1
		var processError *transport.ProcessError
		if errors.As(err, &processError) {
			result.ExitCode = processError.ExitCode
		}
	}
	return result, err
}

func (runtime Runtime) Observe(
	ctx context.Context,
	yard domain.Context,
	records []domain.ProjectRecord,
	live bool,
) (domain.ProjectObservation, error) {
	result := domain.ProjectObservation{
		Presence: make(map[string]domain.ProjectPresence, len(records)),
		Boxes:    make(map[string]domain.ProjectBoxState, len(records)),
	}
	for _, record := range records {
		result.Presence[record.ProjectID] = domain.ProjectPresenceUnknown
		if record.Target == "" || record.Target == "yard" {
			result.Boxes[record.ProjectID] = "-"
		} else {
			result.Boxes[record.ProjectID] = domain.ProjectBoxUnknown
		}
	}

	if yard.AccessKind == domain.AccessRemote {
		if live {
			execution, err := runtime.Execute(ctx, yard, ports.InstanceExecRequest{Command: metadataCommand})
			if err == nil {
				result.Reached = true
				result.Live, result.Warnings = parseMetadata(execution.Stdout)
				markLive(&result)
			}
		}
		return result, nil
	}

	if runtime.Incus != nil {
		instance, err := runtime.Incus.Instance(ctx, yard.IncusProject, yard.YardInstanceName)
		if err == nil && strings.EqualFold(instance.Status, "running") {
			result.Running = true
		}
	}
	if result.Running && runtime.Executor != nil {
		observeProjects(ctx, runtime.Executor, yard, records, &result)
	}
	if live {
		execution, err := runtime.executeSSH(ctx, yard.SSHHost,
			ports.InstanceExecRequest{Command: metadataCommand})
		payload := execution.Stdout
		if err != nil && result.Running && runtime.Executor != nil {
			payload, err = execBytes(ctx, runtime.Executor, yard, metadataCommand, nil)
		}
		if err == nil {
			result.Reached = true
			result.Live, result.Warnings = parseMetadata(payload)
			markLive(&result)
		}
	}
	return result, nil
}

func (runtime Runtime) ConvergeMetadata(
	ctx context.Context,
	yard domain.Context,
	records []domain.ProjectRecord,
) error {
	var failures []error
	for _, record := range records {
		payload, err := json.Marshal(struct {
			Schema          int                `json:"schema"`
			IdentityVersion int                `json:"identityVersion,omitempty"`
			Yard            string             `json:"yard"`
			ProjectID       string             `json:"projectId"`
			Name            string             `json:"name"`
			Mode            domain.ProjectMode `json:"mode"`
			Target          string             `json:"target,omitempty"`
			ImportedAt      string             `json:"importedAt,omitempty"`
		}{
			1, record.IdentityVersion, yard.YardName, record.ProjectID,
			record.Name, record.Mode, record.Target, record.ImportedAt,
		})
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", record.ProjectID, err))
			continue
		}
		request := ports.InstanceExecRequest{
			Command: []string{
				"tee", filepath.Join(filepath.Dir(record.YardPath), ".subyard-meta.json"),
			},
			Stdin: append(payload, '\n'),
			User:  uint32(yard.DevUID), Group: uint32(yard.DevUID),
		}
		if _, err := runtime.Execute(ctx, yard, request); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", record.ProjectID, err))
		}
	}
	return errors.Join(failures...)
}

func observeProjects(
	ctx context.Context,
	executor ports.InstanceExecutor,
	yard domain.Context,
	records []domain.ProjectRecord,
	result *domain.ProjectObservation,
) {
	payload, err := execBytes(ctx, executor, yard, []string{
		"find", "/srv/workspaces", "-mindepth", "2", "-maxdepth", "2",
		"-type", "d", "-name", "src", "-printf", "%h\n",
	}, nil)
	if err == nil {
		for id := range result.Presence {
			result.Presence[id] = domain.ProjectMissing
		}
		for _, line := range strings.Split(strings.TrimSpace(string(payload)), "\n") {
			if id := filepath.Base(strings.TrimSpace(line)); id != "." && id != "" {
				result.Presence[id] = domain.ProjectPresent
			}
		}
	}
	payload, err = execBytes(ctx, executor, yard, []string{"docker", "ps", "-a", "--filter", "label=subyard.env=1", "--format", `{{index .Labels "subyard.project"}}\t{{.State}}`}, nil)
	if err == nil {
		seen := make(map[string]bool)
		for _, line := range strings.Split(strings.TrimSpace(string(payload)), "\n") {
			id, value, ok := strings.Cut(line, "\t")
			if !ok || id == "" {
				continue
			}
			seen[id] = true
			if value == "running" {
				result.Boxes[id] = domain.ProjectBoxUp
			} else {
				result.Boxes[id] = domain.ProjectBoxDown
			}
		}
		for _, record := range records {
			if record.Target != "" && record.Target != "yard" && !seen[record.ProjectID] {
				result.Boxes[record.ProjectID] = domain.ProjectBoxNone
			}
		}
	}
}

func execBytes(
	ctx context.Context,
	executor ports.InstanceExecutor,
	yard domain.Context,
	command []string,
	stdin []byte,
) ([]byte, error) {
	result, err := executor.Exec(ctx, yard.IncusProject, yard.YardInstanceName, ports.InstanceExecRequest{
		Command: command, Stdin: stdin,
	})
	return result.Stdout, err
}

func parseMetadata(payload []byte) ([]domain.ProjectRecord, []string) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	var records []domain.ProjectRecord
	var warnings []string
	for {
		var wire struct {
			Schema          int                `json:"schema"`
			IdentityVersion int                `json:"identityVersion"`
			ProjectID       string             `json:"projectId"`
			Name            string             `json:"name"`
			Mode            domain.ProjectMode `json:"mode"`
			Target          string             `json:"target"`
			ImportedAt      string             `json:"importedAt"`
		}
		if err := decoder.Decode(&wire); err != nil {
			if !errors.Is(err, io.EOF) {
				warnings = append(warnings, "ignored invalid yard project metadata")
			}
			break
		}
		record := domain.ProjectRecord{
			Schema: 1, IdentityVersion: wire.IdentityVersion,
			ProjectID: wire.ProjectID, Name: wire.Name, Mode: wire.Mode,
			YardPath: "/srv/workspaces/" + wire.ProjectID + "/src", SSHHost: "yard", Target: wire.Target,
			ImportedAt: wire.ImportedAt,
		}
		if wire.Schema != 1 || record.Validate(record.ProjectID) != nil {
			warnings = append(warnings, "ignored invalid yard project metadata")
			continue
		}
		records = append(records, record)
	}
	return records, warnings
}

func markLive(result *domain.ProjectObservation) {
	for id := range result.Presence {
		result.Presence[id] = domain.ProjectMissing
	}
	for _, record := range result.Live {
		result.Presence[record.ProjectID] = domain.ProjectPresent
	}
}
