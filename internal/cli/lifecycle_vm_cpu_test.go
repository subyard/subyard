package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
)

type lifecycleCPUFixture struct {
	*testkit.Incus
	t      *testing.T
	ready  bool
	err    error
	checks int
}

func (fixture *lifecycleCPUFixture) VMCPUConverged(_ context.Context, project, instance string, apply bool) (bool, error) {
	fixture.checks++
	if project != "synthetic" || instance != "yard" || apply {
		fixture.t.Fatal("privilege assessment did not use read-only target readiness")
	}
	return fixture.ready, fixture.err
}

func TestLifecycleVMCPUPrivilegesSkipConvergedStart(t *testing.T) {
	failure := errors.New("invalid owned scheduling metadata")
	for _, scenario := range []struct {
		name                          string
		changed, ready, authorization bool
		failure                       error
	}{
		{name: "converged", ready: true},
		{name: "drift", authorization: true},
		{name: "stopped", changed: true, authorization: true},
		{name: "invalid", failure: failure},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			root := testkit.TempDir(t)
			log := filepath.Join(root, "sudo.log")
			testkit.WriteFile(t, filepath.Join(root, "sudo"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$SUDO_LOG\"\nexit 1\n"), 0o700)
			t.Setenv("PATH", root)
			fixture := &lifecycleCPUFixture{Incus: &testkit.Incus{}, t: t, ready: scenario.ready, err: scenario.failure}
			program := &CLI{options: Options{Incus: fixture}, env: map[string]string{"PATH": root, "SUDO_LOG": log},
				effectiveUID: func() int { return 1000 }, operatorTerminal: func() bool { return false }}
			var diagnostics bytes.Buffer
			err := program.prepareLifecycleVMCPUPrivileges(context.Background(), &diagnostics,
				domain.Context{IncusProject: "synthetic", YardInstanceName: "yard"},
				&lifecycleExecution{action: "start", changed: scenario.changed})
			_, statErr := os.Stat(log)
			if (statErr == nil) != scenario.authorization || (err != nil) != (scenario.authorization || scenario.failure != nil) {
				t.Fatalf("authorization=%t err=%v diagnostic=%q", statErr == nil, err, diagnostics.String())
			}
			if scenario.failure != nil && !errors.Is(err, scenario.failure) {
				t.Fatalf("lost readiness cause: %v", err)
			}
			if fixture.checks != map[bool]int{true: 0, false: 1}[scenario.changed] {
				t.Fatalf("readiness checks=%d", fixture.checks)
			}
			if diagnostics.Len() != 0 {
				t.Fatalf("no-op or failed assessment emitted warning: %q", diagnostics.String())
			}
		})
	}
}
