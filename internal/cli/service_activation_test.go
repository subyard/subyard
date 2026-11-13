package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
)

func declaredServiceActivationFixture(t *testing.T) (*CLI, *testkit.Incus, *serviceSelection) {
	t.Helper()
	root, environment, _ := nativeFixture(t)
	incus := lifecycleIncus()
	program, err := New(Options{RepositoryRoot: root, Environment: environment, Incus: incus})
	if err != nil {
		t.Fatal(err)
	}
	selection := infrastructureFixture(t, program, incus, "fixture-service")
	if err := selection.registry.Remove(selection.record, selection.publication); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config/profiles/synthetic-service/profile.json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body = []byte(strings.Replace(string(body), `"id":"fixture-service"`, `"id":"fixture-service","registration_activation":"fixture-owner"`, 1))
	testkit.WriteFile(t, path, body, 0o644)
	program, err = New(Options{RepositoryRoot: root, Environment: environment, Incus: incus})
	if err != nil {
		t.Fatal(err)
	}
	return program, incus, selection
}

func registrationStageFixture(program *CLI, id string) *activationStageReconciler {
	platform := newInitPlatformFixture()
	platform.converged[ports.ReconcileStageTestVMs] = true
	return &activationStageReconciler{configuration: program, id: id, stage: ports.ReconcileStageTestVMs,
		inspectApplicability: func(context.Context) (activationApplicability, error) {
			return activationApplicability{state: "installed", applies: true}, nil
		},
		platform: func(context.Context, activationApplicability) (ports.ReconcileStageRunner, error) {
			return platform, nil
		},
	}
}

func TestServiceRegistrationDoesNotBindSameRelease(t *testing.T) {
	for _, scenario := range []string{"complete", "repair", "no-journal", "new-release-resume"} {
		complete := scenario == "complete"
		fixture := newConfigApplyRepairFixture(t, false)
		request, journal := repairRequest(t, pendingInitFixture{configApplyRepairFixture: fixture})
		if scenario == "no-journal" {
			if err := os.Remove(fixture.journalPath); err != nil {
				t.Fatal(err)
			}
		} else if !complete {
			journal.Checkpoint = releasetransition.JournalReconciling
			journal.Releases.From = journal.Goal.Target
			if scenario == "new-release-resume" {
				journal.Releases.From = "previous-release"
			}
			payload, err := json.Marshal(journal)
			if err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, fixture.journalPath, payload, 0o600)
		}
		for _, mode := range []releasetransition.ProcessMode{releasetransition.ProcessInspect, releasetransition.ProcessConverge} {
			request.Mode = mode
			operation := fixture.cli.releaseActivationOperation(request)
			if operation.activation.repair != (scenario == "complete" || scenario == "repair") || operation.activation.complete != complete {
				t.Fatalf("repair classification: %+v", operation.activation)
			}
			owner := registrationStageFixture(operation, "test-vm-broker")
			before, beforeErr := owner.Observe(context.Background(), journal.Releases, releasetransition.ReleaseLinks{})
			owners := operation.bindServiceRegistrationActivation(request, []releasetransition.V2ActivationReconciler{owner})
			after, afterErr := owners[0].Observe(context.Background(), journal.Releases, releasetransition.ReleaseLinks{})
			if owners[0] != owner || (owner.planned != nil) != (scenario == "new-release-resume") || (owner.after != nil) != (scenario == "new-release-resume") || !reflect.DeepEqual(before, after) || beforeErr != afterErr || owner.activationConfiguration() != operation {
				t.Fatalf("same-release owner changed: before=%+v after=%+v errors=%v/%v", before, after, beforeErr, afterErr)
			}
		}
	}
}

