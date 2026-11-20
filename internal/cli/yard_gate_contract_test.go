package cli

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/yardnetwork"
)

// The verdict table is the gate's contract. Columns are the selected yard's
// state, then another yard's: o open, p pending findings only, r limited to
// repair. Cells: ok, refuse, repair (admitted as the selected yard's repair),
// skip (an optional action left out), repair+skip.
func TestVerdictTable(t *testing.T) {
	other := "other"
	subjects := []struct {
		name    string
		subject gateSubject
	}{
		{"undeclared", gateSubject{}},
		{"selected writer", gateSubject{declared: true}},
		{"selected writer kept by pending", gateSubject{declared: true, pending: true}},
		{"plain init", initSubject(initArguments{mode: initReconcile})},
		{"init --configs", initSubject(initArguments{mode: initConfigs})},
		{"local yard setting", configAuthoringSubject("selected", []configAuthoringRequest{{scope: "yard"}})},
		{"local host setting", configAuthoringSubject("selected", []configAuthoringRequest{{scope: "host"}})},
		{"Git setting", configAuthoringSubject("selected", []configAuthoringRequest{{scope: "yard", git: true}})},
		{"link to another yard", gateSubject{declared: true, host: true, pending: true, acts: []yardAction{{yard: other}}}},
		{"link from the selected yard", gateSubject{declared: true, host: true, pending: true, acts: []yardAction{{yard: "selected"}, {yard: other}}}},
		{"policy change of another yard", gateSubject{declared: true, host: true, pending: true, acts: []yardAction{{yard: other, refusePending: true}}}},
		{"apply --all-local", gateSubject{declared: true, member: memberConfig, acts: []yardAction{{yard: other, optional: true}}}},
		{"sync writing another yard", configSyncSubject([]string{"yards/other/config.env"}, nil)},
		{"sync refreshing another yard", configSyncSubject([]string{"yards/selected/config.env"}, []string{"other"})},
	}
	columns := []string{"oo", "op", "or", "po", "pp", "pr", "ro", "rp", "rr"}
	want := map[string][]string{
		"undeclared":                      {"ok", "refuse", "refuse", "refuse", "refuse", "refuse", "refuse", "refuse", "refuse"},
		"selected writer":                 {"ok", "ok", "ok", "refuse", "refuse", "refuse", "refuse", "refuse", "refuse"},
		"selected writer kept by pending": {"ok", "ok", "ok", "ok", "ok", "ok", "refuse", "refuse", "refuse"},
		"plain init":                      {"ok", "ok", "ok", "ok", "ok", "ok", "repair", "repair", "repair"},
		"init --configs":                  {"ok", "ok", "ok", "refuse", "refuse", "refuse", "refuse", "refuse", "refuse"},
		"local yard setting":              {"ok", "ok", "ok", "refuse", "refuse", "refuse", "repair", "repair", "repair"},
		"local host setting":              {"ok", "refuse", "ok", "refuse", "refuse", "refuse", "repair", "refuse", "repair"},
		"Git setting":                     {"ok", "refuse", "refuse", "refuse", "refuse", "refuse", "refuse", "refuse", "refuse"},
		"link to another yard":            {"ok", "ok", "refuse", "ok", "ok", "refuse", "ok", "ok", "refuse"},
		"link from the selected yard":     {"ok", "ok", "refuse", "ok", "ok", "refuse", "refuse", "refuse", "refuse"},
		"policy change of another yard":   {"ok", "refuse", "refuse", "ok", "refuse", "refuse", "ok", "refuse", "refuse"},
		"apply --all-local":               {"ok", "skip", "skip", "refuse", "refuse", "refuse", "repair", "repair+skip", "repair+skip"},
		"sync writing another yard":       {"ok", "refuse", "ok", "refuse", "refuse", "refuse", "repair", "refuse", "repair"},
		"sync refreshing another yard":    {"ok", "refuse", "refuse", "refuse", "refuse", "refuse", "repair", "refuse", "refuse"},
	}
	state := func(code byte) *yardGateState {
		switch code {
		case 'p':
			return &yardGateState{pending: true}
		case 'r':
			return &yardGateState{failed: true}
		}
		return nil
	}
	for _, row := range subjects {
		expected, ok := want[row.name]
		if !ok || len(expected) != len(columns) {
			t.Fatalf("row %q has no complete expectation", row.name)
		}
		for index, column := range columns {
			gate := yardGate{yards: map[string]*yardGateState{}}
			for _, yard := range []struct {
				name string
				code byte
			}{{"selected", column[0]}, {other, column[1]}} {
				if found := state(yard.code); found != nil {
					gate.yards[yard.name] = found
					gate.order = append(gate.order, yard.name)
				}
			}
			sort.Strings(gate.order)
			verdict := gate.verdict("selected", row.subject)
			got := "ok"
			switch {
			case len(verdict.refusals) != 0:
				got = "refuse"
			case verdict.repair && len(verdict.skip) != 0:
				got = "repair+skip"
			case verdict.repair:
				got = "repair"
			case len(verdict.skip) != 0:
				got = "skip"
			}
			if got != expected[index] {
				t.Errorf("%s with %s: %s, want %s (%v)", row.name, column, got, expected[index], verdict.err())
			}
		}
	}
	// Every kind of repair finding gates alike.
	for _, finding := range []yardGateState{{drift: true}, {failed: true}, {config: true}, {unreachable: true}} {
		gate := yardGate{yards: map[string]*yardGateState{"selected": &finding}, order: []string{"selected"}}
		if verdict := gate.verdict("selected", initSubject(initArguments{mode: initReconcile})); !verdict.repair || verdict.err() != nil {
			t.Errorf("finding %+v is not repaired by init: %v", finding, verdict.err())
		}
		if verdict := gate.verdict("selected", gateSubject{declared: true}); verdict.err() == nil {
			t.Errorf("finding %+v admits a non-repair", finding)
		}
	}
	// Refusals come in a stable order: selected, written yards, then acted-on yards.
	gate := yardGate{yards: map[string]*yardGateState{"selected": {failed: true}, "a": {pending: true}, "b": {failed: true}}, order: []string{"a", "b", "selected"}}
	verdict := gate.verdict("selected", gateSubject{declared: true, scope: scopeNamed, named: []string{"a"}, acts: []yardAction{{yard: "b"}}})
	if len(verdict.refusals) != 3 || !strings.Contains(verdict.refusals[0].Error(), "yard selected") ||
		!strings.Contains(verdict.refusals[1].Error(), "yard a") || !strings.Contains(verdict.refusals[2].Error(), "yard b") {
		t.Fatalf("refusal order: %v", verdict.err())
	}
}

