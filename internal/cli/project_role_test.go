package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerinventory"
	"github.com/Subyard/Subyard/internal/shellquote"
	"github.com/Subyard/Subyard/internal/state"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestCanonicalProjectRoleDenialPreservesStaleControllerState(t *testing.T) {
	for _, name := range []string{"sync", "code"} {
		for _, test := range []struct {
			name          string
			expired       bool
			fresh         bool
			compatibility bool
			renamed       bool
			unreachable   bool
		}{
			{name: "invalidated/native"},
			{name: "expired/native", expired: true},
			{name: "invalidated/compatibility", compatibility: true},
			{name: "expired/compatibility", expired: true, compatibility: true},
			{name: "renamed/native", renamed: true},
			{name: "renamed/compatibility", renamed: true, compatibility: true},
			{name: "renamed-fresh/native", renamed: true, fresh: true},
			{name: "renamed-fresh/compatibility", renamed: true, fresh: true, compatibility: true},
			{name: "unreachable/native", unreachable: true},
			{name: "unreachable/compatibility", unreachable: true, compatibility: true},
		} {
			if test.fresh && name != "code" {
				continue
			}
			t.Run(name+"/"+test.name, func(t *testing.T) {
				root, environment, stateDirectory := nativeFixture(t)
				manifest := filepath.Join(root, "config", "commands.registry")
				current, err := os.ReadFile(manifest)
				if err != nil {
					t.Fatal(err)
				}
				writeCLIFile(t, manifest, string(current)+"sync||@project||local|mutate|dynamic|public|projects|project-target|sync [path]|sync|--name --target --yes --help|\n", 0o600)
				writeDisabledProjectRole(t, root)
				if err := os.Mkdir(stateDirectory, 0o700); err != nil {
					t.Fatal(err)
				}
				writeCLIFile(t, filepath.Join(stateDirectory, "sentinel"), "existing project state\n", 0o600)
				source := filepath.Join(root, "source")
				if err := os.Mkdir(source, 0o700); err != nil {
					t.Fatal(err)
				}
				now := time.Unix(1_000, 0).UTC()
				fetchedAt := time.Unix(1, 0).UTC()
				if test.expired {
					fetchedAt = now.Add(-ownerinventory.Freshness - time.Second)
				}
				if test.fresh {
					fetchedAt = now
				}
				ownerRoot := filepath.Join(environmentValue(environment, "SUBYARD_HOME"), "owner-inventory")
				trust, err := ownerinventory.NewSSHHostTrust("remote.example ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
				if err != nil {
					t.Fatal(err)
				}
				connection := ownerinventory.Connection{
					HostID: "remote-owner", Destination: "dev@remote.example", Trust: &trust,
					Yards: map[string]ownerinventory.YardRoute{"build": {SSHHost: "yard-remote"}},
				}
				if test.compatibility {
					connection.Yards = nil
					writeProjectCompatibilityRoute(t, root, "")
				}
				if err := (ownerinventory.Connections{Root: ownerRoot}).Write(connection); err != nil {
					t.Fatal(err)
				}
				inventory := inventoryResult("remote-owner", "build", "Demo").inventory
				if err := (ownerinventory.Cache{Root: ownerRoot}).Write(ownerinventory.Snapshot{
					FetchedAt: fetchedAt, Inventory: inventory,
				}); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(ownerRoot, "tmp"), 0o700); err != nil {
					t.Fatal(err)
				}
				bin := t.TempDir()
				responsePath := filepath.Join(bin, "response")
				if test.renamed {
					inventory.HostID = "renamed-owner"
				}
				writeHostRPCFixture(t, responsePath, inventory)
				physicalCall := filepath.Join(bin, "physical-call")
				inventoryReply := "cat " + shellquote.Word(responsePath)
				if test.unreachable {
					inventoryReply = "exit 255"
				}
				roleReply := "printf 'selected yard role does not accept work projects\\n' >&2; exit 1"
				if test.renamed {
					roleReply = "printf x > " + shellquote.Word(physicalCall) + "; printf 'unexpected owner query after identity mismatch\\n' >&2; exit 1"
				}
				writeCLIFile(t, filepath.Join(bin, "ssh"), "#!/bin/sh\n"+trustedSSHMock(t)+
					"case \"$*\" in\n"+
					"  *rpc*--stdio*) "+inventoryReply+" ;;\n"+
					"  *_project-state*preview*|*_project-state*check-role*) "+roleReply+" ;;\n"+
					"  *) printf x > "+shellquote.Word(physicalCall)+"; exit 1 ;;\nesac\n", 0o700)
				writeCLIFile(t, filepath.Join(bin, "code"), "#!/bin/sh\nprintf x > "+shellquote.Word(physicalCall)+"\n", 0o700)
				t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
				argument := "Demo"
				if name == "sync" {
					argument = source
				}
				var stdout, stderr bytes.Buffer
				incus := lifecycleIncus()
				program, err := New(Options{
					RepositoryRoot: root, Program: "yard", Arguments: []string{"-Y", "remote-owner/build", name, argument, "--yes"},
					Environment: append(environment, "SUBYARD_HOST_ID=controller"), WorkingDir: root,
					Stdout: &stdout, Stderr: &stderr, Clock: testkit.NewManualClock(now), Incus: incus, Executor: incus,
				})
				if err != nil {
					t.Fatal(err)
				}
				stateRoots := []string{filepath.Join(root, "config"), filepath.Join(root, "state"), filepath.Join(root, "data")}
				before := make([][]string, len(stateRoots))
				for index, path := range stateRoots {
					before[index] = nativeTreeSnapshot(t, path)
				}
				wantCode, wantError := 1, "does not accept work projects"
				if test.renamed {
					wantCode, wantError = 2, "owner HostID mismatch"
					if test.fresh {
						wantCode = 1
					}
				}
				if code := program.Run(context.Background()); code != wantCode || !strings.Contains(stderr.String(), wantError) {
					t.Errorf("canonical %s refusal: code=%d stderr=%q; want code=%d error=%q", name, code, stderr.String(), wantCode, wantError)
				}
				if _, err := os.Lstat(physicalCall); !os.IsNotExist(err) || len(incus.ExecCalls) != 0 || len(incus.PowerUpdates) != 0 || len(incus.ConfigUpdates) != 0 {
					t.Fatalf("role refusal reached an editor or physical action: marker=%v exec=%d power=%d config=%d", err, len(incus.ExecCalls), len(incus.PowerUpdates), len(incus.ConfigUpdates))
				}
				for index, path := range stateRoots {
					if !slices.Equal(before[index], nativeTreeSnapshot(t, path)) {
						t.Errorf("role refusal changed controller %s files", filepath.Base(path))
					}
				}
			})
		}
	}
}

