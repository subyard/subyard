package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/rpc"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestMigrationGateDirectsInstalledTargetToMigrate(t *testing.T) {
	for _, test := range []struct{ active, target, retry, want string }{
		{"release-b", "release-b", "run yard update", "run yard migrate"},
		{"release-b", "release-b", "run yard update --check", "run yard migrate --check"},
		{"release-a", "release-b", "run yard update", "run yard update"},
		{"release-b", "release-b", "restore the verified release", "restore the verified release"},
	} {
		outcome := publicMutationGateOutcome(releasetransition.Outcome{
			Active: releasetransition.ReleaseID(test.active), Target: releasetransition.ReleaseID(test.target),
			Retry: test.retry, Message: "runtime readiness requires reconciliation",
		})
		if outcome.Action != test.want || outcome.Message == "" {
			t.Fatalf("gate=%#v want action=%q", outcome, test.want)
		}
	}
}

func TestMigrateRPCBypassesOrdinaryRecoveryGate(t *testing.T) {
	root, environment, marker := migrateCLIFixture(t, true)
	installUnfinishedMutationGateFixture(t, root, environment, filepath.Join(root, "runtime"))
	current := filepath.Join(root, "runtime", "current")
	if err := os.Remove(current); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/1.2.3-aaaaaaaaaaaa", current); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "runtime", "previous")); err != nil {
		t.Fatal(err)
	}
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.currentMigrationContext()
	if err != nil {
		t.Fatal(err)
	}
	handler := &rpcHandler{cli: program, loaded: loaded}
	result, err := handler.Handle(context.Background(), rpc.Call{
		Method: "operation.plan", OperationID: "migrate-recovery",
		Params: json.RawMessage(`{"command":"migrate","arguments":[]}`),
	}, nil)
	if err != nil {
		t.Fatalf("plan current migration: %v", err)
	}
	plan := result.(domain.OperationPlan)
	if plan.Assessment == nil || plan.Assessment.Action != "migrate.apply" {
		t.Fatalf("plan=%#v", plan)
	}
	_, err = handler.Handle(context.Background(), rpc.Call{
		Method: "operation.execute", OperationID: plan.OperationID,
		Params: json.RawMessage(`{"confirmed":true}`),
	}, func(string, any) (uint64, error) { return 1, nil })
	if err != nil {
		t.Fatalf("execute current migration: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("migration not applied: %v", err)
	}
}

func TestMigrateRejectsExplicitYardSelection(t *testing.T) {
	root, environment, marker := migrateCLIFixture(t, true)
	var stderr bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Program: "yard",
		Arguments: []string{"-Y", "other", "migrate", "--yes"}, Environment: environment, Stderr: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 2 || !strings.Contains(stderr.String(), "without a yard selector") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("selected-yard migration mutated host: %v", err)
	}
}

