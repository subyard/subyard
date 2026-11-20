package reconcileruntime

import (
	"context"
	"fmt"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/ports"
)

// promptSudoGrantScript publishes the developer's passwordless grant, as base
// provisioning does for DEV_SUDO=yes.
const promptSudoGrantScript = `set -eu
user="$1"
sudoers="/etc/sudoers.d/90-subyard-$user"
printf '%s ALL=(ALL) NOPASSWD:ALL\n' "$user" > "$sudoers.tmp"
chmod 0440 "$sudoers.tmp"
if ! visudo -cf "$sudoers.tmp" >/dev/null; then
  rm -f "$sudoers.tmp"
  echo "sudoers validation failed; left $user without sudo" >&2
  exit 1
fi
mv -f "$sudoers.tmp" "$sudoers"
`

// withPromptSudoGrant runs the coding-agent policy installation and, under
// DEV_SUDO=prompt, publishes the grant only after it succeeds. Base provisioning
// keeps an existing grant but never creates one for prompt, so a failure or
// interruption before this point leaves the yard without sudo rather than with
// sudo and no approval prompts.
func (runtime Runtime) withPromptSudoGrant(ctx context.Context, install func() error) error {
	if err := install(); err != nil {
		return err
	}
	if !config.SudoApprovalPrompt(runtime.environmentValue("DEV_SUDO")) {
		return nil
	}
	result, err := runtime.Executor.Exec(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, ports.InstanceExecRequest{
		Command: []string{"sh", "-c", promptSudoGrantScript, "sh", runtime.devUser()},
	})
	if err != nil {
		return fmt.Errorf("publish the developer sudo grant: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("publish the developer sudo grant: exit %d", result.ExitCode)
	}
	return nil
}
