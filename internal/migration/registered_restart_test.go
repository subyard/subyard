package migration

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/yardnetwork"
)

type restartNative struct {
	*testkit.Incus
	root string
}

func (native *restartNative) Instance(ctx context.Context, project, name string) (ports.InstanceInfo, error) {
	if err := ctx.Err(); err != nil {
		return ports.InstanceInfo{}, err
	}
	instance, err := native.Incus.Instance(ctx, project, name)
	if err != nil {
		return instance, err
	}
	status, err := os.ReadFile(filepath.Join(native.root, "status"))
	instance.Status = strings.TrimSpace(string(status))
	return instance, err
}
func (native *restartNative) ReconcileState(ctx context.Context, project, name, _, _, _ string) (ports.ReconcileState, error) {
	instance, err := native.Instance(ctx, project, name)
	return ports.ReconcileState{InstanceFound: err == nil, Instance: instance}, err
}
func (native *restartNative) Exec(_ context.Context, _, _ string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	action := request.Command[len(request.Command)-1]
	if action == "apply" {
		if err := os.WriteFile(filepath.Join(native.root, "policy"), []byte("installed"), 0600); err != nil {
			return ports.InstanceExecResult{}, err
		}
	}
	if action == "check" {
		if _, err := os.Stat(filepath.Join(native.root, "policy")); errors.Is(err, os.ErrNotExist) {
			return ports.InstanceExecResult{ExitCode: 1}, &ports.GuestExitError{Code: 1}
		}
		if _, err := os.Stat(filepath.Join(native.root, "memory")); err == nil {
			return ports.InstanceExecResult{ExitCode: 10}, &ports.GuestExitError{Code: 10}
		}
	}
	return ports.InstanceExecResult{}, nil
}
func (native *restartNative) AdoptRegisteredInstance(_ context.Context, before ports.InstanceInfo, values map[string]string) error {
	current := native.Instances[before.Project+"/"+before.Name]
	for key, value := range values {
		current.Config[key] = value
		current.LocalConfig[key] = value
	}
	native.Instances[before.Project+"/"+before.Name] = current
	return nil
}

type restartNetwork struct{}

func (restartNetwork) Check(context.Context, yardnetwork.Yard) error  { return nil }
func (restartNetwork) Ensure(context.Context, yardnetwork.Yard) error { return nil }
func (restartNetwork) WithStart(_ context.Context, _ yardnetwork.Yard, start func() error) error {
	return start()
}

