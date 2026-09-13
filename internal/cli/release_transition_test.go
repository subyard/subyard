package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/testyardmigration"
)

func TestCandidateProcessRejectsUnsafeSourceIngressDescriptor(t *testing.T) {
	home := t.TempDir()
	valid := releasetransition.SourceIngressRequest{
		SchemaVersion: releasetransition.SourceIngressRequestSchemaV1,
		Kind:          releasetransition.SourceIngressPreGoV1,
		SourceRoot:    filepath.Join(home, "source"),
		DataHome:      filepath.Join(home, ".subyard"),
		BinDir:        filepath.Join(home, ".local", "bin"),
		RC:            filepath.Join(home, ".bashrc"),
		LoginRC:       filepath.Join(home, ".profile"),
	}
	tests := map[string]func(*releasetransition.SourceIngressRequest){
		"unknown schema":  func(value *releasetransition.SourceIngressRequest) { value.SchemaVersion++ },
		"unknown kind":    func(value *releasetransition.SourceIngressRequest) { value.Kind = "arbitrary" },
		"relative source": func(value *releasetransition.SourceIngressRequest) { value.SourceRoot = "source" },
		"filesystem root": func(value *releasetransition.SourceIngressRequest) { value.DataHome = "/" },
		"unbounded path": func(value *releasetransition.SourceIngressRequest) {
			value.SourceRoot = filepath.Join(home, strings.Repeat("x", 4096))
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			descriptor := valid
			mutate(&descriptor)
			request := releasetransition.ProcessRequest{
				SchemaVersion:  releasetransition.ProcessProtocolSchemaV1,
				Mode:           releasetransition.ProcessInspect,
				RuntimeRoot:    filepath.Join(home, "runtime"),
				ConfigHome:     filepath.Join(home, ".config", "subyard"),
				Target:         "release-a",
				Direction:      releasetransition.DirectionActivateTarget,
				ArtifactDigest: releasetransition.Fingerprint(strings.Repeat("a", 64)),
				SourceIngress:  &descriptor,
			}
			_, err := executeReleaseTransitionRequest(
				context.Background(), t.TempDir(), request, nil, nil, nil, nil,
			)
			if err == nil || !strings.Contains(err.Error(), "source ingress") {
				t.Fatalf("unsafe source descriptor error = %v", err)
			}
		})
	}

	customRoles := valid
	customRoles.BinDir = "/var/tmp/subyard-bin"
	customRoles.LoginRC = filepath.Join(home, ".config", "shell", "profile")
	if err := customRoles.Validate(); err != nil {
		t.Fatalf("syntactically safe custom source roles were rejected before OS-home validation: %v", err)
	}

	payload, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatal(err)
	}
	wantFields := []string{
		"schemaVersion", "kind", "sourceRoot", "dataHome", "binDir", "rc", "loginRC",
	}
	slices.Sort(wantFields)
	actualFields := make([]string, 0, len(fields))
	for field := range fields {
		actualFields = append(actualFields, field)
	}
	slices.Sort(actualFields)
	if !slices.Equal(actualFields, wantFields) {
		t.Fatalf("source descriptor fields = %v, want %v", actualFields, wantFields)
	}
}

func TestCandidateTransitionOptionsCarryTrustedRecoveryInputs(t *testing.T) {
	replacement := &releasetransition.JournalReplacement{
		Transaction:   "tx-source-v0111",
		Fingerprint:   releasetransition.Fingerprint(strings.Repeat("a", 64)),
		Reason:        releasetransition.JournalReplacementPostActivationScopeV0111,
		SourceVersion: "0.11.1",
	}
	request := releasetransition.ProcessRequest{Replacement: replacement}
	options := candidateTransitionOptions(request, releasetransition.V2Options{})
	if options.CandidateVersion != Version || options.Replacement == nil ||
		*options.Replacement != *replacement {
		t.Fatalf("candidate transition options = %#v", options)
	}
}

func TestCandidateProcessRejectsRegistryOutsideVerifiedManifest(t *testing.T) {
	repositoryRoot := t.TempDir()
	registry := []byte("{}\n")
	writeReleaseTransitionTestFile(t,
		filepath.Join(repositoryRoot, "config", "release-transition.json"), registry, 0o600,
	)
	digest := sha256.Sum256(registry)
	request := releasetransition.ProcessRequest{
		SchemaVersion:  releasetransition.ProcessProtocolSchemaV1,
		Mode:           releasetransition.ProcessInspect,
		RuntimeRoot:    t.TempDir(),
		ConfigHome:     t.TempDir(),
		RegistryDigest: releasetransition.Fingerprint(fmt.Sprintf("%x", digest)),
	}
	writeReleaseTransitionTestFile(t,
		filepath.Join(repositoryRoot, "config", "release-transition.json"),
		append(registry, '\n'), 0o600,
	)

	_, err := executeReleaseTransitionRequest(
		context.Background(), repositoryRoot, request, nil, nil, nil, nil,
	)
	if err == nil || !strings.Contains(err.Error(), "verified release manifest") {
		t.Fatalf("registry binding error = %v", err)
	}
}

