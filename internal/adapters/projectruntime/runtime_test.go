package projectruntime

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/shellquote"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestRemoteCommandQuotingKeepsEveryArgumentLiteral(t *testing.T) {
	got := shellquote.Command([]string{"git", "clone", "--", "x'; touch /tmp/escaped", "/srv/workspaces/id/src"})
	want := "'git' 'clone' '--' 'x'\\''; touch /tmp/escaped' '/srv/workspaces/id/src'"
	if got != want {
		t.Fatalf("quoted command = %q, want %q", got, want)
	}
}

func TestParseMetadataRejectsInvalidEntryAndTrailingJSON(t *testing.T) {
	records, warnings := parseMetadata([]byte(
		`{"schema":1,"projectId":"valid-12345678","name":"Valid","mode":"sync","target":"yard"}` + "\n" +
			`{"schema":1,"projectId":"../escape","name":"Bad","mode":"sync"}` + "\n" + `{`,
	))
	if len(records) != 1 || records[0].ProjectID != "valid-12345678" || len(warnings) != 2 {
		t.Fatalf("invalid live metadata was not isolated: records=%#v warnings=%#v", records, warnings)
	}
}

func TestObserveUsesInjectedIncusAndFallsBackFromSSH(t *testing.T) {
	root := t.TempDir()
	ssh := filepath.Join(root, "ssh")
	if err := os.WriteFile(ssh, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	fake := &testkit.Incus{
		Instances: map[string]ports.InstanceInfo{"subyard/yard": {Status: "Running"}},
		ExecSteps: []testkit.IncusExecStep{
			{Result: ports.InstanceExecResult{Stdout: []byte("/srv/workspaces/known-12345678\n")}},
			{Result: ports.InstanceExecResult{Stdout: []byte("box-12345678\trunning\n")}},
			{Result: ports.InstanceExecResult{Stdout: []byte(`{"schema":1,"projectId":"live-12345678","name":"Live","mode":"sync","target":"yard"}`)}},
		},
	}
	observation, err := (Runtime{Incus: fake, Executor: fake, SSHBinary: ssh}).Observe(
		context.Background(), domain.Context{
			AccessKind: domain.AccessLocal, IncusProject: "subyard", YardInstanceName: "yard", SSHHost: "yard",
		}, []domain.ProjectRecord{
			{ProjectID: "known-12345678", YardPath: "/srv/workspaces/known-12345678/src"},
			{ProjectID: "box-12345678", YardPath: "/srv/workspaces/box-12345678/src", Target: "openclaw"},
		}, true,
	)
	if err != nil || !observation.Running || !observation.Reached || len(observation.Live) != 1 ||
		observation.Presence["live-12345678"] != domain.ProjectPresent ||
		observation.Boxes["box-12345678"] != domain.ProjectBoxUp {
		t.Fatalf("unexpected injected observation: %#v err=%v", observation, err)
	}
}