func TestGuestTmpResumesCutBetweenPhysicalStopAndStart(t *testing.T) {
	for _, test := range []struct {
		name, power         string
		failStart, failStop int
	}{{"running", "running", 1, 0}, {"stopped", "stopped", 2, 0}, {"restore-stop-cut", "stopped", 0, 2}, {"guard-stale", "stopped", 0, 0}, {"uuid-drift", "stopped", 0, 0}, {"desired-drift", "stopped", 0, 0}, {"partial-start", "stopped", 1, 0}, {"cancelled", "stopped", 0, 0}, {"missing-script", "stopped", 0, 0}} {
		power := test.power
		t.Run(test.name, func(t *testing.T) {
			root := testkit.TempDir(t)
			if err := os.Mkdir(filepath.Join(root, "scripts"), 0700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(root, "scripts/guest-disk-tmp.sh"), []byte("trusted policy\n"), 0600)
			testkit.WriteFile(t, filepath.Join(root, "status"), []byte(power), 0600)
			testkit.WriteFile(t, filepath.Join(root, "memory"), []byte("tmpfs"), 0600)
			script := fmt.Sprintf(`#!/bin/sh
set -eu
case "$1" in
 stop)
  [ "$#" = 1 ]
  count=0; if [ -f "$STATE/stops" ];then count=$(cat "$STATE/stops");fi
  count=$((count+1));printf '%%s' "$count" > "$STATE/stops"
  [ "$count" != %d ] || exit 8
  printf stopped > "$STATE/status" ;;
 start)
  [ "$2" = --reconcile ]
  count=0; if [ -f "$STATE/starts" ];then count=$(cat "$STATE/starts");fi
  count=$((count+1));printf '%%s' "$count" > "$STATE/starts"
  [ "$count" != %d ] || exit 7
  printf running > "$STATE/status"
  if [ -f "$STATE/policy" ];then rm -f "$STATE/memory";fi ;;
 *) exit 99 ;;
esac
`, test.failStop, test.failStart)
			if test.name == "partial-start" {
				script = strings.Replace(script, `[ "$count" != 1 ] || exit 7`, `[ "$count" != 1 ] || { printf running > "$STATE/status"; exit 7; }`, 1)
			}
			testkit.WriteFile(t, filepath.Join(root, "scripts/lifecycle-guard.sh"), []byte(script), 0700)
			configuration := map[string]string{"volatile.uuid": "uuid-original", "user.subyard.desired_power": power, "user.subyard.managed": "true", "user.subyard.initialized": "true"}
			native := &restartNative{Incus: &testkit.Incus{Instances: map[string]ports.InstanceInfo{"fixture/yard": {Project: "fixture", Name: "yard", Config: configuration, LocalConfig: maps.Clone(configuration)}}}, root: root}
			runtime := reconcileruntime.Runtime{RepositoryRoot: root, Incus: native, Executor: native, NetworkPolicy: restartNetwork{}, Yard: domain.Context{YardName: "default", IncusProject: "fixture", YardInstanceName: "yard"}, Environment: []string{"PATH=" + os.Getenv("PATH"), "STATE=" + root}}
			asks := 0
			consent := func(domain.ConfirmationRequest) (bool, error) { asks++; return true, nil }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var guards []func() error
			if test.name == "cancelled" {
				guards = append(guards, func() error {
					status, _ := os.ReadFile(filepath.Join(root, "status"))
					if string(status) == "running" {
						cancel()
					}
					return ctx.Err()
				})
			}
			if test.name == "missing-script" {
				if err := os.Remove(filepath.Join(root, "scripts/guest-disk-tmp.sh")); err != nil {
					t.Fatal(err)
				}
			}
			if test.name == "guard-stale" || strings.HasSuffix(test.name, "-drift") {
				guards = append(guards, func() error {
					status, _ := os.ReadFile(filepath.Join(root, "status"))
					if string(status) != "running" {
						return nil
					}
					current := native.Instances["fixture/yard"]
					if test.name == "uuid-drift" {
						current.Config["volatile.uuid"] = "foreign-uuid"
					}
					if test.name == "desired-drift" {
						current.Config["user.subyard.desired_power"] = "running"
					}
					return domain.ErrPlanStale
				})
			}
			if err := migrateRegisteredDiskTmp(ctx, runtime, consent, guards...); err == nil {
				t.Fatal("native start cut or stale input was completed")
			}
			if test.name == "missing-script" {
				if _, err := os.Stat(filepath.Join(root, "starts")); !errors.Is(err, os.ErrNotExist) || asks != 0 {
					t.Fatal("missing source caused consent or temporary boot")
				}
				return
			}
			after, err := native.Instance(context.Background(), "fixture", "yard")
			if err != nil {
				t.Fatal(err)
			}
			if test.name == "uuid-drift" || test.name == "desired-drift" {
				if after.Status != "running" {
					t.Fatal("restoration stopped a changed native identity or intent")
				}
				return
			}
			expectedStatus, expectedMarker := "stopped", ""
			if test.name == "restore-stop-cut" {
				expectedStatus = "running"
			}
			if after.Status != expectedStatus || after.Config["user.subyard.disk_tmp"] != expectedMarker {
				t.Fatalf("cut state: %#v", after)
			}
			if test.name == "restore-stop-cut" {
				stops, _ := os.ReadFile(filepath.Join(root, "stops"))
				err := migrateRegisteredDiskTmp(context.Background(), runtime, func(request domain.ConfirmationRequest) (bool, error) {
					if !strings.Contains(request.Summary, "restore stopped power") || strings.Contains(request.Consequences[0], "start this yard") {
						t.Fatal("restoration plan requests an unnecessary restart")
					}
					return false, nil
				})
				unchanged, _ := os.ReadFile(filepath.Join(root, "stops"))
				current, _ := native.Instance(context.Background(), "fixture", "yard")
				if !errors.Is(err, domain.ErrOperationDeclined) || string(stops) != string(unchanged) || current.Status != "running" {
					t.Fatal("restoration decline changed power", err)
				}
			}
			// Reconstruct the concrete runtime from durable native state after the cut.
			resumed := runtime
			resumed.Incus = &restartNative{Incus: native.Incus, root: root}
			resumed.Executor = resumed.Incus.(*restartNative)
			if err := migrateRegisteredDiskTmp(context.Background(), resumed, consent); err != nil {
				t.Fatal("resume", err)
			}
			after, err = native.Instance(context.Background(), "fixture", "yard")
			if err != nil {
				t.Fatal(err)
			}
			if after.Status != power || after.Config["user.subyard.desired_power"] != power || after.Config["volatile.uuid"] != "uuid-original" || after.Config["user.subyard.disk_tmp"] != "v1" {
				t.Fatalf("resumed state: %#v", after)
			}
			ready, err := resumed.RegisteredDiskTmpReady(context.Background())
			if err != nil || !ready {
				t.Fatalf("readiness trapped after resume: %t %v", ready, err)
			}
			beforeAsks := asks
			starts, _ := os.ReadFile(filepath.Join(root, "starts"))
			if err := migrateRegisteredDiskTmp(context.Background(), resumed, consent); err != nil {
				t.Fatal(err)
			}
			repeated, _ := os.ReadFile(filepath.Join(root, "starts"))
			if string(starts) != string(repeated) || asks != beforeAsks {
				t.Fatal("completed repeat restarted")
			}
		})
	}
}