func TestReleaseTransitionIngressSkipsUnboundLegacyLeaf(t *testing.T) {
	home := t.TempDir()
	program, err := New(Options{
		RepositoryRoot: repositoryRoot(t),
		Environment:    []string{"HOME=" + home},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := releasetransition.ProcessRequest{
		RuntimeRoot: filepath.Join(home, ".subyard", "runtime"),
		ConfigHome:  filepath.Join(home, ".config", "subyard"),
	}
	previous := releasetransition.ReleaseID("release-previous")
	ingress, err := program.releaseTransitionIngressFactory(request)(
		releasetransition.ReleasePair{
			From: "release-current", Previous: &previous, Target: "release-target",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ingress.Inspect(context.Background(), &releasetransition.V2IngressBinding{
		Steps: []releasetransition.V2IngressStepBinding{{
			Kind: releasetransition.V2SourceImport,
		}},
	})
	if err != nil {
		t.Fatalf("dormant legacy ingress blocked a non-legacy resume: %v", err)
	}
}

func TestRouteConsumerActivationReconcilerConvergesForwardAfterLegacyOwner(t *testing.T) {
	port := &routeConsumerActivationFixture{
		owner:     string(testyardmigration.StateLegacyDirectory),
		before:    `{"schemaVersion":1,"active":true,"consumers":[]}`,
		verifyErr: errors.New("route device is absent"),
	}
	reconciler := &routeConsumerActivationReconciler{port: port}
	releases := releasetransition.ReleasePair{From: "release-a", Target: "release-b"}
	links := releasetransition.ReleaseLinks{Active: "release-a"}
	legacy, err := reconciler.Observe(context.Background(), releases, links)
	if err != nil || legacy.Converged || port.commits != 0 {
		t.Fatalf("legacy route observation = %#v port=%#v err=%v", legacy, port, err)
	}

	port.owner = string(testyardmigration.StateCurrent)
	drift, err := reconciler.Observe(context.Background(), releases, links)
	if err != nil || drift.Converged {
		t.Fatalf("route drift observation = %#v err=%v", drift, err)
	}
	if err := reconciler.Reconcile(context.Background(), links); err != nil {
		t.Fatal(err)
	}
	ready, err := reconciler.Observe(context.Background(), releases, links)
	if err != nil || !ready.Converged || port.commits != 1 {
		t.Fatalf("reconciled route observation = %#v port=%#v err=%v", ready, port, err)
	}
}

func TestRouteConsumerActivationObservesRegisteredBackendWithoutRecreatingIt(t *testing.T) {
	owner := `{"name":"yard-test-yard","project":"subyard-test-yard","status":"RUNNING","config":{"user.subyard.managed":"true","user.subyard.name":"test-yard"}}`
	for _, test := range []struct {
		name, projects, instances string
		absent, rejected          bool
	}{
		{name: "removed", projects: `[]`, instances: `[]`, absent: true},
		{name: "default-remains", projects: `[{"name":"subyard"}]`, instances: `[{"project":"subyard","name":"yard","status":"RUNNING","config":{"user.subyard.managed":"true","user.subyard.name":"default"}}]`, absent: true},
		{name: "keep-data", projects: `[{"name":"subyard-test-yard"}]`, instances: `[]`, absent: true},
		{name: "running", projects: `[{"name":"subyard-test-yard"}]`, instances: `[` + owner + `]`},
		{name: "stopped", projects: `[{"name":"subyard-test-yard"}]`, instances: `[` + strings.ReplaceAll(owner, "RUNNING", "STOPPED") + `]`},
		{name: "foreign", projects: `[{"name":"subyard-test-yard"}]`, instances: `[` + strings.ReplaceAll(owner, `"true"`, `"false"`) + `]`, rejected: true},
		{name: "wrong-name", projects: `[{"name":"subyard-test-yard"}]`, instances: `[` + strings.ReplaceAll(owner, `"yard-test-yard"`, `"foreign"`) + `]`, rejected: true},
		{name: "duplicate", projects: `[{"name":"subyard-test-yard"}]`, instances: `[` + owner + `,` + owner + `]`, rejected: true},
		{name: "conflict", projects: `[{"name":"subyard-test-yard"},{"name":"subyard-e2e-yard"}]`, instances: `[]`, rejected: true},
		{name: "orphan", projects: `[]`, instances: `[` + owner + `]`, rejected: true},
		{name: "malformed", projects: `[]`, instances: `{`, rejected: true},
		{name: "null", projects: `[]`, instances: `null`, rejected: true},
		{name: "null-projects", projects: `null`, instances: `[]`, rejected: true},
		{name: "incomplete-projects", projects: `[{}]`, instances: `[]`, rejected: true},
		{name: "incomplete", projects: `[]`, instances: `[{}]`, rejected: true},
		{name: "null-row", projects: `[]`, instances: `[null]`, rejected: true},
		{name: "command-failure", projects: `[]`, instances: `command-failure`, rejected: true},
		{name: "unknown-state", projects: `[{"name":"subyard-test-yard"}]`, instances: `[` + strings.ReplaceAll(owner, "RUNNING", "BROKEN") + `]`, rejected: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			configHome := filepath.Join(root, "config")
			registration := filepath.Join(configHome, "yards", "test-yard", "config.env")
			if err := os.MkdirAll(filepath.Dir(registration), 0o700); err != nil {
				t.Fatal(err)
			}
			for path, contents := range map[string]string{
				registration:                          "YARD_TEMPLATE=test-vms\n",
				filepath.Join(root, "projects.json"):  test.projects,
				filepath.Join(root, "instances.json"): test.instances,
			} {
				if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			incus := filepath.Join(root, "incus")
			if err := os.WriteFile(incus, []byte(`#!/bin/sh
set -eu
root=${0%/*}
case "$*" in
  'project list --format=json') cat "$root/projects.json" ;;
  'project get subyard-test-yard features.images') printf 'false\n' ;;
  'list --all-projects --format=json')
    [ "$(cat "$root/instances.json")" != command-failure ] || exit 1
    cat "$root/instances.json" ;;
  *) exit 2 ;;
esac
`), 0o700); err != nil {
				t.Fatal(err)
			}
			mutations := 0
			options := testyardmigration.Options{
				ConfigHome: configHome, DataHome: filepath.Join(root, "data"), Incus: incus, Executable: incus,
				RunYard: func(context.Context, string, io.Writer, ...string) error {
					mutations++
					return errors.New("unexpected owner mutation")
				},
			}
			if test.name == "removed" {
				t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
				options.Incus = ""
			}
			reconciler := &routeConsumerActivationReconciler{port: &testYardRouteConsumerActivation{options: options}}
			pair := releasetransition.ReleasePair{From: "release-b", Target: "release-b"}
			links := releasetransition.ReleaseLinks{Active: "release-b"}
			observation, err := reconciler.Observe(context.Background(), pair, links)
			if test.rejected {
				if err == nil {
					t.Fatalf("unsafe backend accepted: %#v", observation)
				}
				return
			}
			if err != nil || observation.Converged != test.absent {
				t.Fatalf("activation observation = %#v, %v", observation, err)
			}
			if test.absent {
				if err := reconciler.Reconcile(context.Background(), links); err != nil {
					t.Fatal(err)
				}
				before, err := testyardmigration.PrepareRouteConsumers(context.Background(), options)
				if err != nil || testyardmigration.VerifyRouteConsumers(context.Background(), options, before) == nil {
					t.Fatalf("one-time migration accepted absent backend: %s, %v", before, err)
				}
			}
			if mutations != 0 {
				t.Fatalf("activation replayed owner or broker mutation %d times", mutations)
			}
		})
	}
}

type routeConsumerActivationFixture struct {
	owner     string
	before    string
	verifyErr error
	commits   int
}

func (fixture *routeConsumerActivationFixture) PrepareOwner(context.Context) (string, error) {
	return fixture.owner, nil
}

func (fixture *routeConsumerActivationFixture) Prepare(context.Context) (string, error) {
	return fixture.before, nil
}

func (fixture *routeConsumerActivationFixture) Verify(context.Context, string) error {
	return fixture.verifyErr
}

func (fixture *routeConsumerActivationFixture) Commit(context.Context, string) error {
	fixture.commits++
	fixture.verifyErr = nil
	return nil
}

func TestCandidateReleaseTransitionProtocolInspectsThenConverges(t *testing.T) {
	repositoryRoot := t.TempDir()
	registry, err := os.ReadFile(filepath.Join("..", "..", "config", "release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeReleaseTransitionTestFile(t,
		filepath.Join(repositoryRoot, "config", "release-transition.json"), registry, 0o600,
	)
	runtimeRoot := filepath.Join(t.TempDir(), "runtime")
	for _, release := range []string{"0.8.0-aaaaaaaaaaaa", "release-b"} {
		if err := os.MkdirAll(filepath.Join(runtimeRoot, "releases", release), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("releases/0.8.0-aaaaaaaaaaaa", filepath.Join(runtimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
	configHome := filepath.Join(t.TempDir(), "config")
	settingsPath := filepath.Join(configHome, "yards", "hermes", "config.env")
	writeReleaseTransitionTestFile(t, settingsPath,
		[]byte("YARD_TEMPLATE=e2e-vms\nNESTED_E2E_VMS=0\nSSH_PORT=2224\n"), 0o600,
	)
	request := releasetransition.ProcessRequest{
		SchemaVersion: releasetransition.ProcessProtocolSchemaV1,
		Mode:          releasetransition.ProcessInspect,
		RuntimeRoot:   runtimeRoot, ConfigHome: configHome,
		Target: "release-b", Direction: releasetransition.DirectionActivateTarget,
		ArtifactDigest: releasetransition.Fingerprint(strings.Repeat("a", 64)),
	}
	grant := releasetransition.Authorization("confirmed-operation-grant")
	verify := func(_ releasetransition.PlanToken, authorization releasetransition.Authorization) bool {
		return authorization == grant
	}
	owner := releaseTransitionOwnerFixture{}
	inspected, err := executeReleaseTransitionRequest(
		context.Background(), repositoryRoot, request, verify, nil, owner, nil,
	)
	if err != nil || inspected.Inspection == nil || !inspected.Inspection.Assessment.Changed ||
		len(inspected.Inspection.Decisions) != 2 {
		t.Fatalf("inspection = %#v, err=%v", inspected, err)
	}
	request.Mode = releasetransition.ProcessConverge
	request.Execution = &releasetransition.Execution{
		Plan: inspected.Inspection.Plan, Authorization: grant,
	}
	converged, err := executeReleaseTransitionRequest(
		context.Background(), repositoryRoot, request, verify, nil, owner, nil,
	)
	if err != nil || converged.Outcome == nil ||
		converged.Outcome.Status != releasetransition.StatusReady {
		t.Fatalf("outcome = %#v, err=%v", converged, err)
	}
	links, err := releasetransition.NewRuntimeLinkStore(runtimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	observed, err := links.Observe()
	if err != nil || observed.Active != "release-b" || observed.Previous == nil ||
		*observed.Previous != "0.8.0-aaaaaaaaaaaa" {
		t.Fatalf("links = %#v, err=%v", observed, err)
	}
	request.Mode = releasetransition.ProcessInspect
	request.Execution = nil
	repeated, err := executeReleaseTransitionRequest(
		context.Background(), repositoryRoot, request, verify, nil, owner, nil,
	)
	if err != nil || repeated.Inspection == nil || repeated.Inspection.Outcome == nil ||
		repeated.Inspection.Outcome.Status != releasetransition.StatusReady ||
		repeated.Inspection.Outcome.Target != "release-b" ||
		repeated.Inspection.Outcome.Transaction == nil ||
		converged.Outcome.Transaction == nil ||
		*repeated.Inspection.Outcome.Transaction != *converged.Outcome.Transaction {
		repeatedTransaction := "none"
		if repeated.Inspection != nil && repeated.Inspection.Outcome != nil &&
			repeated.Inspection.Outcome.Transaction != nil {
			repeatedTransaction = string(*repeated.Inspection.Outcome.Transaction)
		}
		convergedTransaction := "none"
		if converged.Outcome != nil && converged.Outcome.Transaction != nil {
			convergedTransaction = string(*converged.Outcome.Transaction)
		}
		t.Fatalf("completed transition transactions: inspected=%s converged=%s err=%v",
			repeatedTransaction, convergedTransaction, err)
	}
	settings, err := os.ReadFile(settingsPath)
	if err != nil || !strings.Contains(string(settings), "YARD_TEMPLATE='test-vms'") ||
		strings.Contains(string(settings), "NESTED_E2E_VMS") {
		t.Fatalf("settings = %q, err=%v", settings, err)
	}

	request.Mode = releasetransition.ProcessInspect
	request.Target = "0.8.0-aaaaaaaaaaaa"
	request.Direction = releasetransition.DirectionActivatePrevious
	request.ArtifactDigest = writeSealedRollbackTarget(t, runtimeRoot, string(request.Target), "0.8.0", registry)
	request.Yard = "default"
	wire, err := json.Marshal(request)
	if err != nil || bytes.Contains(wire, []byte("rollbackTarget")) {
		t.Fatalf("rollback request extended frozen wire: %s, %v", wire, err)
	}
	if err := json.Unmarshal(wire, &request); err != nil {
		t.Fatal(err)
	}
	request.Execution = nil
	reconciler := &processDriftReconciler{failOnce: true}
	reconcilers := []releasetransition.V2ActivationReconciler{reconciler}
	inspected, err = executeReleaseTransitionRequest(
		context.Background(), repositoryRoot, request, verify, reconcilers, owner, nil,
	)
	if err != nil || inspected.Inspection == nil || !inspected.Inspection.Assessment.Changed {
		t.Fatalf("rollback inspection = %#v, err=%v", inspected, err)
	}
	request.Mode = releasetransition.ProcessConverge
	request.Execution = &releasetransition.Execution{
		Plan: inspected.Inspection.Plan, Authorization: grant,
	}
	converged, err = executeReleaseTransitionRequest(
		context.Background(), repositoryRoot, request, verify, reconcilers, owner, nil,
	)
	if err != nil || converged.Outcome == nil || converged.Outcome.Status != releasetransition.StatusRecovering {
		t.Fatalf("interrupted rollback outcome = %#v, err=%v", converged, err)
	}
	transaction := converged.Outcome.Transaction
	request.Mode = releasetransition.ProcessInspect
	request.Execution = nil
	store, err := releasetransition.NewPOSIXV2Store(configHome)
	if err != nil {
		t.Fatal(err)
	}
	pendingJournal, err := store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	pendingLedger, err := store.ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	enginePath := filepath.Join(runtimeRoot, "releases", string(request.Target), "bin", "yard-engine")
	engine, err := os.ReadFile(enginePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{"changed", "missing"} {
		if mutation == "changed" {
			err = os.WriteFile(enginePath, []byte("changed"), 0o700)
		} else {
			err = os.Remove(enginePath)
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := executeReleaseTransitionRequest(context.Background(), repositoryRoot, request,
			verify, reconcilers, owner, nil); err == nil {
			t.Fatalf("pending rollback accepted %s sealed target", mutation)
		}
		afterJournal, journalErr := store.ReadCurrentJournal()
		afterLedger, ledgerErr := store.ReadLedger()
		afterLinks, linkErr := links.Observe()
		if journalErr != nil || ledgerErr != nil || linkErr != nil ||
			!bytes.Equal(pendingJournal.Payload, afterJournal.Payload) ||
			!bytes.Equal(pendingLedger.Payload, afterLedger.Payload) ||
			afterLinks.Active != request.Target || afterLinks.Previous == nil || *afterLinks.Previous != "release-b" {
			t.Fatal("failed pending rollback verification changed protected state or links")
		}
		if err := os.WriteFile(enginePath, engine, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	resume, err := executeReleaseTransitionRequest(context.Background(), repositoryRoot, request, verify, reconcilers, owner, nil)
	if err != nil || resume.Inspection == nil || resume.Inspection.Resume == nil ||
		transaction == nil || *resume.Inspection.Resume != *transaction {
		t.Fatalf("rollback resume = %#v, err=%v", resume, err)
	}
	request.Mode = releasetransition.ProcessConverge
	request.Execution = &releasetransition.Execution{Plan: resume.Inspection.Plan}
	wire, err = json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(wire, &request); err != nil {
		t.Fatal(err)
	}
	converged, err = executeReleaseTransitionRequest(context.Background(), repositoryRoot, request,
		func(releasetransition.PlanToken, releasetransition.Authorization) bool {
			t.Fatal("rollback resume requested fresh authorization")
			return false
		}, reconcilers, owner, nil)
	if err != nil || converged.Outcome == nil || converged.Outcome.Status != releasetransition.StatusReady ||
		converged.Outcome.Transaction == nil || *converged.Outcome.Transaction != *transaction {
		t.Fatalf("rollback outcome = %#v, err=%v", converged, err)
	}
	observed, err = links.Observe()
	if err != nil || observed.Active != "0.8.0-aaaaaaaaaaaa" || observed.Previous == nil ||
		*observed.Previous != "release-b" {
		t.Fatalf("rollback links = %#v, err=%v", observed, err)
	}
}

func TestCandidateProcessBlocksLegacyRollbackBelowCompiledHorizon(t *testing.T) {
	repositoryRoot := t.TempDir()
	registryPayload, err := os.ReadFile(filepath.Join("..", "..", "config", "release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeReleaseTransitionTestFile(t,
		filepath.Join(repositoryRoot, "config", "release-transition.json"), registryPayload, 0o600,
	)
	registry, registryDigest, err := releasetransition.ParseRegistryV2(
		registryPayload, releasetransition.BuiltinCapabilityCatalog(),
	)
	if err != nil {
		t.Fatal(err)
	}
	configHome := filepath.Join(t.TempDir(), "config")
	if err := os.MkdirAll(configHome, 0o700); err != nil {
		t.Fatal(err)
	}
	ledger := releasetransition.BaselineLedgerV2(registry)
	for _, migration := range registry.Migrations {
		ledger, err = ledger.Advance(registry, migration)
		if err != nil {
			t.Fatal(err)
		}
	}
	ledgerPayload, _, err := releasetransition.MarshalLedgerV2(ledger, registry)
	if err != nil {
		t.Fatal(err)
	}
	store, err := releasetransition.NewPOSIXV2Store(configHome)
	if err != nil {
		t.Fatal(err)
	}
	missing, err := store.ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapLedger(missing, ledgerPayload); err != nil {
		t.Fatal(err)
	}
	runtimeRoot := filepath.Join(t.TempDir(), "runtime")
	for _, release := range []string{"0.7.9-aaaaaaaaaaaa", "release-b"} {
		if err := os.MkdirAll(filepath.Join(runtimeRoot, "releases", release), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("releases/release-b", filepath.Join(runtimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/0.7.9-aaaaaaaaaaaa", filepath.Join(runtimeRoot, "previous")); err != nil {
		t.Fatal(err)
	}
	authorizations := 0
	reconciler := &processDriftReconciler{}
	request := releasetransition.ProcessRequest{
		SchemaVersion: releasetransition.ProcessProtocolSchemaV1,
		Mode:          releasetransition.ProcessInspect, RuntimeRoot: runtimeRoot, ConfigHome: configHome,
		Target: "0.7.9-aaaaaaaaaaaa", Direction: releasetransition.DirectionActivatePrevious,
		ArtifactDigest: releasetransition.Fingerprint(strings.Repeat("a", 64)),
		RegistryDigest: registryDigest,
	}
	request.ArtifactDigest = writeSealedRollbackTarget(t, runtimeRoot, string(request.Target), "0.7.9", nil)
	response, err := executeReleaseTransitionRequest(
		context.Background(), repositoryRoot, request,
		func(releasetransition.PlanToken, releasetransition.Authorization) bool {
			authorizations++
			return true
		},
		[]releasetransition.V2ActivationReconciler{reconciler},
		releaseTransitionOwnerFixture{}, nil,
	)
	if err != nil || response.Inspection != nil || response.Outcome == nil ||
		response.Outcome.Status != releasetransition.StatusOperatorActionRequired ||
		response.Outcome.Code != releasetransition.CodeRollbackIncompatible ||
		authorizations != 0 || reconciler.observes != 0 || reconciler.reconciles != 0 {
		t.Fatalf("rollback horizon response=%#v authorizations=%d reconciler=%#v err=%v",
			response, authorizations, reconciler, err)
	}
	wire, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("blocked rollback cannot cross frozen V1: %v", err)
	}
	var decoded releasetransition.ProcessResponse
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Inspection != nil || decoded.Outcome == nil || decoded.Outcome.Code != releasetransition.CodeRollbackIncompatible {
		t.Fatalf("blocked rollback lost V1 outcome: %#v", decoded)
	}
	links, err := releasetransition.NewRuntimeLinkStore(runtimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	observed, err := links.Observe()
	if err != nil || observed.Active != "release-b" || observed.Previous == nil ||
		*observed.Previous != "0.7.9-aaaaaaaaaaaa" {
		t.Fatalf("blocked rollback changed links: %#v err=%v", observed, err)
	}
	after, err := store.ReadLedger()
	journal, journalErr := store.ReadCurrentJournal()
	if err != nil || !bytes.Equal(after.Payload, ledgerPayload) || journalErr != nil || journal.Exists {
		t.Fatalf("blocked rollback changed protected state: ledger=%q journal=%#v err=%v journalErr=%v",
			after.Payload, journal, err, journalErr)
	}
}

func TestCandidateProcessCannotRollbackWithoutSealedTarget(t *testing.T) {
	request := releasetransition.ProcessRequest{
		SchemaVersion: releasetransition.ProcessProtocolSchemaV1,
		Mode:          releasetransition.ProcessInspect, Direction: releasetransition.DirectionActivatePrevious,
		RuntimeRoot: t.TempDir(), ConfigHome: t.TempDir(),
		Target: "0.8.0-aaaaaaaaaaaa", ArtifactDigest: releasetransition.Fingerprint(strings.Repeat("a", 64)),
	}
	if _, err := executeReleaseTransitionRequest(context.Background(), t.TempDir(), request,
		func(releasetransition.PlanToken, releasetransition.Authorization) bool {
			t.Fatal("missing sealed target reached authorization")
			return false
		}, nil, nil, nil,
	); err == nil || !strings.Contains(err.Error(), "rollback target") {
		t.Fatalf("missing sealed rollback target error = %v", err)
	}
}

func writeSealedRollbackTarget(t *testing.T, runtimeRoot, target, version string, registry []byte) releasetransition.Fingerprint {
	t.Helper()
	root := filepath.Join(runtimeRoot, "releases", target)
	engine := []byte("#!/bin/sh\nprintf 'yard-engine " + version + "\\n'\n")
	writeReleaseTransitionTestFile(t, filepath.Join(root, "bin", "yard-engine"), engine, 0o700)
	manifest := fmt.Sprintf("%x  ./bin/yard-engine\n", sha256.Sum256(engine))
	if registry != nil {
		writeReleaseTransitionTestFile(t, filepath.Join(root, "config", "release-transition.json"), registry, 0o600)
		manifest += fmt.Sprintf("%x  ./config/release-transition.json\n", sha256.Sum256(registry))
	}
	writeReleaseTransitionTestFile(t, filepath.Join(root, "runtime-files.sha256"), []byte(manifest), 0o600)
	return releasetransition.Fingerprint(fmt.Sprintf("%x", sha256.Sum256([]byte(manifest))))
}

func TestOwnerOverrideDriftBetweenInspectAndConvergeIsPlanStale(t *testing.T) {
	repositoryRoot := t.TempDir()
	registry, err := os.ReadFile(filepath.Join("..", "..", "config", "release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeReleaseTransitionTestFile(t,
		filepath.Join(repositoryRoot, "config", "release-transition.json"), registry, 0o600,
	)
	runtimeRoot := filepath.Join(t.TempDir(), "runtime")
	if err := os.MkdirAll(filepath.Join(runtimeRoot, "releases", "release-a"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/release-a", filepath.Join(runtimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	legacyRegistration := filepath.Join(configHome, "yards", testyardmigration.LegacyYard, "config.env")
	legacyOverride := filepath.Join(
		configHome, "yards", testyardmigration.LegacyYard, "overrides", "runtime.env",
	)
	writeReleaseTransitionTestFile(t, legacyRegistration, []byte("YARD_TEMPLATE=test-vms\n"), 0o600)
	writeReleaseTransitionTestFile(t, legacyOverride, []byte("NESTED_E2E_VMS=0\n"), 0o600)
	incus := filepath.Join(root, "incus")
	writeReleaseTransitionTestFile(t, incus, []byte(`#!/bin/sh
set -eu
case "$*" in
  "project list --format=json") printf '[{"name":"subyard-e2e-yard"}]\n' ;;
  "project get subyard-e2e-yard features.images") printf 'false\n' ;;
  "list yard-e2e-yard --project subyard-e2e-yard --format=json")
    printf '[{"name":"yard-e2e-yard","status":"RUNNING"}]\n' ;;
  *) exit 2 ;;
esac
`), 0o700)
	realOwner := &ownerRegistrationTransition{options: func() (testyardmigration.Options, error) {
		return testyardmigration.Options{
			Executable: filepath.Join(root, "yard"), Incus: incus,
			ConfigHome: configHome, DataHome: filepath.Join(root, "data"),
			Environment: os.Environ(),
			RunYard: func(
				_ context.Context,
				yard string,
				output io.Writer,
				arguments ...string,
			) error {
				if yard != testyardmigration.LegacyYard {
					return errors.New("unexpected owner mutation")
				}
				switch strings.Join(arguments, " ") {
				case "check":
					return nil
				case "test-vms status":
					_, err := io.WriteString(output, "ttl_remaining_seconds\t0\n")
					return err
				default:
					return errors.New("unexpected owner mutation")
				}
			},
		}, nil
	}}
	owner := &trackedOwnerRegistration{delegate: realOwner}
	request := releasetransition.ProcessRequest{
		SchemaVersion: releasetransition.ProcessProtocolSchemaV1,
		Mode:          releasetransition.ProcessInspect,
		RuntimeRoot:   runtimeRoot, ConfigHome: configHome,
		Target: "release-a", Direction: releasetransition.DirectionActivateTarget,
		ArtifactDigest: releasetransition.Fingerprint(strings.Repeat("a", 64)),
	}
	verify := func(releasetransition.PlanToken, releasetransition.Authorization) bool { return true }
	inspected, err := executeReleaseTransitionRequest(
		context.Background(), repositoryRoot, request, verify, nil, owner, nil,
	)
	if err != nil || inspected.Inspection == nil || !inspected.Inspection.Assessment.Changed {
		t.Fatalf("owner inspection = %#v, err=%v", inspected, err)
	}
	writeReleaseTransitionTestFile(t, legacyOverride, []byte("NESTED_E2E_VMS=1\n"), 0o600)

	request.Mode = releasetransition.ProcessConverge
	request.Execution = &releasetransition.Execution{
		Plan: inspected.Inspection.Plan, Authorization: "confirmed",
	}
	converged, err := executeReleaseTransitionRequest(
		context.Background(), repositoryRoot, request, verify, nil, owner, nil,
	)
	if err != nil || converged.Outcome == nil ||
		converged.Outcome.Status != releasetransition.StatusOperatorActionRequired ||
		converged.Outcome.Code != releasetransition.CodePlanStale || owner.commits != 0 {
		t.Fatalf("override drift outcome = %#v owner=%#v err=%v", converged, owner, err)
	}
	currentRegistration := filepath.Join(
		configHome, "yards", testyardmigration.CurrentYard, "config.env",
	)
	if _, err := os.Lstat(currentRegistration); !os.IsNotExist(err) {
		t.Fatalf("override drift published canonical registration: %v", err)
	}
}

func TestOwnerRegistrationTransitionPreparesProspectiveSourceRegistration(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	writeReleaseTransitionTestFile(t, filepath.Join(
		configHome, "yards", testyardmigration.LegacyYard, "projects", ".lock",
	), []byte{}, 0o600)
	incus := filepath.Join(root, "incus")
	writeReleaseTransitionTestFile(t, incus, []byte(`#!/bin/sh
set -eu
case "$*" in
  "project list --format=json") printf '[{"name":"subyard-e2e-yard"}]\n' ;;
  "project get subyard-e2e-yard features.images") printf 'false\n' ;;
  "list yard-e2e-yard --project subyard-e2e-yard --format=json")
    printf '[{"name":"yard-e2e-yard","status":"STOPPED"}]\n' ;;
  "config get yard-e2e-yard user.subyard.desired_power --project subyard-e2e-yard")
    printf 'running\n' ;;
  *) exit 2 ;;
esac
`), 0o700)
	registrationPath := filepath.Join(
		configHome, "yards", testyardmigration.LegacyYard, "config.env",
	)
	view := prospectiveOwnerSettingsView{snapshots: map[string]config.PersistentFileSnapshot{
		registrationPath: {Exists: true, Content: []byte("YARD_TEMPLATE=test-vms\n")},
	}}
	owner := &ownerRegistrationTransition{options: func() (testyardmigration.Options, error) {
		return testyardmigration.Options{
			Executable: filepath.Join(root, "yard"), Incus: incus,
			ConfigHome: configHome, DataHome: filepath.Join(root, "data"),
			Environment: os.Environ(),
			RunYard: func(context.Context, string, io.Writer, ...string) error {
				return errors.New("stopped prospective yard unexpectedly executed")
			},
		}, nil
	}}

	observation, err := owner.Prepare(context.Background(), view)
	if err != nil {
		t.Fatal(err)
	}
	if observation.State != releasetransition.OwnerRegistrationLegacyDirectoryProjects ||
		observation.Registration == "" || observation.Overrides == "" ||
		observation.Controller == "" || !observation.SharedImages {
		t.Fatalf("prospective owner observation = %#v", observation)
	}
	if _, err := os.Lstat(registrationPath); !os.IsNotExist(err) {
		t.Fatalf("prospective owner inspection wrote registration: %v", err)
	}
	original := []byte("YARD_TEMPLATE=test-vms\nNESTED_E2E_VMS=0\n")
	desired := []byte("YARD_TEMPLATE='test-vms'\n")
	writeReleaseTransitionTestFile(t, registrationPath, original, 0o600)
	view.snapshots[registrationPath] = config.PersistentFileSnapshot{Exists: true, Content: desired}
	observation, err = owner.Prepare(context.Background(), view)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(desired)
	if observation.Registration != releasetransition.Fingerprint(fmt.Sprintf("%x", digest[:])) {
		t.Fatalf("post-settings owner identity = %#v", observation)
	}
	if payload, readErr := os.ReadFile(registrationPath); readErr != nil || !bytes.Equal(payload, original) {
		t.Fatalf("prospective owner changed live registration to %q: %v", payload, readErr)
	}
}

func TestForwardMaterializedConfigResumeKeepsScopeAcrossLegacyRename(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	registry, err := os.ReadFile(filepath.Join("..", "..", "config", "release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeReleaseTransitionTestFile(t,
		filepath.Join(root, "config", "release-transition.json"), registry, 0o600)
	writeCLIFile(t, filepath.Join(root, "config", "subyard.env"), strings.Join([]string{
		"SHIFT_MODE=shift", "FORWARD_SSH_AGENT=0", "DEV_SUDO=0", "DEV_UID=1000", "DEV_USER=dev",
		"SSH_PORT=2222", "STORAGE_PATH=" + filepath.Join(root, "data", "storage"),
		"HOST_BASE=" + filepath.Join(root, "host"), "RESTRICTED_DISK_PATHS=" + filepath.Join(root, "host"),
	}, "\n")+"\n", 0o600)
	configHome := environmentValue(environment, "SUBYARD_CONFIG_HOME")
	legacy := filepath.Join(configHome, "yards", testyardmigration.LegacyYard)
	current := filepath.Join(configHome, "yards", testyardmigration.CurrentYard)
	writeReleaseTransitionTestFile(t, filepath.Join(legacy, "config.env"),
		[]byte("YARD_TEMPLATE=test-vms\n"), 0o600)
	runtimeRoot := filepath.Join(root, "runtime-legacy-config-resume")
	for _, release := range []string{"release-a", "release-b"} {
		if err := os.MkdirAll(filepath.Join(runtimeRoot, "releases", release), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("releases/release-a", filepath.Join(runtimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
	fake := &testkit.Incus{Instances: make(map[string]ports.InstanceInfo)}
	program, err := New(Options{
		RepositoryRoot: root, Environment: environment, Incus: fake, Executor: fake,
		Config: &recordingConfigApplier{}, Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := releasetransition.ProcessRequest{
		SchemaVersion: releasetransition.ProcessProtocolSchemaV1,
		Mode:          releasetransition.ProcessInspect, RuntimeRoot: runtimeRoot, ConfigHome: configHome,
		Target: "release-b", Direction: releasetransition.DirectionActivateTarget,
		ArtifactDigest:      releasetransition.Fingerprint(strings.Repeat("a", 64)),
		InheritedSettingIDs: program.releaseTransitionInheritedSettingIDs(),
	}
	reconciler := func() *materializedConfigActivationReconciler {
		return &materializedConfigActivationReconciler{
			cli: program, configHome: configHome,
			goal:           releasetransition.Goal{Target: request.Target, Direction: request.Direction},
			artifactDigest: request.ArtifactDigest,
		}
	}
	owner := &interruptedRenameOwner{legacy: legacy, current: current}
	verify := func(releasetransition.PlanToken, releasetransition.Authorization) bool { return true }
	first, err := executeReleaseTransitionRequest(context.Background(), root, request, verify,
		[]releasetransition.V2ActivationReconciler{&observeOnlyMaterialized{
			materializedConfigActivationReconciler: reconciler(),
		}}, owner, nil)
	if err != nil || first.Inspection == nil {
		t.Fatalf("inspect forward transition = %#v, err=%v", first, err)
	}
	request.Mode = releasetransition.ProcessConverge
	request.Execution = &releasetransition.Execution{Plan: first.Inspection.Plan, Authorization: "confirmed"}
	interrupting := &observeOnlyMaterialized{materializedConfigActivationReconciler: reconciler(), fail: true}
	interrupted, err := executeReleaseTransitionRequest(context.Background(), root, request, verify,
		[]releasetransition.V2ActivationReconciler{interrupting}, owner, nil)
	if err != nil || interrupted.Outcome == nil ||
		interrupted.Outcome.Status != releasetransition.StatusRecovering {
		t.Fatalf("interrupted activation = %#v, outcome=%+v err=%v",
			interrupted, interrupted.Outcome, err)
	}
	store, err := releasetransition.NewPOSIXV2Store(configHome)
	if err != nil {
		t.Fatal(err)
	}
	complete, err := reconciler().sourceMigrationsComplete(store)
	if err != nil || !complete {
		t.Fatalf("source migrations complete=%t err=%v", complete, err)
	}
	request.Mode, request.Execution = releasetransition.ProcessInspect, nil
	fresh := reconciler()
	resume, err := executeReleaseTransitionRequest(context.Background(), root, request, verify,
		[]releasetransition.V2ActivationReconciler{&observeOnlyMaterialized{
			materializedConfigActivationReconciler: fresh,
		}}, owner, nil)
	if err != nil || resume.Inspection == nil || resume.Inspection.Resume == nil || fresh.allLocal {
		t.Fatalf("fresh selected-scope resume = %#v, allLocal=%t err=%v", resume, fresh.allLocal, err)
	}
	request.Mode = releasetransition.ProcessConverge
	request.Execution = &releasetransition.Execution{Plan: resume.Inspection.Plan}
	settled, err := executeReleaseTransitionRequest(context.Background(), root, request, verify,
		[]releasetransition.V2ActivationReconciler{&observeOnlyMaterialized{
			materializedConfigActivationReconciler: fresh,
		}}, owner, nil)
	if err != nil || settled.Outcome == nil || settled.Outcome.Status != releasetransition.StatusReady {
		t.Fatalf("resumed forward transition = %#v, outcome=%+v err=%v",
			settled, settled.Outcome, err)
	}
}

func TestCandidateReleaseTransitionProtocolReportsInvalidRegistry(t *testing.T) {
	repositoryRoot := t.TempDir()
	runtimeRoot := t.TempDir()
	configHome := t.TempDir()
	for _, root := range []string{runtimeRoot, configHome} {
		if err := os.Chmod(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeReleaseTransitionTestFile(t,
		filepath.Join(repositoryRoot, "config", "release-transition.json"), []byte("{\n"), 0o600,
	)
	if err := os.MkdirAll(filepath.Join(runtimeRoot, "releases", "release-a"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/release-a", filepath.Join(runtimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
	request := releasetransition.ProcessRequest{
		SchemaVersion:  releasetransition.ProcessProtocolSchemaV1,
		Mode:           releasetransition.ProcessInspect,
		RuntimeRoot:    runtimeRoot,
		ConfigHome:     configHome,
		Target:         "release-b",
		Direction:      releasetransition.DirectionActivateTarget,
		ArtifactDigest: releasetransition.Fingerprint(strings.Repeat("a", 64)),
	}

	response, err := executeReleaseTransitionRequest(
		context.Background(), repositoryRoot, request,
		func(releasetransition.PlanToken, releasetransition.Authorization) bool { return false },
		nil, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if response.Inspection != nil || response.Outcome == nil ||
		response.Outcome.Status != releasetransition.StatusOperatorActionRequired ||
		response.Outcome.Code != releasetransition.CodeRegistryInvalid ||
		response.Outcome.Active != "release-a" || response.Outcome.Previous != nil ||
		response.Outcome.Target != "release-b" || response.Outcome.Retry == "" {
		t.Fatalf("invalid registry response = %#v", response)
	}
}

type processDriftReconciler struct {
	converged  bool
	failOnce   bool
	observes   int
	reconciles int
}

func (*processDriftReconciler) ID() string { return "process-drift" }

func (reconciler *processDriftReconciler) Observe(
	context.Context,
	releasetransition.ReleasePair,
	releasetransition.ReleaseLinks,
) (releasetransition.V2ActivationObservation, error) {
	reconciler.observes++
	desired := releasetransition.Fingerprint(strings.Repeat("d", 64))
	actual := desired
	if !reconciler.converged {
		actual = releasetransition.Fingerprint(strings.Repeat("e", 64))
	}
	return releasetransition.V2ActivationObservation{
		Actual: actual, Desired: desired, Converged: reconciler.converged,
	}, nil
}

func (reconciler *processDriftReconciler) Reconcile(
	context.Context,
	releasetransition.ReleaseLinks,
) error {
	reconciler.reconciles++
	reconciler.converged = true
	if reconciler.failOnce {
		reconciler.failOnce = false
		return errors.New("injected failure after activation mutation")
	}
	return nil
}

func TestCandidateProcessRepairsCompletedJournalActivationDrift(t *testing.T) {
	repositoryRoot := t.TempDir()
	registry, err := os.ReadFile(filepath.Join("..", "..", "config", "release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeReleaseTransitionTestFile(t,
		filepath.Join(repositoryRoot, "config", "release-transition.json"), registry, 0o600,
	)
	runtimeRoot := filepath.Join(t.TempDir(), "runtime")
	for _, release := range []string{"release-a", "release-b"} {
		if err := os.MkdirAll(filepath.Join(runtimeRoot, "releases", release), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("releases/release-a", filepath.Join(runtimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
	configHome := filepath.Join(t.TempDir(), "config")
	writeReleaseTransitionTestFile(t,
		filepath.Join(configHome, "yards", "hermes", "config.env"),
		[]byte("YARD_TEMPLATE=e2e-vms\nNESTED_E2E_VMS=0\nSSH_PORT=2224\n"), 0o600,
	)
	request := releasetransition.ProcessRequest{
		SchemaVersion: releasetransition.ProcessProtocolSchemaV1,
		Mode:          releasetransition.ProcessInspect, RuntimeRoot: runtimeRoot, ConfigHome: configHome,
		Target: "release-b", Direction: releasetransition.DirectionActivateTarget,
		ArtifactDigest: releasetransition.Fingerprint(strings.Repeat("a", 64)),
	}
	grant := releasetransition.Authorization("confirmed-migration-grant")
	authorizations := 0
	verify := func(_ releasetransition.PlanToken, authorization releasetransition.Authorization) bool {
		if authorization != grant {
			return false
		}
		authorizations++
		return true
	}
	reconciler := &processDriftReconciler{converged: true}
	reconcilers := []releasetransition.V2ActivationReconciler{reconciler}
	inspected, err := executeReleaseTransitionRequest(
		context.Background(), repositoryRoot, request, verify, reconcilers,
		releaseTransitionOwnerFixture{}, nil,
	)
	if err != nil || inspected.Inspection == nil {
		t.Fatalf("initial inspection=%#v err=%v", inspected, err)
	}
	request.Mode = releasetransition.ProcessConverge
	request.Execution = &releasetransition.Execution{
		Plan: inspected.Inspection.Plan, Authorization: grant,
	}
	completed, err := executeReleaseTransitionRequest(
		context.Background(), repositoryRoot, request, verify, reconcilers,
		releaseTransitionOwnerFixture{}, nil,
	)
	if err != nil || completed.Outcome == nil ||
		completed.Outcome.Status != releasetransition.StatusReady ||
		completed.Outcome.Transaction == nil || authorizations != 1 {
		t.Fatalf("initial convergence=%#v err=%v", completed, err)
	}

	observesBefore := reconciler.observes
	reconcilesBefore := reconciler.reconciles
	request.Mode = releasetransition.ProcessInspect
	request.Execution = nil
	noDrift, err := executeReleaseTransitionRequest(
		context.Background(), repositoryRoot, request, verify, reconcilers,
		releaseTransitionOwnerFixture{}, nil,
	)
	if err != nil || noDrift.Inspection == nil || noDrift.Inspection.Outcome == nil ||
		noDrift.Inspection.Outcome.Status != releasetransition.StatusReady ||
		noDrift.Inspection.Assessment.Changed || authorizations != 1 ||
		reconciler.observes <= observesBefore || reconciler.reconciles != reconcilesBefore {
		t.Fatalf("same-version no-drift inspection=%#v authorizations=%d reconciler=%#v err=%v",
			noDrift, authorizations, reconciler, err)
	}

	observesBefore = reconciler.observes
	reconciler.converged = false
	reconciler.failOnce = true
	repeat, err := executeReleaseTransitionRequest(
		context.Background(), repositoryRoot, request, verify, reconcilers,
		releaseTransitionOwnerFixture{}, nil,
	)
	goal := releasetransition.Goal{
		Target: request.Target, Direction: releasetransition.DirectionActivateTarget,
	}
	if err != nil || repeat.Inspection == nil || repeat.Inspection.Resume != nil ||
		!strings.HasPrefix(string(repeat.Inspection.Plan), "plan-v1-") ||
		repeat.Inspection.Outcome == nil ||
		repeat.Inspection.Outcome.Status != releasetransition.StatusRecovering ||
		repeat.Inspection.Outcome.Code != releasetransition.CodeRecoveryPending ||
		repeat.Inspection.Outcome.Transaction == nil ||
		*repeat.Inspection.Outcome.Transaction != *completed.Outcome.Transaction ||
		!repeat.Inspection.Assessment.Changed || reconciler.observes <= observesBefore {
		t.Fatalf("completed migration inspection=%#v err=%v", repeat, err)
	}
	// The frozen v0.11.2 protected caller requires the historical transaction,
	// while Resume must remain absent so this new plan receives a fresh grant.
	if err := repeat.Inspection.ValidateOutcome(goal); err != nil {
		t.Fatalf("completed migration process inspection is invalid: %v", err)
	}
	request.Mode = releasetransition.ProcessConverge
	request.Execution = &releasetransition.Execution{Plan: repeat.Inspection.Plan}
	unconfirmed, err := executeReleaseTransitionRequest(
		context.Background(), repositoryRoot, request, verify, reconcilers,
		releaseTransitionOwnerFixture{}, nil,
	)
	if err != nil || unconfirmed.Outcome == nil ||
		unconfirmed.Outcome.Code != releasetransition.CodeConfirmationRequired ||
		authorizations != 1 || reconciler.reconciles != reconcilesBefore {
		t.Fatalf("historical transaction authorized a new repair: %#v err=%v", unconfirmed, err)
	}
	request.Execution = &releasetransition.Execution{
		Plan: repeat.Inspection.Plan, Authorization: grant,
	}
	interrupted, err := executeReleaseTransitionRequest(
		context.Background(), repositoryRoot, request, verify, reconcilers,
		releaseTransitionOwnerFixture{}, nil,
	)
	if err != nil || interrupted.Outcome == nil ||
		interrupted.Outcome.Status != releasetransition.StatusRecovering ||
		interrupted.Outcome.Transaction == nil ||
		*interrupted.Outcome.Transaction == *completed.Outcome.Transaction || authorizations != 2 {
		t.Fatalf("activation repair interruption=%#v authorizations=%d reconciler=%#v err=%v",
			interrupted, authorizations, reconciler, err)
	}

	request.Mode = releasetransition.ProcessInspect
	request.Execution = nil
	resume, err := executeReleaseTransitionRequest(
		context.Background(), repositoryRoot, request, verify, reconcilers,
		releaseTransitionOwnerFixture{}, nil,
	)
	if err != nil || resume.Inspection == nil || resume.Inspection.Resume == nil ||
		*resume.Inspection.Resume != *interrupted.Outcome.Transaction ||
		resume.Inspection.Outcome == nil ||
		resume.Inspection.Outcome.Status != releasetransition.StatusRecovering {
		t.Fatalf("activation repair resume=%#v err=%v", resume, err)
	}
	observesBefore = reconciler.observes
	request.Mode = releasetransition.ProcessConverge
	request.Execution = &releasetransition.Execution{Plan: resume.Inspection.Plan}
	settled, err := executeReleaseTransitionRequest(
		context.Background(), repositoryRoot, request, verify, reconcilers,
		releaseTransitionOwnerFixture{}, nil,
	)
	if err != nil || settled.Outcome == nil ||
		settled.Outcome.Status != releasetransition.StatusReady ||
		settled.Outcome.Transaction == nil ||
		*settled.Outcome.Transaction != *interrupted.Outcome.Transaction ||
		authorizations != 2 || reconciler.observes < observesBefore+3 {
		t.Fatalf("completed migration convergence=%#v authorizations=%d reconciler=%#v err=%v",
			settled, authorizations, reconciler, err)
	}
}

func TestReleaseTransitionEstablishesOperationContextBeforeExecution(t *testing.T) {
	home := t.TempDir()
	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: repositoryRoot(t),
		Environment:    []string{"HOME=" + home},
		Stdin:          strings.NewReader(operationContextProtocolRequest(t, home)),
		Stderr:         &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.runReleaseTransition(context.Background(), nil); code != 1 {
		t.Fatalf("release transition status = %d, stderr=%q", code, stderr.String())
	}
	if operationID := program.env["SUBYARD_OPERATION_ID"]; operationID == "" {
		t.Fatal("release transition execution has no operation context")
	}
}

func TestReleaseTransitionRejectsInvalidInheritedOperationContext(t *testing.T) {
	home := t.TempDir()
	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: repositoryRoot(t),
		Environment: []string{
			"HOME=" + home,
			"SUBYARD_OPERATION_ID=invalid/context",
		},
		Stdin:  strings.NewReader(operationContextProtocolRequest(t, home)),
		Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.runReleaseTransition(context.Background(), nil); code != 2 ||
		!strings.Contains(stderr.String(), "operation context is invalid") {
		t.Fatalf("release transition status = %d, stderr=%q", code, stderr.String())
	}
}

func TestReleaseTransitionMigrationUsesPinnedCandidateEngine(t *testing.T) {
	repositoryRoot := "/proc/123/fd/9"
	program := &CLI{
		options: Options{
			RepositoryRoot: repositoryRoot,
			DispatcherPath: "/memfd:subyard-verified-yard-engine",
		},
		baseEnv: map[string]string{"HOME": t.TempDir()},
		env:     map[string]string{},
	}
	options, err := program.releaseTransitionTestYardOptions(
		releasetransition.ProcessRequest{
			RuntimeRoot: filepath.Join(t.TempDir(), "runtime"),
			ConfigHome:  filepath.Join(t.TempDir(), "config"),
		},
	)
	want := filepath.Join(repositoryRoot, "bin", "yard-engine")
	if err != nil || options.Executable != want {
		t.Fatalf("migration executable = %q, %v; want %q", options.Executable, err, want)
	}
}

func TestPowerActivationUsesPinnedCandidateEngine(t *testing.T) {
	root := repositoryRoot(t)
	home := t.TempDir()
	configHome := filepath.Join(home, ".config", "subyard")
	environment := []string{
		"HOME=" + home,
		"SUBYARD_OPERATOR_HOME=" + home,
		"SUBYARD_CONFIG_HOME=" + configHome,
		"SUBYARD_HOME=" + filepath.Join(home, ".subyard"),
	}
	dispatcher := "/memfd:subyard-verified-yard-engine"
	program, err := New(Options{
		RepositoryRoot: root,
		DispatcherPath: dispatcher,
		Environment:    environment,
	})
	if err != nil {
		t.Fatal(err)
	}
	reconciler, ok := program.powerActivationReconciler(releasetransition.ProcessRequest{
		Yard:       "default",
		ConfigHome: configHome,
	}).(*activationStageReconciler)
	if !ok {
		t.Fatal("power activation reconciler has an unexpected implementation")
	}
	platform, err := reconciler.platform(context.Background(), activationApplicability{
		state: "installed", applies: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime, ok := platform.(reconcileruntime.Runtime)
	if !ok {
		t.Fatalf("power platform type = %T", platform)
	}
	want := filepath.Join(root, "bin", "yard-engine")
	for _, name := range []string{"SUBYARD_DISPATCHER_PATH", "SUBYARD_POWER_ENGINE_SOURCE"} {
		if !slices.Contains(runtime.Environment, name+"="+want) {
			t.Fatalf("power platform %s does not use pinned engine %q: %q",
				name, want, runtime.Environment)
		}
	}
	if program.options.DispatcherPath != dispatcher {
		t.Fatalf("power platform changed active dispatcher to %q", program.options.DispatcherPath)
	}
}

func TestMaterializedConfigReconcileUsesTargetContextForRevalidation(t *testing.T) {
	root := repositoryRoot(t)
	home := t.TempDir()
	configHome := filepath.Join(home, ".config", "subyard")
	staleRoot := filepath.Join(home, "releases", "stale", "config")
	staleSource := filepath.Join(home, "releases", "stale", "resolved", "config.toml")
	if err := os.MkdirAll(filepath.Dir(staleRoot), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "config"), staleRoot); err != nil {
		t.Fatal(err)
	}
	writeReleaseTransitionTestFile(t, staleSource, []byte("model = \"stale\"\n"), 0o600)
	writeReleaseTransitionTestFile(t, filepath.Join(configHome, "config.env"),
		[]byte("CODING_TOOL_INTEGRATIONS=codex\n"), 0o600)

	var stdout, stderr bytes.Buffer
	applier := &recordingConfigApplier{}
	program, err := New(Options{
		RepositoryRoot: root,
		Environment: []string{
			"HOME=" + home,
			"SUBYARD_OPERATOR_HOME=" + home,
			"SUBYARD_CONFIG_HOME=" + configHome,
			"SUBYARD_HOME=" + filepath.Join(home, ".subyard"),
			"SUBYARD_CONFIG_DIR=" + staleRoot,
			"SUBYARD_CONFIG_LOADED=1",
			"SUBYARD_ENGINE_CONTEXT=1",
			"CODING_TOOL_INTEGRATIONS=codex",
			"AGENT_codex_CONFIG=" + staleSource,
			"AGENT_codex_CONFIG_DEST=.codex/config.toml",
		},
		Config: applier, Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	program.ensureOperationID()
	targetLoaded, err := program.resolveReleaseTransitionContext("default", configHome)
	if err != nil {
		t.Fatal(err)
	}
	fake := &testkit.Incus{Instances: map[string]ports.InstanceInfo{
		targetLoaded.Context.IncusProject + "/" + targetLoaded.Context.YardInstanceName: {
			Name: targetLoaded.Context.YardInstanceName, Project: targetLoaded.Context.IncusProject,
			Status: "Running",
		},
	}}
	appendMismatchedHashSteps(t, fake, targetLoaded, "0")
	appendMismatchedHashSteps(t, fake, targetLoaded, "0")
	appendMismatchedHashSteps(t, fake, targetLoaded, "0")
	appendHashSteps(t, fake, targetLoaded)
	appendHashSteps(t, fake, targetLoaded)
	program.options.Incus = fake
	program.options.Executor = fake

	reconciler := &materializedConfigActivationReconciler{
		cli: program, yard: "default", configHome: configHome,
	}
	initial, err := reconciler.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
	if err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Reconcile(context.Background(), releasetransition.ReleaseLinks{}); err != nil {
		t.Fatalf("reconcile with stale inherited context: %v; stdout=%s stderr=%s", err, stdout.String(), stderr.String())
	}
	refreshed, err := reconciler.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
	if err != nil {
		t.Fatal(err)
	}
	if initial.Desired != refreshed.Desired {
		t.Fatalf("desired fingerprint changed from %q to %q", initial.Desired, refreshed.Desired)
	}
	if !slices.Equal(applier.yards, []string{"default"}) {
		t.Fatalf("applied yards = %v, want [default]", applier.yards)
	}
}

func TestMaterializedConfigReconcileUsesTrustedCandidateChild(t *testing.T) {
	candidateRoot := "/proc/123/fd/9"
	sealedDispatcher := "/memfd:subyard-verified-yard-engine"
	program := &CLI{
		options: Options{RepositoryRoot: candidateRoot, DispatcherPath: sealedDispatcher},
		baseEnv: map[string]string{
			"SUBYARD_CONFIG_LOADED": "1", "SUBYARD_YARD": "default", "SSH_PORT": "2222",
		},
		env: map[string]string{"SUBYARD_OPERATION_ID": "release-config-operation"},
	}
	reconciler := &materializedConfigActivationReconciler{cli: program}

	nested := reconciler.reconcileCLI()
	want := filepath.Join(candidateRoot, "bin", "yard-engine")
	if nested == program || nested.options.DispatcherPath != want {
		t.Fatalf("nested materialized-config dispatcher = %q, want %q",
			nested.options.DispatcherPath, want)
	}
	if program.options.DispatcherPath != sealedDispatcher {
		t.Fatalf("materialized-config reconcile changed the sealed parent dispatcher to %q",
			program.options.DispatcherPath)
	}
	applier, ok := nested.options.Config.(releaseTransitionConfigApplier)
	if !ok || applier.cli != nested {
		t.Fatalf("materialized-config reconcile applier = %#v, want trusted in-process child", nested.options.Config)
	}
	for _, environment := range []map[string]string{nested.baseEnv, nested.env} {
		if environment["SUBYARD_CONFIG_LOADED"] != "" || environment["SUBYARD_YARD"] != "" ||
			environment["SSH_PORT"] != "" ||
			environment["SUBYARD_OPERATION_ID"] != "release-config-operation" {
			t.Fatal("nested materialized config must reload settings and preserve operation identity")
		}
	}

	injected := &recordingConfigApplier{}
	program.options.Config = injected
	if got := reconciler.reconcileCLI(); got.options.Config != injected ||
		got.options.DispatcherPath != sealedDispatcher {
		t.Fatalf("injected config applier was replaced: %#v", got.options)
	}
}

func TestReleaseTransitionRejectsTrailingRequestJSON(t *testing.T) {
	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: repositoryRoot(t),
		Environment:    []string{"HOME=" + t.TempDir()},
		Stdin:          strings.NewReader("{}{}"),
		Stderr:         &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.runReleaseTransition(context.Background(), nil); code != 2 ||
		!strings.Contains(stderr.String(), "request is invalid") {
		t.Fatalf("release transition status = %d, stderr=%q", code, stderr.String())
	}
}

type releaseTransitionOwnerFixture struct{}

type interruptedRenameOwner struct {
	legacy, current string
}

type observeOnlyMaterialized struct {
	*materializedConfigActivationReconciler
	fail bool
}

func (reconciler *observeOnlyMaterialized) Reconcile(
	context.Context, releasetransition.ReleaseLinks,
) error {
	if reconciler.fail {
		reconciler.fail = false
		return errors.New("injected interruption before materialized config reconciliation")
	}
	return nil
}

func (owner *interruptedRenameOwner) Prepare(
	context.Context, releasetransition.V2SettingsSnapshotView,
) (releasetransition.OwnerRegistrationObservation, error) {
	return releasetransition.OwnerRegistrationObservation{
		State:        releasetransition.OwnerRegistrationLegacyDirectory,
		Registration: releasetransition.Fingerprint(strings.Repeat("1", 64)),
		Overrides:    releasetransition.Fingerprint(strings.Repeat("2", 64)),
		Controller:   releasetransition.Fingerprint(strings.Repeat("3", 64)),
	}, nil
}

func (owner *interruptedRenameOwner) Observe(
	_ context.Context, _ releasetransition.OwnerRegistrationObservation,
) (releasetransition.OwnerRegistrationProgress, error) {
	if _, err := os.Stat(owner.current); err == nil {
		return releasetransition.OwnerRegistrationDesired, nil
	}
	return releasetransition.OwnerRegistrationExpected, nil
}

func (owner *interruptedRenameOwner) Commit(
	_ context.Context, before releasetransition.OwnerRegistrationObservation,
) error {
	if before.TerminalCleanup {
		return nil
	}
	return os.Rename(owner.legacy, owner.current)
}

type prospectiveOwnerSettingsView struct {
	snapshots map[string]config.PersistentFileSnapshot
}

func (view prospectiveOwnerSettingsView) ListYards() ([]string, error) {
	return []string{testyardmigration.LegacyYard}, nil
}

func (view prospectiveOwnerSettingsView) ReadSnapshot(
	path string,
) (config.PersistentFileSnapshot, error) {
	return view.snapshots[path], nil
}

type trackedOwnerRegistration struct {
	delegate releasetransition.V2OwnerRegistration
	commits  int
}

func (owner *trackedOwnerRegistration) Prepare(
	ctx context.Context,
	settings releasetransition.V2SettingsSnapshotView,
) (releasetransition.OwnerRegistrationObservation, error) {
	return owner.delegate.Prepare(ctx, settings)
}

func (owner *trackedOwnerRegistration) Observe(
	ctx context.Context,
	before releasetransition.OwnerRegistrationObservation,
) (releasetransition.OwnerRegistrationProgress, error) {
	return owner.delegate.Observe(ctx, before)
}

func (owner *trackedOwnerRegistration) Commit(
	context.Context,
	releasetransition.OwnerRegistrationObservation,
) error {
	owner.commits++
	return errors.New("owner commit reached after stale inspection")
}

func (releaseTransitionOwnerFixture) Prepare(
	context.Context,
	releasetransition.V2SettingsSnapshotView,
) (
	releasetransition.OwnerRegistrationObservation,
	error,
) {
	return releasetransition.OwnerRegistrationObservation{
		State: releasetransition.OwnerRegistrationAbsent,
	}, nil
}

func (releaseTransitionOwnerFixture) Observe(
	context.Context,
	releasetransition.OwnerRegistrationObservation,
) (releasetransition.OwnerRegistrationProgress, error) {
	return releasetransition.OwnerRegistrationExpected, nil
}

func (releaseTransitionOwnerFixture) Commit(
	context.Context,
	releasetransition.OwnerRegistrationObservation,
) error {
	return nil
}

func TestActivationStageReconcilerPreservesInapplicableRuntime(t *testing.T) {
	platform := newInitPlatformFixture()
	platform.converged[ports.ReconcileStagePower] = false
	reconciler := &activationStageReconciler{
		id: "host-power", stage: ports.ReconcileStagePower,
		inspectApplicability: func(context.Context) (activationApplicability, error) {
			return activationApplicability{state: "absent"}, nil
		},
		platform: func(context.Context, activationApplicability) (ports.ReconcileStageRunner, error) {
			t.Fatal("inapplicable runtime must not construct a platform")
			return nil, nil
		},
	}
	pair := releasetransition.ReleasePair{From: "release-a", Target: "release-b"}
	links := releasetransition.ReleaseLinks{Active: "release-a"}
	observed, err := reconciler.Observe(context.Background(), pair, links)
	if err != nil || !observed.Converged || observed.Actual != observed.Desired {
		t.Fatalf("observation = %#v, err=%v", observed, err)
	}
	if err := reconciler.Reconcile(context.Background(), links); err != nil {
		t.Fatal(err)
	}
	if len(platform.applied) != 0 {
		t.Fatalf("inapplicable runtime applied stages %v", platform.applied)
	}
}

func TestCompletedMaterializedConfigReadinessDoesNotDependOnSelectedYard(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	registry, err := os.ReadFile(filepath.Join("..", "..", "config", "release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	registrySum := sha256.Sum256(registry)
	registryDigest := releasetransition.Fingerprint(fmt.Sprintf("%x", registrySum[:]))
	writeReleaseTransitionTestFile(t,
		filepath.Join(root, "config", "release-transition.json"), registry, 0o600,
	)
	if err := os.MkdirAll(filepath.Join(root, "config", "yards", "profiles"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(root, "config", "yards", "profiles", "test-vms.env"),
		"NESTED_E2E_VMS=1\n", 0o600)
	source := filepath.Join(root, "repo.rules")
	writeCLIFile(t, source, "allow_rule()\n", 0o600)
	writeCLIFile(t, filepath.Join(root, "config", "subyard.env"), strings.Join([]string{
		"SHIFT_MODE=shift",
		"FORWARD_SSH_AGENT=0",
		"DEV_SUDO=0",
		"DEV_UID=1000",
		"DEV_USER=dev",
		"SSH_PORT=2222",
		"STORAGE_PATH=" + filepath.Join(root, "data", "storage"),
		"HOST_BASE=" + filepath.Join(root, "host"),
		"RESTRICTED_DISK_PATHS=" + filepath.Join(root, "host"),
		"CODING_TOOL_INTEGRATIONS=codex",
		"AGENT_codex_RULES=" + source,
		"AGENT_codex_RULES_DEST=.codex/rules/repo.rules",
	}, "\n")+"\n", 0o600)
	configHome := environmentValue(environment, "SUBYARD_CONFIG_HOME")
	if err := os.MkdirAll(filepath.Join(configHome, "yards", testyardmigration.CurrentYard), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(configHome, "yards", testyardmigration.CurrentYard, "config.env"),
		"YARD_TEMPLATE=test-vms\nSSH_PORT=2223\n", 0o600)
	if err := os.MkdirAll(filepath.Join(configHome, "yards", "hermes"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(configHome, "yards", "hermes", "config.env"),
		"YARD_TEMPLATE=e2e-vms\nNESTED_E2E_VMS=0\nSSH_PORT=2224\n", 0o600)
	runtimeRoot := filepath.Join(root, "runtime")
	for _, release := range []string{"release-a", "release-b"} {
		if err := os.MkdirAll(filepath.Join(runtimeRoot, "releases", release), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("releases/release-a", filepath.Join(runtimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
	request := releasetransition.ProcessRequest{
		SchemaVersion:  releasetransition.ProcessProtocolSchemaV1,
		Mode:           releasetransition.ProcessInspect,
		RuntimeRoot:    runtimeRoot,
		ConfigHome:     configHome,
		Target:         "release-b",
		Direction:      releasetransition.DirectionActivateTarget,
		ArtifactDigest: releasetransition.Fingerprint(strings.Repeat("a", 64)),
		RegistryDigest: registryDigest,
	}
	completedReconciler := &processDriftReconciler{converged: true}
	inspected, err := executeReleaseTransitionRequest(
		context.Background(), root, request,
		func(releasetransition.PlanToken, releasetransition.Authorization) bool { return true },
		[]releasetransition.V2ActivationReconciler{completedReconciler},
		releaseTransitionOwnerFixture{}, nil,
	)
	if err != nil || inspected.Inspection == nil {
		t.Fatalf("inspect complete fixture transition = %#v, err=%v", inspected, err)
	}
	request.Mode = releasetransition.ProcessConverge
	request.Execution = &releasetransition.Execution{
		Plan: inspected.Inspection.Plan, Authorization: "fixture-authorization",
	}
	completed, err := executeReleaseTransitionRequest(
		context.Background(), root, request,
		func(releasetransition.PlanToken, releasetransition.Authorization) bool { return true },
		[]releasetransition.V2ActivationReconciler{completedReconciler},
		releaseTransitionOwnerFixture{}, nil,
	)
	if err != nil || completed.Outcome == nil ||
		completed.Outcome.Status != releasetransition.StatusReady {
		t.Fatalf("complete fixture transition = %#v, err=%v", completed, err)
	}

	fake := &testkit.Incus{Instances: make(map[string]ports.InstanceInfo)}
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root,
		Incus: fake, Executor: fake, Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.resolveReleaseTransitionContext(testyardmigration.CurrentYard, configHome)
	if err != nil {
		t.Fatal(err)
	}
	fake.Instances[loaded.Context.IncusProject+"/"+loaded.Context.YardInstanceName] = ports.InstanceInfo{
		Name: loaded.Context.YardInstanceName, Project: loaded.Context.IncusProject, Status: "Running",
	}
	defaultLoaded, err := program.resolveReleaseTransitionContext("default", configHome)
	if err != nil {
		t.Fatal(err)
	}
	updateProgram, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Environment: environmentList(defaultLoaded.Environment, nil), WorkingDir: root,
		Incus: fake, Executor: fake, Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	fake.ExecSteps = []testkit.IncusExecStep{{
		Result: ports.InstanceExecResult{
			Stdout:   []byte(strings.Repeat("0", 64) + "  /home/dev/.codex/rules/repo.rules\n"),
			ExitCode: 0,
		},
	}, {
		Result: ports.InstanceExecResult{
			Stdout:   []byte(strings.Repeat("0", 64) + "  /home/dev/.codex/rules/repo.rules\n"),
			ExitCode: 0,
		},
	}}
	observe := func(
		candidate *CLI,
		yard string,
		releases releasetransition.ReleasePair,
	) releasetransition.V2ActivationObservation {
		t.Helper()
		observation, observeErr := (&materializedConfigActivationReconciler{
			cli: candidate, yard: yard, configHome: configHome,
			goal: releasetransition.Goal{
				Target: request.Target, Direction: request.Direction,
			},
			artifactDigest: request.ArtifactDigest,
			registryDigest: request.RegistryDigest,
		}).Observe(context.Background(), releases, releasetransition.ReleaseLinks{})
		if observeErr != nil {
			t.Fatalf("observe completed materialized config for %s: %v", yard, observeErr)
		}
		return observation
	}
	sourceRelease := releasetransition.ReleasePair{From: "release-a", Target: "release-b"}
	if sourceBootstrap := observe(program, "default", sourceRelease); sourceBootstrap.Converged {
		t.Fatalf("source transition ignored drift in another local yard: %#v", sourceBootstrap)
	}
	fake.ExecSteps = append(fake.ExecSteps, testkit.IncusExecStep{Result: ports.InstanceExecResult{
		Stdout: []byte(strings.Repeat("0", 64) + "  /home/dev/.codex/rules/repo.rules\n"),
	}})
	activeRelease := releasetransition.ReleasePair{From: "release-b", Target: "release-b"}
	bootstrap := observe(updateProgram, "default", activeRelease)
	nextCommand := observe(program, testyardmigration.CurrentYard, activeRelease)
	if bootstrap != nextCommand || bootstrap.Converged {
		t.Fatalf("completed readiness depends on selected yard: bootstrap=%#v next=%#v",
			bootstrap, nextCommand)
	}
	mismatched := &materializedConfigActivationReconciler{
		cli: program, yard: "default", configHome: configHome,
		goal: releasetransition.Goal{
			Target: "release-c", Direction: request.Direction,
		},
		artifactDigest: request.ArtifactDigest,
		registryDigest: request.RegistryDigest,
	}
	mismatchedObservation, err := mismatched.Observe(
		context.Background(), activeRelease, releasetransition.ReleaseLinks{},
	)
	if err != nil || !mismatchedObservation.Converged || mismatched.allLocal {
		t.Fatalf("mismatched journal widened scope: %#v allLocal=%t err=%v",
			mismatchedObservation, mismatched.allLocal, err)
	}

	completedStore, err := releasetransition.NewPOSIXV2Store(configHome)
	if err != nil {
		t.Fatal(err)
	}
	completedLedger, err := completedStore.ReadLedger()
	if err != nil || !completedLedger.Exists {
		t.Fatalf("read completed ledger: exists=%t err=%v", completedLedger.Exists, err)
	}
	freshConfigHome := filepath.Join(root, "fresh-state")
	if err := os.MkdirAll(filepath.Join(freshConfigHome, "yards", testyardmigration.CurrentYard), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(freshConfigHome, "yards", testyardmigration.CurrentYard, "config.env"),
		"YARD_TEMPLATE=test-vms\nSSH_PORT=2223\n", 0o600)
	freshFake := &testkit.Incus{Instances: make(map[string]ports.InstanceInfo)}
	freshProgram, err := New(Options{
		RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root,
		Incus: freshFake, Executor: freshFake, Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	freshLoaded, err := freshProgram.resolveReleaseTransitionContext(
		testyardmigration.CurrentYard, freshConfigHome,
	)
	if err != nil {
		t.Fatal(err)
	}
	freshFake.Instances[freshLoaded.Context.IncusProject+"/"+freshLoaded.Context.YardInstanceName] = ports.InstanceInfo{
		Name:    freshLoaded.Context.YardInstanceName,
		Project: freshLoaded.Context.IncusProject,
		Status:  "Running",
	}
	freshFake.ExecSteps = []testkit.IncusExecStep{{Result: ports.InstanceExecResult{
		Stdout:   []byte(strings.Repeat("0", 64) + "  /home/dev/.codex/rules/repo.rules\n"),
		ExitCode: 0,
	}}}
	frozen := &materializedConfigActivationReconciler{
		cli: freshProgram, yard: "default", configHome: freshConfigHome,
		goal: releasetransition.Goal{
			Target: request.Target, Direction: request.Direction,
		},
		artifactDigest: request.ArtifactDigest,
		registryDigest: request.RegistryDigest,
	}
	beforeJournal, err := frozen.Observe(
		context.Background(), activeRelease, releasetransition.ReleaseLinks{},
	)
	if err != nil || beforeJournal.Converged || !frozen.allLocal {
		t.Fatalf("observe before journal: %#v err=%v", beforeJournal, err)
	}
	freshStore, err := releasetransition.NewPOSIXV2Store(freshConfigHome)
	if err != nil {
		t.Fatal(err)
	}
	missingLedger, err := freshStore.ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	if err := freshStore.CompareAndSwapLedger(missingLedger, completedLedger.Payload); err != nil {
		t.Fatal(err)
	}
	freshFake.ExecSteps = append(freshFake.ExecSteps, testkit.IncusExecStep{Result: ports.InstanceExecResult{
		Stdout:   []byte(strings.Repeat("0", 64) + "  /home/dev/.codex/rules/repo.rules\n"),
		ExitCode: 0,
	}})
	freshRuntimeRoot := filepath.Join(root, "fresh-runtime")
	if err := os.MkdirAll(filepath.Join(freshRuntimeRoot, "releases", "release-b"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/release-b", filepath.Join(freshRuntimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
	activationRequest := request
	activationRequest.Mode = releasetransition.ProcessInspect
	activationRequest.RuntimeRoot = freshRuntimeRoot
	activationRequest.ConfigHome = freshConfigHome
	activationRequest.Execution = nil
	drift := &processDriftReconciler{failOnce: true}
	activationInspection, err := executeReleaseTransitionRequest(
		context.Background(), root, activationRequest,
		func(releasetransition.PlanToken, releasetransition.Authorization) bool { return true },
		[]releasetransition.V2ActivationReconciler{drift}, releaseTransitionOwnerFixture{}, nil,
	)
	if err != nil || activationInspection.Inspection == nil {
		t.Fatalf("inspect activation-only transition: %#v err=%v", activationInspection, err)
	}
	activationRequest.Mode = releasetransition.ProcessConverge
	activationRequest.Execution = &releasetransition.Execution{
		Plan: activationInspection.Inspection.Plan, Authorization: "fixture-authorization",
	}
	interrupted, err := executeReleaseTransitionRequest(
		context.Background(), root, activationRequest,
		func(releasetransition.PlanToken, releasetransition.Authorization) bool { return true },
		[]releasetransition.V2ActivationReconciler{drift}, releaseTransitionOwnerFixture{}, nil,
	)
	if err != nil || interrupted.Outcome == nil ||
		interrupted.Outcome.Status != releasetransition.StatusRecovering {
		t.Fatalf("interrupt activation-only transition: %#v err=%v", interrupted, err)
	}
	activationSnapshot, err := freshStore.ReadCurrentJournal()
	if err != nil || !activationSnapshot.Exists {
		t.Fatalf("read activation-only journal: exists=%t err=%v", activationSnapshot.Exists, err)
	}
	activationJournal, err := releasetransition.ParseJournal(activationSnapshot.Payload)
	if err != nil || len(activationJournal.Steps) != 0 ||
		activationJournal.Releases.From != activationJournal.Releases.Target {
		t.Fatalf("activation-only journal=%#v err=%v", activationJournal, err)
	}
	afterJournal, err := frozen.Observe(
		context.Background(), activeRelease, releasetransition.ReleaseLinks{},
	)
	if err != nil || afterJournal != beforeJournal || !frozen.allLocal {
		t.Fatalf("same-release catch-up did not retain all-local scope: before=%#v after=%#v allLocal=%t err=%v",
			beforeJournal, afterJournal, frozen.allLocal, err)
	}
}

func TestReleaseTransitionConfigApplierUsesTrustedInProcessChild(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	writeCLIFile(t, filepath.Join(root, "config", "subyard.env"), strings.Join([]string{
		"SHIFT_MODE=shift",
		"FORWARD_SSH_AGENT=0",
		"DEV_SUDO=0",
		"DEV_UID=1000",
		"DEV_USER=dev",
		"SSH_PORT=2222",
		"STORAGE_PATH=" + filepath.Join(root, "data", "storage"),
		"HOST_BASE=" + filepath.Join(root, "host"),
		"RESTRICTED_DISK_PATHS=" + filepath.Join(root, "host"),
	}, "\n")+"\n", 0o600)
	runtimeRoot := filepath.Join(root, "runtime")
	environment = append(environment,
		"YARD_RUNTIME_ROOT="+runtimeRoot,
		"SUBYARD_OPERATION_ID=release-transition-config",
	)
	installUnfinishedMutationGateFixture(t, root, environment, runtimeRoot)
	platform := newInitPlatformFixture()
	platform.configsConverged = false
	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Environment: environment, WorkingDir: root,
		InitPlatform: platform, Stdout: io.Discard, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := program.inspectMutationGate(context.Background(), "default")
	if err != nil || outcome == nil {
		t.Fatalf("unfinished transition gate outcome=%#v err=%v", outcome, err)
	}
	reconciler := &materializedConfigActivationReconciler{cli: program}
	if err := reconciler.reconcileCLI().options.Config.ApplyConfig(
		context.Background(), "default",
	); err != nil {
		t.Fatalf("trusted config apply: %v, stderr=%q", err, stderr.String())
	}
	if platform.configs != 1 || strings.Contains(stderr.String(), "transition-required") {
		t.Fatalf("trusted config apply configs=%d stderr=%q", platform.configs, stderr.String())
	}
}

func TestMaterializedConfigActivationUsesTrustedProcessChild(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	source := filepath.Join(root, "repo.rules")
	writeCLIFile(t, source, "allow_rule()\n", 0o600)
	writeCLIFile(t, filepath.Join(root, "config", "subyard.env"), strings.Join([]string{
		"SHIFT_MODE=shift",
		"FORWARD_SSH_AGENT=0",
		"DEV_SUDO=0",
		"DEV_UID=1000",
		"DEV_USER=dev",
		"SSH_PORT=2222",
		"STORAGE_PATH=" + filepath.Join(root, "data", "storage"),
		"HOST_BASE=" + filepath.Join(root, "host"),
		"RESTRICTED_DISK_PATHS=" + filepath.Join(root, "host"),
		"CODING_TOOL_INTEGRATIONS=codex",
		"AGENT_codex_RULES=" + source,
		"AGENT_codex_RULES_DEST=.codex/rules/repo.rules",
	}, "\n")+"\n", 0o600)
	runtimeRoot := filepath.Join(root, "runtime")
	configHome := environmentValue(environment, "SUBYARD_CONFIG_HOME")
	environment = append(environment,
		"YARD_RUNTIME_ROOT="+runtimeRoot,
		"SUBYARD_OPERATION_ID=materialized-config-activation",
	)
	installUnfinishedMutationGateFixture(t, root, environment, runtimeRoot)
	dispatcher := filepath.Join(root, "bin", "yard-engine")
	if err := os.MkdirAll(filepath.Dir(dispatcher), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, dispatcher, `#!/bin/sh
printf '%s\n' '{"status":"migration-required","code":"transition-required"}' >&2
exit 1
`, 0o700)
	platform := newInitPlatformFixture()
	platform.configsConverged = false
	fake := &testkit.Incus{Instances: make(map[string]ports.InstanceInfo)}
	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, DispatcherPath: dispatcher, Program: "yard",
		Environment: environment, WorkingDir: root,
		InitPlatform: platform, Incus: fake, Executor: fake,
		Stdout: io.Discard, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.resolveReleaseTransitionContext("default", configHome)
	if err != nil {
		t.Fatal(err)
	}
	fake.Instances[loaded.Context.IncusProject+"/"+loaded.Context.YardInstanceName] = ports.InstanceInfo{
		Name: loaded.Context.YardInstanceName, Project: loaded.Context.IncusProject, Status: "Running",
	}
	digest := sha256.Sum256([]byte("allow_rule()\n"))
	matching := fmt.Sprintf("%x  /home/dev/.codex/rules/repo.rules\n", digest)
	for _, output := range []string{
		strings.Repeat("0", 64) + "  /home/dev/.codex/rules/repo.rules\n",
		strings.Repeat("0", 64) + "  /home/dev/.codex/rules/repo.rules\n",
		matching,
	} {
		fake.ExecSteps = append(fake.ExecSteps, testkit.IncusExecStep{
			Result: ports.InstanceExecResult{Stdout: []byte(output), ExitCode: 0},
		})
	}
	reconciler := &materializedConfigActivationReconciler{
		cli: program, yard: "default", configHome: configHome,
	}
	if err := reconciler.Reconcile(context.Background(), releasetransition.ReleaseLinks{}); err != nil {
		t.Fatalf("materialized config activation: %v, stderr=%q", err, stderr.String())
	}
	if platform.configs != 1 || strings.Contains(stderr.String(), "transition-required") {
		t.Fatalf("materialized config activation configs=%d stderr=%q", platform.configs, stderr.String())
	}
}

func TestActivationStageReconcilerRepairsOnlyItsStableStage(t *testing.T) {
	platform := newInitPlatformFixture()
	platform.converged[ports.ReconcileStageTestVMs] = false
	authorized := 0
	reconciler := &activationStageReconciler{
		id: "test-vm-broker", stage: ports.ReconcileStageTestVMs,
		inspectApplicability: func(context.Context) (activationApplicability, error) {
			return activationApplicability{state: "active", applies: true}, nil
		},
		platform: func(context.Context, activationApplicability) (ports.ReconcileStageRunner, error) {
			return platform, nil
		},
		authorize: func(context.Context) error {
			authorized++
			return nil
		},
	}
	pair := releasetransition.ReleasePair{From: "release-a", Target: "release-b"}
	links := releasetransition.ReleaseLinks{Active: "release-b"}
	observed, err := reconciler.Observe(context.Background(), pair, links)
	if err != nil || observed.Converged || observed.Actual == observed.Desired {
		t.Fatalf("observation = %#v, err=%v", observed, err)
	}
	if err := reconciler.Reconcile(context.Background(), links); err != nil {
		t.Fatal(err)
	}
	if authorized != 1 || !slices.Equal(platform.applied,
		[]ports.ReconcileStageID{ports.ReconcileStageTestVMs}) {
		t.Fatalf("authorized=%d applied=%v", authorized, platform.applied)
	}
	observed, err = reconciler.Observe(context.Background(), pair, links)
	if err != nil || !observed.Converged || observed.Actual != observed.Desired {
		t.Fatalf("fixed point = %#v, err=%v", observed, err)
	}
	if err := reconciler.Reconcile(context.Background(), links); err != nil {
		t.Fatal(err)
	}
	if authorized != 1 || len(platform.applied) != 1 {
		t.Fatalf("fixed point reapplied: authorized=%d applied=%v", authorized, platform.applied)
	}
}

func TestActivationStageReconcilerUsesInstallVerificationAsItsFixedPoint(t *testing.T) {
	platform := &activationVerificationFixture{}
	reconciler := &activationStageReconciler{
		id: "host-power", stage: ports.ReconcileStagePower,
		inspectApplicability: func(context.Context) (activationApplicability, error) {
			return activationApplicability{state: "installed", applies: true}, nil
		},
		platform: func(context.Context, activationApplicability) (ports.ReconcileStageRunner, error) {
			return platform, nil
		},
	}
	pair := releasetransition.ReleasePair{From: "release-a", Target: "release-b"}
	links := releasetransition.ReleaseLinks{Active: "release-b"}
	observed, err := reconciler.Observe(context.Background(), pair, links)
	if err != nil || !observed.Converged || observed.Actual != observed.Desired {
		t.Fatalf("verified installation observation = %#v, err=%v", observed, err)
	}
	if err := reconciler.Reconcile(context.Background(), links); err != nil {
		t.Fatal(err)
	}
	if platform.applied != 0 {
		t.Fatalf("verified host installation was reapplied %d times", platform.applied)
	}
}

func TestActivationStageReconcilerReportsPrivateApplyDiagnostics(t *testing.T) {
	platform := newInitPlatformFixture()
	platform.converged[ports.ReconcileStageTestVMs] = false
	platform.applyErr = errors.New("bounded private detail")
	var diagnostics bytes.Buffer
	reconciler := &activationStageReconciler{
		id: "test-vm-broker", stage: ports.ReconcileStageTestVMs,
		inspectApplicability: func(context.Context) (activationApplicability, error) {
			return activationApplicability{state: "active", applies: true}, nil
		},
		platform: func(context.Context, activationApplicability) (ports.ReconcileStageRunner, error) {
			return platform, nil
		},
		diagnostics: &diagnostics,
	}
	err := reconciler.Reconcile(context.Background(), releasetransition.ReleaseLinks{})
	var phased interface{ ActivationPhase() string }
	if !errors.As(err, &phased) || phased.ActivationPhase() != "apply" ||
		!strings.Contains(diagnostics.String(),
			`activation reconciler "test-vm-broker" apply: bounded private detail`) {
		t.Fatalf("apply error=%v diagnostics=%q", err, diagnostics.String())
	}
}

func TestBrokerActivationPlatformIgnoresInheritedDefaultContext(t *testing.T) {
	repositoryRoot := repositoryRoot(t)
	home := t.TempDir()
	configHome := filepath.Join(home, ".config", "subyard")
	bin := filepath.Join(home, "bin")
	incus := filepath.Join(bin, "incus")
	writeReleaseTransitionTestFile(t, incus, []byte(`#!/bin/sh
set -eu
case "$*" in
  "project list --format=json")
    printf '%s\n' '[{"name":"subyard-test-yard"}]'
    ;;
  "list yard-test-yard --project subyard-test-yard --format=json")
    printf '%s\n' '[{"name":"yard-test-yard","status":"RUNNING","config":{"user.subyard.desired_power":"running"}}]'
    ;;
  "exec yard-test-yard --project subyard-test-yard -- systemctl is-active subyard-test-vms-broker.service")
    printf '%s\n' active
    ;;
  *)
    printf 'unexpected incus call: %s\n' "$*" >&2
    exit 2
    ;;
esac
`), 0o700)
	t.Setenv("PATH", bin)
	writeReleaseTransitionTestFile(
		t,
		filepath.Join(configHome, "yards", testyardmigration.CurrentYard, "config.env"),
		[]byte("YARD_TEMPLATE=test-vms\nSSH_PORT=2224\n"),
		0o600,
	)
	program, err := New(Options{
		RepositoryRoot: repositoryRoot,
		Environment: []string{
			"HOME=" + home,
			"PATH=" + bin,
			"SUBYARD_OPERATOR_HOME=" + home,
			"SUBYARD_CONFIG_HOME=" + configHome,
			"SUBYARD_HOME=" + filepath.Join(home, ".subyard"),
			"SUBYARD_CONFIG_LOADED=1",
			"SUBYARD_ENGINE_CONTEXT=1",
			"SUBYARD_ENGINE_CONTEXT_SCHEMA=1",
			"SUBYARD_YARD=default",
			"NESTED_E2E_VMS=0",
			"YARD_INSTANCE_NAME=yard",
			"INCUS_PROJECT=subyard",
			"SSH_HOST=yard",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	reconciler, ok := program.brokerActivationReconciler(
		releasetransition.ProcessRequest{Yard: "default", ConfigHome: configHome},
	).(*activationStageReconciler)
	if !ok {
		t.Fatal("broker activation reconciler has an unexpected implementation")
	}
	applicability, err := reconciler.inspectApplicability(context.Background())
	if err != nil || !applicability.applies || applicability.state != "active" ||
		applicability.target != testyardmigration.CurrentYard {
		t.Fatalf("broker applicability retained default context: %#v, err=%v", applicability, err)
	}
	platform, err := reconciler.platform(context.Background(), activationApplicability{
		target: testyardmigration.CurrentYard,
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime, ok := platform.(reconcileruntime.Runtime)
	if !ok {
		t.Fatalf("broker platform type = %T", platform)
	}
	if runtime.Yard.YardName != testyardmigration.CurrentYard ||
		runtime.Yard.IncusProject != "subyard-test-yard" ||
		runtime.Yard.YardInstanceName != "yard-test-yard" {
		t.Fatalf("broker platform retained default context: %#v", runtime.Yard)
	}
	want := filepath.Join(repositoryRoot, "bin", "yard-engine")
	if !slices.Contains(runtime.Environment, "SUBYARD_DISPATCHER_PATH="+want) {
		t.Fatalf("broker platform does not use pinned engine %q: %q", want, runtime.Environment)
	}
}

type activationVerificationFixture struct{ applied int }

func (*activationVerificationFixture) CheckStage(context.Context, ports.ReconcileStageID) (bool, error) {
	return false, nil
}

func (fixture *activationVerificationFixture) ApplyStage(context.Context, ports.ReconcileStageID) error {
	fixture.applied++
	return nil
}

func (*activationVerificationFixture) VerifyStage(context.Context, ports.ReconcileStageID) (bool, error) {
	return true, nil
}

func TestInspectPowerActivationApplicabilityRejectsSymlinkAndPreservesAbsence(t *testing.T) {
	root := t.TempDir()
	reconcilerPath := filepath.Join(root, "yard-boot-reconcile")
	unitPath := filepath.Join(root, "subyard-power-reconcile.service")
	environment := map[string]string{
		"SUBYARD_POWER_RECONCILER_PATH": reconcilerPath,
		"SUBYARD_POWER_UNIT_PATH":       unitPath,
	}
	state, err := inspectPowerActivationApplicability(environment)
	if err != nil || state.state != "absent" || state.applies {
		t.Fatalf("absent state = %#v, err=%v", state, err)
	}
	writeReleaseTransitionTestFile(t, reconcilerPath, []byte("engine\n"), 0o700)
	state, err = inspectPowerActivationApplicability(environment)
	if err != nil || state.state != "installed" || !state.applies {
		t.Fatalf("installed state = %#v, err=%v", state, err)
	}
	if err := os.Remove(reconcilerPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("elsewhere", reconcilerPath); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectPowerActivationApplicability(environment); err == nil ||
		!strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("symlink error = %v", err)
	}
}

func writeReleaseTransitionTestFile(t *testing.T, path string, payload []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, payload, mode); err != nil {
		t.Fatal(err)
	}
}
