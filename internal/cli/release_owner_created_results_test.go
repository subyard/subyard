package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"errors"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releaseowner"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/testyardmigration"
	"reflect"
)

func ownerResultCapsuleFixture(t *testing.T) (*releaseOwnerComponents, releaseowner.OwnerBefore, []ports.NativeOwnerCreated) {
	t.Helper()
	hash := strings.Repeat("a", 64)
	before := releaseowner.OwnerBefore{State: "legacy-directory", Registration: hash, Overrides: hash, Controller: hash}
	instance := func(scope, project, name string) *ports.PreparedOwnerInstance {
		return &ports.PreparedOwnerInstance{Scope: scope, Project: project, Instance: name, Server: hash}
	}
	volume := func(scope, project, name string) *ports.PreparedOwnerVolume {
		return &ports.PreparedOwnerVolume{Scope: scope, Project: project, Instance: name, Pool: "original", Volume: name + "-srv", Server: hash, Absent: true}
	}
	record := ownerChildrenRecord{Version: 1, Before: before, Projects: []ports.PreparedImageProject{{Project: "subyard-test-yard", Absent: true, Server: hash}, {Project: "subyard-e2e-yard", Server: hash}}, Inits: []ownerInitChildRecord{
		{Yard: testyardmigration.CurrentYard, Instance: instance("current", "subyard-test-yard", "yard-test-yard"), Volume: volume("current", "subyard-test-yard", "yard-test-yard")},
		{Yard: testyardmigration.LegacyYard, Volume: volume("legacy", "subyard-e2e-yard", "yard-e2e-yard")},
		{Yard: testyardmigration.LegacyYard, Kind: "recreate", Project: &ports.PreparedImageProject{Project: "subyard-e2e-yard", Absent: true, Server: hash}, Instance: instance("recreate", "subyard-e2e-yard", "yard-e2e-yard"), Volume: volume("recreate", "subyard-e2e-yard", "yard-e2e-yard")},
	}}
	metadata, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	wire := ownerChildrenWire{Version: 1, Owner: "owner-registration", Files: []ownerRecipeFile{{Path: "prepared/owner-children.json", Content: metadata}}}
	raw, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	prepared := &preparedOwnerChildren{before: before, raw: raw, wire: wire}
	before.PreparedRecipe = prepared.binding()
	component := &releaseOwnerComponents{ownerChildren: prepared, installedBound: map[string]bool{before.PreparedRecipe: true}}
	result := ports.NativeOwnerCreated{Ref: ports.NativeOwnerCreateRef{Producer: hash, Domain: "operation", Index: 2}, Intent: ports.NativeOwnerCreateIntent{Scope: "current", Kind: "instance", Server: hash, Project: "subyard-test-yard", Resource: "yard-test-yard", Fingerprint: hash, Token: strings.Repeat("1", 32)}, InstanceUUID: "original-created-uuid", Status: 200}
	return component, before, []ports.NativeOwnerCreated{result}
}
func TestNativeOwnerResultCapsuleMatchesOnlyOriginalAbsentTuple(t *testing.T) {
	for _, change := range []string{"valid", "foreign-project", "foreign-resource", "foreign-server", "wrong-scope", "missing-recipe", "duplicate-ref", "preexisting-project", "foreign-pool"} {
		t.Run(change, func(t *testing.T) {
			component, before, values := ownerResultCapsuleFixture(t)
			switch change {
			case "foreign-project":
				values[0].Intent.Project = "foreign"
			case "foreign-resource":
				values[0].Intent.Resource = "foreign"
			case "foreign-server":
				values[0].Intent.Server = strings.Repeat("b", 64)
			case "wrong-scope":
				values[0].Intent.Scope = "legacy"
			case "missing-recipe":
				component.installedBound = nil
			case "duplicate-ref":
				values = append(values, values[0])
			case "preexisting-project":
				values[0].Intent.Scope = "legacy"
				values[0].Intent.Kind = "project"
				values[0].Intent.Project = "subyard-e2e-yard"
				values[0].Intent.Resource = "subyard-e2e-yard"
				values[0].Ref.Domain = "command"
			case "foreign-pool":
				values[0].Intent.Kind = "srv-volume"
				values[0].Intent.Resource = "yard-test-yard-srv"
				values[0].Intent.Pool = "foreign"
				values[0].Ref.Domain = "command"
			}
			err := component.BindOwnerResults(ownerResultTestContext(values), before, strings.Repeat("1", 32), values)
			if change != "valid" {
				if err != domain.ErrPlanStale || len(component.ownerResults) != 0 {
					t.Fatal("foreign tuple attached", err)
				}
				return
			}
			if err != nil || len(component.ownerResults) != 1 || component.ownerResults[0] != values[0] {
				t.Fatal("original tuple lost", err)
			}
			values[0].InstanceUUID = "caller-mutated"
			if component.ownerResults[0].InstanceUUID == values[0].InstanceUUID {
				t.Fatal("result slice alias")
			}
		})
	}
}
func TestNativeOwnerResultRecreateCannotAliasOriginalLegacyRole(t *testing.T) {
	component, before, values := ownerResultCapsuleFixture(t)
	value := values[0]
	value.Intent.Scope = "recreate"
	value.Intent.Project = "subyard-e2e-yard"
	value.Intent.Resource = "yard-e2e-yard"
	if err := component.BindOwnerResults(ownerResultTestContext([]ports.NativeOwnerCreated{value}), before, strings.Repeat("1", 32), []ports.NativeOwnerCreated{value}); err != nil {
		t.Fatal(err)
	}
	value.Intent.Scope = "legacy"
	component.ownerResults = nil
	if err := component.BindOwnerResults(ownerResultTestContext([]ports.NativeOwnerCreated{value}), before, strings.Repeat("1", 32), []ports.NativeOwnerCreated{value}); err != domain.ErrPlanStale {
		t.Fatal("recreate inherited original legacy role", err)
	}
}

