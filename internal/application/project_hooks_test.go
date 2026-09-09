package application

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

func TestProjectHooksBoundExecutionAndDiagnoseMissingDispatcher(t *testing.T) {
	err := RunProjectHooks(context.Background(), domain.Context{}, func(ctx context.Context, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
		deadline, ok := ctx.Deadline()
		if remaining := time.Until(deadline); !ok || remaining > 130*time.Second || remaining < 125*time.Second {
			t.Fatalf("hook request has no bounded deadline: %v", remaining)
		}
		command := append([]string(nil), request.Command...)
		command[len(command)-1] = filepath.Join(t.TempDir(), "missing-dispatcher")
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
