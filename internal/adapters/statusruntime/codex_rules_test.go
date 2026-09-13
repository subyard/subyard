package statusruntime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

func TestCodexRulesProbe(t *testing.T) {
	for _, tc := range []struct {
		name, protected, local, state string
		cliFailure, missingRules      bool
	}{
		{name: "prompt", protected: `{"decision":"prompt"}`, local: `{}`, state: "rules-ok"},
		{name: "allow override", protected: `{"decision":"allow"}`, local: `{}`, state: "incompatible"},
		{name: "local gated", protected: `{"decision":"prompt"}`, local: `{"decision":"prompt"}`, state: "incompatible"},
		{name: "changed schema", protected: `{}`, local: `{}`, state: "incompatible"},
		{name: "invalid JSON", protected: `private diagnostic`, state: "incompatible"},
		{name: "oversized response", protected: `{"decision":"prompt","extra":"` + strings.Repeat("x", 32768) + `"}`, state: "incompatible"},
		{name: "CLI failure", state: "incompatible", cliFailure: true},
		{name: "missing rules", state: "incompatible", missingRules: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			write := func(name, content string, mode os.FileMode) {
				t.Helper()
				path := filepath.Join(home, name)
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(content), mode); err != nil {
					t.Fatal(err)
				}
			}
			if !tc.missingRules {
				write(".codex/rules/a.rules", "", 0o600)
				write(".codex/rules/b.rules", "", 0o600)
			}
			write("protected", tc.protected, 0o600)
			write("local", tc.local, 0o600)
			script := `#!/bin/bash
set -eu
[[ "$PWD" = "$HOME" && "$1 $2" = 'execpolicy check' ]] || exit 1
shift 2
[[ "$1" = --rules && "$2" = "$CODEX_HOME/rules/a.rules" ]] || exit 1
[[ "$3" = --rules && "$4" = "$CODEX_HOME/rules/b.rules" && "$5" = -- ]] || exit 1
shift 5
printf '%s\n' "$*" >> "$HOME/calls"
case "$*" in
  'git commit'*|'git push'*) cat "$HOME/protected" ;;
  *) cat "$HOME/local" ;;
esac
`
			if tc.cliFailure {
				script = "#!/bin/sh\nprintf 'private diagnostic' >&2\nexit 2\n"
			}
			write("bin/codex", script, 0o700)
			runtime := Runtime{Executor: statusExecutorFunc(func(ctx context.Context, _, _ string, req ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
				command := exec.CommandContext(ctx, req.Command[0], req.Command[1:]...)
				command.Env = []string{"HOME=" + home, "CODEX_HOME=" + home + "/.codex", "PATH=" + home + "/bin:/usr/bin:/bin"}
				output, err := command.CombinedOutput()
				if len(output) != 0 {
					t.Fatalf("probe leaked output: %q", output)
				}
				var exitErr *exec.ExitError
				if errors.As(err, &exitErr) {
					return ports.InstanceExecResult{ExitCode: exitErr.ExitCode()}, err
				}
				return ports.InstanceExecResult{}, err
			})}
			if got := runtime.codexRulesStatus(context.Background(), domain.Context{}, true); got.State != tc.state {
				t.Fatalf("got %#v; want %s", got, tc.state)
			}
			if tc.state == "rules-ok" {
				calls, err := os.ReadFile(filepath.Join(home, "calls"))
				want := "git commit\ngit commit -m msg\ngit commit --amend\ngit push\ngit push origin main\ngit push --force-with-lease\ngit status\nsh dev-check.sh\n"
				if err != nil || string(calls) != want {
					t.Fatalf("matcher inputs = %q, err=%v", calls, err)
				}
			}
		})
	}
}

func TestCodexRulesStatusKeepsUnavailableChecksUnknown(t *testing.T) {
	yard := domain.Context{IncusProject: "project", YardInstanceName: "yard", DevUser: "developer", DevUID: 1234}
	for _, tc := range []struct {
		name, state string
		code        int
		err         error
	}{
		{"missing CLI", "missing", 127, errors.New("instance command exited with status 127")},
		{"timeout", "?", 124, errors.New("instance command exited with status 124")},
		{"killed", "?", 137, errors.New("instance command exited with status 137")},
		{"transport error", "?", 0, errors.New("private diagnostic")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			runtime := Runtime{Environment: map[string]string{"CODING_TOOL_INTEGRATIONS": "codex claude opencode pi"},
				Executor: statusExecutorFunc(func(ctx context.Context, project, instance string, req ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
					calls++
					_, bounded := ctx.Deadline()
					if project != "project" || instance != "yard" || !bounded || req.User != 1234 || req.Group != 1234 ||
						req.Environment["HOME"] != "/home/developer" || req.Environment["CODEX_HOME"] != "/home/developer/.codex" ||
						!slices.Equal(req.Command[:5], []string{"/usr/bin/timeout", "--kill-after=1", "8", "/bin/bash", "-c"}) {
						t.Fatal("probe target, identity or time bound changed")
					}
					return ports.InstanceExecResult{ExitCode: tc.code, Stderr: []byte("private diagnostic")}, tc.err
				})}
			statuses := runtime.agentStatus(context.Background(), yard, true)
			if statuses[0].State != tc.state || strings.Contains(statuses[0].Hint, "private diagnostic") || calls != 1 {
				t.Fatalf("statuses = %#v, calls = %d", statuses, calls)
			}
			for _, status := range statuses[1:] {
				if status.State != "unverified" {
					t.Fatalf("unsupported approval check reported %#v", status)
				}
			}
			if got := runtime.codexRulesStatus(context.Background(), yard, false); got.State != "?" || calls != 1 {
				t.Fatalf("stopped yard probed: %#v, calls=%d", got, calls)
			}
		})
	}
}