func TestCanonicalCompatibilityProjectRouteRevalidatedBeforeWork(t *testing.T) {
	for _, change := range []string{"unchanged", "changed", "removed"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			root, environment, _ := nativeFixture(t)
			registration := writeProjectCompatibilityRoute(t, root, "")
			ownerRoot := filepath.Join(environmentValue(environment, "SUBYARD_HOME"), "owner-inventory")
			connection := ownerinventory.Connection{HostID: "remote-owner", Destination: "dev@remote.example"}
			connections := ownerinventory.Connections{Root: ownerRoot}
			if err := connections.Write(connection); err != nil {
				t.Fatal(err)
			}
			now := time.Unix(1_000, 0).UTC()
			inventory := inventoryResult(connection.HostID, "build", "Demo").inventory
			if err := (ownerinventory.Cache{Root: ownerRoot}).Write(ownerinventory.Snapshot{FetchedAt: now, Inventory: inventory}); err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			responsePath := filepath.Join(bin, "response")
			writeHostRPCFixture(t, responsePath, inventory)
			physicalCall := filepath.Join(bin, "physical-call")
			writeCLIFile(t, filepath.Join(bin, "ssh"), "#!/bin/sh\n"+trustedSSHMock(t)+
				"case \"$*\" in\n"+
				"  *rpc*--stdio*) cat "+shellquote.Word(responsePath)+" ;;\n"+
				"  *_project-state*check-role*) exit 0 ;;\n"+
				"  *) printf x > "+shellquote.Word(physicalCall)+"; exit 1 ;;\nesac\n", 0o700)
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: append(environment, "SUBYARD_HOST_ID=controller"),
				WorkingDir: root, Clock: testkit.NewManualClock(now), Incus: lifecycleIncus(), Stdout: io.Discard, Stderr: io.Discard})
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := program.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			beforePreparation := nativeTreeSnapshot(t, root)
			program.allOwnerInventoriesReadOnly(ctx, loaded, false)
			route, _, err := program.ownerYardRouteReadOnly(ctx, loaded, connection.HostID, "build")
			if err != nil {
				t.Fatal(err)
			}
			selected, err := program.activateProjectContext(route, loaded, true)
			if err != nil {
				t.Fatal(err)
			}
			execution, err := program.prepareProjectExecution(ctx, selected, command.Definition{Name: "code"}, []string{"Demo"}, true, true)
			if err != nil {
				t.Fatalf("allowed canonical preparation failed: %v", err)
			}
			if execution.OwnerConnection == nil || !reflect.DeepEqual(*execution.OwnerConnection, connection) {
				t.Fatal("preparation captured a synthesized connection instead of the persisted owner")
			}
			if after := nativeTreeSnapshot(t, root); !slices.Equal(beforePreparation, after) {
				t.Fatal("compatibility route preparation changed controller files")
			}
			// Warm the lease's lock files before checking that stale execution does
			// not change project, cache, or routing state.
			capturedOwner := execution.OwnerConnection
			release, err := program.beginProjectMutation(ctx, execution)
			if err != nil {
				t.Fatalf("persisted owner did not acquire its lease: %v", err)
			}
			release()
			if execution.OwnerConnection != capturedOwner {
				t.Fatal("route revalidation rebound the prepared owner snapshot")
			}
			switch change {
			case "changed":
				writeProjectCompatibilityRoute(t, root, "yard-replacement")
			case "removed":
				if err := os.Remove(registration); err != nil {
					t.Fatal(err)
				}
			}
			beforeExecution := nativeTreeSnapshot(t, root)
			called := false
			prepared := &preparedCommand{CLI: program, Project: execution, Plan: domain.OperationPlan{Confirmed: true},
				execute: func(context.Context, *application.Orchestrator, io.Writer) (domain.AdapterResult, error) {
					called = true
					return domain.AdapterResult{Status: "ok"}, nil
				}}
			_, err = prepared.Execute(ctx, &application.Orchestrator{}, io.Discard)
			if change == "unchanged" {
				if err != nil || !called {
					t.Fatalf("unchanged compatibility route was refused: called=%v err=%v", called, err)
				}
			} else if !errors.Is(err, domain.ErrPlanStale) || called {
				t.Fatalf("stale compatibility route reached work: called=%v err=%v", called, err)
			}
			if _, err := os.Lstat(physicalCall); !os.IsNotExist(err) {
				t.Fatalf("compatibility route preparation or revalidation reached physical work: %v", err)
			}
			if after := nativeTreeSnapshot(t, root); !slices.Equal(beforeExecution, after) {
				t.Fatal("compatibility route execution changed controller files")
			}
		})
	}
}