func TestNewReleaseServiceRegistrationUsesNativeProofAfterReconcile(t *testing.T) {
	for _, scenario := range []string{"one", "UUID", "publish-failure", "broken-config", "broken-config+UUID", "lock-held", "lock-inherited"} {
		t.Run(scenario, func(t *testing.T) {
			program, incus, selection := declaredServiceActivationFixture(t)
			var output bytes.Buffer
			program.options.Stderr = &output
			home := environmentValue(program.options.Environment, "SUBYARD_CONFIG_HOME")
			runtimeRoot := testkit.TempDir(t)
			if err := os.MkdirAll(filepath.Join(runtimeRoot, "releases/previous-release"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("releases/previous-release", filepath.Join(runtimeRoot, "current")); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(scenario, "broken-config") {
				path := filepath.Join(home, "yards/unrelated/config.env")
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, path, []byte("YARD_KIND=vm\nSSH_PORT='unterminated\n"), 0o600)
			}
			request := releasetransition.ProcessRequest{ConfigHome: home, RuntimeRoot: runtimeRoot, Target: "next-release", Mode: releasetransition.ProcessInspect, Direction: releasetransition.DirectionActivateTarget}
			operation := program.releaseActivationOperation(request)
			owner := registrationStageFixture(operation, "fixture-owner")
			releases := releasetransition.ReleasePair{From: "previous-release", Target: request.Target}
			links := releasetransition.ReleaseLinks{Active: releases.From}
			before, err := owner.Observe(context.Background(), releases, links)
			if err != nil {
				t.Fatal(err)
			}
			owner.prospective = func(releasetransition.V2SettingsSnapshotView) (*activationStageReconciler, error) {
				return registrationStageFixture(operation, "fixture-owner"), nil
			}
			operation.bindServiceRegistrationActivation(request, []releasetransition.V2ActivationReconciler{owner})
			prospective, err := owner.ObserveProspective(context.Background(), releases, links, preparedActivationSnapshotFixture{home: home})
			if err != nil || len(prospective.Planned) != 1 {
				t.Fatalf("prospective plan: %+v %v", prospective, err)
			}
			prospective.Planned = nil
			if !reflect.DeepEqual(before, prospective) {
				t.Fatalf("changed prospective observation: %+v", prospective)
			}
			observed, err := owner.Observe(context.Background(), releases, links)
			want := "register service fixture-service (pool default) when exactly one owned runtime is proven; runtime and leases retained"
			if err != nil || len(observed.Planned) != 1 || observed.Planned[0] != want {
				t.Fatalf("conditional plan: %+v %v", observed, err)
			}
			observed.Planned = nil
			if !reflect.DeepEqual(before, observed) {
				t.Fatalf("registration changed native observation: %+v / %+v", before, observed)
			}
			if strings.Contains(scenario, "UUID") {
				delete(incus.Reconcile.Instance.Config, "volatile.uuid")
			}
			if scenario == "publish-failure" {
				if err := os.RemoveAll(filepath.Dir(selection.publication)); err != nil {
					t.Fatal(err)
				}
			}
			if strings.HasPrefix(scenario, "lock-") {
				t.Setenv("SUBYARD_OWNER_LOCK_FD", "")
				dataHome := environmentValue(program.options.Environment, "SUBYARD_HOME")
				_, release, err := uninstall.OwnerLock(context.Background(), dataHome, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer release()
				if scenario == "lock-inherited" {
					borrowed, err := uninstall.BorrowOwnerLock(dataHome)
					if err != nil || borrowed == nil {
						t.Fatalf("borrow barrier: %v", err)
					}
					defer borrowed.Close()
					t.Setenv("SUBYARD_OWNER_LOCK_FD", strconv.Itoa(int(borrowed.Fd())))
				}
			}
			if err := owner.Reconcile(context.Background(), links); err != nil {
				t.Fatalf("metadata step blocked activation: %v", err)
			}
			after, err := owner.Observe(context.Background(), releases, links)
			if (scenario == "one" || scenario == "publish-failure") && len(after.Planned) != 0 {
				t.Fatalf("registered entry retained plan line: %+v", after)
			}
			after.Planned = nil
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("metadata changed readiness: %+v %v", after, err)
			}
			record, resolveErr := selection.registry.Resolve(selection.record.Owner, "fixture-service", "default")
			if scenario == "lock-held" {
				if resolveErr == nil || !strings.Contains(output.String(), "yard service register fixture-service --yard NAME") {
					t.Fatalf("held barrier allowed registration or omitted hint: %+v %v %s", record, resolveErr, &output)
				}
			} else if strings.HasPrefix(scenario, "broken-config") {
				if scenario == "broken-config" && (!strings.Contains(output.String(), "yard service register fixture-service --yard default") || strings.Contains(output.String(), "--yard NAME")) {
					t.Fatalf("hint lost sole proven runtime: %s", &output)
				}
				if resolveErr == nil || !strings.Contains(output.String(), "yard unrelated: load configuration:") || strings.Contains(output.String(), "multiple candidate runtimes") || strings.Contains(output.String(), "yard service register fixture-service --yard unrelated") {
					t.Fatalf("load failure became a candidate: %+v %v %s", record, resolveErr, &output)
				}
				if strings.Contains(scenario, "UUID") && !strings.Contains(output.String(), "service runtime UUID is unproven") {
					t.Fatalf("independent proof finding lost: %s", &output)
				}
			} else if scenario == "UUID" {
				if resolveErr == nil || !strings.Contains(output.String(), "service runtime UUID is unproven; run yard service register fixture-service --yard default") {
					t.Fatalf("native proof bypassed: %+v %v %s", record, resolveErr, &output)
				}
			} else {
				if resolveErr != nil || record.UUID != selection.record.UUID {
					t.Fatalf("registration: %+v %v", record, resolveErr)
				}
				if scenario == "publish-failure" && (!strings.Contains(output.String(), "yard service register fixture-service --yard default") || selection.registry.Published(record, selection.publication) == nil) {
					t.Fatalf("missing publication guidance: %s", &output)
				}
				if scenario == "one" && !strings.Contains(output.String(), "Registered service fixture-service pool default: yard default, project "+record.Project+", instance "+record.Instance+", UUID "+record.UUID) {
					t.Fatalf("missing exact identity: %s", &output)
				}
			}
		})
	}
}
