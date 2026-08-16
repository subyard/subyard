package shelladapter

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
)

func TestRunnerPreservesQuotingAndSeparatesSecret(t *testing.T) {
	runner := fixtureRunner(t, `#!/bin/sh
set -eu
metadata="$(cat <&3)"
secret="$(cat)"
printf '%s' "$metadata" | grep -F '"value":"a b;$(nope)"' >/dev/null
[ "$yard" = default ]
[ "$SUBYARD_OPERATION_ID" = operation-1 ]
printf '%s' "$secret" >&2
printf '%s' '{"schema":1,"operationId":"operation-1","status":"ok","output":{"seen":"a b;$(nope)"}}'
`)
	request := fixtureRequest()
	request.Input = map[string]any{"value": "a b;$(nope)"}
	result, stderr, err := runner.Run(context.Background(), request, strings.NewReader("owner-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Output["seen"] != "a b;$(nope)" {
		t.Fatalf("quoting changed: %#v", result.Output)
	}
	if stderr != "[REDACTED]" {
		t.Fatalf("secret leaked through stderr: %q", stderr)
	}
}

func TestRunnerPassesValidatedArgumentsWithoutShellEvaluation(t *testing.T) {
	runner := fixtureRunner(t, `#!/bin/sh
set -eu
[ "$1" = run ]
[ "$2" = 'a b;$(nope)' ]
printf '%s' '{"schema":1,"operationId":"operation-1","status":"ok"}'
`)
	request := fixtureRequest()
	request.Arguments = []string{"a b;$(nope)"}
	if _, _, err := runner.Run(context.Background(), request, nil); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerRejectsReservedContextEnvironment(t *testing.T) {
	runner := fixtureRunner(t, "#!/bin/sh\nexit 0\n")
	runner.ContextKeys["PATH"] = struct{}{}
	request := fixtureRequest()
	request.Context["PATH"] = "/tmp/untrusted"
	if _, _, err := runner.Run(context.Background(), request, nil); err == nil {
		t.Fatal("reserved adapter environment was accepted")
	}
}

func TestRunnerKillsProcessGroupOnTimeoutAndRejectsPartialOutput(t *testing.T) {
	runner := fixtureRunner(t, `#!/bin/sh
printf '{"schema":1'
sleep 30 &
wait
`)
	runner.Timeout = 50 * time.Millisecond
	started := time.Now()
	_, _, err := runner.Run(context.Background(), fixtureRequest(), nil)
	if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("expected deadline, got %v", err)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("adapter process group was not killed promptly")
	}
}

func TestRunnerRejectsSecretMetadataAndOversizedOutput(t *testing.T) {
	runner := fixtureRunner(t, `#!/bin/sh
head -c 100 /dev/zero
`)
	request := fixtureRequest()
	request.Input = map[string]any{"api_token": "do-not-pass"}
	if _, _, err := runner.Run(context.Background(), request, nil); err == nil {
		t.Fatal("secret metadata was accepted")
	}
	request.Input = nil
	runner.MaxOutput = 10
	if _, _, err := runner.Run(context.Background(), request, nil); err == nil {
		t.Fatal("oversized output was accepted")
	}
}

func TestRunnerRejectsSecretLikeContextKey(t *testing.T) {
	runner := fixtureRunner(t, "#!/bin/sh\nexit 0\n")
	runner.ContextKeys["api_token"] = struct{}{}
	request := fixtureRequest()
	request.Context["api_token"] = "do-not-pass"
	if _, _, err := runner.Run(context.Background(), request, nil); err == nil {
		t.Fatal("secret-like context key was accepted")
	}
}

func TestRunnerRedactsNestedOutput(t *testing.T) {
	runner := fixtureRunner(t, `#!/bin/sh
printf '%s' '{"schema":1,"operationId":"operation-1","status":"ok","output":{"items":[{"api_token":"owner-secret"},{"url":"https://user:pass@example.test"}]}}'
`)
	result, _, err := runner.Run(context.Background(), fixtureRequest(), strings.NewReader("owner-secret"))
	if err != nil {
		t.Fatal(err)
	}
	items, ok := result.Output["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("unexpected nested output: %#v", result.Output)
	}
	first, firstOK := items[0].(map[string]any)
	second, secondOK := items[1].(map[string]any)
	if !firstOK || !secondOK || first["api_token"] != "[REDACTED]" ||
		second["url"] != "https://user:***@example.test" {
		t.Fatalf("nested output was not redacted: %#v", items)
	}
}

func TestRunnerHonorsParentCancellation(t *testing.T) {
	runner := fixtureRunner(t, "#!/bin/sh\nsleep 30\n")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errors.New("operator cancelled"))
	_, _, err := runner.Run(ctx, fixtureRequest(), bytes.NewReader(nil))
	if err == nil {
		t.Fatal("cancelled request ran")
	}
}

