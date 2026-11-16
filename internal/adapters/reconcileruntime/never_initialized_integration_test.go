package reconcileruntime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

// neverInitializedGuest answers the substrate and inventory checks of an
// unprovisioned guest; any other command fails like a guest without python3.
type neverInitializedGuest struct {
	substrate, inventory bool
	requests             [][]string
}

func (guest *neverInitializedGuest) Exec(_ context.Context, _, _ string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	guest.requests = append(guest.requests, request.Command)
	command := strings.Join(request.Command, " ")
	failed := ports.InstanceExecResult{ExitCode: 1}
	switch {
	case strings.Contains(command, "command -v python3"):
		if guest.substrate {
			return ports.InstanceExecResult{}, nil
		}
		return failed, errors.New("exit status 1")
	case strings.Contains(command, "test ! -e"):
		if guest.inventory {
			return failed, errors.New("exit status 1")
		}
		return ports.InstanceExecResult{}, nil
	}
	return ports.InstanceExecResult{ExitCode: 127}, errors.New("python3: command not found")
}

// A yard whose init never finished, in a guest without the integration
// substrate or inventory, has absent applied integration state. A working
// yard whose re-init failed, a yard from a release before the completion mark,
// and a guest with an inventory are read strictly and refuse on the failed read.
func TestAppliedIntegrationStateOfNeverInitializedYardIsAbsent(t *testing.T) {
	for _, test := range []struct {
		name                 string
		initialized, ever    string
		substrate, inventory bool
		absent               bool
	}{
		{name: "never initialized", initialized: "false", absent: true},
		{name: "re-init of a working yard", initialized: "false", ever: "true"},
		{name: "upgraded initialized yard", initialized: "true"},
		{name: "never initialized with substrate", initialized: "false", substrate: true},
		{name: "never initialized with inventory", initialized: "false", inventory: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := map[string]string{"user.subyard.managed": "true", "user.subyard.name": "y8", "user.subyard.initialized": test.initialized}
			if test.ever != "" {
				config["user.subyard.ever_initialized"] = test.ever
			}
			incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{"subyard-y8/yard-y8": {
				Name: "yard-y8", Project: "subyard-y8", Status: "Running", Config: config,
			}}}
			guest := &neverInitializedGuest{substrate: test.substrate, inventory: test.inventory}
			runtime := Runtime{RepositoryRoot: testkit.TempDir(t), Incus: incus, Executor: guest, Integrations: []profile.IntegrationDefinition{},
				Yard: domain.Context{YardName: "y8", IncusProject: "subyard-y8", YardInstanceName: "yard-y8", DevUser: "dev", DevUID: 1000}}
			applied, err := runtime.AppliedIntegrationConfiguration(context.Background())
			if test.absent {
				if err != nil || applied.Installed || applied.Settings != nil || len(applied.Identity) != 0 || len(applied.Selected) != 0 {
					t.Fatalf("never-initialized yard: applied=%#v err=%v", applied, err)
				}
				for _, command := range guest.requests {
					if command[0] == "python3" {
						t.Fatalf("absent state read the inventory: %v", command)
					}
				}
				if _, err := runtime.PrepareDeveloperIdentity(context.Background()); err != nil {
					t.Fatalf("init's identity preparation refused absent state: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("a failed inventory read was accepted: %#v", applied)
			}
		})
	}
}
