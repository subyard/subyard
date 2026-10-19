//go:build linux

package releaseowner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

type ownerResultsFixture struct {
	Components
	values []ports.NativeOwnerCreated
}

func (f *ownerResultsFixture) BindOwnerResults(_ context.Context, _ OwnerBefore, _ string, values []ports.NativeOwnerCreated) error {
	f.values = append([]ports.NativeOwnerCreated(nil), values...)
	return nil
}

func ownerResultsOriginal(t *testing.T) (*session, Request, *invocationObserver, *ownerResultsFixture, string) {
	t.Helper()
	s, b, path := nativeFactFixture(t)
	before := OwnerBefore{State: "legacy-directory", Registration: b.Artifact, Overrides: b.Artifact, Controller: b.Artifact, PreparedRecipe: strings.Repeat("b", 64)}
	token := strings.Repeat("1", 32)
	recipe, err := recipeFingerprint(Request{Op: "apply-owner", Before: &before, Token: token})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.writeFact(b, "intent", Intent{Version: 1, Binding: b, Owner: "owner-registration", Resource: "test-yard", Expected: b.Artifact, Desired: b.Artifact, Source: b.Artifact, Recipe: recipe}); err != nil {
		t.Fatal(err)
	}
	if err = s.writeFact(b, "accepted", Terminal{Version: 1, Binding: b, Owner: "owner-registration", Resource: "test-yard", State: "accepted", Operations: []ports.NativeOperation{}}); err != nil {
		t.Fatal(err)
	}
	observer := &invocationObserver{session: s, binding: b, ownerToken: token}
	completeOwnerProject(t, observer, ownerProjectCreateIntent(observer, "current", "subyard-test-yard"))
	current := b
	current.Ordinal = 2
	s.binding = &current
	s.leases = []*os.File{s.root, s.root}
	s.ownerRelease = func() {}
	fixture := &ownerResultsFixture{}
	s.components = fixture
	return s, Request{Op: "bind-owner-results", Binding: &current, Producer: &b, Before: &before, Token: token}, observer, fixture, path
}
func ownerResultsTerminal(t *testing.T, s *session, observer *invocationObserver, state string) {
	t.Helper()
	if err := s.writeTerminal(Terminal{Version: 1, Binding: observer.binding, Owner: "owner-registration", Resource: "test-yard", State: state, Operations: observer.operations()}); err != nil {
		t.Fatal(err)
	}
}
func TestNativeOwnerResultsRequireExplicitClosedOriginalProducer(t *testing.T) {
	for _, change := range []string{"valid", "missing-terminal", "wrong-transaction", "wrong-plan", "wrong-helper", "wrong-ordinal", "wrong-token", "wrong-recipe", "missing-owner-lease", "duplicate-result"} {
		t.Run(change, func(t *testing.T) {
			s, request, observer, fixture, _ := ownerResultsOriginal(t)
			if change == "duplicate-result" {
				completeOwnerProject(t, observer, ownerProjectCreateIntent(observer, "current", "subyard-test-yard"))
			}
			if change != "missing-terminal" {
				ownerResultsTerminal(t, s, observer, "complete")
			}
			switch change {
			case "wrong-transaction":
				request.Producer.Transaction = "foreign"
			case "wrong-plan":
				request.Producer.Plan = "plan-v1-" + strings.Repeat("c", 64)
			case "wrong-helper":
				request.Producer.Helper = strings.Repeat("c", 64)
			case "wrong-ordinal":
				request.Producer.Ordinal = 3
			case "wrong-token":
				request.Token = strings.Repeat("2", 32)
			case "wrong-recipe":
				request.Before.PreparedRecipe = strings.Repeat("c", 64)
			case "missing-owner-lease":
				s.ownerRelease = nil
			}
			reply, err := s.bindOwnerResults(context.Background(), request)
			if change != "valid" {
				if err == nil && reply.Code == "" {
					t.Fatal("foreign producer admitted")
				}
				if len(fixture.values) != 0 {
					t.Fatal("refusal attached original results")
				}
				return
			}
			if err != nil || reply.Code != "" || len(fixture.values) != 1 {
				t.Fatal("closed original producer refused", reply.Code, err)
			}
			created := fixture.values[0]
			if created.Ref.Producer != ownerCreateProducer(*request.Producer) {
				t.Fatal("producer address lost")
			}
			fresh := &invocationObserver{session: s, binding: *request.Binding}
			actual, err := fresh.ReadOwnerCreated(created.Ref)
			if err != nil || actual != created {
				t.Fatal("explicit original ref not readable", err)
			}
			foreign := created.Ref
			foreign.Producer = ownerCreateProducer(*request.Binding)
			if _, err = fresh.ReadOwnerCreated(foreign); err == nil {
				t.Fatal("index from new invocation aliased original")
			}
		})
	}
}
func TestNativeOwnerResultsKnownFailedDoesNotSupplyMissingOuterTerminal(t *testing.T) {
	s, request, observer, fixture, _ := ownerResultsOriginal(t)
	ownerResultsTerminal(t, s, observer, "failed")
	if reply, err := s.bindOwnerResults(context.Background(), request); err != nil || reply.Code != "" || len(fixture.values) != 1 {
		t.Fatal(reply.Code, err)
	}
	directory, err := s.factsDirectory(*request.Producer)
	if err != nil {
		t.Fatal(err)
	}
	// Test-owned removal models loss of the outer proof, never product cleanup.
	file := os.NewFile(uintptr(directory), "facts")
	file.Close()
	path := filepath.Join(s.root.Name(), "migrations/native-custody/v1", request.Producer.Transaction, factName(request.Producer.Ordinal, "terminal"))
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	fresh := &invocationObserver{session: s, binding: *request.Binding}
	if _, err = fresh.ReadOwnerCreated(fixture.values[0].Ref); err == nil {
		t.Fatal("nested success replaced missing outer terminal")
	}
}
func TestNativeOwnerResultsFreshProcessUsesOriginalFacts(t *testing.T) {
	if os.Getenv("SUBYARD_OWNER_RESULTS_CHILD") == "1" {
		raw, err := base64.StdEncoding.DecodeString(os.Getenv("SUBYARD_OWNER_RESULTS_REQUEST"))
		if err != nil {
			t.Fatal(err)
		}
		var request Request
		if json.Unmarshal(raw, &request) != nil {
			t.Fatal("child request syntax")
		}
		root, err := os.Open(os.Getenv("SUBYARD_OWNER_RESULTS_ROOT"))
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		fixture := &ownerResultsFixture{}
		s := &session{root: root, binding: request.Binding, leases: []*os.File{root, root}, ownerRelease: func() {}, components: fixture}
		reply, err := s.bindOwnerResults(context.Background(), request)
		if err != nil || reply.Code != "" || len(fixture.values) != 1 {
			t.Fatal("fresh original result restore", reply.Code, err)
		}
		if fixture.values[0].Ref.Producer != ownerCreateProducer(*request.Producer) {
			t.Fatal("fresh producer drift")
		}
		return
	}
	s, request, observer, _, _ := ownerResultsOriginal(t)
	ownerResultsTerminal(t, s, observer, "complete")
	raw, _ := json.Marshal(request)
	child := exec.Command(os.Args[0], "-test.run=^TestNativeOwnerResultsFreshProcessUsesOriginalFacts$", "-test.count=1")
	child.Env = append(os.Environ(), "SUBYARD_OWNER_RESULTS_CHILD=1", "SUBYARD_OWNER_RESULTS_REQUEST="+base64.StdEncoding.EncodeToString(raw), "SUBYARD_OWNER_RESULTS_ROOT="+s.root.Name())
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("fresh process: %v %s", err, output)
	}
}
func TestNativeOwnerResultsCompactedCommandStillRequiresExactOriginalClosure(t *testing.T) {
	s, request, observer, fixture, path := ownerResultsOriginal(t)
	ownerResultsTerminal(t, s, observer, "complete")
	var closure ownerCreateClosure
	closure.Version, closure.Binding = 1, *request.Producer
	for _, check := range []struct {
		kind string
		hash *string
	}{{"intent", &closure.Intent}, {"accepted", &closure.Accepted}, {"terminal", &closure.Terminal}} {
		raw, err := os.ReadFile(filepath.Join(path, factName(request.Producer.Ordinal, check.kind)))
		if err != nil {
			t.Fatal(err)
		}
		*check.hash = digest(raw)
	}
	closure.Commands = observer.commands
	for _, phase := range []string{"intent", "terminal"} {
		name := factName(request.Producer.Ordinal, "command-001-"+phase)
		raw, err := os.ReadFile(filepath.Join(path, name))
		if err != nil {
			t.Fatal(err)
		}
		closure.Facts = append(closure.Facts, ownerCreateClosureFact{Name: name, Size: uint64(len(raw)), SHA256: digest(raw)})
		if err = os.Remove(filepath.Join(path, name)); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := json.Marshal(closure)
	testkit.WriteFile(t, filepath.Join(path, factName(request.Producer.Ordinal, "closure")), append(raw, '\n'), 0600)
	reply, err := s.bindOwnerResults(context.Background(), request)
	if err != nil || reply.Code != "" || len(fixture.values) != 1 {
		t.Fatal("original closure refused", reply.Code, err)
	}
}

func TestNativeOwnerResultsReadonlyBindingRequiresOriginalContext(t *testing.T) {
	for _, change := range []string{"valid", "foreign-session", "foreign-artifact", "foreign-helper"} {
		t.Run(change, func(t *testing.T) {
			s, request, observer, fixture, _ := ownerResultsOriginal(t)
			ownerResultsTerminal(t, s, observer, "complete")
			s.binding = nil
			s.leases = nil
			s.ownerRelease = nil
			request.Binding = nil
			s.context = &Context{Session: request.Producer.Session, Artifact: request.Producer.Artifact}
			s.image = request.Producer.Helper
			switch change {
			case "foreign-session":
				s.context.Session = strings.Repeat("c", 64)
			case "foreign-artifact":
				s.context.Artifact = strings.Repeat("c", 64)
			case "foreign-helper":
				s.image = strings.Repeat("c", 64)
			}
			reply, err := s.bindOwnerResults(context.Background(), request)
			if change == "valid" {
				if err != nil || reply.Code != "" || len(fixture.values) != 1 {
					t.Fatal("readonly original binding required grant", reply.Code, err)
				}
			} else if err == nil || len(fixture.values) != 0 {
				t.Fatal("foreign readonly context admitted")
			}
		})
	}
}

func addOwnerResultProducer(t *testing.T, s *session, request Request, ordinal uint64) *invocationObserver {
	t.Helper()
	binding := *request.Producer
	binding.Ordinal = ordinal
	directory := filepath.Join(s.root.Name(), "migrations/native-custody/v1", binding.Transaction)
	reservation, _ := json.Marshal(nativeReservation{Version: 1, Binding: binding, Files: 2570, Bytes: 16 << 20})
	testkit.WriteFile(t, filepath.Join(directory, factName(ordinal, "reservation")), append(reservation, '\n'), 0600)
	recipe, _ := recipeFingerprint(Request{Op: "apply-owner", Before: request.Before, Token: request.Token})
	if err := s.writeFact(binding, "intent", Intent{Version: 1, Binding: binding, Owner: "owner-registration", Resource: "test-yard", Expected: binding.Artifact, Desired: binding.Artifact, Source: binding.Artifact, Recipe: recipe}); err != nil {
		t.Fatal(err)
	}
	if err := s.writeFact(binding, "accepted", Terminal{Version: 1, Binding: binding, Owner: "owner-registration", Resource: "test-yard", State: "accepted", Operations: []ports.NativeOperation{}}); err != nil {
		t.Fatal(err)
	}
	return &invocationObserver{session: s, binding: binding, ownerToken: request.Token}
}
func TestNativeOwnerResultLineageRetainsEarlierSuccessAcrossClosedRetries(t *testing.T) {
	s, request, first, fixture, _ := ownerResultsOriginal(t)
	current := *request.Binding
	current.Ordinal = 5
	s.binding = &current
	request.Binding = &current
	failure := ports.NativeOperation{Kind: "instance-state", Server: first.binding.Artifact, Project: "subyard-test-yard", Resource: "yard-test-yard"}
	if err := first.Before(failure); err != nil {
		t.Fatal(err)
	}
	failure.ID = "later-stop"
	if err := first.Accepted(failure); err != nil {
		t.Fatal(err)
	}
	failure.Status = 400
	if err := first.Terminal(failure); err != nil {
		t.Fatal(err)
	}
	if !first.failed(&ports.NativeOperationFailure{Operation: failure, Cause: errors.New("known stop failure")}) {
		t.Fatal("reachable knownfailed branch lost")
	}
	ownerResultsTerminal(t, s, first, "failed")
	if reply, err := s.bindOwnerResults(context.Background(), request); err != nil || reply.Code != "" {
		t.Fatal(reply.Code, err)
	}
	original := fixture.values[0]
	second := addOwnerResultProducer(t, s, request, 3)
	completeOwnerProject(t, second, ownerProjectCreateIntent(second, "recreate", "subyard-e2e-yard"))
	ownerResultsTerminal(t, s, second, "complete")
	request.Producer = &second.binding
	if reply, err := s.bindOwnerResults(context.Background(), request); err != nil || reply.Code != "" || len(fixture.values) != 2 || len(s.ownerOrigins) != 2 {
		t.Fatal("retry lost earlier causal result", reply.Code, err)
	}
	empty := addOwnerResultProducer(t, s, request, 4)
	ownerResultsTerminal(t, s, empty, "complete")
	request.Producer = &empty.binding
	if reply, err := s.bindOwnerResults(context.Background(), request); err != nil || reply.Code != "" || len(fixture.values) != 2 || len(s.ownerOrigins) != 2 {
		t.Fatal("emptyretry replaced result authority", reply.Code, err)
	}
	request.Producer = &first.binding
	if reply, err := s.bindOwnerResults(context.Background(), request); err != nil || reply.Code != "" || len(fixture.values) != 2 {
		t.Fatal("exactproducer rebind was not idempotent", reply.Code, err)
	}
	fresh := &invocationObserver{session: s, binding: current}
	if value, err := fresh.ReadOwnerCreated(original.Ref); err != nil || value != original {
		t.Fatal("old ref unavailable afterretry", err)
	}
	conflicting := addOwnerResultProducer(t, s, request, 6)
	completeOwnerProject(t, conflicting, ownerProjectCreateIntent(conflicting, "current", "subyard-test-yard"))
	ownerResultsTerminal(t, s, conflicting, "complete")
	request.Producer = &conflicting.binding
	if reply, err := s.bindOwnerResults(context.Background(), request); err == nil && reply.Code == "" {
		t.Fatal("new producer silently replaced original role")
	}
}

func (f *ownerResultsFixture) ResolveOwnerTarget(_ context.Context, target Target) (Target, error) {
	target.InstanceUUID = "original-created-uuid"
	return target, nil
}
func TestNativeOwnerResultVerbsPassStrictSessionAdmission(t *testing.T) {
	s, request, observer, fixture, _ := ownerResultsOriginal(t)
	ownerResultsTerminal(t, s, observer, "complete")
	root, err := IdentityOf(s.root)
	if err != nil {
		t.Fatal(err)
	}
	s.context.Root = root
	request.Session = s.context.Session
	raw, _ := json.Marshal(request)
	if reply, err := s.handle(context.Background(), raw, nil); err != nil || reply.Code != "" || len(fixture.values) != 1 {
		t.Fatal("closed verb refused at admission", reply.Code, err)
	}
	target := Target{Owner: "original-owner", Source: strings.Repeat("a", 64), Desired: strings.Repeat("b", 64), InstanceUUID: "absent"}
	raw, _ = json.Marshal(Request{Op: "resolve-owner-target", Session: s.context.Session, Target: &target})
	reply, err := s.handle(context.Background(), raw, nil)
	if err != nil || len(reply.Targets) != 1 || reply.Targets[0].InstanceUUID != "original-created-uuid" {
		t.Fatal("resolver verb refused at admission", err)
	}
	for _, op := range []string{"bind-owner-results", "resolve-owner-target"} {
		s.ownerRelease = nil
		mutation := Request{Op: op, Session: s.context.Session, Binding: request.Binding, Producer: request.Producer, Before: request.Before, Token: request.Token}
		if op == "resolve-owner-target" {
			mutation = Request{Op: op, Session: s.context.Session, Binding: request.Binding, Target: &target}
		}
		raw, _ = json.Marshal(mutation)
		if _, err = s.handle(context.Background(), raw, nil); err == nil {
			t.Fatal("effect-time result binding lost owner lease", op)
		}
	}
	raw, _ = json.Marshal(Request{Op: "capture-owner", Session: s.context.Session, Target: &target})
	if _, err = s.handle(context.Background(), raw, nil); err == nil {
		t.Fatal("unrelated verb accepted target field")
	}
	foreign := map[string]any{"op": "bind-owner-results", "session": s.context.Session, "producer": request.Producer, "before": request.Before, "token": request.Token, "path": "arbitrary"}
	raw, _ = json.Marshal(foreign)
	if _, err = s.handle(context.Background(), raw, nil); err == nil {
		t.Fatal("new verbs widened native fields")
	}
	s.binding = nil
	s.leases = nil
	s.ownerRelease = nil
	s.context.Artifact = request.Producer.Artifact
	s.image = request.Producer.Helper
	request.Binding = nil
	raw, _ = json.Marshal(request)
	if reply, err := s.handle(context.Background(), raw, nil); err != nil || reply.Code != "" {
		t.Fatal("pure explicit result binding acquired mutation grant", reply.Code, err)
	}
}