func writeProjectCompatibilityRoute(t *testing.T, root, sshHost string) string {
	t.Helper()
	directory := filepath.Join(root, "state", "yards", "preview-remote")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	contents := "ACCESS_KIND=remote\nOWNER_ENDPOINT=dev@remote.example\nOWNER_YARD_NAME=build\n"
	if sshHost != "" {
		contents += "SSH_HOST=" + sshHost + "\n"
	}
	path := filepath.Join(directory, "config.env")
	writeCLIFile(t, path, contents, 0o600)
	return path
}

func TestProjectRoleRejectsAdmissionBeforeStateMutation(t *testing.T) {
	root, environment, stateDirectory := nativeFixture(t)
	writeDisabledProjectRole(t, root)
	source := filepath.Join(root, "sample-source")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := program.prepareProjectExecution(context.Background(), loaded, command.Definition{Name: "sync"},
		[]string{source}, true, false); err == nil || !strings.Contains(err.Error(), "does not accept work projects") {
		t.Fatalf("project sync reached disabled yard: %v", err)
	}
	for _, test := range []struct {
		owner bool
		args  []string
	}{
		{true, []string{"preview", source, "sync", "sample", "0"}},
		{true, []string{"reserve", "op-test", source, "sync", "sample", "0"}},
		{true, []string{"upsert", "sample-id", "sample", "sync", "yard"}},
		{false, []string{"write", "sample-id", "sample", source, "/srv/workspaces/sample-id/src", "sync", "yard"}},
		{false, []string{"set", "sample-id", "name", "sample"}},
		{false, []string{"upsert-yard", "sample-id", "sample", "sync", "yard", "yard"}},
	} {
		var output bytes.Buffer
		program.options.Stderr = &output
		if code := program.runProjectState(context.Background(), loaded, test.args, test.owner); code != 1 ||
			!strings.Contains(output.String(), "does not accept work projects") {
			t.Fatalf("internal state %v owner=%t code=%d error=%q", test.args, test.owner, code, output.String())
		}
	}
	if _, err := os.Lstat(stateDirectory); !os.IsNotExist(err) {
		t.Fatalf("denied project admission created state: %v", err)
	}
}