type ownerResultTestReader struct {
	ports.NativeOperationObserver
	values  []ports.NativeOwnerCreated
	missing bool
}

func (r *ownerResultTestReader) ReadOwnerCreated(ref ports.NativeOwnerCreateRef) (ports.NativeOwnerCreated, error) {
	if !r.missing {
		for _, value := range r.values {
			if value.Ref == ref {
				return value, nil
			}
		}
	}
	return ports.NativeOwnerCreated{}, errors.New("original result unavailable")
}
func ownerResultTestContext(values []ports.NativeOwnerCreated) context.Context {
	return ports.WithNativeOperationObserver(context.Background(), &ownerResultTestReader{values: append([]ports.NativeOwnerCreated(nil), values...)})
}
func TestNativeOwnerResultTargetProjectionKeepsOriginalAuthority(t *testing.T) {
	for _, change := range []string{"valid", "wrong-uuid", "wrong-token", "lost-fact", "ordinary-target", "foreign-target"} {
		t.Run(change, func(t *testing.T) {
			component, before, values := ownerResultCapsuleFixture(t)
			reader := &ownerResultTestReader{values: values}
			ctx := ports.WithNativeOperationObserver(context.Background(), reader)
			if err := component.BindOwnerResults(ctx, before, strings.Repeat("1", 32), values); err != nil {
				t.Fatal(err)
			}
			value := values[0]
			instance := ports.InstanceInfo{Project: value.Intent.Project, Name: value.Intent.Resource, Config: map[string]string{"volatile.uuid": value.InstanceUUID, "user.subyard.owner_create": value.Intent.Token}}
			switch change {
			case "wrong-uuid":
				instance.Config["volatile.uuid"] = "foreign-uuid"
			case "wrong-token":
				instance.Config["user.subyard.owner_create"] = strings.Repeat("2", 32)
			case "lost-fact":
				reader.missing = true
			}
			incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{instance.Project + "/" + instance.Name: instance}}
			component.cli = &CLI{options: Options{Incus: incus}}
			original := releaseowner.Target{Owner: "synthetic", Class: "profile-runtime", Yard: "test-yard", Project: value.Intent.Project, Instance: value.Intent.Resource, InstanceUUID: "absent", Source: strings.Repeat("b", 64), PreparedRecipe: strings.Repeat("c", 64), Actual: strings.Repeat("d", 64), Desired: strings.Repeat("e", 64)}
			if change == "ordinary-target" {
				original.InstanceUUID = "original-preexisting"
			}
			if change == "foreign-target" {
				original.Instance = "foreign"
			}
			resolved, err := component.ResolveOwnerTarget(context.Background(), original)
			if change == "wrong-uuid" || change == "wrong-token" || change == "lost-fact" {
				if err != domain.ErrPlanStale {
					t.Fatal("foreign physical identity admitted", err)
				}
				return
			}
			expected := original
			if change == "valid" {
				expected.InstanceUUID = value.InstanceUUID
			}
			if err != nil || !reflect.DeepEqual(resolved, expected) {
				t.Fatal("projection changed original authority", err)
			}
			if original.InstanceUUID == value.InstanceUUID {
				t.Fatal("approved target mutated")
			}
		})
	}
}

