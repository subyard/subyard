package statusruntime

import (
	"context"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

func (runtime Runtime) codexRulesStatus(ctx context.Context, yard domain.Context, running bool) domain.AgentStatus {
	status := domain.AgentStatus{Name: "codex", State: "?", Hint: "home rule check unavailable"}
	if !running {
		status.Hint = "yard stopped; home rules not checked"
		return status
	}
	if runtime.Executor == nil {
		return status
	}
	user := yard.DevUser
	if user == "" {
		user = runtime.Environment["DEV_USER"]
	}
	if user == "" {
		user = "dev"
	}
	if !domain.SafeName(user) {
		return status
	}
	uid := yard.DevUID
	if uid <= 0 {
		uid = 1000
	}
	devHome := "/home/" + user
	timeout := runtime.ProbeTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result, err := runtime.Executor.Exec(probeCtx, yard.IncusProject, yard.YardInstanceName, ports.InstanceExecRequest{
		Command: []string{"/usr/bin/timeout", "--kill-after=1", "8", "/bin/bash", "-c", codexRulesCommand},
		User:    uint32(uid), Group: uint32(uid),
		Environment: map[string]string{
			"HOME": devHome, "CODEX_HOME": devHome + "/.codex", "BASH_ENV": "/dev/null",
			"PATH": devHome + "/.local/bin:" + devHome + "/.npm-global/bin:/usr/local/bin:/usr/bin:/bin",
		},
	})
	switch {
	case probeCtx.Err() != nil || result.ExitCode == 124 || result.ExitCode == 137:
		status.Hint = "home rule check timed out; retry status"
	case result.ExitCode == 127:
		status.State, status.Hint = "missing", "Codex CLI not found on the default developer PATH"
	case result.ExitCode != 0:
		status.State, status.Hint = "incompatible", "home rule check failed; review rules or update Subyard"
	case err != nil:
		status.Hint = "home rule check unavailable; check installation or update Subyard"
	default:
		status.State, status.Hint = "rules-ok", "home matcher: commit/push prompt; session approvals unverified"
	}
	return status
}

// Codex owns rule parsing. The guest returns only an exit code; commands after
// -- are matcher input, never executed. Coreutils bounds time and captured output.
const codexRulesCommand = `
set -euo pipefail
exec >/dev/null 2>&1
cd "$HOME"
command -v codex >/dev/null || exit 127
shopt -s nullglob
files=("$CODEX_HOME"/rules/*.rules)
[ "${#files[@]}" -gt 0 ] || exit 1
rules=()
for file in "${files[@]}"; do rules+=(--rules "$file"); done
check() {
  local expected="$1"
  shift
  codex execpolicy check "${rules[@]}" -- "$@" 2>/dev/null |
    head -c 32768 |
    jq -es --arg expected "$expected" '
      length == 1 and (.[0] | type == "object" and
        if $expected == "prompt" then .decision == "prompt"
        else (has("decision") | not) or .decision == "allow" end)
    ' >/dev/null || exit 1
}
check prompt git commit
check prompt git commit -m msg
check prompt git commit --amend
check prompt git push
check prompt git push origin main
check prompt git push --force-with-lease
check local git status
check local sh dev-check.sh
`