func TestProjectRoleDirectInternalEndpointRejectsAdmission(t *testing.T) {
	root, environment, stateDirectory := nativeFixture(t)
	writeDisabledProjectRole(t, root)
	manifest := filepath.Join(root, "config", "commands.registry")
	current, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, manifest, string(current)+"_project-state||@project-state||local|mutate|dynamic|hidden|internal|none|_project-state|owner project state||\n", 0o600)
	for _, arguments := range [][]string{
		{"_project-state", "preview", "/synthetic/source", "sync", "sample", "0"},
		{"_state", "upsert-yard", "sample-id", "sample", "sync", "yard", "yard"},
	} {
		var stderr bytes.Buffer
		program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment,
			Arguments: arguments, Stderr: &stderr})
		if err != nil {
			t.Fatal(err)
		}
		if code := program.Run(context.Background()); code != 1 || !strings.Contains(stderr.String(), "does not accept work projects") {
			t.Errorf("direct state endpoint %v code=%d error=%q", arguments, code, stderr.String())
		}
	}
	if _, err := os.Lstat(stateDirectory); !os.IsNotExist(err) {
		t.Fatalf("direct owner endpoint created project state: %v", err)
	}
}

func TestProjectRoleRejectsExistingProjectCommands(t *testing.T) {
	root, environment, stateDirectory := nativeFixture(t)
	writeDisabledProjectRole(t, root)
	store, err := state.NewFileStore(stateDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), domain.ProjectRecord{
		Schema: 1, ProjectID: "demo-12345678", Name: "Demo", HostPath: "/host/Demo",
		YardPath: "/srv/workspaces/demo-12345678/src", Mode: domain.ProjectSync,
		SSHHost: "yard", Target: "yard",
	}); err != nil {
		t.Fatal(err)
	}
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"shell", "up", "code"} {
		if _, err := program.prepareProjectExecution(context.Background(), loaded, command.Definition{Name: name},
			[]string{"demo-12345678"}, true, false); err == nil || !strings.Contains(err.Error(), "does not accept work projects") {
			t.Errorf("existing project %s reached disabled yard: %v", name, err)
		}
	}
}

