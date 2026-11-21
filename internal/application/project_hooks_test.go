package application

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestProjectHooksBoundExecutionAndDiagnoseMissingDispatcher(t *testing.T) {
	err := RunProjectHooks(context.Background(), domain.Context{}, func(ctx context.Context, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
		wantCommand := []string{"sh", "-c", `[ -x "$1" ] && exec timeout --kill-after=5s 120s "$1"`, "subyard", projectHooksDispatcher}
		if !slices.Equal(request.Command, wantCommand) || len(request.Environment) != 1 || request.Environment["HOME"] != "/home/" {
			t.Fatalf("unscoped request changed: %#v", request)
		}
		deadline, ok := ctx.Deadline()
		if remaining := time.Until(deadline); !ok || remaining > 130*time.Second || remaining < 125*time.Second {
			t.Fatalf("hook request has no bounded deadline: %v", remaining)
		}
		command := append([]string(nil), request.Command...)
		command[len(command)-1] = filepath.Join(testkit.TempDir(t), "missing-dispatcher")
		if err := exec.CommandContext(ctx, command[0], command[1:]...).Run(); err == nil {
			t.Fatal("absent dispatcher was silently accepted")
		}
		return ports.InstanceExecResult{ExitCode: 1, Stderr: []byte("private hook output")}, errors.New("private transport output")
	})
	if err == nil || !strings.Contains(err.Error(), "yard init") || strings.Contains(err.Error(), "private") {
		t.Fatalf("unsafe or missing recovery warning: %v", err)
	}
}

func TestProjectHooksHonorCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := RunProjectHooks(ctx, domain.Context{}, func(ctx context.Context, _ ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
		if ctx.Err() != context.Canceled {
			t.Fatal("hook execution detached from the project request")
		}
		return ports.InstanceExecResult{}, ctx.Err()
	})
	if err == nil {
		t.Fatal("cancelled hook was reported as successful")
	}
}

func TestProjectHookFailureHasPublicActivationDiagnostic(t *testing.T) {
	for _, test := range []struct {
		result ports.InstanceExecResult
		err    error
		reason string
	}{
		{ports.InstanceExecResult{ExitCode: 1, Stdout: []byte("private stdout"), Stderr: []byte("private stderr")}, &ports.GuestExitError{Code: 1}, "exited unsuccessfully"},
		{ports.InstanceExecResult{ExitCode: 124, Stderr: []byte("private stderr")}, &ports.GuestExitError{Code: 124}, "timed out"},
		{ports.InstanceExecResult{ExitCode: 137}, &ports.GuestExitError{Code: 137}, "timed out"},
		{ports.InstanceExecResult{Stderr: []byte("private stderr")}, errors.New("private transport output"), "could not be executed"},
		{ports.InstanceExecResult{}, fmt.Errorf("private transport: %w", context.Canceled), "were cancelled"},
		{ports.InstanceExecResult{}, fmt.Errorf("private transport: %w", context.DeadlineExceeded), "timed out"},
	} {
		err := RunProjectHooks(context.Background(), domain.Context{YardName: "sample"}, func(context.Context, ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
			return test.result, test.err
		})
		var diagnostic interface{ ActivationDiagnostic() (string, string) }
		if !errors.As(err, &diagnostic) {
			t.Fatalf("hook failure has no activation diagnostic: %v", err)
		}
		message, retry := diagnostic.ActivationDiagnostic()
		want := "yard sample: optional agent project hooks " + test.reason + "; the hook pass failed or its state is undetermined, see the integration's status"
		if message != want || retry != "run yard migrate" || strings.Contains(message+err.Error(), "private") {
			t.Fatalf("diagnostic = %q / %q (%v)", message, retry, err)
		}
		if err.Error() != "an optional agent project hook failed; run 'yard init' to repair wiring and retry active integrations, or use the integration's sync command" {
			t.Fatalf("public warning text changed: %q", err.Error())
		}
	}
}

func TestProjectHookFailureNamesOnlyApprovedDispatcherRecords(t *testing.T) {
	const a = "/hooks/a"
	const b = "/hooks/b"
	err := RunProjectHooks(context.Background(), domain.Context{YardName: "sample"}, func(_ context.Context, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
		if request.Environment["SUBYARD_PROJECT_HOOK_REPORT"] != "1" || !strings.Contains(request.Command[2], "3>&1 >/dev/null 2>&1") {
			t.Fatal("hook streams are not isolated from dispatcher records")
		}
		return ports.InstanceExecResult{ExitCode: 1, Stdout: []byte("subyard-project-hook-failed:/hooks/b\nsubyard-project-hook-failed:/unapproved\nsubyard-project-hook-failed:/hooks/a\nsubyard-project-hook-failed:/hooks/a\n"), Stderr: []byte("private hook output")}, nil
	}, b, a)
	var failure *ProjectHookFailure
	if !errors.As(err, &failure) {
		t.Fatal(err)
	}
	message, retry := failure.ActivationDiagnostic()
	want := "yard sample: project hook /hooks/a failed; yard sample: project hook /hooks/b failed"
	if message != want || retry != "run yard migrate" {
		t.Fatalf("diagnostic = %q / %q", message, retry)
	}
}

func TestProjectHookLabelsDoNotDeterminePassOutcome(t *testing.T) {
	for _, test := range []struct {
		code int
		err  error
		want string
	}{
		{0, nil, ""},
		{124, &ports.GuestExitError{Code: 124}, "yard sample: project hook /hooks/a failed; yard sample: optional agent project hooks timed out"},
		{0, context.Canceled, "yard sample: project hook /hooks/a failed; yard sample: optional agent project hooks were cancelled"},
	} {
		err := RunProjectHooks(context.Background(), domain.Context{YardName: "sample"}, func(context.Context, ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
			return ports.InstanceExecResult{ExitCode: test.code, Stdout: []byte("subyard-project-hook-failed:/hooks/a\n")}, test.err
		}, "/hooks/a")
		if test.want == "" && err == nil {
			continue
		}
		var failure *ProjectHookFailure
		if !errors.As(err, &failure) {
			t.Fatalf("expected hook failure, got %v", err)
		}
		if got, _ := failure.ActivationDiagnostic(); got != test.want {
			t.Fatalf("diagnostic = %q, want %q", got, test.want)
		}
	}
}