func TestNativeCreatedActivationKeepsOriginalStableSource(t *testing.T) {
	original := releaseowner.Target{Owner: "owner", Yard: "default", Class: "profile-runtime", Scope: "original", Source: strings.Repeat("a", 64), Desired: strings.Repeat("b", 64), Project: "project", Instance: "yard", InstanceUUID: "original-created-uuid", PreparedRecipe: strings.Repeat("c", 64)}
	for _, field := range []string{"progress", "source", "desired", "scope", "uuid", "project", "instance"} {
		t.Run(field, func(t *testing.T) {
			current := original
			switch field {
			case "progress":
				current.State = "current"
				current.Actual = "actual"
				current.Before = "new-own-baseline"
				current.PreparedRecipe = strings.Repeat("d", 64)
			case "source":
				current.Source = strings.Repeat("d", 64)
			case "desired":
				current.Desired = strings.Repeat("d", 64)
			case "scope":
				current.Scope = "foreign"
			case "uuid":
				current.InstanceUUID = "foreign"
			case "project":
				current.Project = "foreign"
			case "instance":
				current.Instance = "foreign"
			}
			if sameCreatedActivationSource(current, original) != (field == "progress") {
				t.Fatal("stable target scope changed", field)
			}
		})
	}
}

func TestNativeOwnerResultOptionalFingerprintKeepsAuthenticatedIdentity(t *testing.T) {
	for _, change := range []string{"empty-fingerprint", "known-fingerprint", "foreign-server", "foreign-project", "foreign-resource", "foreign-scope", "foreign-pool", "foreign-domain", "wrong-ref", "wrong-producer", "forged-request", "wrong-uuid", "unsafe-uuid", "missing-result"} {
		t.Run(change, func(t *testing.T) {
			component, before, values := ownerResultCapsuleFixture(t)
			if change != "known-fingerprint" {
				values[0].Intent.Fingerprint = ""
			}
			values[0].Intent.Domain = "operation"
			values[0].Intent.RequestDigest = strings.Repeat("d", 64)
			values[0].OperationID = "11111111-2222-4333-8444-555555555555"
			values[0].IdentityDigest = strings.Repeat("e", 64)
			values[0].ETagDigest = strings.Repeat("f", 64)
			reader := &ownerResultTestReader{values: append([]ports.NativeOwnerCreated(nil), values...)}
			switch change {
			case "foreign-server":
				values[0].Intent.Server = strings.Repeat("b", 64)
			case "foreign-project":
				values[0].Intent.Project = "foreign"
			case "foreign-resource":
				values[0].Intent.Resource = "foreign"
			case "foreign-scope":
				values[0].Intent.Scope = "legacy"
			case "foreign-pool":
				values[0].Intent.Pool = "foreign"
			case "foreign-domain":
				values[0].Ref.Domain = "command"
			case "wrong-ref":
				values[0].Ref.Index++
			case "wrong-producer":
				values[0].Ref.Producer = strings.Repeat("b", 64)
			case "forged-request":
				values[0].Intent.RequestDigest = strings.Repeat("b", 64)
			case "wrong-uuid":
				values[0].InstanceUUID = "foreign-uuid"
			case "unsafe-uuid":
				values[0].InstanceUUID = "../foreign"
			case "missing-result":
				reader.missing = true
			}
			ctx := ports.WithNativeOperationObserver(context.Background(), reader)
			err := component.BindOwnerResults(ctx, before, strings.Repeat("1", 32), values)
			if change != "empty-fingerprint" && change != "known-fingerprint" {
				if !errors.Is(err, domain.ErrPlanStale) || component.ownerResultReader != nil || len(component.ownerResults) != 0 {
					t.Fatal("untrusted result bound", err)
				}
				return
			}
			if err != nil {
				t.Fatal("authenticated optional fingerprint rejected", err)
			}
			if got, err := component.checkedOwnerResults(); err != nil || !reflect.DeepEqual(got, values) {
				t.Fatal("original result changed", err)
			}
			reader.missing = true
			if _, err := component.checkedOwnerResults(); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatal("lost immutable result reused", err)
			}
		})
	}
}