func TestProjectRoleRecheckedBeforeMutation(t *testing.T) {
	root, environment, stateDirectory := nativeFixture(t)
	if err := os.MkdirAll(filepath.Join(root, "config", "yards", "profiles"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "state", "yards", "default"), 0o700); err != nil {
		t.Fatal(err)
	}
	preset := filepath.Join(root, "config", "yards", "profiles", "projects.env")
	writeCLIFile(t, preset, "ALLOWS_PROJECTS=true\n", 0o600)
	writeCLIFile(t, filepath.Join(root, "state", "yards", "default", "config.env"), "YARD_TEMPLATE=projects\n", 0o600)
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, preset, "ALLOWS_PROJECTS=false\n", 0o600)
	if _, err := program.beginProjectMutation(context.Background(), &projectExecution{Loaded: loaded, RequiresProjects: true}); err == nil || !strings.Contains(err.Error(), "does not accept work projects") {
		t.Fatalf("project role change was not rechecked: %v", err)
	}
	if _, err := os.Lstat(stateDirectory); !os.IsNotExist(err) {
		t.Fatalf("denied mutation created state: %v", err)
	}
}

func TestCanonicalRemoteProjectRoleRecheckedOnOwner(t *testing.T) {
	for _, name := range []string{"sync", "code"} {
		t.Run(name, func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			// The controller role is unrelated to the selected remote owner's role.
			writeDisabledProjectRole(t, root)
			program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root})
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := program.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			store := ownerinventory.Connections{Root: filepath.Join(loaded.Context.Paths.DataHome, "owner-inventory")}
			connection := ownerinventory.Connection{HostID: "remote-owner", Destination: "owner-alias",
				Yards: map[string]ownerinventory.YardRoute{"build": {SSHHost: "yard-remote"}}}
			if err := store.Write(connection); err != nil {
				t.Fatal(err)
			}
			route, _, err := program.ownerYardRouteReadOnly(context.Background(), loaded, connection.HostID, "build")
			if err != nil {
				t.Fatal(err)
			}
			selected, err := program.activateProjectContext(route, loaded, true)
			if err != nil {
				t.Fatal(err)
			}
			bin := filepath.Join(root, "fake-bin")
			if err := os.MkdirAll(bin, 0o700); err != nil {
				t.Fatal(err)
			}
			calls, denied := filepath.Join(root, "owner-calls"), filepath.Join(root, "owner-denied")
			writeCLIFile(t, filepath.Join(bin, "ssh"), "#!/bin/sh\n"+trustedSSHMock(t)+
				"printf '%s\\n' \"$*\" >> "+shellquote.Word(calls)+"\n"+
				"if [ -f "+shellquote.Word(denied)+" ]; then printf 'selected yard role does not accept work projects\\n' >&2; exit 1; fi\nexit 0\n", 0o700)
			t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
			execution := &projectExecution{Loaded: selected, RequiresProjects: projectCommandRequiresProjects(name)}
			if err := program.captureProjectOwner(context.Background(), execution); err != nil {
				t.Fatal(err)
			}
			release, err := program.beginProjectMutation(context.Background(), execution)
			if err != nil {
				t.Fatalf("canonical %s failed owner role revalidation: %v", name, err)
			}
			release()
			payload, err := os.ReadFile(calls)
			if err != nil || !strings.Contains(string(payload), "check-role") || !strings.Contains(string(payload), "build") {
				t.Fatalf("role check did not select the owner yard: %q err=%v", payload, err)
			}
			before := nativeTreeSnapshot(t, store.Root)
			writeCLIFile(t, denied, "denied\n", 0o600)
			physicalWork := false
			prepared := &preparedCommand{CLI: program, Project: execution, Plan: domain.OperationPlan{Confirmed: true},
				execute: func(context.Context, *application.Orchestrator, io.Writer) (domain.AdapterResult, error) {
					physicalWork = true
					return domain.AdapterResult{Status: "ok"}, nil
				}}
			if _, err := prepared.Execute(context.Background(), &application.Orchestrator{}, io.Discard); err == nil ||
				!strings.Contains(err.Error(), "role on owner") || physicalWork {
				t.Fatalf("owner role change reached %s execution: physical=%v err=%v", name, physicalWork, err)
			}
			if after := nativeTreeSnapshot(t, store.Root); !slices.Equal(before, after) {
				t.Fatalf("role refusal changed controller routing state: before=%v after=%v", before, after)
			}
			if _, err := os.Lstat(selected.Context.Paths.StateDir); !os.IsNotExist(err) {
				t.Fatalf("role refusal created project state: %v", err)
			}
		})
	}
}

