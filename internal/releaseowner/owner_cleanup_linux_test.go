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
	"github.com/Subyard/Subyard/internal/uninstall"
	"golang.org/x/sys/unix"
)

type localOwnerCleanupFixture struct {
	noEffects
	calls int
	fail  bool
}

func (f *localOwnerCleanupFixture) CleanupOwner(context.Context, OwnerBefore, string) error {
	f.calls++
	if f.fail {
		return errors.New("local archive refusal")
	}
	return nil
}

func ownerCleanupFactFixture(t *testing.T) (*session, Request, string) {
	t.Helper()
	s, b, directory := nativeFactFixture(t)
	r := Request{Op: "cleanup-owner", Binding: &b, Before: &OwnerBefore{State: "legacy-flat", Registration: b.Artifact, Overrides: b.Artifact, Controller: b.Artifact}, Token: strings.Repeat("a", 32)}
	recipe, err := recipeFingerprint(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.writeFact(b, "intent", Intent{Version: 1, Binding: b, Owner: "owner-registration-cleanup", Resource: "test-yard", Expected: b.Artifact, Desired: b.Artifact, Source: b.Artifact, Recipe: recipe}); err != nil {
		t.Fatal(err)
	}
	return s, r, directory
}
func TestOwnerCleanupReturnedLocalFailureHasClosedTerminal(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			s, r, _ := ownerCleanupFactFixture(t)
			component := &localOwnerCleanupFixture{fail: fail}
			s.components = component
			reply, err := s.apply(context.Background(), r, "owner-registration-cleanup", "test-yard", func() error { return component.CleanupOwner(s.effectContext, *r.Before, r.Token) })
			expected := "complete"
			if fail {
				expected = "failed"
			}
			if err != nil || reply.Terminal == nil || reply.Terminal.State != expected || len(reply.Terminal.Operations) != 0 {
				t.Fatalf("local cleanup closure: %#v %v", reply, err)
			}
			if component.calls != 1 {
				t.Fatal("cleanup callback count")
			}
		})
	}
}
func TestOwnerCleanupNeverClosesUnexpectedMutation(t *testing.T) {
	s, r, _ := ownerCleanupFactFixture(t)
	s.components = &localOwnerCleanupFixture{}
	reply, err := s.apply(context.Background(), r, "owner-registration-cleanup", "test-yard", func() error {
		observer := ports.NativeOperationObserverFrom(s.effectContext)
		if err := observer.Before(ports.NativeOperation{Kind: "instance-state", Server: r.Binding.Artifact, Project: "p", Resource: "i"}); err != nil {
			return err
		}
		return errors.New("unexpected submit remained unresolved")
	})
	if err != nil || reply.Code != "unknown" || reply.Terminal != nil {
		t.Fatal("unexpected effect was classified local", reply, err)
	}
}
func TestOwnerCleanupQueryRequiresOriginalRecipeAndExclusiveCustody(t *testing.T) {
	for _, variant := range []string{"exact", "wrong-token", "wrong-kind", "remote-intent", "pending-terminal"} {
		t.Run(variant, func(t *testing.T) {
			s, r, directory := ownerCleanupFactFixture(t)
			s.components = &localOwnerCleanupFixture{}
			accepted := Terminal{Version: 1, Binding: *r.Binding, Owner: "owner-registration-cleanup", Resource: "test-yard", State: "accepted", Operations: []ports.NativeOperation{}}
			if err := s.writeFact(*r.Binding, "accepted", accepted); err != nil {
				t.Fatal(err)
			}
			r.Op = "query-owner-cleanup"
			if reply, err := s.queryOwnerCleanup(context.Background(), r); err != nil || reply.Code != "unknown" {
				t.Fatal("unheld cleanup custody accepted", reply, err)
			}
			root := s.context.DataHome
			update, err := os.OpenFile(filepath.Join(root, "migrations/update.lock"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				t.Fatal(err)
			}
			if err := unix.Flock(int(update.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
				t.Fatal(err)
			}
			if err := unix.Flock(int(s.root.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
				t.Fatal(err)
			}
			_, release, err := uninstall.OwnerLock(context.Background(), root, nil)
			if err != nil {
				t.Fatal(err)
			}
			owner, err := uninstall.BorrowOwnerLock(root)
			if err != nil || owner == nil {
				t.Fatal(err)
			}
			s.ownerRelease = release
			s.leases = []*os.File{update, owner}
			t.Cleanup(s.releaseLeases)
			switch variant {
			case "wrong-token":
				r.Token = strings.Repeat("b", 32)
			case "wrong-kind":
				raw, err := os.ReadFile(filepath.Join(directory, factName(r.Binding.Ordinal, "intent")))
				if err != nil {
					t.Fatal(err)
				}
				var value Intent
				if err := json.Unmarshal(raw, &value); err != nil {
					t.Fatal(err)
				}
				value.Owner = "owner-registration"
				raw, _ = json.Marshal(value)
				if err := os.WriteFile(filepath.Join(directory, factName(r.Binding.Ordinal, "intent")), append(raw, '\n'), 0600); err != nil {
					t.Fatal(err)
				}
			case "remote-intent":
				if err := s.writeFact(*r.Binding, "command-001-intent", commandFact{Kind: "shipped-handler", Source: r.Binding.Artifact}); err != nil {
					t.Fatal(err)
				}
			case "pending-terminal":
				pending := accepted
				pending.State = "complete"
				raw, _ := json.Marshal(pending)
				if err := os.WriteFile(filepath.Join(directory, "."+factName(r.Binding.Ordinal, "terminal")+".pending"), append(raw, '\n'), 0600); err != nil {
					t.Fatal(err)
				}
			}
			reply, err := s.queryOwnerCleanup(context.Background(), r)
			if err != nil {
				t.Fatal(err)
			}
			if variant == "exact" || variant == "pending-terminal" {
				expected := "failed"
				if variant == "pending-terminal" {
					expected = "complete"
				}
				if reply.Terminal == nil || reply.Terminal.State != expected {
					t.Fatal("closed local invocation not recovered", reply)
				}
			} else if reply.Code != "unknown" || reply.Terminal != nil {
				t.Fatal("foreign or remote work closed as local", reply)
			}
		})
	}
}