// goldenCommand is the contract of one command: per verb, read, early (it is
// dispatched before the gate), or a write that is either outside the repair
// set or a member of it.
type goldenCommand struct {
	prepared bool
	// verbs maps each registry verb, or "" for a command without verbs, to its
	// class: read, write or a repair member's name.
	verbs map[string]string
	// options maps each registry option to read (makes the invocation a
	// reader), value (takes a value) or flag.
	options map[string]string
}

func flags(names ...string) map[string]string {
	result := map[string]string{"--yes": "flag", "--help": "read"}
	for _, name := range names {
		class := "flag"
		if kind, option, found := strings.Cut(name, ":"); found {
			class, name = kind, option
		}
		result[name] = class
	}
	return result
}

var goldenCommands = map[string]goldenCommand{
	"network":     {prepared: true, verbs: map[string]string{"status": "read", "link": "write", "unlink": "write", "isolation": "write", "reconcile": "write"}, options: flags("--json")},
	"init":        {prepared: true, verbs: map[string]string{"": "init"}, options: flags("--configs", "--reset", "value:--profile", "--continue-legacy", "--adopt")},
	"start":       {prepared: true, verbs: map[string]string{"": "start"}, options: flags()},
	"stop":        {prepared: true, verbs: map[string]string{"": "stop"}, options: flags("--force")},
	"cleanup":     {prepared: true, verbs: map[string]string{"": "write"}, options: flags("--soft", "--hard")},
	"keys":        {prepared: true, verbs: map[string]string{"trust": "write", "untrust": "write", "add": "write", "import": "write", "list": "write", "status": "write", "history": "write", "sync": "write", "auto-sync": "write", "materialize": "write", "rotate": "write", "rollback": "write", "revoke": "write", "delete": "write", "resolve": "write", "move": "write"}, options: flags("value:--kind", "value:--zone", "value:--consumer", "value:--file", "--local-only", "--exclusive", "--dry-run", "--manual-only", "--all", "--now")},
	"ssh-agent":   {prepared: false, verbs: map[string]string{"unlock": "write", "status": "read", "lock": "read"}, options: flags("value:--key", "value:--ttl", "--json")},
	"shell":       {prepared: false, verbs: map[string]string{"": "shell"}, options: flags("--root")},
	"provision":   {prepared: true, verbs: map[string]string{"": "write"}, options: flags("-l", "--list")},
	"service":     {prepared: true, verbs: map[string]string{"register": "write", "install": "write", "start": "write", "stop": "write", "update": "write", "remove": "write"}, options: flags("value:--owner", "value:--pool", "value:--yard")},
	"test-vms":    {prepared: true, verbs: map[string]string{"logs": "read", "status": "read", "refresh": "write", "revoke": "write", "recover": "write", "retire-legacy": "write"}, options: flags("value:--owner", "value:--pool", "value:--slot", "value:-n", "-f", "--json")},
	"teardown":    {prepared: true, verbs: map[string]string{"": "teardown"}, options: flags("--keep-data", "--reset-config", "--full")},
	"sync":        {prepared: true, verbs: map[string]string{"": "write"}, options: flags("value:--name", "value:--target")},
	"bind":        {prepared: true, verbs: map[string]string{"": "write"}, options: flags("value:--name", "value:--target")},
	"clone":       {prepared: true, verbs: map[string]string{"": "write"}, options: flags("value:--name", "value:--target")},
	"code":        {prepared: true, verbs: map[string]string{"": "write"}, options: flags()},
	"codex":       {prepared: true, verbs: map[string]string{"export": "read", "open": "read"}, options: flags("--check", "value:--config")},
	"export":      {prepared: true, verbs: map[string]string{"": "write"}, options: flags()},
	"remove":      {prepared: true, verbs: map[string]string{"": "write"}, options: flags("--soft")},
	"up":          {prepared: true, verbs: map[string]string{"": "write"}, options: flags("--rebuild")},
	"down":        {prepared: true, verbs: map[string]string{"": "write"}, options: flags()},
	"host":        {prepared: true, verbs: map[string]string{"add": "write", "list": "read", "rename": "write", "remove": "write", "repair": "write"}, options: flags("value:--source-host-id")},
	"remote":      {prepared: true, verbs: map[string]string{"add": "write", "repair-key": "write", "remove": "write", "list": "write"}, options: flags("value:--yard")},
	"update":      {prepared: true, verbs: map[string]string{"": "write"}, options: flags("read:--check", "--json", "value:--version", "--offline", "--rollback", "--force")},
	"migrate":     {prepared: true, verbs: map[string]string{"": "write"}, options: flags("--check", "--json")},
	"integration": {prepared: true, verbs: map[string]string{"enable": "write", "disable": "integration disable", "status": "read"}, options: flags("--json")},
	"profile":     {prepared: true, verbs: map[string]string{"list": "read", "enable": "write", "disable": "profile disable", "setup": "write", "status": "read"}, options: flags("--json")},
	"config": {prepared: true, verbs: map[string]string{"fields": "read", "show": "read", "paths": "read", "validate": "early", "repair-alias": "early", "reduce-selection": "early",
		"set": "config", "unset": "config", "import": "config", "edit": "config", "repair-registration": "write", "status": "read", "apply": "config", "sync": "config"},
		options: flags("--all-local", "--json", "--check", "--adopt", "value:--host-id", "value:--checkout", "--offline", "--apply", "--init", "value:--scope", "--local", "--git", "value:-m", "value:--message")},
	"rpc":                 {prepared: false, verbs: map[string]string{"": "write"}, options: map[string]string{"--stdio": "flag"}},
	"svc":                 {prepared: false, verbs: map[string]string{"": "write"}, options: flags()},
	"_authorize":          {prepared: false, verbs: map[string]string{"": "write"}, options: map[string]string{}},
	"_state":              {prepared: false, verbs: map[string]string{"": "write"}, options: map[string]string{}},
	"_project-state":      {prepared: false, verbs: map[string]string{"": "write"}, options: map[string]string{}},
	"_migrate":            {prepared: false, verbs: map[string]string{"paths": "write", "check": "write"}, options: map[string]string{}},
	"_release-transition": {prepared: false, verbs: map[string]string{"": "write"}, options: map[string]string{}},
	"_vm-start-policy":    {prepared: false, verbs: map[string]string{"": "write"}, options: map[string]string{}},
	"_keys-exchange":      {prepared: true, verbs: map[string]string{"": "write"}, options: map[string]string{}},
	"_keys-auto-sync":     {prepared: true, verbs: map[string]string{"": "write"}, options: map[string]string{}},
	"_keys-init":          {prepared: true, verbs: map[string]string{"": "write"}, options: map[string]string{}},
}