func TestMigrateHelpDoesNotRequireYardConfiguration(t *testing.T) {
	root, environment, _ := migrateCLIFixture(t, false)
	writeCLIFile(t, filepath.Join(root, "config", "subyard.env"), "invalid configuration\n", 0600)
	var stdout, stderr bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Program: "yard",
		Arguments: []string{"migrate", "--help"}, Environment: environment,
		Stdout: &stdout, Stderr: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 || !strings.Contains(stdout.String(), "--check") {
		t.Fatalf("migrate help: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestMigrateCheckReadsInstalledRuntimeWithoutLoadingYardConfiguration(t *testing.T) {
	root, environment, marker := migrateCLIFixture(t, false)
	writeCLIFile(t, filepath.Join(root, "config", "subyard.env"), "invalid configuration\n", 0600)
	var stdout, stderr bytes.Buffer
	prompt := &testkit.Prompt{}
	program, err := New(Options{RepositoryRoot: root, Program: "yard",
		Arguments: []string{"migrate", "--check", "--json"}, Environment: environment,
		Prompt: prompt, Stdout: &stdout, Stderr: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("migrate check: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var report struct {
		Outcome struct {
			Status string `json:"status"`
		} `json:"outcome"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil || report.Outcome.Status != "ready" {
		t.Fatalf("current report=%q err=%v", stdout.String(), err)
	}
	if len(prompt.Requests) != 0 {
		t.Fatalf("check prompted: %#v", prompt.Requests)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("check executed a mutation: %v", err)
	}
}

func TestMigrateAppliesOnlyAfterConfirmationAndSkipsReadyWork(t *testing.T) {
	for _, test := range []struct {
		name                  string
		pending, consent      bool
		wantCode, wantPrompts int
		wantApply             bool
	}{
		{"declined", true, false, 1, 1, false},
		{"confirmed", true, true, 0, 1, true},
		{"ready", false, false, 0, 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, environment, marker := migrateCLIFixture(t, test.pending)
			var stdout, stderr bytes.Buffer
			prompt := &testkit.Prompt{Answers: []bool{test.consent}}
			program, err := New(Options{RepositoryRoot: root, Program: "yard",
				Arguments: []string{"migrate"}, Environment: environment,
				Prompt: prompt, Stdout: &stdout, Stderr: &stderr})
			if err != nil {
				t.Fatal(err)
			}
			if code := program.Run(context.Background()); code != test.wantCode {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if len(prompt.Requests) != test.wantPrompts {
				t.Fatalf("prompts=%#v", prompt.Requests)
			}
			_, err = os.Stat(marker)
			if (err == nil) != test.wantApply {
				t.Fatalf("applied=%v want=%v err=%v", err == nil, test.wantApply, err)
			}
			if test.wantCode == 0 && !strings.Contains(stdout.String(), "ready") {
				t.Fatalf("no ready result: %q", stdout.String())
			}
		})
	}
}

func migrateCLIFixture(t *testing.T, pending bool) (string, []string, string) {
	t.Helper()
	root, environment, _ := nativeFixture(t)
	manifest, err := os.ReadFile(filepath.Join(repositoryRoot(t), "config", "commands.registry"))
	if err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(root, "config", "commands.registry"), string(manifest), 0600)
	runtimeRoot := filepath.Join(root, "runtime")
	release := "1.2.3-aaaaaaaaaaaa"
	destination := filepath.Join(runtimeRoot, "releases", release)
	for _, directory := range []string{"bin", "config"} {
		if err := os.MkdirAll(filepath.Join(destination, directory), 0700); err != nil {
			t.Fatal(err)
		}
	}
	marker := filepath.Join(root, "applied")
	registry := `{"schemaVersion":2,"minimumEpochs":{"settings":1},"currentEpochs":{"settings":1},"migrations":[]}`
	ready := fmt.Sprintf(`{"schemaVersion":1,"activationReconciliationOwned":true,"inspection":{"plan":"plan-v1-%s","assessment":{"action":"release.transition.v2","effect":"mutation","changed":false,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible"},"outcome":{"status":"ready","reachedGoal":true,"active":%q,"target":%q,"code":"ready","message":"verified"}}}`, strings.Repeat("a", 64), release, release)
	needed := fmt.Sprintf(`{"schemaVersion":1,"activationReconciliationOwned":true,"inspection":{"plan":"plan-v1-%s","assessment":{"action":"release.transition.v2","effect":"mutation","changed":true,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible","consequences":["reconcile the installed runtime"]},"decisions":[{"scope":"activation","resource":"activation.fixture","decision":"canonicalize","result":"converged"}],"outcome":{"status":"migration-required","reachedGoal":false,"active":%q,"target":%q,"code":"transition-required","message":"runtime drift","retry":"run yard update"}}}`, strings.Repeat("b", 64), release, release)
	completed := fmt.Sprintf(`{"schemaVersion":1,"activationReconciliationOwned":true,"outcome":{"status":"ready","reachedGoal":true,"active":%q,"target":%q,"code":"ready","message":"verified","transaction":"tx-0123456789abcdef"}}`, release, release)
	engine := fmt.Sprintf(`#!/bin/sh
set -eu
case "${1:-}" in
  --version) printf 'yard-engine 1.2.3\n' ;;
  _release-transition)
    request=$(cat)
    case "$request" in
      *'"mode":"inspect"'*)
        if [ "${MIGRATE_PENDING:-0}" = 1 ] && [ ! -f "$MIGRATE_MARKER" ]; then
          printf '%%s\n' '%s'
        else
          printf '%%s\n' '%s'
        fi ;;
      *'"mode":"converge"'*)
        if [ "$MIGRATE_PENDING" = 1 ] && [ ! -e "$MIGRATE_MARKER" ]; then
          [ "${SUBYARD_RELEASE_TRANSITION_GRANT_FD:-}" = 3 ]
          IFS= read -r grant <&3
          [ -n "$grant" ]
          : > "$MIGRATE_MARKER"
        fi
        printf '%%s\n' '%s' ;;
      *) exit 64 ;;
    esac ;;
  *) exit 64 ;;
esac
`, needed, ready, completed)
	writeCLIFile(t, filepath.Join(destination, "bin", "yard-engine"), engine, 0700)
	writeCLIFile(t, filepath.Join(destination, "config", "release-transition.json"), registry, 0600)
	checksums := fmt.Sprintf("%x  ./bin/yard-engine\n%x  ./config/release-transition.json\n", sha256.Sum256([]byte(engine)), sha256.Sum256([]byte(registry)))
	writeCLIFile(t, filepath.Join(destination, "runtime-files.sha256"), checksums, 0600)
	if err := os.Symlink("releases/"+release, filepath.Join(runtimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
	flag := "0"
	if pending {
		flag = "1"
	}
	return root, append(environment, "YARD_RUNTIME_ROOT="+runtimeRoot, "MIGRATE_MARKER="+marker, "MIGRATE_PENDING="+flag), marker
}
