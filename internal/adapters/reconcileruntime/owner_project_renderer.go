package reconcileruntime

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"time"
)

const ownerProjectRenderer = "scripts/lib/project-config.sh"

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

func ownerRenderEnvironment(environment []string, key, value string) []string {
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, key+"=") {
			result = append(result, entry)
		}
	}
	return append(result, key+"="+value)
}