// Every mutating registry command, verb and option has a golden row. The row
// fixes whether the command is prepared, how admission classifies each verb
// and option shape, and the repair member its handler's own parser declares.
func TestGoldenCommandTable(t *testing.T) {
	manifest := shippedManifest(t)
	program := &CLI{}
	for _, definition := range manifest.Commands() {
		if definition.Effect != command.EffectMutate {
			continue
		}
		golden, ok := goldenCommands[definition.Name]
		if !ok {
			t.Errorf("mutating command %q has no golden row", definition.Name)
			continue
		}
		behavior, err := resolveCoreCommand(definition)
		if err != nil {
			t.Fatal(err)
		}
		if prepared := behavior.prepare != nil; prepared != golden.prepared {
			t.Errorf("%s prepared=%t, golden %t", definition.Name, prepared, golden.prepared)
		}
		verbs := slices.Clone(definition.Verbs)
		if len(verbs) == 0 {
			verbs = []string{""}
		}
		for _, verb := range verbs {
			if _, ok := golden.verbs[verb]; !ok {
				t.Errorf("%s verb %q is unclassified", definition.Name, verb)
			}
		}
		for _, option := range definition.Options {
			if _, ok := golden.options[option]; !ok {
				t.Errorf("%s option %q is unclassified", definition.Name, option)
			}
		}
		for verb, class := range golden.verbs {
			if verb != "" && !slices.Contains(definition.Verbs, verb) {
				t.Errorf("%s golden verb %q is not in the registry", definition.Name, verb)
			}
			base := []string{}
			if verb != "" {
				base = append(base, verb)
			}
			base = append(base, completeArguments(definition.Name, verb)...)
			admission := program.admission(definition, true, resource.Definition{}, false, base, false)
			if class == "early" {
				if admission.early == earlyNone {
					t.Errorf("%s %s is not dispatched before the gate", definition.Name, verb)
				}
				continue
			}
			if admission.readOnly != (class == "read") {
				t.Errorf("%s %s read-only=%t, golden %s", definition.Name, verb, admission.readOnly, class)
			}
			if class != "read" && class != "write" {
				subject := program.provisionalSubject(gateRequest{handler: definition.Handler, name: definition.Name, arguments: base})
				if subject.member.String() != class && !strings.HasPrefix(subject.member.String(), class+" ") {
					t.Errorf("%s %s member %q, golden %q", definition.Name, verb, subject.member, class)
				}
			}
			// A value is never an option: each value-taking option followed by a
			// flag-like value classifies as the option with a plain value.
			for option, optionClass := range golden.options {
				if optionClass != "value" {
					continue
				}
				plain := program.admission(definition, true, resource.Definition{}, false, append(slices.Clone(base), option, "value"), false).readOnly
				for _, value := range []string{"--check", "--yes", "--git", "--json"} {
					shape := append(slices.Clone(base), option, value)
					if got := program.admission(definition, true, resource.Definition{}, false, shape, false).readOnly; got != plain {
						t.Errorf("%s %v read-only=%t, with a plain value %t", definition.Name, shape, got, plain)
					}
				}
			}
		}
	}
}

