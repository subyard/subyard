package releaseowner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func nativeFactFixture(t *testing.T) (*session, Binding, string) {
	t.Helper()
	root := testkit.TempDir(t)
	path := filepath.Join(root, "migrations", "native-custody", "v1", "transaction")
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	a := strings.Repeat("a", 64)
	b := Binding{Session: a, Transaction: "transaction", Plan: "plan-v1-" + a, Journal: a, Artifact: a, Helper: a, Ordinal: 1}
	reservation, err := json.Marshal(nativeReservation{Version: 1, Binding: b, Files: 2570, Bytes: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(path, factName(b.Ordinal, "reservation")), append(reservation, '\n'), 0o600)
	return &session{root: file, binding: &b, context: &Context{Session: b.Session, DataHome: root, ConfigHome: root}}, b, path
}

func TestNativeCommandUnknownCannotBeClosedBySDKFailure(t *testing.T) {
	s, b, _ := nativeFactFixture(t)
	o := &invocationObserver{session: s, binding: b}
	command, err := o.CommandStarted("shipped-handler", b.Artifact)
	if err != nil {
		t.Fatal(err)
	}
	op := ports.NativeOperation{Kind: "instance-state", Server: b.Artifact, Project: "project", Resource: "yard"}
	if err := o.Before(op); err != nil {
		t.Fatal(err)
	}
	op.ID = "operation-1"
	if err := o.Accepted(op); err != nil {
		t.Fatal(err)
	}
	op.Status = 400
	if err := o.Terminal(op); err != nil {
		t.Fatal(err)
	}
	failure := &ports.NativeOperationFailure{Operation: op, Cause: errors.New("failed")}
	if o.failed(failure) {
		t.Fatal("SDK failure hid unresolved command")
	}
	if err := o.CommandTerminal(command); err != nil {
		t.Fatal(err)
	}
	if !o.failed(failure) {
		t.Fatal("complete command pair did not close foreground transport")
	}
	if err := o.CommandTerminal(command); err == nil {
		t.Fatal("duplicate command terminal accepted")
	}
}

func TestNativeObserverDoesNotLoseNestedOperationFailure(t *testing.T) {
	s, b, _ := nativeFactFixture(t)
	o := &invocationObserver{session: s, binding: b}
	first := ports.NativeOperation{Kind: "instance-state", Server: b.Artifact, Project: "project", Resource: "yard", ID: "operation-1"}
	second := first
	second.ID = "operation-2"
	for _, op := range []ports.NativeOperation{first, second} {
		before := op
		before.ID = ""
		if err := o.Before(before); err != nil {
			t.Fatal(err)
		}
		if err := o.Accepted(op); err != nil {
			t.Fatal(err)
		}
	}
	first.Status = 400
	second.Status = 200
	if err := o.Terminal(first); err != nil {
		t.Fatal(err)
	}
	if err := o.Terminal(second); err != nil {
		t.Fatal(err)
	}
	if o.closed() == nil {
		t.Fatal("later successful operation hid earlier failure")
	}
	failure := &ports.NativeOperationFailure{Operation: first, Cause: errors.New("failed")}
	if !o.failed(fmt.Errorf("component: %w", failure)) {
		t.Fatal("closed SDK failure lost")
	}
	if o.failed(errors.Join(failure, errors.New("opaque transport lost"))) {
		t.Fatal("unproven sibling effect classified closed")
	}
	if err := o.Terminal(first); err == nil {
		t.Fatal("duplicate terminal accepted")
	}
	if o.failed(failure) {
		t.Fatal("observer fault did not close admission")
	}
}

func TestNativeObserverUncertaintyCannotBeHiddenByLaterSuccess(t *testing.T) {
	s, b, _ := nativeFactFixture(t)
	o := &invocationObserver{session: s, binding: b}
	op := ports.NativeOperation{Kind: "instance-state", Server: b.Artifact, Project: "project", Resource: "yard", ID: "operation-1"}
	before := op
	before.ID = ""
	if err := o.Before(before); err != nil {
		t.Fatal(err)
	}
	if err := o.Accepted(op); err != nil {
		t.Fatal(err)
	}
	op.Status = 200
	if err := o.Terminal(op); err != nil {
		t.Fatal(err)
	}
	o.Uncertain()
	if o.closed() == nil {
		t.Fatal("unknown prior request became complete")
	}
}

func TestNativeApplyRetainsAcceptedWorkAfterOwnerCancellation(t *testing.T) {
	s, b, _ := nativeFactFixture(t)
	request := Request{Op: "apply-ingress", Binding: &b, Step: &IngressStep{Kind: "source"}}
	recipe, err := recipeFingerprint(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.writeFact(b, "intent", Intent{Version: 1, Binding: b, Owner: "source-ingress", Resource: "source", Expected: b.Artifact, Desired: b.Artifact, Source: b.Artifact, Recipe: recipe}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reply, err := s.apply(ctx, request, "source-ingress", "source", func() error {
		cancel()
		if s.effectContext.Err() != nil {
			return errors.New("owner cancellation killed accepted native work")
		}
		if _, ok := s.effectContext.Deadline(); !ok {
			return errors.New("accepted work lost native deadline")
		}
		return nil
	})
	if err != nil || reply.Code != "" || reply.Terminal == nil || reply.Terminal.State != "complete" {
		t.Fatalf("accepted work did not drain: %#v %v", reply, err)
	}
}

func TestNativeApplyKnownFailedIsTerminalWithoutReplay(t *testing.T) {
	for _, opaque := range []bool{false, true} {
		t.Run(fmt.Sprint(opaque), func(t *testing.T) {
			s, b, _ := nativeFactFixture(t)
			request := Request{Op: "apply-ingress", Binding: &b, Step: &IngressStep{Kind: "source"}}
			recipe, _ := recipeFingerprint(request)
			if err := s.writeFact(b, "intent", Intent{Version: 1, Binding: b, Owner: "source-ingress", Resource: "source", Expected: b.Artifact, Desired: b.Artifact, Source: b.Artifact, Recipe: recipe}); err != nil {
				t.Fatal(err)
			}
			calls := 0
			apply := func() error {
				calls++
				o := ports.NativeOperationObserverFrom(s.effectContext)
				op := ports.NativeOperation{Kind: "instance-state", Server: b.Artifact, Project: "project", Resource: "yard", ID: "operation-1"}
				before := op
				before.ID = ""
				if err := o.Before(before); err != nil {
					return err
				}
				if err := o.Accepted(op); err != nil {
					return err
				}
				op.Status = 400
				if err := o.Terminal(op); err != nil {
					return err
				}
				failure := &ports.NativeOperationFailure{Operation: op, Cause: errors.New("failed")}
				if opaque {
					return errors.Join(failure, errors.New("opaque transport lost"))
				}
				return failure
			}
			reply, err := s.apply(context.Background(), request, "source-ingress", "source", apply)
			if err != nil {
				t.Fatal(err)
			}
			if opaque {
				if reply.Code != "unknown" || reply.Terminal != nil {
					t.Fatalf("opaque failure became terminal: %#v", reply)
				}
			} else if reply.Code != "failed" || reply.Terminal == nil || reply.Terminal.State != "failed" {
				t.Fatalf("known failure lost: %#v", reply)
			}
			if _, err = s.apply(context.Background(), request, "source-ingress", "source", apply); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatal("accepted failed/unknown invocation replayed")
			}
		})
	}
}

func TestNativeFactPublicationRetriesOnlyExactPendingBytes(t *testing.T) {
	for _, valid := range []bool{false, true} {
		t.Run(fmt.Sprint(valid), func(t *testing.T) {
			s, b, path := nativeFactFixture(t)
			raw := []byte("{\"value\":\"captured\"}\n")
			if !valid {
				raw = []byte("{\"value\":\"foreign\"}\n")
			}
			pending := filepath.Join(path, "."+factName(b.Ordinal, "accepted")+".pending")
			testkit.WriteFile(t, pending, raw, 0o600)
			err := s.writeFact(b, "accepted", struct {
				Value string `json:"value"`
			}{"captured"})
			if !valid {
				if err == nil {
					t.Fatal("foreign staging fact overwritten")
				}
				actual, _ := os.ReadFile(pending)
				if string(actual) != string(raw) {
					t.Fatal("foreign staging bytes changed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			actual, err := os.ReadFile(filepath.Join(path, factName(b.Ordinal, "accepted")))
			if err != nil || string(actual) != string(raw) {
				t.Fatalf("staged fact not published exactly: %q %v", actual, err)
			}
			if _, err := os.Lstat(pending); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("published pending fact retained")
			}
		})
	}
}

func TestNativeImageToken401ClosesOnlyExactOriginalTokenKinds(t *testing.T) {
	for _, role := range []string{"current", "legacy", "recreate"} {
		for _, choice := range []string{"primary", "fallback"} {
			kind := "owner-image-secret-" + role + "-" + choice
			t.Run(kind, func(t *testing.T) {
				s, b, _ := nativeFactFixture(t)
				observer := &invocationObserver{session: s, binding: b}
				operation := ports.NativeOperation{Kind: kind, Server: b.Artifact, Project: "source", Resource: strings.Repeat("a", 64)}
				if err := observer.Before(operation); err != nil {
					t.Fatal(err)
				}
				operation.ID = "11111111-2222-3333-4444-555555555555"
				if err := observer.Accepted(operation); err != nil {
					t.Fatal(err)
				}
				operation.Status = 401
				if err := observer.Terminal(operation); err != nil {
					t.Fatal(err)
				}
				if err := observer.closed(); err != nil {
					t.Fatal("exact token terminal lost closure", err)
				}
				if observer.failed(&ports.NativeOperationFailure{Operation: operation, Cause: errors.New("token consumed")}) {
					t.Fatal("consumed token became component failure")
				}
				for _, changed := range []ports.NativeOperation{
					{Kind: kind + "-extra", ID: operation.ID, Resource: operation.Resource, Status: 401},
					{Kind: "owner-create-instance", ID: operation.ID, Resource: operation.Resource, Status: 401},
					{Kind: kind, ID: "operation-1", Resource: operation.Resource, Status: 401},
					{Kind: kind, ID: strings.ToUpper("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"), Resource: operation.Resource, Status: 401},
					{Kind: kind, ID: operation.ID, Resource: "image", Status: 401},
					{Kind: kind, ID: operation.ID, Resource: strings.Repeat("A", 64), Status: 401},
				} {
					if nativeOperationSucceeded(changed) {
						t.Fatal("foreign 401 accepted", changed.Kind)
					}
				}
			})
		}
	}
}

type ownerTokenDrainFixture struct {
	Components
	calls   int
	bounded bool
	result  ports.NativeOperation
	failure error
}

func (f *ownerTokenDrainFixture) DrainOwnerImageSecret(ctx context.Context, original ports.NativeOperation) (ports.NativeOperation, error) {
	f.calls++
	if deadline, ok := ctx.Deadline(); ok {
		f.bounded = time.Until(deadline) > 0 && time.Until(deadline) <= 30*time.Second
	}
	if f.failure != nil {
		return ports.NativeOperation{}, f.failure
	}
	value := f.result
	if value.Kind == "" {
		value = original
		value.Status = 401
	}
	return value, nil
}
func TestNativeTokenRecoveryRequiresBothOriginalLeasesAndRetainsOuterUnknown(t *testing.T) {
	for _, mode := range []string{"none", "update-only", "missing-owner-registration", "both", "foreign-uuid", "missing-remote"} {
		t.Run(mode, func(t *testing.T) {
			s, b, _ := nativeFactFixture(t)
			if err := s.writeFact(b, "accepted", Terminal{Version: 1, Binding: b, Owner: "owner-registration", Resource: "test-yard", State: "accepted", Operations: []ports.NativeOperation{}}); err != nil {
				t.Fatal(err)
			}
			observer := &invocationObserver{session: s, binding: b}
			operation := ports.NativeOperation{Kind: "owner-image-secret-current-primary", Server: b.Artifact, Project: "source", Resource: strings.Repeat("a", 64)}
			if err := observer.Before(operation); err != nil {
				t.Fatal(err)
			}
			operation.ID = "11111111-2222-3333-4444-555555555555"
			if err := observer.Accepted(operation); err != nil {
				t.Fatal(err)
			}
			fixture := &ownerTokenDrainFixture{}
			s.components = fixture
			if mode != "none" {
				s.leases = []*os.File{s.root}
			}
			if mode != "none" && mode != "update-only" {
				s.leases = append(s.leases, s.root)
			}
			if mode != "missing-owner-registration" {
				s.ownerRelease = func() {}
			}
			if mode == "foreign-uuid" {
				fixture.result = operation
				fixture.result.ID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
				fixture.result.Status = 401
			}
			if mode == "missing-remote" {
				fixture.failure = errors.New("original operation unavailable")
			}
			reply, err := s.queryInvocation(context.Background(), b)
			if err != nil || reply.Code != "unknown" {
				t.Fatal("missing outer terminal became complete", reply.Code, err)
			}
			allowed := mode == "both" || mode == "foreign-uuid" || mode == "missing-remote"
			if (fixture.calls != 0) != allowed {
				t.Fatal("token DELETE admission ignored original leases")
			}
			if allowed && !fixture.bounded {
				t.Fatal("token cleanup lost fixed30s deadline")
			}
			if mode == "both" {
				if reply.Terminal == nil || reply.Terminal.State != "unknown" || len(reply.Terminal.Operations) != 1 || reply.Terminal.Operations[0].Status != 401 {
					t.Fatal("exact token terminal lost")
				}
			} else if reply.Terminal != nil {
				t.Fatal("foreign or unproven operation got terminal")
			}
		})
	}
}
