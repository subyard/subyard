//go:build linux

package releaseowner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

type materializedRecoveryFixture struct {
	Components
	target              Target
	completion          AfterOwnerCompletion
	queries             int
	active, unavailable bool
}

func (f *materializedRecoveryFixture) ValidateMaterializedAfterOwnerRecovery(_ context.Context, target Target, completed AfterOwnerCompletion) error {
	if target != f.target || completed != f.completion {
		return errors.New("original derived target binding")
	}
	return nil
}
func (f *materializedRecoveryFixture) QueryOperation(_ context.Context, expected ports.NativeOperation) (ports.NativeOperation, error) {
	f.queries++
	if f.unavailable {
		return ports.NativeOperation{}, errors.New("expired exact ID")
	}
	expected.Status = 200
	if f.active {
		expected.Status = 103
	}
	return expected, nil
}
func TestMaterializedAfterOwnerRecoveryClosesOnlyBoundQuiescentInvocation(t *testing.T) {
	for _, mode := range []string{"sealed", "refresh", "no-operations", "missing-accepted", "operation-unaccepted", "active", "remote-absent", "command-open", "operation-hole", "command-hole", "extra", "pending", "create", "token", "duplicate-id", "malformed-terminal", "missing-producer", "missing-owner-lease", "producer-scope", "source", "expected", "recipe"} {
		t.Run(mode, func(t *testing.T) {
			s, request, origin, _, path := ownerResultsOriginal(t)
			ownerResultsTerminal(t, s, origin, "complete")
			if reply, err := s.bindOwnerResults(context.Background(), request); err != nil || reply.Code != "" {
				t.Fatal(reply, err)
			}
			completed, err := s.ownerCompletion()
			if err != nil {
				t.Fatal(err)
			}
			b := *s.binding
			raw, _ := json.Marshal(nativeReservation{Version: 1, Binding: b, Files: 2570, Bytes: 16 << 20})
			testkit.WriteFile(t, filepath.Join(path, factName(b.Ordinal, "reservation")), append(raw, '\n'), 0600)
			target := Target{Owner: "materialized-config", Yard: "test-yard", Class: "materialized-files", Project: "project", Instance: "yard", InstanceUUID: "original-uuid", Source: strings.Repeat("a", 64), Actual: strings.Repeat("b", 64), Desired: strings.Repeat("c", 64), PreparedRecipe: strings.Repeat("d", 64), AfterOwner: "v1:materialized-files:" + strings.Repeat("e", 64), Before: strings.Repeat("f", 64), State: "stale"}
			fixture := &materializedRecoveryFixture{target: target, completion: completed, active: mode == "active", unavailable: mode == "remote-absent" || mode == "sealed"}
			s.components = fixture
			recipe, _ := recipeFingerprint(Request{Op: "apply-activation", Target: &target})
			intent := Intent{Version: 1, Binding: b, Owner: target.Owner, Resource: target.Yard, Expected: target.Actual, Desired: target.Desired, Source: target.Source, Recipe: recipe}
			switch mode {
			case "expected":
				intent.Expected = target.Desired
			case "recipe":
				intent.Recipe = target.Desired
			case "source":
				target.Source = target.Desired
			case "producer-scope":
				b.Plan = "plan-v1-" + strings.Repeat("e", 64)
				s.binding = &b
				intent.Binding = b
				reservation, _ := json.Marshal(nativeReservation{Version: 1, Binding: b, Files: 2570, Bytes: 16 << 20})
				testkit.WriteFile(t, filepath.Join(path, factName(b.Ordinal, "reservation")), append(reservation, '\n'), 0600)
			case "missing-producer":
				s.ownerCompleted = nil
			case "missing-owner-lease":
				s.ownerRelease = nil
			}
			if err := s.writeFact(b, "intent", intent); err != nil {
				t.Fatal(err)
			}
			if mode != "missing-accepted" {
				if err := s.writeFact(b, "accepted", Terminal{Version: 1, Binding: b, Owner: target.Owner, Resource: target.Yard, State: "accepted", Operations: []ports.NativeOperation{}}); err != nil {
					t.Fatal(err)
				}
			}
			o := &invocationObserver{session: s, binding: b}
			command, err := o.CommandStarted("shipped-handler", b.Artifact)
			if err != nil {
				t.Fatal(err)
			}
			if mode != "command-open" {
				if err := o.CommandTerminal(command); err != nil {
					t.Fatal(err)
				}
			}
			op := ports.NativeOperation{Kind: "instance-exec", Server: b.Artifact, Project: target.Project, Resource: target.Instance}
			if mode == "token" {
				op.Kind = "owner-image-secret-current-primary"
				op.Resource = b.Artifact
			}
			if mode != "no-operations" {
				if err := o.Before(op); err != nil {
					t.Fatal(err)
				}
				op.ID = "operation-1"
				if mode == "token" {
					op.ID = "11111111-2222-3333-4444-555555555555"
				}
				if mode != "operation-unaccepted" {
					if err := o.Accepted(op); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "sealed" {
					op.Status = 200
					if err := o.Terminal(op); err != nil {
						t.Fatal(err)
					}
				}
			}
			switch mode {
			case "operation-hole":
				os.Rename(filepath.Join(path, factName(b.Ordinal, "operation-001-intent")), filepath.Join(path, factName(b.Ordinal, "operation-002-intent")))
			case "command-hole":
				os.Rename(filepath.Join(path, factName(b.Ordinal, "command-001-intent")), filepath.Join(path, factName(b.Ordinal, "command-002-intent")))
			case "extra", "create":
				if err := s.writeFact(b, mode, map[string]int{"version": 1}); err != nil {
					t.Fatal(err)
				}
			case "pending":
				testkit.WriteFile(t, filepath.Join(path, "."+factName(b.Ordinal, "operation-002-intent")), []byte("pending\n"), 0600)
			case "duplicate-id":
				before := op
				before.ID = ""
				if err := s.writeFact(b, "operation-002-intent", before); err != nil {
					t.Fatal(err)
				}
				if err := s.writeFact(b, "operation-002-accepted", op); err != nil {
					t.Fatal(err)
				}
			case "malformed-terminal":
				op.Status = 500
				if err := s.writeFact(b, "operation-001-terminal", op); err != nil {
					t.Fatal(err)
				}
			}
			s.context.Root, _ = IdentityOf(s.root)
			query, _ := json.Marshal(Request{Op: "query-invocation", Binding: &b, Target: &target})
			reply, err := s.handle(context.Background(), query, nil)
			positive := mode == "sealed" || mode == "refresh" || mode == "no-operations"
			if positive {
				if err != nil || reply.Code != "failed" || reply.Terminal == nil || reply.Terminal.State != "failed" {
					t.Fatal("quiescent incomplete request did not close FAILED", reply, err)
				}
				if mode == "sealed" && fixture.queries != 0 {
					t.Fatal("sealed terminal re-queried expiring operation")
				}
				terminal, err := s.readTerminal(b)
				if err != nil || terminal.State != "failed" {
					t.Fatal("failed closure not durable", err)
				}
			} else {
				if err == nil && reply.Code != "unknown" {
					t.Fatal("unproved work gained retry", reply)
				}
				if _, err := s.readTerminal(b); err == nil {
					t.Fatal("unproved invocation got whole terminal")
				}
			}
		})
	}
}