// completeArguments completes a verb so its handler's parser accepts it.
func completeArguments(name, verb string) []string {
	switch name + " " + verb {
	case "network link", "network unlink":
		return []string{"default", "named"}
	case "ssh-agent unlock":
		return []string{"--key", "/tmp/key", "--ttl", "1h"}
	case "profile status", "profile enable", "profile disable", "profile setup":
		return []string{"sample"}
	case "integration enable", "integration disable":
		return []string{"codex"}
	}
	if name != "config" {
		return nil
	}
	switch verb {
	case "set":
		return []string{"SSH_PORT", "2230", "--scope", "yard", "--local"}
	case "unset", "edit":
		return []string{"SSH_PORT", "--scope", "yard", "--local"}
	case "import":
		return []string{"AGENT_codex_RULES", "/tmp/rules", "--scope", "yard", "--local"}
	}
	return nil
}

func shippedManifest(t *testing.T) command.Manifest {
	t.Helper()
	file, err := os.Open(filepath.Join(repositoryRoot(t), "config", "commands.registry"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	manifest, err := command.Parse(file)
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

// The admission of a prepared command is read only by the gate methods and
// Execute; the CLI's pre-dispatch admission only where it is set, carried
// to the prepared command, or decides a non-prepared mutator.
func TestAdmissionIsReadOnlyByTheGate(t *testing.T) {
	allowed := map[string][]string{
		"gateRecord":   {"yard_gate_verdict.go:gateAdmission", "yard_gate_verdict.go:gateSkipped", "cli.go:Handle"},
		"gateDeclared": {"yard_gate_verdict.go:setGateSubject", "yard_gate_verdict.go:gateSubject"},
		"gatePlanned":  {"yard_gate_verdict.go:bindGate", "cli.go:Handle"},
		"gateAdmitted": {"admission.go:admitReleaseGate", "admission.go:admitTeardownGate", "admission.go:admitTeardownInvocation", "admission.go:admitRPCOperation", "cli.go:Run", "cli.go:Handle", "cli.go:runReleaseTransitionYardCommandIO", "registered_yard_migration.go:finishRegisteredInitMigrations", "yard_gate_verdict.go:gateAdmission", "yard_scoped_gate.go:guardEffects", "resource_commands.go:runResourceCommand"},
	}
	files, err := filepath.Glob(filepath.Join(repositoryRoot(t), "internal/cli/*.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			site := filepath.Base(path) + ":" + function.Name.Name
			ast.Inspect(function.Body, func(node ast.Node) bool {
				selector, ok := node.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if sites, watched := allowed[selector.Sel.Name]; watched && !slices.Contains(sites, site) {
					t.Errorf("%s reads %s outside the gate", site, selector.Sel.Name)
				}
				return true
			})
		}
	}
}

// The closed list of non-prepared mutators, with the handlers this gate does
// not decide, is exactly the mutating handlers that have no prepared command.
func TestNonPreparedMutatorsAreTheHandlersWithoutPrepare(t *testing.T) {
	manifest := shippedManifest(t)
	var handlers []string
	for _, definition := range manifest.Commands() {
		behavior, err := resolveCoreCommand(definition)
		if err != nil {
			t.Fatal(err)
		}
		if definition.Effect == command.EffectMutate && behavior.prepare == nil && !slices.Contains(handlers, definition.Handler) {
			handlers = append(handlers, definition.Handler)
		}
	}
	var listed []string
	for handler := range nonPreparedMutators {
		listed = append(listed, handler)
	}
	for handler := range gateDispatchedElsewhere {
		listed = append(listed, handler)
	}
	sort.Strings(handlers)
	sort.Strings(listed)
	if !slices.Equal(handlers, listed) {
		t.Fatalf("non-prepared mutators %v, listed %v", handlers, listed)
	}
	for handler := range handlerSubjects {
		if _, overlap := nonPreparedMutators[handler]; overlap {
			t.Errorf("%s is both prepared and not", handler)
		}
	}
}

// oracleExcluded are commands the host-free oracle cannot run: sessions,
// terminals, network downloads, project copies and internal protocols.
var oracleExcluded = []string{"rpc", "shell", "ssh-agent", "_authorize", "_state", "_project-state", "_migrate", "_release-transition",
	"_vm-start-policy", "_keys-exchange", "_keys-auto-sync", "_keys-init", "svc", "code", "codex", "up", "down", "sync", "bind", "clone", "remote", "update", "test-vms"}

// The differential oracle runs generated shapes of every mutating command on a
// ready release with recording fakes. A shape that reaches any effect was not
// admitted as read-only, and when it ran as a prepared command, that command
// declared its gate subject.
func TestDifferentialAdmissionOracle(t *testing.T) {
	if testing.Short() {
		t.Skip("runs every generated command shape")
	}
	harness := newRepairHarnessWith(t, func(yardFailureScenario, string) {})
	t.Chdir(harness.scenario.fixture.cli.options.RepositoryRoot)
	// The fixture registry carries every command the oracle runs.
	registry := filepath.Join(harness.scenario.fixture.cli.options.RepositoryRoot, "config", "commands.registry")
	present := map[string]bool{}
	for _, line := range strings.Split(string(mustReadConfigFixture(t, registry)), "\n") {
		if fields := strings.Split(line, "|"); len(fields) > 1 && !strings.HasPrefix(line, "#") {
			present[fields[0]] = true
			for _, alias := range strings.Split(fields[1], ",") {
				present[alias] = alias != ""
			}
		}
	}
	for _, line := range strings.Split(string(mustReadConfigFixture(t, filepath.Join(repositoryRoot(t), "config", "commands.registry"))), "\n") {
		fields := strings.Split(line, "|")
		if len(fields) < 2 || strings.HasPrefix(line, "#") || present[fields[0]] || fields[1] != "" && present[fields[1]] {
			continue
		}
		testkit.WriteFile(t, registry, append(mustReadConfigFixture(t, registry), line+"\n"...), 0o600)
	}
	var bound []*preparedCommand
	gateBindObserver = func(prepared *preparedCommand) {
		if prepared.gateAdmission() == nil {
			t.Errorf("Run reached binding for %s without an admission", prepared.Definition.Handler)
		}
		bound = append(bound, prepared)
	}
	t.Cleanup(func() { gateBindObserver = nil })
	program := &CLI{}
	networkPolicy, _ := yardnetwork.Decode(nil)
	networkHost := &networkCLIHost{policy: networkPolicy, allowWrites: true, ready: true}
	snapshot := func() string {
		var digest strings.Builder
		for _, root := range []string{filepath.Join(harness.scenario.request.ConfigHome, "yards"), filepath.Join(harness.scenario.request.ConfigHome, "overrides")} {
			for _, path := range filesUnder(t, root) {
				digest.WriteString(path + "=" + string(mustReadConfigFixtureOrEmpty(path)) + "\n")
			}
		}
		return fmt.Sprintf("%d/%d/%s", harness.effects(), networkHost.writes, digest.String())
	}
	shapes, effective := 0, 0
	for _, definition := range shippedManifest(t).Commands() {
		golden, known := goldenCommands[definition.Name]
		if definition.Effect != command.EffectMutate || !known || slices.Contains(oracleExcluded, definition.Name) {
			continue
		}
		verbs := slices.Clone(definition.Verbs)
		if len(verbs) == 0 {
			verbs = []string{""}
		}
		for _, verb := range verbs {
			base := []string{}
			if verb != "" {
				base = append(base, verb)
			}
			base = append(base, completeArguments(definition.Name, verb)...)
			generated := [][]string{base, append([]string{"--yes"}, base...), append(slices.Clone(base), "--yes")}
			for option, class := range golden.options {
				switch class {
				case "value":
					generated = append(generated, append(slices.Clone(base), option, "x"), append(slices.Clone(base), option+"=x"))
					for _, value := range []string{"--check", "--help", "--yes", "--git"} {
						generated = append(generated, append(slices.Clone(base), option, value))
					}
				default:
					generated = append(generated, append(slices.Clone(base), option))
				}
			}
			for _, shape := range generated {
				shapes++
				before := snapshot()
				bound = nil
				arguments := append([]string{"-Y", "default", definition.Name}, shape...)
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				var stdout, stderr bytes.Buffer
				options := harness.scenario.fixture.cli.options
				options.Arguments, options.Stdout, options.Stderr = arguments, &stdout, &stderr
				options.Prompt = confirmingPrompt{during: func() {}}
				options.Stdin = strings.NewReader("")
				options.AdapterRunner, options.InitPlatform = harness.adapter, harness.platform
				options.NetworkPolicy = &yardnetwork.Service{Host: networkHost, Lock: testNetworkPolicyLock{}}
				cli, err := New(options)
				if err != nil {
					t.Fatal(err)
				}
				cli.nestedCommand = func(context.Context, []string, io.Writer) int { return 0 }
				cli.sourceBinding = harness.bindSources
				cli.Run(ctx)
				cancel()
				if strings.Contains(stderr.String(), "unknown command") {
					t.Fatalf("yard %s never reached its handler: %s", strings.Join(arguments, " "), stderr.String())
				}
				if snapshot() == before {
					continue
				}
				effective++
				t.Logf("effect: %s %s", definition.Name, strings.Join(shape, " "))
				if admission := program.admission(definition, true, resource.Definition{}, false, shape, false); admission.readOnly {
					t.Errorf("yard %s reached an effect but was admitted as read-only", strings.Join(arguments, " "))
				}
				for _, prepared := range bound {
					if !prepared.gateSubject().declared {
						t.Errorf("yard %s ran prepared %s without a declared gate subject", strings.Join(arguments, " "), prepared.Definition.Handler)
					}
				}
			}
		}
	}
	// The oracle must observe effects to prove anything. In this host-free
	// fixture the lifecycle, init and local configuration writers reach theirs;
	// most other handlers stop at their own host checks.
	if shapes < 100 || effective < 8 {
		t.Fatalf("%d shapes generated, %d reached an effect", shapes, effective)
	}
	t.Logf("%d shapes, %d reached an effect", shapes, effective)
}

// --check makes a reader only where the handler's parser reads it as the
// flag: bare config sync and update, never as another option's value.
func TestCheckOptionIsReadByTheHandlersParser(t *testing.T) {
	manifest := shippedManifest(t)
	program := &CLI{}
	for _, test := range []struct {
		name      string
		arguments []string
		read      bool
	}{
		{"config", []string{"sync", "--check"}, true},
		{"config", []string{"sync", "push", "-m", "--check", "--yes"}, false},
		{"config", []string{"sync", "push", "--message", "--check"}, false},
		{"config", []string{"sync", "pull", "--check"}, false},
		{"config", []string{"sync", "-m", "--check"}, false},
		{"update", []string{"--check"}, true},
		{"update", []string{"--version", "--check"}, false},
		{"update", []string{"--runtime-root", "--check", "--yes"}, false},
	} {
		definition, ok := manifest.Lookup(test.name)
		if !ok {
			t.Fatal(test.name)
		}
		if got := program.admission(definition, true, resource.Definition{}, false, test.arguments, false).readOnly; got != test.read {
			t.Errorf("%s %v read-only=%t, want %t", test.name, test.arguments, got, test.read)
		}
	}
}
