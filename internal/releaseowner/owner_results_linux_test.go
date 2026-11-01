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
	"reflect"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/sys/unix"
)

type ownerResultsFixture struct {
	Components
	values        []ports.NativeOwnerCreated
	rejectCreated bool
	physical      string
	physicalError error
	captured      int
	boundPhysical string
}

func (f *ownerResultsFixture) CaptureOwnerPhysical(ctx context.Context, _ OwnerBefore, _ string, _ OwnerActionReference) (string, error) {
	f.captured++
	observer := ports.NativeOperationObserverFrom(ctx).(*invocationObserver)
	if _, err := observer.session.readTerminal(observer.binding); err == nil {
		return "", errors.New("physical output captured after terminal")
	}
	return f.physical, f.physicalError
}

func (f *ownerResultsFixture) BindOwnerPhysical(_ context.Context, _ OwnerBefore, _ string, _ OwnerActionReference, physical string) error {
	f.boundPhysical = physical
	return nil
}

func (f *ownerResultsFixture) BindOwnerResults(_ context.Context, _ OwnerBefore, _ string, values []ports.NativeOwnerCreated) error {
	if f.rejectCreated && len(values) > 0 {
		return errors.New("created resource does not match original capsule")
	}
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
	op := "apply-owner"
	if request.ProducerAction != nil {
		op = "apply-owner-action"
	}
	recipe, _ := recipeFingerprint(Request{Op: op, Binding: &binding, Before: request.Before, Token: request.Token, Action: request.ProducerAction})
	if err := s.writeFact(binding, "intent", Intent{Version: 1, Binding: binding, Owner: "owner-registration", Resource: "test-yard", Expected: binding.Artifact, Desired: binding.Artifact, Source: binding.Artifact, Recipe: recipe}); err != nil {
		t.Fatal(err)
	}
	if err := s.writeFact(binding, "accepted", Terminal{Version: 1, Binding: binding, Owner: "owner-registration", Resource: "test-yard", State: "accepted", Operations: []ports.NativeOperation{}}); err != nil {
		t.Fatal(err)
	}
	return &invocationObserver{session: s, binding: binding, ownerToken: request.Token, action: request.ProducerAction}
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

func ownerResultsPhysical(t *testing.T, s *session, observer *invocationObserver) {
	t.Helper()
	root, err := observer.creationRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.writeFact(observer.binding, "physical", ownerPhysicalFact{1, observer.binding, root.Recipe, root.Source, strings.Repeat("d", 64)}); err != nil {
		t.Fatal(err)
	}
}

func TestNativeOwnerActionResultsKeepExactProducerAndConsumer(t *testing.T) {
	for _, change := range []string{"valid", "retained", "retained-rejected-create", "wrong-producer-action", "wrong-token", "wrong-payload", "wrong-consumer-action", "missing-consumer-action", "foreign-program", "missing-terminal", "readonly", "finalizer", "duplicate-role", "physical", "physical-failed", "physical-retained", "physical-source", "physical-recipe", "physical-binding", "physical-digest", "physical-noncanonical", "physical-rejected", "physical-missing-terminal", "physical-missing"} {
		t.Run(change, func(t *testing.T) {
			s, request, _, fixture, _ := ownerResultsOriginal(t)
			producer := OwnerActionReference{Group: *request.Producer, Program: strings.Repeat("a", 64), Index: 0, Action: OwnerAction{Operation: "project", Slot: 0}}
			request.ProducerAction = &producer
			first := addOwnerResultProducer(t, s, request, 3)
			created := completeOwnerProject(t, first, ownerProjectCreateIntent(first, "current", "subyard-test-yard"))
			if change != "physical-missing" {
				root, err := first.creationRoot()
				if err != nil {
					t.Fatal(err)
				}
				fact := ownerPhysicalFact{1, first.binding, root.Recipe, root.Source, strings.Repeat("d", 64)}
				switch change {
				case "physical-source":
					fact.Source = strings.Repeat("e", 64)
				case "physical-recipe":
					fact.Recipe = strings.Repeat("e", 64)
				case "physical-binding":
					fact.Binding.Ordinal++
				case "physical-digest":
					fact.Physical = "invalid"
				}
				if err := s.writeFact(first.binding, "physical", fact); err != nil {
					t.Fatal(err)
				}
				if change == "physical-noncanonical" {
					raw, _ := json.MarshalIndent(fact, "", " ")
					testkit.WriteFile(t, filepath.Join(s.root.Name(), "migrations/native-custody/v1", first.binding.Transaction, factName(first.binding.Ordinal, "physical")), append(raw, '\n'), 0600)
				}
			}
			member := ports.NativeOwnerMember{Device: 1, Inode: 2, UID: uint32(os.Getuid()), Mode: unix.S_IFREG | 0600, Links: 1, Size: 25, Digest: first.binding.Artifact}
			if change == "retained" {
				if err := first.RecordOwnerRestoration(request.Token, first.binding.Artifact, member); err != nil {
					t.Fatal(err)
				}
			}
			if change != "missing-terminal" && change != "physical-missing-terminal" {
				state := "complete"
				if change == "physical-failed" {
					state = "failed"
				}
				ownerResultsTerminal(t, s, first, state)
			}
			consumer := producer
			consumer.Index, consumer.Action = 1, OwnerAction{Operation: "init", Slot: 0}
			request.ProducerAction = &consumer
			second := addOwnerResultProducer(t, s, request, 4)
			s.binding = &second.binding
			request.Binding, request.Action, request.Producer, request.ProducerAction = &second.binding, &consumer, &first.binding, &producer
			switch change {
			case "retained":
				request.RetainedOnly = true
			case "physical-retained":
				request.RetainedOnly = true
			case "physical-rejected":
				fixture.rejectCreated = true
			case "retained-rejected-create":
				request.RetainedOnly, fixture.rejectCreated = true, true
			case "wrong-producer-action":
				copy := producer
				copy.Action.Slot = 1
				request.ProducerAction = &copy
			case "wrong-token":
				request.Token = strings.Repeat("2", 32)
			case "wrong-payload":
				request.Before.PreparedRecipe = strings.Repeat("c", 64)
			case "wrong-consumer-action":
				copy := consumer
				copy.Action.Operation = "project"
				request.Action = &copy
			case "missing-consumer-action":
				request.Action = nil
			case "foreign-program":
				copy := consumer
				copy.Program = strings.Repeat("c", 64)
				request.Action = &copy
			case "readonly":
				request.Binding, request.Action = nil, nil
				s.context = &Context{Session: first.binding.Session, Artifact: first.binding.Artifact}
				s.image = first.binding.Helper
			}
			reply, err := s.bindOwnerResults(context.Background(), request)
			valid := change == "valid" || change == "retained" || change == "readonly" || change == "finalizer" || change == "duplicate-role" || change == "physical" || change == "physical-failed" || change == "physical-retained"
			if !valid {
				if err == nil && reply.Code == "" {
					t.Fatal("unbound action admitted")
				}
				if fixture.boundPhysical != "" {
					t.Fatal("refused producer attached physical output")
				}
				return
			}
			if err != nil || reply.Code != "" || len(s.ownerOrigins) != 1 || s.ownerCompleted != nil {
				t.Fatal("action result admission or completion", reply.Code, err)
			}
			if change == "physical" || change == "physical-failed" {
				if fixture.boundPhysical != strings.Repeat("d", 64) {
					t.Fatal("closed producer physical output lost")
				}
			}
			if change == "retained" || change == "physical-retained" {
				if len(fixture.values) != 0 {
					t.Fatal("retired create retained")
				}
				if change == "retained" {
					if got, err := s.ownerOrigins[created.Ref.Producer].ReadOwnerRestoration(request.Token, first.binding.Artifact); err != nil || got != member {
						t.Fatal("restoration lost", err)
					}
				}
				if fixture.boundPhysical != "" {
					t.Fatal("retired producer restored physical output")
				}
				if _, err := s.ownerOrigins[created.Ref.Producer].ReadOwnerCreated(created.Ref); err == nil {
					t.Fatal("retired create readable")
				}
				return
			}
			if len(fixture.values) != 1 || fixture.values[0] != created {
				t.Fatal("original create lost")
			}
			if change == "readonly" {
				return
			}
			if err := s.checkOwnerResultEffectBinding(second.binding); err != nil {
				t.Fatal(err)
			}
			foreign := consumer
			foreign.Program = strings.Repeat("c", 64)
			s.ownerActions[second.binding] = foreign
			if s.checkOwnerResultEffectBinding(second.binding) == nil {
				t.Fatal("foreign consumer admitted")
			}
			s.ownerActions[second.binding] = consumer
			request.Producer, request.ProducerAction = &second.binding, &consumer
			if change == "duplicate-role" {
				completeOwnerProject(t, second, ownerProjectCreateIntent(second, "current", "subyard-test-yard"))
			}
			ownerResultsTerminal(t, s, second, "complete")
			ownerResultsPhysical(t, s, second)
			if reply, err = s.bindOwnerResults(context.Background(), request); change == "duplicate-role" {
				if err == nil && reply.Code == "" {
					t.Fatal("conflicting producer admitted")
				}
				return
			} else if err != nil || reply.Code != "" || len(fixture.values) != 1 || len(s.ownerOrigins) != 2 || s.ownerCompleted != nil {
				t.Fatal("empty action lost origin or completed group", reply.Code, err)
			}
			if change == "finalizer" {
				final := consumer
				final.Index, final.Action = 2, OwnerAction{Operation: "verify-desired"}
				request.ProducerAction = &final
				last := addOwnerResultProducer(t, s, request, 5)
				ownerResultsTerminal(t, s, last, "complete")
				ownerResultsPhysical(t, s, last)
				request.Producer = &last.binding
				if reply, err = s.bindOwnerResults(context.Background(), request); err != nil || reply.Code != "" || s.ownerCompleted == nil || *s.ownerCompleted != last.binding {
					t.Fatal("finalizer did not complete group", reply.Code, err)
				}
			}
		})
	}
}

func TestNativeOwnerPhysicalCaptureRequiresClosedInvocation(t *testing.T) {
	for _, change := range []string{"complete", "failed", "unknown", "capture-error", "failed-capture-error", "invalid-digest"} {
		t.Run(change, func(t *testing.T) {
			s, b, path := nativeFactFixture(t)
			before := OwnerBefore{State: "legacy-directory", Registration: b.Artifact, Overrides: b.Artifact, Controller: b.Artifact, PreparedRecipe: strings.Repeat("b", 64)}
			action := OwnerActionReference{Group: b, Program: b.Artifact, Action: OwnerAction{Operation: "init", Slot: 0}}
			request := Request{Op: "apply-owner-action", Binding: &b, Before: &before, Token: strings.Repeat("1", 32), Action: &action}
			recipe, err := recipeFingerprint(request)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.writeFact(b, "intent", Intent{Version: 1, Binding: b, Owner: "owner-registration", Resource: "test-yard", Expected: b.Artifact, Desired: b.Artifact, Source: b.Artifact, Recipe: recipe}); err != nil {
				t.Fatal(err)
			}
			fixture := &ownerResultsFixture{physical: strings.Repeat("d", 64)}
			if strings.HasSuffix(change, "capture-error") {
				fixture.physicalError = errors.New("physical observation unavailable")
			}
			if change == "invalid-digest" {
				fixture.physical = "invalid"
			}
			s.components = fixture
			var actionError error
			reply, err := s.apply(context.Background(), request, "owner-registration", "test-yard", func() error {
				observer := ports.NativeOperationObserverFrom(s.effectContext)
				if change == "unknown" {
					_, err := observer.CommandStarted("shipped-handler", b.Artifact)
					if err != nil {
						return err
					}
					return errors.New("unclosed command")
				}
				if strings.HasPrefix(change, "failed") {
					op := ports.NativeOperation{Kind: "instance-state", Server: b.Artifact, Project: "project", Resource: "yard"}
					if err := observer.Before(op); err != nil {
						return err
					}
					op.ID = "operation-1"
					if err := observer.Accepted(op); err != nil {
						return err
					}
					op.Status = 400
					if err := observer.Terminal(op); err != nil {
						return err
					}
					actionError = &ports.NativeOperationFailure{Operation: op, Cause: publicComponentFailure{message: "original action failed", retry: "retry the original operation"}}
				}
				return actionError
			})
			if err != nil {
				t.Fatal(err)
			}
			if change != "complete" && change != "failed" {
				if reply.Code != "unknown" || reply.Terminal != nil {
					t.Fatal("incomplete physical output became terminal", reply.Code)
				}
				if change == "unknown" && fixture.captured != 0 {
					t.Fatal("unclosed invocation captured physical output")
				}
				if change == "failed-capture-error" && !reflect.DeepEqual(reply.Diagnostic, componentDiagnostic(actionError)) {
					t.Fatal("capture failure hid original action diagnostic")
				}
				return
			}
			if reply.Terminal == nil || fixture.captured != 1 || reply.Terminal.State != change {
				t.Fatal("closed physical output missing", reply.Code)
			}
			raw, err := os.ReadFile(filepath.Join(path, factName(b.Ordinal, "physical")))
			var fact ownerPhysicalFact
			if err != nil || strict(raw, &fact) != nil || fact != (ownerPhysicalFact{1, b, recipe, b.Artifact, fixture.physical}) {
				t.Fatal("physical output binding lost", err)
			}
		})
	}
}

func TestNativeOwnerActionRetainedOriginKeepsInstallationAndCache(t *testing.T) {
	s, request, _, fixture, _ := ownerResultsOriginal(t)
	producer := OwnerActionReference{Group: *request.Producer, Program: strings.Repeat("a", 64), Action: OwnerAction{Operation: "power", Slot: 1}}
	request.ProducerAction = &producer
	first := addOwnerResultProducer(t, s, request, 3)
	command, err := first.CommandStarted("owner-host-power-install", first.binding.Artifact)
	if err != nil || first.CommandTerminal(command) != nil {
		t.Fatal("installation command")
	}
	installation, err := first.RecordHostPowerInstallation(command, hostInstallationInput())
	if err != nil {
		t.Fatal(err)
	}
	guard := ports.OwnerImageSourceGuard{Path: []byte("/original/cache"), State: "file:" + strings.Repeat("a", 64)}
	cache := ports.OwnerImageCacheSuccessor{Policy: first.binding.Artifact, Before: []ports.OwnerImageSourceGuard{guard}, After: []ports.OwnerImageSourceGuard{guard}}
	if err := first.RecordOwnerImageCache(cache); err != nil {
		t.Fatal(err)
	}
	ownerResultsPhysical(t, s, first)
	ownerResultsTerminal(t, s, first, "complete")
	consumer := producer
	consumer.Index, consumer.Action = 1, OwnerAction{Operation: "init", Slot: 0}
	request.ProducerAction = &consumer
	second := addOwnerResultProducer(t, s, request, 4)
	s.binding = &second.binding
	request.Binding, request.Action, request.Producer, request.ProducerAction, request.RetainedOnly = &second.binding, &consumer, &first.binding, &producer, true
	if reply, err := s.bindOwnerResults(context.Background(), request); err != nil || reply.Code != "" || len(fixture.values) != 0 || len(s.ownerOrigins) != 1 {
		t.Fatal(reply.Code, err)
	}
	actual, err := second.ReadHostPowerInstallation(installation.Producer)
	if err != nil || actual == nil || !reflect.DeepEqual(*actual, installation) {
		t.Fatal("installation lost", err)
	}
	facts, err := second.ReadOwnerImageCache(cache.Policy)
	if err != nil || len(facts) != 1 {
		t.Fatal("cache lost", err)
	}
	second.action = nil
	if _, err := second.ReadHostPowerInstallation(installation.Producer); err == nil {
		t.Fatal("legacy mixed with action installation")
	}
	if _, err := second.ReadOwnerImageCache(cache.Policy); err == nil {
		t.Fatal("legacy mixed with action cache")
	}
}
