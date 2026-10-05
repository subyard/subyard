package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerinventory"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/state"
	"github.com/Subyard/Subyard/internal/testkit"
)

// Execute the real tar/diff/readback commands against private native trees.
type exportRouteNativeData struct{ yard domain.Context }

func (data exportRouteNativeData) Execute(ctx context.Context, yard domain.Context, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	return data.Stream(ctx, yard, request, bytes.NewReader(request.Stdin))
}

func (data exportRouteNativeData) Stream(ctx context.Context, yard domain.Context, request ports.InstanceExecRequest, input io.Reader) (ports.InstanceExecResult, error) {
	if yard != data.yard {
		return ports.InstanceExecResult{}, errors.New("export changed the selected controller transport")
	}
	command := exec.CommandContext(ctx, request.Command[0], request.Command[1:]...)
	command.Stdin = input
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	result := ports.InstanceExecResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if command.ProcessState != nil {
		result.ExitCode = command.ProcessState.ExitCode()
	}
	return result, err
}

func TestProjectExportSameHostRemoteAliasRetainsNativeRoute(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	configHome := environmentValue(environment, "SUBYARD_CONFIG_HOME")
	if err := os.MkdirAll(filepath.Join(configHome, "yards", "remote"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(configHome, "yards", "remote", "config.env"), []byte("ACCESS_KIND=remote\nOWNER_ENDPOINT=dev@owner.example\nOWNER_YARD_NAME=owner-only\nSSH_HOST=owner-data\n"), 0o600)
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: append(environment, "SUBYARD_HOST_ID=shared-host"), WorkingDir: root})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("remote")
	if err != nil {
		t.Fatal(err)
	}
	program.env["SUBYARD_YARD_EXPLICIT"], program.env["SUBYARD_YARD"] = "1", "remote"
	if _, err := program.loadContext("owner-only"); err == nil {
		t.Fatal("controller unexpectedly contains the owner yard registration")
	}
	connections := ownerinventory.Connections{Root: filepath.Join(loaded.Context.Paths.DataHome, "owner-inventory")}
	registration, exists, err := program.remoteControl(loaded, 0).Lookup(context.Background(), "remote")
	if err != nil || !exists || registration.SSHHost != loaded.Context.SSHHost {
		t.Fatalf("native lookup lost the registered data-plane authority: %v", err)
	}
	registrations, err := program.remoteControl(loaded, 0).List(context.Background())
	if err != nil || len(registrations) != 1 || registrations[0] != registration {
		t.Fatalf("native list lost the registered data-plane authority: %v", err)
	}
	connection := ownerinventory.Connection{HostID: "shared-host", Destination: loaded.Context.OwnerEndpoint}
	if changed, err := mergeLegacyRoutes(&connection, registrations); err != nil || !changed || connection.Yards["owner-only"].SSHHost != loaded.Context.SSHHost {
		t.Fatalf("native registration projected a different route: changed=%v err=%v", changed, err)
	}
	if err := connections.Write(connection); err != nil {
		t.Fatal(err)
	}
	remote := inventoryResult("shared-host", "owner-only", "Demo")
	match, err := program.resolveOwnerProjectFromInventories(context.Background(), loaded, "Demo", true, true, []ownerInventoryResult{inventoryResult("shared-host", "default", ""), remote})
	if err != nil || match.Yard != "remote" {
		t.Fatalf("owner-only yard resolved as controller local config: route=%q err=%v", match.Yard, err)
	}
	selected, err := program.activateProjectContext(match.Yard, loaded, false)
	if err != nil || selected.Context != loaded.Context {
		t.Fatalf("selected owner transport changed: %v", err)
	}
	if name, value, err := program.ownerYardRoute(context.Background(), loaded, "shared-host", "owner-only"); err != nil || name != "remote" || value != selected.Context {
		t.Fatalf("write route changed selected owner transport: name=%q err=%v", name, err)
	}
	source, guest := testkit.TempDir(t), testkit.TempDir(t)
	testkit.WriteFile(t, filepath.Join(source, "content.txt"), []byte("controller baseline\n"), 0o600)
	testkit.WriteFile(t, filepath.Join(guest, "content.txt"), []byte("native guest change\n"), 0o600)
	canonical := match.Record
	canonical.HostPath, canonical.SourceKey, canonical.ImportedAt = source, state.SourceKey(source), "2026-10-04T00:00:00Z"
	canonical.SSHHost = loaded.Context.SSHHost
	match.Record.SourceKey = canonical.SourceKey
	project := &projectExecution{Loaded: selected, Record: match.Record}
	if err := hydrateExportProject(project, canonical); err != nil {
		t.Fatal(err)
	}
	// The host-free data plane represents the managed guest with a private tree.
	project.Record.YardPath = guest
	canonical = project.Record
	if err := project.prepareSource(context.Background(), program); err != nil {
		t.Fatal(err)
	}
	defer project.closePreparedSource()
	archive, err := project.preparedArchive.Open(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := application.ProjectArchiveTreeDigest(archive)
	if err = errors.Join(err, archive.Close()); err != nil {
		t.Fatal(err)
	}
	data := exportRouteNativeData{yard: selected.Context}
	observed, err := application.ObserveProjectTree(context.Background(), data, selected.Context, guest)
	if err != nil {
		t.Fatal(err)
	}
	project.exportSourceTree, project.exportObservedTree, project.ActionChanged = tree, observed, true
	if err := project.prepareProjectExportDestination(context.Background(), program); err != nil {
		t.Fatal(err)
	}
	if err := domain.ValidateOperationSteps(project.exportOperationSteps()); err != nil {
		t.Fatal(err)
	}
	runner := application.ProjectActionRunner{Data: data, PreparedArchive: project.preparedArchive, Project: canonical, Yard: selected.Context, ExportSourceTree: tree, ExportObservedTree: observed, Exports: project.controllerExport.store}
	operationID := "same-host-native-export-" + state.SourceKey(source)[:16]
	draft, err := runner.PrepareExport(context.Background(), operationID, "")
	if err != nil {
		t.Fatal(err)
	}
	defer draft.Close()
	path, err := runner.PublishExport(context.Background(), draft)
	if err != nil {
		t.Fatal(err)
	}
	patch, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(patch), "+native guest change") || !strings.Contains(string(patch), "--- a/content.txt") {
		t.Fatalf("native export patch mismatch: %v", err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("native export destination lost its protected mode")
	}
	connection.Yards["owner-only"] = ownerinventory.YardRoute{SSHHost: "foreign-data"}
	if err := connections.Write(connection); err != nil {
		t.Fatal(err)
	}
	if _, _, err := program.ownerYardRouteReadOnly(context.Background(), loaded, "shared-host", "owner-only"); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("same-host remote route conflict bypassed: %v", err)
	}
	localProgram, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: append(environment, "SUBYARD_HOST_ID=shared-host"), WorkingDir: root})
	if err != nil {
		t.Fatal(err)
	}
	local, err := localProgram.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	// A genuine local selection must never inspect unrelated remote connections.
	local.Context.Paths.DataHome = testkit.TempDir(t)
	testkit.WriteFile(t, filepath.Join(local.Context.Paths.DataHome, "owner-inventory"), []byte("not a directory"), 0o600)
	if name, value, err := localProgram.ownerYardRouteReadOnly(context.Background(), local, "shared-host", "default"); err != nil || name != "default" || value.AccessKind != domain.AccessLocal {
		t.Fatalf("genuine local route changed: name=%q err=%v", name, err)
	}
}

