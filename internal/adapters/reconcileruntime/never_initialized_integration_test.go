package reconcileruntime

import (
	"context"
	"errors"
	"os"
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

// A guest that init never provisioned is not observed by profile runtime
// handlers, like a stopped yard; a working yard whose re-init failed still runs
// them and refuses on their failure.
func TestProfileRuntimesOfNeverProvisionedGuestAreDeferred(t *testing.T) {
	for _, test := range []struct {
		name     string
		ever     string
		deferred bool
	}{
		{name: "never initialized", deferred: true},
		{name: "re-init of a working yard", ever: "true"},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := map[string]string{"user.subyard.managed": "true", "user.subyard.name": "y8", "user.subyard.initialized": "false"}
			if test.ever != "" {
				config["user.subyard.ever_initialized"] = test.ever
			}
			incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{"subyard-y8/yard-y8": {
				Name: "yard-y8", Project: "subyard-y8", Status: "Running", Config: config,
			}}}
			root := testkit.TempDir(t)
			definition := profile.Definition{Name: "sample", Root: root, Runtime: &profile.RuntimeHook{ActivationID: "sample-runtime", Handler: "missing-runtime.sh"}}
			runtime := Runtime{RepositoryRoot: root, Incus: incus, Executor: &neverInitializedGuest{},
				Yard: domain.Context{YardName: "y8", IncusProject: "subyard-y8", YardInstanceName: "yard-y8", DevUser: "dev", DevUID: 1000}}
			observations, err := runtime.observeProfileRuntimes(context.Background(), []profile.Definition{definition})
			if test.deferred {
				if err != nil || observations["sample-runtime"].State != ports.RuntimeStateDeferred {
					t.Fatalf("never-provisioned guest: %#v err=%v", observations, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("a re-initialized yard skipped its runtime handler: %#v", observations)
			}
		})
	}
}

// Init plans the project hooks of a guest it never provisioned conditionally,
// without observing them, and its check before writes accepts that plan. A
// working yard whose re-init failed is observed and refuses on the failure.
func TestProjectHooksOfNeverProvisionedGuestArePlannedConditionally(t *testing.T) {
	for _, test := range []struct {
		name  string
		ever  string
		never bool
	}{
		{name: "never initialized", never: true},
		{name: "re-init of a working yard", ever: "true"},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := map[string]string{"user.subyard.managed": "true", "user.subyard.name": "y8", "user.subyard.initialized": "false"}
			if test.ever != "" {
				config["user.subyard.ever_initialized"] = test.ever
			}
			instance := ports.InstanceInfo{Name: "yard-y8", Project: "subyard-y8", Status: "Running", Config: config}
			incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{"subyard-y8/yard-y8": instance},
				Reconcile: ports.ReconcileState{InstanceFound: true, Instance: instance}}
			root := testkit.TempDir(t)
			if err := os.MkdirAll(root+"/config", 0o700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, root+"/config/projects-changed.sh", []byte("#!/bin/sh\n"), 0o600)
			guest := &neverInitializedGuest{}
			runtime := Runtime{RepositoryRoot: root, Incus: incus, Executor: guest, Integrations: []profile.IntegrationDefinition{},
				Yard: domain.Context{YardName: "y8", IncusProject: "subyard-y8", YardInstanceName: "yard-y8", DevUser: "dev", DevUID: 1000}}
			plan, err := runtime.PrepareProjectHooks(context.Background(), false)
			if !test.never {
				if err == nil {
					t.Fatal("a re-initialized yard skipped its project hook observation")
				}
				return
			}
			if err != nil || !plan.conditional {
				t.Fatalf("never-provisioned guest: plan=%#v err=%v", plan, err)
			}
			runtime.HookPlan = plan
			if err := runtime.CheckProjectHookPlan(context.Background(), true); err != nil {
				t.Fatalf("check before writes: %v", err)
			}
			for _, command := range guest.requests {
				if command[0] == "bash" {
					t.Fatalf("project hooks were observed in a never-provisioned guest: %v", command)
				}
			}
		})
	}
}