func TestNativeOwnerEmptyFingerprintRestoresAcrossFreshProcess(t *testing.T) {
	ctx := context.Background()
	if root := os.Getenv("SUBYARD_OWNER_EMPTY_FP_RECORD"); root != "" {
		raw, err := os.ReadFile(filepath.Join(root, "capsule.json"))
		if err != nil {
			t.Fatal(err)
		}
		var wire ownerChildrenWire
		if json.Unmarshal(raw, &wire) != nil {
			t.Fatal("original capsule")
		}
		var record ownerChildrenRecord
		for _, file := range wire.Files {
			if file.Path == "prepared/owner-children.json" {
				if json.Unmarshal(file.Content, &record) != nil {
					t.Fatal("original record")
				}
			}
		}
		prepared := &preparedOwnerChildren{before: record.Before, raw: raw, wire: wire}
		before := record.Before
		before.PreparedRecipe = prepared.binding()
		component := &releaseOwnerComponents{ownerChildren: prepared, installedBound: map[string]bool{before.PreparedRecipe: true}}
		raw, err = os.ReadFile(filepath.Join(root, "results.json"))
		if err != nil {
			t.Fatal(err)
		}
		var values []ports.NativeOwnerCreated
		if json.Unmarshal(raw, &values) != nil || len(values) != 1 || values[0].Intent.Fingerprint != "" {
			t.Fatal("original typed result")
		}
		if err = component.BindOwnerResults(ownerResultTestContext(values), before, strings.Repeat("1", 32), values); err != nil {
			t.Fatal("fresh restoration", err)
		}
		if got, err := component.checkedOwnerResults(); err != nil || !reflect.DeepEqual(got, values) {
			t.Fatal("fresh original identity", err)
		}
		return
	}
	component, before, values := ownerResultCapsuleFixture(t)
	values[0].Intent.Fingerprint = ""
	values[0].Intent.Domain = "operation"
	values[0].Intent.RequestDigest = strings.Repeat("d", 64)
	if err := component.BindOwnerResults(ownerResultTestContext(values), before, strings.Repeat("1", 32), values); err != nil {
		t.Fatal("original binding", err)
	}
	root := testkit.TempDir(t)
	testkit.WriteFile(t, filepath.Join(root, "capsule.json"), component.ownerChildren.raw, 0600)
	raw, _ := json.Marshal(values)
	testkit.WriteFile(t, filepath.Join(root, "results.json"), raw, 0600)
	process := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNativeOwnerEmptyFingerprintRestoresAcrossFreshProcess$")
	process.Env = append(os.Environ(), "SUBYARD_OWNER_EMPTY_FP_RECORD="+root)
	if output, err := process.CombinedOutput(); err != nil {
		t.Fatalf("fresh process: %v %s", err, output)
	}
}