func TestRunnerSynthesizesDirectLeafResult(t *testing.T) {
	runner := fixtureRunner(t, `#!/bin/sh
printf 'stdout:%s\n' "$*"
printf 'stderr\n' >&2
`)
	action := runner.Actions["fixture"]["run"]
	action.Direct = true
	runner.Actions["fixture"]["run"] = action
	request := fixtureRequest()
	request.Arguments = []string{"up", "--yes"}
	result, diagnostics, err := runner.Run(context.Background(), request, nil)
	if err != nil || result.Status != "ok" || result.Output["command"] != "run" {
		t.Fatalf("direct leaf failed: result=%#v diagnostics=%q err=%v", result, diagnostics, err)
	}
	if diagnostics != "stdout:up --yes\nstderr\n" {
		t.Fatalf("direct leaf diagnostics changed: %q", diagnostics)
	}

	var live bytes.Buffer
	runner.Diagnostics = &live
	if _, diagnostics, err = runner.Run(context.Background(), request, nil); err != nil || diagnostics != "" {
		t.Fatalf("direct leaf did not stream: diagnostics=%q err=%v", diagnostics, err)
	}
	if !strings.Contains(live.String(), "stdout:up --yes\n") || !strings.Contains(live.String(), "stderr\n") {
		t.Fatalf("direct leaf live output is incomplete: %q", live.String())
	}

	live.Reset()
	if _, diagnostics, err = runner.Run(context.Background(), request, strings.NewReader("protected")); err != nil || diagnostics == "" {
		t.Fatalf("protected run failed: diagnostics=%q err=%v", diagnostics, err)
	}
	if live.Len() != 0 {
		t.Fatalf("protected leaf streamed output: %q", live.String())
	}
}

func TestRunnerCapturesDirectProbeOutputWithLiveDiagnosticsConfigured(t *testing.T) {
	runner := fixtureRunner(t, "#!/bin/sh\nprintf 'changed\\n'\n")
	action := runner.Actions["fixture"]["run"]
	action.Direct = true
	action.Capture = true
	runner.Actions["fixture"]["run"] = action
	var live bytes.Buffer
	runner.Diagnostics = &live

	_, diagnostics, err := runner.Run(context.Background(), fixtureRequest(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if diagnostics != "changed\n" {
		t.Fatalf("captured diagnostics changed: %q", diagnostics)
	}
	if live.Len() != 0 {
		t.Fatalf("captured probe leaked into live diagnostics: %q", live.String())
	}
}

func TestRunnerActionTimeoutOverridesTheDefault(t *testing.T) {
	runner := fixtureRunner(t, "#!/bin/sh\nsleep 0.1\n")
	runner.Timeout = 20 * time.Millisecond
	action := runner.Actions["fixture"]["run"]
	action.Direct = true
	action.Timeout = time.Second
	runner.Actions["fixture"]["run"] = action

	if _, _, err := runner.Run(context.Background(), fixtureRequest(), nil); err != nil {
		t.Fatalf("action-specific timeout was not honored: %v", err)
	}
}

func fixtureRunner(t *testing.T, script string) Runner {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "adapter.sh")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return Runner{
		RepositoryRoot: root,
		Actions:        map[string]map[string]Action{"fixture": {"run": {Path: path}}},
		ContextKeys:    map[string]struct{}{"yard": {}},
		Timeout:        time.Second,
	}
}

func fixtureRequest() domain.AdapterRequest {
	return domain.AdapterRequest{
		Schema:      1,
		OperationID: "operation-1",
		Adapter:     "fixture",
		Action:      "run",
		Context:     map[string]string{"yard": "default"},
	}
}
