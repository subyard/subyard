package reconcileruntime

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
)

const ownerProjectRenderer = "scripts/lib/project-config.sh"

// RenderOwnerProjectConfig runs only the retained pure renderer bytes. It does
// not materialize an execution tree, query Incus, select a yard or mutate it.
func (runtime Runtime) RenderOwnerProjectConfig(ctx context.Context, original *PreparedInstalledRuntime) ([]byte, error) {
	if original == nil || original.closed.Load() || original.owner != "owner-native-runtime" || original.runtime != runtime.configRuntimeBinding() || original.resources != runtime.installedResourceBinding() || original.ownerInputs == nil {
		return nil, domain.ErrPlanStale
	}
	helper, err := original.ownerRendererBytes(ownerProjectRenderer)
	if err != nil {
		return nil, err
	}
	return runtime.renderProjectConfig(ctx, helper)
}

func (runtime Runtime) renderProjectConfig(ctx context.Context, helper []byte) ([]byte, error) {
	probe, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// Privileged shell mode disables inherited BASH_ENV/functions; this probe
	// executes only the fixed captured renderer, without changing process UID.
	command := exec.CommandContext(probe, "bash", "--noprofile", "--norc", "-peu", "-s")
	command.Env = append([]string(nil), runtime.Environment...)
	for index := range command.Env {
		command.Env[index] = runtime.bindingPath(command.Env[index])
	}
	command.Stdin = io.MultiReader(bytes.NewReader(helper), bytes.NewBufferString("\nsubyard_project_config_render\n"))
	output := &profileRuntimeBoundedBuffer{limit: 32 << 20}
	command.Stdout, command.Stderr = output, io.Discard
	if err := command.Run(); err != nil || output.exceeded {
		return nil, errors.New("original owner project rendering unavailable")
	}
	return bytes.Clone(output.Bytes()), nil
}

// ownerRendererBytes resolves only a fixed renderer leaf in the original
// approved source table and checks its content address before execution.
func (original *PreparedInstalledRuntime) ownerRendererBytes(layout string) ([]byte, error) {
	metadata, _, err := decodeMaterializedMetadata(original)
	if err != nil || metadata.Integrations == nil {
		return nil, domain.ErrPlanStale
	}
	for _, source := range metadata.Integrations.Sources {
		if source.Layout == layout {
			helper := original.files[source.Payload]
			if len(helper) == 0 || len(helper) > 32<<20 || source.Payload != materializedBlobPath(helper) {
				return nil, domain.ErrPlanStale
			}
			return helper, nil
		}
	}
	return nil, errors.New("original owner renderer unavailable")
}

func ownerRenderEnvironment(environment []string, key, value string) []string {
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, key+"=") {
			result = append(result, entry)
		}
	}
	return append(result, key+"="+value)
}
