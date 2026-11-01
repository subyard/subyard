//go:build linux

package releaseowner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func hostInstallationInput() ports.NativeHostPowerInstallation {
	h := strings.Repeat("a", 64)
	v := ports.NativeHostPowerInstallation{HostSource: h, Output: h, Consumer: ports.NativeHostPowerConsumer{EngineDigest: h, UnitDigest: h, ContractsDigest: h, FilesDigest: h}}
	for i, role := range []string{"engine", "service", "publication"} {
		v.Consumer.Generations = append(v.Consumer.Generations, ports.NativeResourceGeneration{Role: role, Value: h})
		v.Identities = append(v.Identities, ports.NativeHostPowerIdentity{Role: role, Device: 1, Inode: uint64(i + 1)})
	}
	return v
}
func hostInstallationOriginal(t *testing.T) (*session, Request, *invocationObserver, string) {
	t.Helper()
	s, b, path := nativeFactFixture(t)
	before := OwnerBefore{State: "legacy-directory", Registration: b.Artifact, Overrides: b.Artifact, Controller: b.Artifact, PreparedRecipe: strings.Repeat("b", 64)}
	token := strings.Repeat("1", 32)
	recipe, e := recipeFingerprint(Request{Op: "apply-owner", Before: &before, Token: token})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.writeFact(b, "intent", Intent{Version: 1, Binding: b, Owner: "owner-registration", Resource: "test-yard", Expected: b.Artifact, Desired: b.Artifact, Source: b.Artifact, Recipe: recipe}); e != nil {
		t.Fatal(e)
	}
	if e = s.writeFact(b, "accepted", Terminal{Version: 1, Binding: b, Owner: "owner-registration", Resource: "test-yard", State: "accepted", Operations: []ports.NativeOperation{}}); e != nil {
		t.Fatal(e)
	}
	o := &invocationObserver{session: s, binding: b, ownerToken: token}
	return s, Request{Op: "bind-owner-results", Producer: &b, Before: &before, Token: token}, o, path
}
func TestNativeHostInstallationFreshRetryRequiresWholeFailedProducer(t *testing.T) {
	for _, cut := range []string{"valid", "missing-whole", "missing-command-terminal", "foreign-fact", "interrupted-result", "compacted", "partial-compaction", "contradictory-full", "contradictory-partial", "different-intent", "different-journal", "restored-record"} {
		t.Run(cut, func(t *testing.T) {
			s, request, o, path := hostInstallationOriginal(t)
			command, e := o.CommandStarted("owner-host-power-install", o.binding.Artifact)
			if e != nil {
				t.Fatal(e)
			}
			if e = o.CommandTerminal(command); e != nil {
				t.Fatal(e)
			}
			result, e := o.RecordHostPowerInstallation(command, hostInstallationInput())
			if e != nil {
				t.Fatal(e)
			}
			op := ports.NativeOperation{Kind: "instance-state", Server: o.binding.Artifact, Project: "project", Resource: "yard"}
			if e = o.Before(op); e != nil {
				t.Fatal(e)
			}
			op.ID = "start-original"
			if e = o.Accepted(op); e != nil {
				t.Fatal(e)
			}
			op.Status = 400
			if e = o.Terminal(op); e != nil {
				t.Fatal(e)
			}
			failure := &ports.NativeOperationFailure{Operation: op, Cause: errors.New("confirmed initial start failure")}
			if !o.failed(errors.Join(fmt.Errorf("component: %w", failure), nil)) {
				t.Fatal("production singleton cleanup join did not preserve closed SDK failure")
			}
			if o.failed(errors.Join(failure, errors.New("unclosed cleanup"))) {
				t.Fatal("multiple causes classified failed")
			}
			if cut != "missing-whole" && cut != "interrupted-result" {
				ownerResultsTerminal(t, s, o, "failed")
			}
			if cut == "missing-command-terminal" {
				if e = os.Remove(filepath.Join(path, "001.command-001-terminal.json")); e != nil {
					t.Fatal(e)
				}
			}
			if cut == "foreign-fact" {
				var fact hostPowerInstallationFact
				raw, _ := os.ReadFile(filepath.Join(path, "001.host-power-installation.json"))
				if e = json.Unmarshal(raw, &fact); e != nil {
					t.Fatal(e)
				}
				fact.Result.Source = strings.Repeat("c", 64)
				raw, _ = json.Marshal(fact)
				testkit.WriteFile(t, filepath.Join(path, "001.host-power-installation.json"), append(raw, '\n'), 0600)
			}
			if cut == "compacted" || cut == "partial-compaction" || cut == "contradictory-full" || cut == "contradictory-partial" {
				closure := ownerCreateClosure{Version: 1, Binding: o.binding, Commands: o.commands}
				for _, p := range []struct {
					k   string
					out *string
				}{{"intent", &closure.Intent}, {"accepted", &closure.Accepted}, {"terminal", &closure.Terminal}} {
					raw, e := os.ReadFile(filepath.Join(path, factName(1, p.k)))
					if e != nil {
						t.Fatal(e)
					}
					*p.out = digest(raw)
				}
				for _, phase := range []string{"intent", "terminal"} {
					name := factName(1, "command-001-"+phase)
					raw, e := os.ReadFile(filepath.Join(path, name))
					if e != nil {
						t.Fatal(e)
					}
					closure.Facts = append(closure.Facts, ownerCreateClosureFact{Name: name, Size: uint64(len(raw)), SHA256: digest(raw)})
					if cut == "compacted" || phase == "terminal" && cut != "contradictory-full" {
						if e = os.Remove(filepath.Join(path, name)); e != nil {
							t.Fatal(e)
						}
					}
				}
				if cut == "contradictory-full" || cut == "contradictory-partial" {
					closure.Commands = append([]commandFact(nil), closure.Commands...)
					closure.Commands[0].Source = strings.Repeat("c", 64)
					encoded, _ := json.Marshal(closure.Commands[0])
					encoded = append(encoded, '\n')
					for i := range closure.Facts {
						closure.Facts[i].Size = uint64(len(encoded))
						closure.Facts[i].SHA256 = digest(encoded)
					}
				}
				raw, _ := json.Marshal(closure)
				testkit.WriteFile(t, filepath.Join(path, "001.closure.json"), append(raw, '\n'), 0600)
			}
			if cut == "restored-record" {
				o.restored = true
				if _, e = o.RecordHostPowerInstallation(command, hostInstallationInput()); e == nil {
					t.Fatal("restored observer recorded new result")
				}
				return
			}
			current := o.binding
			current.Ordinal = 2
			if cut == "different-journal" {
				current.Journal = strings.Repeat("c", 64)
			}
			s.binding = &current
			s.leases = []*os.File{s.root, s.root}
			s.ownerRelease = func() {}
			request.Binding = &current
			s.components = &ownerResultsFixture{}
			reply, e := s.bindOwnerResults(context.Background(), request)
			valid := cut == "valid" || cut == "compacted" || cut == "partial-compaction" || cut == "different-intent" || cut == "different-journal"
			if !valid {
				if e == nil && reply.Code == "" {
					t.Fatal("incomplete/foreign original imported")
				}
				return
			}
			if e != nil || reply.Code != "" {
				t.Fatal(reply, e)
			}
			if len(s.ownerOrigins) != 1 {
				t.Fatal("install-only producer discarded with empty creates")
			}
			root, e := o.creationRoot()
			if e != nil {
				t.Fatal(e)
			}
			root.Binding = current
			if cut == "different-intent" {
				root.Expected = strings.Repeat("c", 64)
			}
			// Fresh live observer retains its own checked invocation; restored origin never does.
			raw, _ := json.Marshal(root)
			testkit.WriteFile(t, filepath.Join(path, factName(2, "intent")), append(raw, '\n'), 0600)
			accept, _ := json.Marshal(Terminal{Version: 1, Binding: current, Owner: "owner-registration", Resource: "test-yard", State: "accepted", Operations: []ports.NativeOperation{}})
			testkit.WriteFile(t, filepath.Join(path, factName(2, "accepted")), append(accept, '\n'), 0600)
			fresh := &invocationObserver{session: s, binding: current, ownerToken: request.Token}
			actual, e := fresh.ReadHostPowerInstallation(result.Producer)
			if cut == "different-intent" || cut == "different-journal" {
				if e == nil {
					t.Fatal("changed current intent imported")
				}
				return
			}
			if e != nil || actual == nil || !reflect.DeepEqual(*actual, result) {
				t.Fatal(actual, e)
			}
			if _, _, e = s.ownerOrigins[result.Producer].CheckedInvocation(); e == nil {
				t.Fatal("restored origin gained checked invocation")
			}
		})
	}
}
func TestNativeHostInstallationRequiresTerminalAndSingleInstallBudget(t *testing.T) {
	s, _, o, path := hostInstallationOriginal(t)
	_ = s
	c, e := o.CommandStarted("owner-host-power-install", o.binding.Artifact)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = o.RecordHostPowerInstallation(c, hostInstallationInput()); e == nil {
		t.Fatal("result before commandterminal")
	}
	if _, e = os.Stat(filepath.Join(path, "001.host-power-installation.json")); !os.IsNotExist(e) {
		t.Fatal("unclosed command published result", e)
	}
	_, _, o, _ = hostInstallationOriginal(t)
	if _, e = o.CommandStarted("owner-host-power-install", o.binding.Artifact); e != nil {
		t.Fatal(e)
	}
	if _, e = o.CommandStarted("owner-host-power-install", o.binding.Artifact); e == nil {
		t.Fatal("duplicate installer admitted")
	}
}