func TestGuestTmpMaintenanceFailuresHaveSafePublicDiagnostics(t *testing.T) {
	for _, test := range []struct{ name, stage, class string }{
		{"stale", "post-consent-check", "stale-input"}, {"cancel", "post-consent-check", "cancelled"}, {"deadline", "post-consent-check", "deadline"}, {"unknown", "post-consent-check", "unclassified"},
		{"exit", "stop", "child-exit-37"}, {"missing", "stop", "command-not-found"}, {"permission", "stop", "permission-denied"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := testkit.TempDir(t)
			if err := os.Mkdir(filepath.Join(root, "scripts"), 0700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(root, "scripts/guest-disk-tmp.sh"), []byte("trusted policy\n"), 0600)
			testkit.WriteFile(t, filepath.Join(root, "status"), []byte("running"), 0600)
			testkit.WriteFile(t, filepath.Join(root, "memory"), []byte("tmpfs"), 0600)
			if test.name != "missing" {
				mode := os.FileMode(0700)
				if test.name == "permission" {
					mode = 0600
				}
				testkit.WriteFile(t, filepath.Join(root, "scripts/lifecycle-guard.sh"), []byte("#!/bin/sh\nexit 37\n"), mode)
			}
			configuration := map[string]string{"volatile.uuid": "uuid-original", "user.subyard.desired_power": "running", "user.subyard.managed": "true", "user.subyard.initialized": "true"}
			native := &restartNative{Incus: &testkit.Incus{Instances: map[string]ports.InstanceInfo{"fixture/yard": {Project: "fixture", Name: "yard", Config: configuration, LocalConfig: maps.Clone(configuration)}}}, root: root}
			runtime := reconcileruntime.Runtime{RepositoryRoot: root, Incus: native, Executor: native, Yard: domain.Context{YardName: "default", IncusProject: "fixture", YardInstanceName: "yard"}, Environment: []string{"PATH=" + os.Getenv("PATH")}}
			consented := false
			original := map[string]error{"stale": domain.ErrPlanStale, "cancel": context.Canceled, "deadline": context.DeadlineExceeded, "unknown": errors.New("private-fixture-detail")}[test.name]
			err := migrateRegisteredDiskTmp(context.Background(), runtime, func(domain.ConfirmationRequest) (bool, error) { consented = true; return true, nil }, func() error {
				if consented {
					return original
				}
				return nil
			})
			var diagnostic interface{ ActivationDiagnostic() (string, string) }
			if err == nil || !errors.As(err, &diagnostic) {
				t.Fatal("maintenance failure lost public diagnostic", err)
			}
			message, retry := diagnostic.ActivationDiagnostic()
			if message != "registered yard maintenance "+test.stage+" failed ("+test.class+")" || retry != "run yard update --check" || strings.Contains(err.Error(), "private-fixture-detail") || strings.Contains(message, root) {
				t.Fatal("unsafe or imprecise maintenance diagnostic", message, retry, err)
			}
			if original != nil && !errors.Is(err, original) {
				t.Fatal("maintenance diagnostic lost original cause")
			}
			after, _ := native.Instance(context.Background(), "fixture", "yard")
			if after.Status != "running" || after.Config["user.subyard.disk_tmp"] != "" || !consented {
				t.Fatal("failed maintenance changed native completion or skipped late consent")
			}
		})
	}
}
