//go:build linux

package releaseowner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func ownerCreateFixture(t *testing.T) (*invocationObserver, string) {
	t.Helper()
	s, b, path := nativeFactFixture(t)
	intent := Intent{1, b, "owner-registration", "test-yard", b.Artifact, b.Artifact, b.Artifact, b.Artifact}
	if err := s.writeFact(b, "intent", intent); err != nil {
		t.Fatal(err)
	}
	if err := s.writeFact(b, "accepted", Terminal{Version: 1, Binding: b, Owner: intent.Owner, Resource: intent.Resource, State: "accepted", Operations: []ports.NativeOperation{}}); err != nil {
		t.Fatal(err)
	}
	return &invocationObserver{session: s, binding: b, ownerToken: strings.Repeat("1", 32)}, path
}

func ownerProjectCreateIntent(o *invocationObserver, scope, project string) ports.NativeOwnerCreateIntent {
	return ports.NativeOwnerCreateIntent{Scope: scope, Kind: "project", Domain: "command", Token: strings.Repeat("2", 32), RequestDigest: o.binding.Artifact, Server: o.binding.Artifact, Project: project, Resource: project}
}

func completeOwnerProject(t *testing.T, o *invocationObserver, intent ports.NativeOwnerCreateIntent) ports.NativeOwnerCreated {
	t.Helper()
	ref, err := o.BeforeOwnerCreate(intent)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.CommandTerminal(ref.Index); err != nil {
		t.Fatal(err)
	}
	value := ports.NativeOwnerCreated{Ref: ref, Intent: intent, Status: 200, IdentityDigest: o.binding.Artifact, ETagDigest: o.binding.Artifact}
	if err := o.RecordOwnerCreated(value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestNativeOwnerCreateUsesOriginalIndicesAndKeepsMissingResultUnknown(t *testing.T) {
	o, path := ownerCreateFixture(t)
	prior, err := o.CommandStarted("readonly-closure-fixture", o.binding.Artifact)
	if err != nil || o.CommandTerminal(prior) != nil {
		t.Fatal(err)
	}
	current := completeOwnerProject(t, o, ownerProjectCreateIntent(o, "current", "subyard-test-yard"))
	legacy := completeOwnerProject(t, o, ownerProjectCreateIntent(o, "legacy", "subyard-e2e-yard"))
	if current.Ref.Index != 2 || legacy.Ref.Index != 3 || current.Ref == legacy.Ref {
		t.Fatal("canonical and compensation creates reused a reservation")
	}
	for _, want := range []ports.NativeOwnerCreated{current, legacy} {
		got, err := o.ReadOwnerCreated(want.Ref)
		if err != nil || got != want {
			t.Fatal("exact original result lost", err)
		}
	}
	if err := o.closed(); err != nil {
		t.Fatal(err)
	}
	// Export only actual producer facts for the paired Rust parser corpus.
	if output := os.Getenv("SUBYARD_NATIVE_OWNER_CREATE_CORPUS"); output != "" {
		if err := o.session.writeFact(o.binding, "terminal", Terminal{Version: 1, Binding: o.binding, Owner: "owner-registration", Resource: "test-yard", State: "complete", Operations: []ports.NativeOperation{}}); err != nil {
			t.Fatal(err)
		}
		type file struct {
			Name string `json:"name"`
			Raw  []byte `json:"raw"`
		}
		var files []file
		entries, err := os.ReadDir(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			raw, err := os.ReadFile(filepath.Join(path, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, file{entry.Name(), raw})
		}
		raw, err := json.Marshal([]struct {
			Name        string `json:"name"`
			Transaction string `json:"transaction"`
			Files       []file `json:"files"`
			Accepted    bool   `json:"accepted"`
		}{{"actual-indexed-canonical-compensation", o.binding.Transaction, files, true}})
		if err != nil || os.WriteFile(output, raw, 0600) != nil {
			t.Fatal("cannot export owning creation corpus", err)
		}
	}

	missing, _ := ownerCreateFixture(t)
	ref, err := missing.BeforeOwnerCreate(ownerProjectCreateIntent(missing, "current", "subyard-test-yard"))
	if err != nil || missing.CommandTerminal(ref.Index) != nil {
		t.Fatal(err)
	}
	if missing.closed() == nil {
		t.Fatal("closed command hid missing own-create result")
	}
	if _, err := missing.ReadOwnerCreated(ref); err == nil {
		t.Fatal("missing result inferred from resource name")
	}
}

func TestNativeOwnerCreateRefusesForeignOrConflictingResults(t *testing.T) {
	for _, change := range []string{"before-response", "token", "ref", "identity", "uuid-on-sync", "collision", "uncertain"} {
		t.Run(change, func(t *testing.T) {
			o, _ := ownerCreateFixture(t)
			intent := ownerProjectCreateIntent(o, "current", "subyard-test-yard")
			ref, err := o.BeforeOwnerCreate(intent)
			if err != nil {
				t.Fatal(err)
			}
			if change != "before-response" && o.CommandTerminal(ref.Index) != nil {
				t.Fatal("command terminal failed")
			}
			result := ports.NativeOwnerCreated{Ref: ref, Intent: intent, Status: 200, IdentityDigest: o.binding.Artifact, ETagDigest: o.binding.Artifact}
			switch change {
			case "token":
				result.Intent.Token = strings.Repeat("3", 32)
			case "ref":
				result.Ref.Index++
			case "identity":
				result.IdentityDigest = ""
			case "uuid-on-sync":
				result.InstanceUUID = "foreign-uuid"
			case "collision":
				if err := o.RecordOwnerCreated(result); err != nil {
					t.Fatal(err)
				}
				result.ETagDigest = strings.Repeat("3", 64)
			case "uncertain":
				o.Uncertain()
			}
			if o.RecordOwnerCreated(result) == nil || o.closed() == nil {
				t.Fatal("foreign or unproved result admitted")
			}
		})
	}
}

func TestNativeOwnerCreateAsyncRequiresOriginalAcceptedUUID(t *testing.T) {
	for _, uuid := range []string{"", "original-instance-uuid"} {
		t.Run(uuid, func(t *testing.T) {
			o, _ := ownerCreateFixture(t)
			intent := ownerProjectCreateIntent(o, "current", "subyard-test-yard")
			intent.Kind, intent.Domain, intent.Resource = "instance", "operation", "yard-test-yard"
			ref, err := o.BeforeOwnerCreate(intent)
			if err != nil {
				t.Fatal(err)
			}
			op := ports.NativeOperation{Kind: "owner-create-instance", Server: intent.Server, Project: intent.Project, Resource: intent.Resource, ID: "original-operation"}
			if err := o.Accepted(op); err != nil {
				t.Fatal(err)
			}
			op.Status = 200
			if err := o.Terminal(op); err != nil {
				t.Fatal(err)
			}
			err = o.RecordOwnerCreated(ports.NativeOwnerCreated{Ref: ref, Intent: intent, OperationID: op.ID, Status: 200, InstanceUUID: uuid, IdentityDigest: intent.Server, ETagDigest: intent.Server})
			if uuid == "" {
				if err == nil || o.closed() == nil {
					t.Fatal("successful SDK operation replaced missing resource UUID")
				}
			} else if err != nil || o.closed() != nil {
				t.Fatal("exact accepted operation lost", err)
			}
		})
	}
}

func TestNativeOwnerCreateChecksBothResultLeavesBeforeSubmit(t *testing.T) {
	o, path := ownerCreateFixture(t)
	// Existing protected facts exhaust the original byte reservation. The
	// component must not even reserve a new indexed request in that state.
	for n := 0; n < 16; n++ {
		testkit.WriteFile(t, filepath.Join(path, factName(o.binding.Ordinal, "fixture-filler-"+string(rune('a'+n)))), make([]byte, 1<<20), 0600)
	}
	if _, err := o.BeforeOwnerCreate(ownerProjectCreateIntent(o, "current", "subyard-test-yard")); err == nil || len(o.commands) != 0 {
		t.Fatal("creation crossed exhausted original capacity")
	}
	if _, err := os.Stat(filepath.Join(path, factName(o.binding.Ordinal, "command-001-intent"))); !os.IsNotExist(err) {
		t.Fatal("capacity failure admitted a request intent")
	}
}

func TestNativeOwnerCreateFingerprintOriginalFacts(t *testing.T) {
	o, path := ownerCreateFixture(t)
	intent := ownerProjectCreateIntent(o, "current", "subyard-test-yard")
	intent.Kind, intent.Domain, intent.Resource = "instance", "operation", "yard-test-yard"
	intent.Fingerprint = strings.Repeat("a", 64)
	ref, err := o.BeforeOwnerCreate(intent)
	if err != nil {
		t.Fatal(err)
	}
	op := ports.NativeOperation{Kind: "owner-create-instance", Server: intent.Server, Project: intent.Project, Resource: intent.Resource, ID: "original-image-operation"}
	if err = o.Accepted(op); err != nil {
		t.Fatal(err)
	}
	op.Status = 200
	if err = o.Terminal(op); err != nil {
		t.Fatal(err)
	}
	value := ports.NativeOwnerCreated{Ref: ref, Intent: intent, OperationID: op.ID, Status: 200, InstanceUUID: "original-instance-uuid", IdentityDigest: intent.Server, ETagDigest: intent.Server}
	if err = o.RecordOwnerCreated(value); err != nil {
		t.Fatal(err)
	}
	if got, e := o.ReadOwnerCreated(ref); e != nil || got != value {
		t.Fatal("original fingerprint result changed", e)
	}
	if err = o.closed(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"wrong-kind", "bad-sha"} {
		bad := intent
		if change == "wrong-kind" {
			bad.Kind = "project"
			bad.Domain = "command"
			bad.Resource = bad.Project
		} else {
			bad.Fingerprint = strings.Repeat("A", 64)
		}
		if validOwnerCreate(bad) {
			t.Fatal("malformed image selection accepted", change)
		}
	}
	if output := os.Getenv("SUBYARD_NATIVE_OWNER_CREATE_FINGERPRINT_CORPUS"); output != "" {
		if err = o.session.writeFact(o.binding, "terminal", Terminal{Version: 1, Binding: o.binding, Owner: "owner-registration", Resource: "test-yard", State: "complete", Operations: []ports.NativeOperation{op}}); err != nil {
			t.Fatal(err)
		}
		type file struct {
			Name string `json:"name"`
			Raw  []byte `json:"raw"`
		}
		var files []file
		entries, e := os.ReadDir(path)
		if e != nil {
			t.Fatal(e)
		}
		for _, entry := range entries {
			raw, e := os.ReadFile(filepath.Join(path, entry.Name()))
			if e != nil {
				t.Fatal(e)
			}
			files = append(files, file{entry.Name(), raw})
		}
		raw, e := json.Marshal([]struct {
			Name        string `json:"name"`
			Transaction string `json:"transaction"`
			Files       []file `json:"files"`
			Accepted    bool   `json:"accepted"`
		}{{"actual-instance-fingerprint", o.binding.Transaction, files, true}})
		if e != nil {
			t.Fatal(e)
		}
		testkit.WriteFile(t, output, raw, 0600)
	}
}