var _ ports.YardExecutor = exportRouteNativeData{}

func TestLegacyRouteProjectionDefaultsAndConflicts(t *testing.T) {
	ctx := context.Background()
	root, environment, _ := nativeFixture(t)
	configHome := environmentValue(environment, "SUBYARD_CONFIG_HOME")
	if err := os.MkdirAll(filepath.Join(configHome, "yards", "remote"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(configHome, "yards", "remote", "config.env")
	testkit.WriteFile(t, path, []byte("ACCESS_KIND=remote\nOWNER_ENDPOINT=owner.example\nOWNER_YARD_NAME=inner\n"), 0o600)
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	registrations, err := program.remoteControl(loaded, 0).List(ctx)
	if err != nil || len(registrations) != 1 {
		t.Fatalf("native default registration unavailable: %v", err)
	}
	connection := ownerinventory.Connection{HostID: "owner", Destination: "owner.example"}
	if _, err := mergeLegacyRoutes(&connection, registrations); err != nil || connection.Yards["inner"].SSHHost != "yard-remote" {
		t.Fatalf("missing-only default route fallback changed: %v", err)
	}
	testkit.WriteFile(t, path, []byte("ACCESS_KIND=remote\nOWNER_ENDPOINT=owner.example\nOWNER_YARD_NAME=inner\nSSH_HOST=custom-data\n"), 0o600)
	registrations, err = program.remoteControl(loaded, 0).List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mergeLegacyRoutes(&connection, registrations); err == nil || connection.Yards["inner"].SSHHost != "yard-remote" {
		t.Fatal("changed registered route silently overwrote persisted authority")
	}
	registrations[0].SSHHost = "-unsafe"
	if _, err := mergeLegacyRoutes(&connection, registrations); err == nil {
		t.Fatal("unsafe legacy record data plane accepted")
	}
	registrations[0].SSHHost = "custom-data"
	second := registrations[0]
	second.Spec.LegacyAlias, second.SSHHost = "other", "foreign-data"
	fresh := ownerinventory.Connection{HostID: "owner", Destination: "owner.example"}
	if _, err := mergeLegacyRoutes(&fresh, append(registrations, second)); err == nil {
		t.Fatal("conflicting native alias authorities merged")
	}
}