func TestOwnerProjectAssessmentDoesNotWriteState(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, test := range []struct {
			name string
			args []string
			deny bool
			code int
		}{
			{"role allowed", []string{"check-role"}, false, 0},
			{"role denied", []string{"check-role"}, true, 1},
			{"role invalid", []string{"check-role", "extra"}, false, 2},
			{"preview allowed", []string{"preview", "/host/New", "sync", "New", "0"}, false, 0},
			{"preview denied", []string{"preview", "/host/New", "sync", "New", "0"}, true, 1},
		} {
			t.Run(test.name+map[bool]string{false: "/absent", true: "/legacy"}[legacy], func(t *testing.T) {
				root, environment, stateDirectory := nativeFixture(t)
				manifest := filepath.Join(root, "config", "commands.registry")
				current, err := os.ReadFile(manifest)
				if err != nil {
					t.Fatal(err)
				}
				writeCLIFile(t, manifest, string(current)+"_project-state||@project-state||local|mutate|dynamic|hidden|internal|none|_project-state|owner project state||\n", 0o600)
				if legacy {
					store, err := state.NewFileStore(stateDirectory)
					if err != nil {
						t.Fatal(err)
					}
					if err := store.Put(context.Background(), domain.ProjectRecord{Schema: 1,
						ProjectID: "legacy-id", Name: "Legacy", HostPath: "/host/Legacy",
						YardPath: state.YardPath("legacy-id"), Mode: domain.ProjectSync, SSHHost: "yard", Target: "yard"}); err != nil {
						t.Fatal(err)
					}
					if err := os.Chmod(filepath.Join(stateDirectory, "legacy-id.json"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				// A read-only endpoint must also bypass release recovery before dispatch.
				runtimeRoot := filepath.Join(root, "runtime")
				environment = append(environment, "YARD_RUNTIME_ROOT="+runtimeRoot,
					"V2_GATE_CAPTURE="+filepath.Join(root, "recovery-called"))
				installUnfinishedV2MutationGateFixture(t, root, environment, runtimeRoot)
				if test.deny {
					writeDisabledProjectRole(t, root)
				}
				var stderr bytes.Buffer
				program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment,
					Arguments: append([]string{"_project-state"}, test.args...), Stderr: &stderr, Stdout: io.Discard})
				if err != nil {
					t.Fatal(err)
				}
				before := nativeTreeSnapshot(t, root)
				if code := program.Run(context.Background()); code != test.code {
					t.Fatalf("owner assessment code=%d want=%d stderr=%q", code, test.code, stderr.String())
				}
				if after := nativeTreeSnapshot(t, root); !slices.Equal(before, after) {
					t.Fatalf("assessment changed state: before=%v after=%v", before, after)
				}
			})
		}
	}
}

func writeDisabledProjectRole(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "state", "yards", "default"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "config", "yards", "profiles"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(root, "config", "yards", "profiles", "no-projects.env"), "ALLOWS_PROJECTS=false\n", 0o600)
	writeCLIFile(t, filepath.Join(root, "state", "yards", "default", "config.env"), "YARD_TEMPLATE=no-projects\n", 0o600)
}
