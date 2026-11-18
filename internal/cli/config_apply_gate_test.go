package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
)

func TestConfigSyncPullRetriesPublishedMaterializedDrift(t *testing.T) {
	fixture := newConfigApplyRepairFixture(t, true)
	options := fixture.cli.options
	root, home := options.RepositoryRoot, fixture.cli.env["SUBYARD_CONFIG_HOME"]
	options.Environment = slices.DeleteFunc(slices.Clone(options.Environment), func(value string) bool {
		return strings.HasPrefix(value, "HOST_BASE=") || strings.HasPrefix(value, "RESTRICTED_DISK_PATHS=")
	})
	options.Environment = append(options.Environment,
		"GIT_AUTHOR_NAME=Subyard Test", "GIT_AUTHOR_EMAIL=test@invalid",
		"GIT_COMMITTER_NAME=Subyard Test", "GIT_COMMITTER_EMAIL=test@invalid",
	)
	writeCLIFile(t, filepath.Join(home, "yards/named/config.env"), fmt.Sprintf("SSH_PORT=2223\nHOST_BASE=%s/host-named\nRESTRICTED_DISK_PATHS=%s/host-named\n", root, root), 0o600)
	active, err := filepath.EvalSymlinks(filepath.Join(fixture.cli.env["YARD_RUNTIME_ROOT"], "current"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(active, "config", "release-transition.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(filepath.Join(active, "config"), os.DirFS(filepath.Join(root, "config"))); err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(filepath.Join(active, "config/agents/codex"), os.DirFS(filepath.Join(repositoryRoot(t), "config/agents/codex"))); err != nil {
		t.Fatal(err)
	}
	commands := filepath.Join(active, "config", "commands.registry")
	shipped := mustReadConfigFixture(t, filepath.Join(repositoryRoot(t), "config", "commands.registry"))
	for line := range strings.SplitSeq(string(shipped), "\n") {
		if strings.HasPrefix(line, "config|") {
			testkit.WriteFile(t, commands, append(mustReadConfigFixture(t, commands), []byte(line+"\n")...), 0o600)
		}
	}
	var manifest []byte
	if err := fs.WalkDir(os.DirFS(active), ".", func(path string, entry fs.DirEntry, err error) error {
		if err == nil && entry.Type().IsRegular() && path != "runtime-files.sha256" {
			manifest = fmt.Appendf(manifest, "%x  ./%s\n", sha256.Sum256(mustReadConfigFixture(t, filepath.Join(active, path))), path)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(active, "runtime-files.sha256"), manifest, 0o600)
	asset := "codex/rules/repo.rules"
	published := filepath.Join(home, config.GitSettingsRelativePath, "overrides/host/agents", asset)
	engine := string(mustReadConfigFixture(t, filepath.Join(active, "bin", "yard-engine")))
	engine = strings.ReplaceAll(engine, fmt.Sprintf("[ -e %q ]", fixture.readyMarker), fmt.Sprintf("[ -e %q ] || ! grep -qs updated %q", fixture.readyMarker, published))
	engine = strings.Replace(engine, `"consequences":["repair release activation"]},"outcome"`, `"consequences":["repair release activation"]},"decisions":[
{"resource":"activation.materialized-config","scope":"activation","decision":"canonicalize","result":"drift"},
{"resource":"activation.materialized-config","scope":"yard-attribution","decision":"preserve","result":"drift default"},
{"resource":"activation.materialized-config","scope":"yard-attribution","decision":"preserve","result":"drift named"}],"outcome"`, 1)
	sealCurrentEngine(t, fixture, engine)
	options.RepositoryRoot = active
	fake := options.Incus.(*testkit.Incus)
	applier := &recordingConfigApplier{}
	options.Config = applier
	run := func(want int, args ...string) string {
		t.Helper()
		var output bytes.Buffer
		options.Arguments, options.Stdout, options.Stderr = args, &output, &output
		cli, err := New(options)
		if err != nil {
			t.Fatal(err)
		}
		if code := cli.Run(context.Background()); code != want {
			t.Fatalf("%v exited %d, want %d\n%s", args, code, want, output.String())
		}
		return output.String()
	}
	remote, publisher := configBareSourceRepository(t, "owner-a", "")
	assetPath := filepath.Join(publisher, "hosts/owner-a/overrides/agents", asset)
	writeConfigCommandFile(t, assetPath, "original\n")
	writeConfigCommandFile(t, filepath.Join(publisher, "hosts/owner-a/yards/named/config.env"), "SSH_PORT=2223\n")
	commitConfigSource(t, publisher, "initial asset")
	runConfigSyncGit(t, publisher, "push", "-q")
	run(0, "config", "sync", "connect", remote, "--host-id", "owner-a", "--yes")
	writeConfigCommandFile(t, assetPath, "updated\n")
	commitConfigSource(t, publisher, "updated asset")
	runConfigSyncGit(t, publisher, "push", "-q")
	observe := func(converged bool, passes int) {
		t.Helper()
		fake.ExecSteps = nil
		for range passes {
			for _, yard := range []string{"default", "named"} {
				loaded := loadConfigCommandContext(t, active, options.Environment, yard)
				if converged {
					appendHashSteps(t, fake, loaded)
				} else {
					appendMismatchedHashSteps(t, fake, loaded, "0")
				}
			}
		}
	}
	history, err := fixture.cli.readReleaseHistory()
	if err != nil || history == nil {
		t.Fatalf("read release history: %v", err)
	}
	pending := filepath.Join(home, "release-transition/v2/.journal.json.pending")
	writeCLIFile(t, pending, "pending-fixture\n", 0o600)
	observe(false, 2)
	refusal := "protected release publication is pending"
	output := run(1, "config", "sync", "pull", "--apply", "--yes")
	if !strings.Contains(output, refusal+"; next: yard update") ||
		string(mustReadConfigFixture(t, published)) != "updated\n" || len(applier.yards) != 0 {
		t.Fatalf("published pull refusal or effects: %s, %v", output, applier.yards)
	}
	for _, args := range [][]string{{"config", "apply"}, {"init"}, {"status"}, {"update", "--check", "--offline"}} {
		want := 1
		if args[0] == "status" || args[0] == "update" {
			want = 0
		}
		if output := run(want, args...); !strings.Contains(output, refusal) || !strings.Contains(output, "yard update") || strings.Contains(output, ".journal.json.pending") {
			t.Fatalf("%v gave inconsistent advice: %s", args, output)
		}
	}
	// A source failure can block init while a different yard blocks update.
	const serviceBlocker = "yard named: guest service fixture-health readiness check failed"
	const inspectSource = "yard migrate --check"
	mixedEngine := strings.Replace(engine, `{"resource":"activation.materialized-config","scope":"yard-attribution","decision":"preserve","result":"drift named"}],"outcome"`,
		`{"resource":"activation.core-guest-runtime","scope":"yard-attribution","decision":"preserve","result":"failed named"}],
"blockers":[{"code":"dependency-unavailable","resource":"activation.core-guest-runtime","message":"`+serviceBlocker+`","retry":"yard -Y named init"}],"outcome"`, 1)
	mixedEngine = strings.ReplaceAll(mixedEngine, `"status":"migration-required"`, `"status":"operator-action-required","transaction":"tx-0123456789abcdef"`)
	mixedEngine = strings.ReplaceAll(mixedEngine, `"code":"transition-required","message":"activation drift","retry":"run yard update"`,
		`"code":"dependency-unavailable","message":"`+serviceBlocker+`","retry":"yard -Y named init"`)
	sealCurrentEngine(t, fixture, mixedEngine)
	for _, check := range []struct {
		exit int
		args []string
	}{
		{0, []string{"update", "--check", "--offline"}},
		{0, []string{"status"}},
		{0, []string{"-Y", "named", "status"}},
		{1, []string{"-Y", "named", "init"}},
		{1, []string{"-Y", "named", "provision"}},
		{1, []string{"update"}},
	} {
		output := run(check.exit, check.args...)
		if slices.Contains(check.args, "--check") {
			var report struct {
				Advice struct {
					Message string `json:"message"`
					Retry   string `json:"retry"`
				} `json:"advice"`
			}
			if err := json.Unmarshal([]byte(output), &report); err != nil {
				t.Fatalf("decode update check: %v\n%s", err, output)
			}
			if report.Advice.Retry != inspectSource {
				t.Fatalf("update check did not give one diagnostic command: %s", output)
			}
			output = report.Advice.Message + "\n" + report.Advice.Retry
		}
		if !strings.Contains(output, serviceBlocker) || !strings.Contains(output, refusal) || !strings.Contains(output, inspectSource) ||
			strings.Contains(output, "yard -Y named init") || strings.Contains(output, "next: yard update") {
			t.Fatalf("%v sent the operator back to a blocked command: %s", check.args, output)
		}
	}
	program, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	named, err := program.loadContext("named")
	if err != nil {
		t.Fatal(err)
	}
	for _, execute := range []bool{false, true} {
		fault := program.admitRPCOperation(context.Background(), named, command.Definition{Handler: "@provision", Name: "provision"}, nil, execute)
		var report mutationGateOutcome
		if fault == nil || json.Unmarshal([]byte(fault.Message), &report) != nil || report.Action != inspectSource ||
			strings.Contains(report.Message, "yard -Y named init") {
			t.Fatalf("RPC admission advice: %#v", fault)
		}
	}
	ctx := context.Background()
	if readiness := program.ownerReleaseReadiness(ctx); !strings.Contains(readiness.Message, serviceBlocker) ||
		!strings.Contains(readiness.Message, refusal) || readiness.Next != inspectSource {
		t.Fatalf("owner readiness advised a blocked repair: %#v", readiness)
	}
	outcome, inspection, err := program.inspectMutationGateInspection(ctx, "named")
	if err != nil {
		t.Fatal(err)
	}
	gate, ok := program.yardScopedGate(outcome, inspection)
	if !ok {
		t.Fatal("mixed readiness was not yard scoped")
	}
	gate = program.withYardAdvice(ctx, gate)
	var skipped bytes.Buffer
	program.options.Stderr = &skipped
	prepared := &preparedCommand{CLI: program, gateRecord: &gateAdmission{kind: admissionOpen, request: gateRequest{yard: "healthy"}, gate: &gate}}
	prepared.setGateSubject(gateSubject{declared: true, member: memberConfig, acts: []yardAction{{yard: "named", optional: true}}})
	if kept := prepared.gateSkipped([]configTarget{{Name: "named"}}, true); len(kept) != 0 || !strings.Contains(skipped.String(), refusal) ||
		strings.Contains(skipped.String(), "yard -Y named init") {
		t.Fatalf("optional target suggested a blocked repair: %v\n%s", kept, &skipped)
	}
	if err := os.Remove(pending); err != nil {
		t.Fatal(err)
	}
	recheckPrompt := &callbackPrompt{callback: func() { writeCLIFile(t, pending, "pending-fixture\n", 0o600) }}
	options.Prompt = recheckPrompt
	options.Executor = &selectionOnlyIntegrationExecutor{source: published, staleFiles: true}
	if output := run(1, "-Y", "named", "config", "apply"); len(recheckPrompt.requests) != 1 || len(applier.yards) != 0 ||
		!strings.Contains(output, refusal) || !strings.Contains(output, "next: "+inspectSource) {
		t.Fatalf("post-consent source refusal or effects: %s, %v", output, applier.yards)
	}
	options.Prompt, options.Executor = nil, fake
	sealCurrentEngine(t, fixture, engine)
	if err := os.Remove(pending); err != nil {
		t.Fatal(err)
	}
	observe(false, 1)
	if output := run(0, "update", "--check", "--offline"); !strings.Contains(output, "yard -Y default config apply") {
		t.Fatalf("update check omitted admissible repair: %s", output)
	}
	if output := run(0, "update", "--check", "--json", "--offline"); !strings.Contains(output, `"retry":"run yard update"`) {
		t.Fatalf("update JSON changed its retry: %s", output)
	}
	observe(true, 1)
	if output := run(0, "update", "--check", "--offline"); !strings.Contains(output, "yard -Y default init") || strings.Contains(output, "config apply") {
		t.Fatalf("update check recommended a file refresh for current files: %s", output)
	}
	probe := &readinessProbeExecutor{InstanceExecutor: &selectionOnlyIntegrationExecutor{source: published, staleFiles: true}}
	options.Executor = probe
	started := time.Now()
	if output := run(0, "-Y", "named", "status"); !strings.Contains(output, "next: yard -Y named config apply") {
		t.Fatalf("status omitted admissible repair: %s", output)
	}
	if len(probe.deadlines) == 0 {
		t.Fatal("status did not probe materialized files")
	}
	for _, deadline := range probe.deadlines {
		if deadline.IsZero() || deadline.After(started.Add(profile.ReadinessTimeout+time.Second)) || deadline != probe.deadlines[0] {
			t.Fatalf("status file probes did not share the release readiness deadline: %v", probe.deadlines)
		}
	}
	const privateDiagnostic = "synthetic private guest probe detail"
	probe.failure = errors.New(privateDiagnostic)
	for _, invocation := range []struct {
		code int
		yard string
		args []string
	}{
		{0, "named", []string{"-Y", "named", "status"}},
		{0, "default", []string{"update", "--check", "--offline"}},
		{1, "named", []string{"-Y", "named", "provision"}},
	} {
		output := run(invocation.code, invocation.args...)
		if strings.Contains(output, privateDiagnostic) || !strings.Contains(output, "yard "+invocation.yard+": cannot inspect materialized config") ||
			!strings.Contains(output, "yard -Y "+invocation.yard+" config status") {
			t.Fatalf("%v did not redact its probe failure: %s", invocation.args, output)
		}
	}
	program, err = New(options)
	if err != nil {
		t.Fatal(err)
	}
	if readiness := program.ownerReleaseReadiness(ctx); strings.Contains(readiness.Message, privateDiagnostic) ||
		!strings.Contains(readiness.Message, "yard default: cannot inspect materialized config") || readiness.Next != "yard -Y default config status" {
		t.Fatalf("owner readiness did not redact its probe failure: %#v", readiness)
	}
	options.Executor = fake

	checkout := strings.TrimSpace(run(0, "config", "sync", "path"))
	for _, scenario := range []struct {
		verb      string
		converged bool
	}{
		{"pull", true},
		{"push", false},
	} {
		before := configSourceGitOutput(t, checkout, "rev-parse", "HEAD")
		prompt := &callbackPrompt{callback: func() {
			writeConfigCommandFile(t, filepath.Join(publisher, "hosts/owner-a/yards/named/config.env"),
				fmt.Sprintf("# Move upstream during %s consent.\nSSH_PORT=2223\n", scenario.verb))
			commitConfigSource(t, publisher, "move upstream")
			runConfigSyncGit(t, publisher, "push", "-q")
			if scenario.converged {
				writeCLIFile(t, fixture.readyMarker, "", 0o600)
			}
		}}
		options.Prompt = prompt
		observe(false, 1)
		want, unconsumed := 1, 0
		message := "upstream changed after preview"
		for _, yard := range []string{"default", "named"} {
			loaded := loadConfigCommandContext(t, active, options.Environment, yard)
			if scenario.converged {
				appendHashSteps(t, fake, loaded)
			} else {
				appendMismatchedHashSteps(t, fake, loaded, "0")
			}
		}
		if scenario.converged {
			want, message = 0, "drift converged after confirmation"
			for _, yard := range []string{"default", "named"} {
				size := len(fake.ExecSteps)
				appendMismatchedHashSteps(t, fake, loadConfigCommandContext(t, active, options.Environment, yard), "0")
				unconsumed += len(fake.ExecSteps) - size
			}
		}
		output := run(want, "config", "sync", scenario.verb, "--apply")
		if !strings.Contains(output, message) || len(prompt.requests) != 1 || len(applier.yards) != 0 ||
			len(fake.ExecSteps) != unconsumed || configSourceGitOutput(t, checkout, "rev-parse", "HEAD") != before {
			t.Fatalf("consumer-only %s selected unexpected writes or observations: %s, %v", scenario.verb, output, applier.yards)
		}
		options.Prompt = nil
		if scenario.converged {
			if err := os.Remove(fixture.readyMarker); err != nil {
				t.Fatal(err)
			}
		}
		run(0, "config", "sync", "pull", "--yes")
	}

	prompt := &callbackPrompt{callback: func() {
		namedAsset := filepath.Join(publisher, "hosts/owner-a/yards/named/overrides/agents", asset)
		writeConfigCommandFile(t, namedAsset, "named update\n")
		commitConfigSource(t, publisher, "update named asset")
		runConfigSyncGit(t, publisher, "push", "-q")
	}}
	options.Prompt = prompt
	observe(false, 2)
	if output := run(1, "config", "sync", "pull", "--apply"); !strings.Contains(output, "upstream changed after preview") ||
		len(prompt.requests) != 1 || len(applier.yards) != 0 {
		t.Fatalf("consumer-only pull accepted moved upstream: %s, %v", output, applier.yards)
	}
	options.Prompt = nil

	observe(false, 2)
	beforeHash := fmt.Sprintf("%x", sha256.Sum256([]byte("updated\n")))
	afterHash := fmt.Sprintf("%x", sha256.Sum256([]byte("named update\n")))
	for range 2 {
		appendHashSteps(t, fake, loadConfigCommandContext(t, active, options.Environment, "default"))
		start := len(fake.ExecSteps)
		appendHashSteps(t, fake, loadConfigCommandContext(t, active, options.Environment, "named"))
		for i := start; i < len(fake.ExecSteps); i++ {
			result := &fake.ExecSteps[i].Result
			result.Stdout = bytes.ReplaceAll(result.Stdout, []byte(beforeHash), []byte(afterHash))
		}
	}
	if output := run(0, "config", "sync", "pull", "--apply", "--yes"); !strings.Contains(output, "applied generation") ||
		!slices.Equal(applier.yards, []string{"default", "named"}) || len(fake.ExecSteps) != 0 {
		t.Fatalf("retry did not converge exactly both consumers: %s, %v", output, applier.yards)
	}
	// An unreachable discovery-only yard cannot prevent a named publication.
	namedAsset := filepath.Join(publisher, "hosts/owner-a/yards/named/overrides/agents", asset)
	writeConfigCommandFile(t, namedAsset, "named only\n")
	commitConfigSource(t, publisher, "change the named override")
	runConfigSyncGit(t, publisher, "push", "-q")
	fake.ExecSteps = nil
	applier.yards = nil
	defaultTarget := loadConfigCommandContext(t, active, options.Environment, "default")
	namedTarget := loadConfigCommandContext(t, active, options.Environment, "named")
	for range 2 {
		start := len(fake.ExecSteps)
		appendHashSteps(t, fake, defaultTarget)
		for i := start; i < len(fake.ExecSteps); i++ {
			fake.ExecSteps[i] = testkit.IncusExecStep{Err: fmt.Errorf("guest agent is unreachable")}
		}
		appendMismatchedHashSteps(t, fake, namedTarget, "0")
	}
	for range 2 {
		start := len(fake.ExecSteps)
		appendHashSteps(t, fake, namedTarget)
		for i := start; i < len(fake.ExecSteps); i++ {
			result := &fake.ExecSteps[i].Result
			result.Stdout = bytes.ReplaceAll(result.Stdout, []byte(afterHash), fmt.Appendf(nil, "%x", sha256.Sum256([]byte("named only\n"))))
		}
	}
	output = run(1, "-Y", "named", "config", "sync", "pull", "--apply", "--yes")
	namedPublished := filepath.Join(home, config.GitSettingsRelativePath, "yards/named/overrides/agents", asset)
	if string(mustReadConfigFixture(t, namedPublished)) != "named only\n" || !slices.Equal(applier.yards, []string{"named"}) ||
		len(fake.ExecSteps) != 0 || strings.Contains(output, "config.apply.default") ||
		!strings.Contains(output, "configuration refresh is incomplete: yard default materialized config") {
		t.Fatalf("named publication selected the wrong consumers: %s, %v", output, applier.yards)
	}

	fake.ExecSteps = nil
	applier.yards = nil
	appendFailedDiscovery := func() {
		start := len(fake.ExecSteps)
		for _, target := range []config.Loaded{defaultTarget, namedTarget} {
			appendHashSteps(t, fake, target)
		}
		for i := start; i < len(fake.ExecSteps); i++ {
			fake.ExecSteps[i] = testkit.IncusExecStep{Err: fmt.Errorf("guest agent is unreachable")}
		}
	}
	appendFailedDiscovery()
	appendFailedDiscovery()
	output = run(1, "config", "sync", "pull", "--apply", "--yes")
	if strings.Contains(output, "already converged") || !strings.Contains(output, "configuration refresh is incomplete") ||
		!strings.Contains(output, "yard default materialized config") || !strings.Contains(output, "yard named materialized config") || len(applier.yards) != 0 {
		t.Fatalf("no-change pull hid unreadable consumers: %s, %v", output, applier.yards)
	}
	appendFailedDiscovery()
	for _, yard := range []string{"default", "named"} {
		appendHashSteps(t, fake, loadConfigCommandContext(t, active, options.Environment, yard))
	}
	output = run(0, "config", "sync", "pull", "--apply", "--yes")
	if !strings.Contains(output, "already converged") || strings.Contains(output, "configuration refresh is incomplete") ||
		len(fake.ExecSteps) != 0 || len(applier.yards) != 0 {
		t.Fatalf("no-change pull retained recovered discovery failures: %s, %v", output, applier.yards)
	}

	for _, scenario := range []struct{ verb, result string }{
		{"pull", "already converged"}, {"pull", "applied generation"}, {"push", "applied generation"}, {"", "applied generation"},
	} {
		if scenario.result == "applied generation" {
			repo := checkout
			if scenario.verb == "pull" {
				repo = publisher
			}
			writeConfigCommandFile(t, filepath.Join(repo, "hosts/owner-a/yards/named/config.env"),
				fmt.Sprintf("# Document the named yard port on %s.\nSSH_PORT=2223\n", scenario.verb))
			commitConfigSource(t, repo, "document named yard port")
			if scenario.verb == "pull" {
				runConfigSyncGit(t, publisher, "push", "-q")
			}
		}
		applier.yards = nil
		observe(false, 2)
		for range 2 {
			for _, yard := range []string{"default", "named"} {
				appendHashSteps(t, fake, loadConfigCommandContext(t, active, options.Environment, yard))
			}
		}
		args := []string{"config", "sync"}
		if scenario.verb != "" {
			args = append(args, scenario.verb)
		}
		if output := run(0, append(args, "--apply", "--yes")...); !strings.Contains(output, scenario.result) ||
			!slices.Equal(applier.yards, []string{"default", "named"}) || len(fake.ExecSteps) != 0 {
			t.Fatalf("%s retry did not converge exactly both consumers: %s, %v", scenario.verb, output, applier.yards)
		}
	}

	after, err := fixture.cli.readReleaseHistory()
	if err != nil || after == nil || *after != *history {
		t.Fatalf("consumer refresh changed release history: %v", err)
	}
}

type readinessProbeExecutor struct {
	ports.InstanceExecutor
	deadlines []time.Time
	failure   error
}

func (executor *readinessProbeExecutor) Exec(ctx context.Context, project, instance string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	command := request.Command
	if len(command) > 2 && command[0] == "sha256sum" || len(command) == 11 && command[0] == "python3" && command[4] == "observe" {
		deadline, _ := ctx.Deadline()
		executor.deadlines = append(executor.deadlines, deadline)
		if executor.failure != nil {
			return ports.InstanceExecResult{}, executor.failure
		}
	}
	return executor.InstanceExecutor.Exec(ctx, project, instance, request)
}

type sampleRepairPlatformFixture struct {
	*initPlatformFixture
	observation *ports.RuntimeObservation
	installed   *ports.RuntimeObservation
	installedID string
	// provisioned runs when the ordinary provision stage is applied.
	provisioned func()
}

func (fixture *sampleRepairPlatformFixture) ApplyStage(ctx context.Context, stage ports.ReconcileStageID) error {
	err := fixture.initPlatformFixture.ApplyStage(ctx, stage)
	if err == nil && stage == ports.ReconcileStageProvision && fixture.provisioned != nil {
		fixture.provisioned()
	}
	return err
}

func (fixture *sampleRepairPlatformFixture) ObserveInstalledRuntime(ctx context.Context, id string) (ports.RuntimeObservation, error) {
	if id == firstNonempty(fixture.installedID, "core-guest-runtime") && fixture.installed != nil {
		return *fixture.installed, nil
	}
	return fixture.initPlatformFixture.ObserveInstalledRuntime(ctx, id)
}

func (fixture *sampleRepairPlatformFixture) ObserveProfileRuntimes(context.Context) (map[string]ports.RuntimeObservation, error) {
	if fixture.observation != nil {
		return map[string]ports.RuntimeObservation{"sample-runtime": *fixture.observation}, nil
	}
	observation := ports.RuntimeObservation{State: "current", Actual: strings.Repeat("b", 64), Desired: strings.Repeat("b", 64)}
	if !fixture.converged[ports.ReconcileStageProfileRuntimes] {
		observation.State, observation.Actual = "stale", strings.Repeat("a", 64)
	}
	return map[string]ports.RuntimeObservation{"sample-runtime": observation}, nil
}

func TestConfigApplyRepairAdmitsOnlySelectedPendingInputs(t *testing.T) {
	for _, scenario := range []string{"pending", "stale", "pending neighbor"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newConfigApplyRepairFixture(t, scenario == "pending neighbor")
			diagnostics := ports.RuntimeDiagnostics{{Parameter: "sample.input.invalid", Installed: "changed", Desired: "current"}}
			state := ports.RuntimeObservation{State: ports.RuntimeStatePending, Actual: strings.Repeat("a", 64), Desired: strings.Repeat("b", 64), Diagnostics: &diagnostics}
			if scenario == "stale" {
				state.State = ports.RuntimeStateStale
			}
			platform := &sampleRepairPlatformFixture{initPlatformFixture: newInitPlatformFixture(), observation: &state}
			for stage := range platform.converged {
				platform.converged[stage] = true
			}
			fixture.cli.options.InitPlatform = platform
			permit, err := fixture.cli.prepareConfigApplyRepair(context.Background(), "default", false, fixture.outcome)
			if err != nil || (permit != nil) != (scenario == "pending") {
				t.Fatalf("pending repair permit=%#v err=%v", permit, err)
			}
		})
	}
}

func TestConfigApplyRepairUsesCompletedProtectedGateAndStableLock(t *testing.T) {
	fixture := newConfigApplyRepairFixture(t, false)
	observed, err := fixture.cli.inspectMutationGate(context.Background(), "default")
	if err != nil || observed == nil {
		t.Fatalf("inspect completed drift gate: outcome=%#v err=%v", observed, err)
	}
	if !sameConfigApplyGate(fixture.outcome, *observed) {
		t.Fatalf("fixture outcome differs from protected inspection: want=%#v got=%#v",
			fixture.outcome, *observed)
	}
	options, available, err := fixture.cli.mutationGateReleaseOptions()
	if err != nil || !available {
		t.Fatalf("release options: available=%v err=%v", available, err)
	}

	store, err := releasetransition.NewPOSIXV2Store(options.ConfigHome)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil || !snapshot.Exists {
		t.Fatalf("read fixture journal: exists=%v err=%v", snapshot.Exists, err)
	}
	journal, err := releasetransition.ParseJournal(snapshot.Payload)
	if err != nil || !configApplyRepairJournalMatches(journal, fixture.outcome) {
		t.Fatalf("fixture journal mismatch: journal=%#v err=%v", journal, err)
	}
	registryPayload, err := readConfigApplyRegistry(fixture.cli.options.RepositoryRoot)
	if err != nil {
		t.Fatal(err)
	}
	registry, registryDigest, err := releasetransition.ParseRegistryV2(
		registryPayload, releasetransition.BuiltinCapabilityCatalog(),
	)
	if err != nil || registryDigest != journal.RegistryDigest {
		t.Fatalf("fixture registry mismatch: got=%q want=%q err=%v", registryDigest, journal.RegistryDigest, err)
	}
	ledgerSnapshot, err := store.ReadLedger()
	if err != nil || !ledgerSnapshot.Exists {
		t.Fatalf("read fixture ledger: exists=%v err=%v", ledgerSnapshot.Exists, err)
	}
	ledger, _, err := releasetransition.ParseLedgerV2(ledgerSnapshot.Payload, registry)
	if err != nil {
		t.Fatalf("parse fixture ledger: %v", err)
	}
	pending, err := registry.PendingPath(ledger)
	if err != nil || len(pending) != 0 {
		t.Fatalf("fixture ledger incomplete: pending=%v err=%v", pending, err)
	}
	permit, err := fixture.cli.prepareConfigApplyRepair(
		context.Background(), "default", false, fixture.outcome,
	)
	if err != nil || permit == nil {
		t.Fatalf("prepare completed config repair: permit=%#v err=%v", permit, err)
	}
	before, err := os.ReadFile(fixture.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := fixture.cli.lockConfigApplyRepair(context.Background(), permit)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := os.WriteFile(fixture.readyMarker, []byte("ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.cli.finishConfigApplyRepair(context.Background(), permit); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(fixture.journalPath)
	if err != nil || string(after) != string(before) {
		t.Fatalf("config repair changed completed journal: err=%v", err)
	}
}

func TestConfigApplyRepairBindsCheckpointAuthorityAfterConversion(t *testing.T) {
	fixture := newConfigApplyRepairFixture(t, false)
	payload, err := readConfigApplyRegistry(fixture.cli.options.RepositoryRoot)
	if err != nil {
		t.Fatal(err)
	}
	registry, digest, err := releasetransition.ParseRegistryV2(payload, releasetransition.BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	store, err := releasetransition.NewPOSIXV2Store(fixture.cli.env["SUBYARD_CONFIG_HOME"])
	if err != nil {
		t.Fatal(err)
	}
	initial, err := store.ReadMigrationLedger(registry, digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConvertMigrationLedger(initial, registry, digest, registry, digest); err != nil {
		t.Fatal(err)
	}
	permit, err := fixture.cli.prepareConfigApplyRepair(context.Background(), "default", false, fixture.outcome)
	if err != nil || permit == nil || !permit.checkpointed {
		t.Fatalf("checkpoint repair permit=%#v, err=%v", permit, err)
	}
	unlock, err := fixture.cli.lockConfigApplyRepair(context.Background(), permit)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	checkpoint, err := store.ReadMigrationCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(permit.configHome, "release-transition", "v2", "history-checkpoint.json"),
		string(checkpoint.Payload)+"\n", 0o600)
	if unlock, err := fixture.cli.lockConfigApplyRepair(context.Background(), permit); err == nil {
		unlock()
		t.Fatal("changed checkpoint authority admitted a previously assessed repair")
	}
}

func TestConfigApplyRepairRejectsUnfinishedAndCorruptProtectedJournals(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	runtimeRoot := filepath.Join(root, "runtime-v2-config-apply-pending")
	environment = append(environment,
		"YARD_RUNTIME_ROOT="+runtimeRoot,
		"V2_GATE_CAPTURE="+filepath.Join(root, "capture"),
	)
	journalPath, _ := installUnfinishedV2MutationGateFixture(t, root, environment, runtimeRoot)
	program, err := New(Options{
		RepositoryRoot: root, Environment: environment, WorkingDir: root,
		Incus: lifecycleIncus(), Executor: lifecycleIncus(),
	})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := program.inspectMutationGate(context.Background(), "default")
	if err != nil || outcome == nil {
		t.Fatalf("inspect pending gate: outcome=%#v err=%v", outcome, err)
	}
	permit, err := program.prepareConfigApplyRepair(context.Background(), "default", false, *outcome)
	if err != nil || permit != nil {
		t.Fatalf("pending gate permit=%#v err=%v", permit, err)
	}
	if err := os.WriteFile(journalPath, []byte("{broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	eligible := releasetransition.Outcome{
		Status: releasetransition.StatusMigrationRequired,
		Code:   releasetransition.CodeTransitionRequired,
		Active: "1.2.3-aaaaaaaaaaaa", Target: "1.2.3-aaaaaaaaaaaa",
		Retry: "run yard update",
	}
	permit, err = program.prepareConfigApplyRepair(context.Background(), "default", false, eligible)
	if err != nil || permit != nil {
		t.Fatalf("corrupt gate permit=%#v err=%v", permit, err)
	}
}

func TestConfigApplyRepairRejectsUnselectedAllLocalDrift(t *testing.T) {
	fixture := newConfigApplyRepairFixture(t, true)
	permit, err := fixture.cli.prepareConfigApplyRepair(
		context.Background(), "default", false, fixture.outcome,
	)
	if err != nil || permit != nil {
		t.Fatalf("partial all-local permit=%#v err=%v", permit, err)
	}
}

func TestConfigApplyRepairLockRejectsJournalChangedAfterConfirmation(t *testing.T) {
	fixture := newConfigApplyRepairFixture(t, false)
	permit, err := fixture.cli.prepareConfigApplyRepair(
		context.Background(), "default", false, fixture.outcome,
	)
	if err != nil || permit == nil {
		t.Fatalf("prepare config repair: permit=%#v err=%v", permit, err)
	}
	payload, err := os.ReadFile(fixture.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.journalPath, append(payload, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if unlock, err := fixture.cli.lockConfigApplyRepair(context.Background(), permit); err == nil {
		unlock()
		t.Fatal("lock accepted a journal changed after confirmation")
	}
}

func TestConfigApplyRepairRejectsMissingCorruptAndPendingLedgers(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{name: "missing", mutate: func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "corrupt", mutate: func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("{broken\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "pending", mutate: func(t *testing.T, path string) {
			registryPayload, err := os.ReadFile(filepath.Join(repositoryRoot(t), "config", "release-transition.json"))
			if err != nil {
				t.Fatal(err)
			}
			registry, _, err := releasetransition.ParseRegistryV2(
				registryPayload, releasetransition.BuiltinCapabilityCatalog(),
			)
			if err != nil {
				t.Fatal(err)
			}
			payload, _, err := releasetransition.MarshalLedgerV2(
				releasetransition.BaselineLedgerV2(registry), registry,
			)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, payload, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newConfigApplyRepairFixture(t, false)
			ledgerPath := filepath.Join(filepath.Dir(fixture.journalPath), "ledger.json")
			test.mutate(t, ledgerPath)
			permit, err := fixture.cli.prepareConfigApplyRepair(
				context.Background(), "default", false, fixture.outcome,
			)
			if err != nil || permit != nil {
				t.Fatalf("unsafe ledger permit=%#v err=%v", permit, err)
			}
		})
	}
}

func TestConfigApplyRepairExactSelectionMustCoverEveryDriftedYard(t *testing.T) {
	fixture := newConfigApplyRepairFixture(t, true)
	permit, err := fixture.cli.prepareActivationRepair(
		context.Background(), "default", true, fixture.outcome, activationRepairMode{requireDrift: true, selectedNames: []string{"default"}},
	)
	if err != nil || permit != nil {
		t.Fatalf("partial exact-selection permit=%#v err=%v", permit, err)
	}
}

func TestConfigApplyRepairIgnoresLegacyFiles(t *testing.T) {
	fixture := newConfigApplyRepairFixture(t, false)
	options, _, err := fixture.cli.mutationGateReleaseOptions()
	if err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(options.ConfigHome, "migrations", "transactions", "legacy", "transaction.json")
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, legacyPath, "{broken\n", 0o600)
	gate, err := fixture.cli.inspectMutationGate(context.Background(), "default")
	if err != nil || gate == nil || !sameConfigApplyGate(*gate, fixture.outcome) {
		t.Fatalf("V1 files should not change the V2 gate: gate=%#v err=%v", gate, err)
	}
	permit, err := fixture.cli.prepareConfigApplyRepair(context.Background(), "default", false, *gate)
	if err != nil || permit == nil {
		t.Fatalf("legacy corruption blocked repair: permit=%#v err=%v", permit, err)
	}
}

type configApplyRepairFixture struct {
	cli         *CLI
	outcome     releasetransition.Outcome
	journalPath string
	readyMarker string
}

func newConfigApplyRepairFixture(t *testing.T, named bool) configApplyRepairFixture {
	root, environment, _ := nativeFixture(t)
	writeRuntimeProfileFixture(t, root)
	for _, name := range []string{"config/profiles/test-vms/profile.json", "config/profiles/test-vms/service-binding.py", "config/profiles/test-vms/full-cleanup.py", "scripts/lib/teardown-plan.py"} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o755)
		if filepath.Ext(path) == ".json" {
			mode = 0o644
		}
		testkit.WriteFile(t, path, mustReadConfigFixture(t, filepath.Join(repositoryRoot(t), name)), mode)
	}
	sampleHandler := filepath.Join(root, "config", "profiles", "sample", "runtime.sh")
	if err := os.MkdirAll(filepath.Dir(sampleHandler), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, sampleHandler, "#!/bin/sh\nprintf '%s\\n' '{\"state\":\"absent\",\"actual\":\"\",\"desired\":\"\"}'\n", 0o700)
	writeCLIFile(t, filepath.Join(root, "config", "subyard.env"), strings.Join([]string{
		"SHIFT_MODE=shift", "FORWARD_SSH_AGENT=0", "DEV_SUDO=0", "DEV_UID=1000",
		"DEV_USER=dev", "SSH_PORT=2222",
		"STORAGE_PATH=" + filepath.Join(root, "data", "storage"),
		"HOST_BASE=" + filepath.Join(root, "host"),
		"RESTRICTED_DISK_PATHS=" + filepath.Join(root, "host"),
	}, "\n")+"\n", 0o600)
	migrations, err := os.ReadFile(filepath.Join(repositoryRoot(t), "config", "migrations.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(root, "config", "migrations.json"), string(migrations), 0o600)
	source := filepath.Join(root, "repo.rules")
	writeCLIFile(t, source, "allow_rule()\n", 0o600)
	writeCLIFile(t, filepath.Join(root, "config", "agents.env"), strings.Join([]string{
		"CODING_TOOL_INTEGRATIONS=codex",
		"CODING_TOOL_GIT_APPROVAL=off",
		"AGENT_codex_RULES=" + source,
		"AGENT_codex_RULES_DEST=.codex/rules/repo.rules",
	}, "\n")+"\n", 0o600)
	if named {
		configHome := environmentValue(environment, "SUBYARD_CONFIG_HOME")
		namedPath := filepath.Join(configHome, "yards", "named", "config.env")
		if err := os.MkdirAll(filepath.Dir(namedPath), 0o700); err != nil {
			t.Fatal(err)
		}
		writeCLIFile(t, namedPath,
			"SSH_PORT=2223\n", 0o600)
		if _, err := os.Stat(namedPath); err != nil {
			t.Fatalf("named registration before gate fixture: %v", err)
		}
	}
	runtimeRoot := filepath.Join(root, "runtime-v2-config-apply")
	environment = append(environment,
		"YARD_RUNTIME_ROOT="+runtimeRoot,
		"V2_GATE_CAPTURE="+filepath.Join(root, "capture"),
		"SUBYARD_POWER_RECONCILER_PATH="+filepath.Join(root, "absent-power-reconciler"),
		"SUBYARD_POWER_UNIT_PATH="+filepath.Join(root, "absent-power-unit"),
	)
	journalPath, _ := installUnfinishedV2MutationGateFixture(t, root, environment, runtimeRoot)
	registryPayload, err := os.ReadFile(filepath.Join(repositoryRoot(t), "config", "release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(root, "config", "release-transition.json"), string(registryPayload), 0o600)
	writeCLIFile(t, filepath.Join(runtimeRoot, "releases", "1.2.3-aaaaaaaaaaaa",
		"config", "release-transition.json"), string(registryPayload), 0o600)
	if named {
		if _, err := os.Stat(filepath.Join(environmentValue(environment, "SUBYARD_CONFIG_HOME"),
			"yards", "named", "config.env")); err != nil {
			t.Fatalf("named registration after unfinished gate fixture: %v", err)
		}
	}
	readyMarker, outcome := completeConfigApplyGateFixture(
		t, root, environment, runtimeRoot, journalPath,
	)
	fake := &testkit.Incus{Instances: map[string]ports.InstanceInfo{}}
	program, err := New(Options{
		RepositoryRoot: root, Environment: environment, WorkingDir: root,
		Incus: fake, Executor: fake,
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.resolveReleaseTransitionContext(context.Background(),
		"default", environmentValue(environment, "SUBYARD_CONFIG_HOME"),
	)
	if err != nil {
		t.Fatal(err)
	}
	targets, err := program.localConfigTargets(loaded, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range targets {
		fake.Instances[target.Loaded.Context.IncusProject+"/"+target.Loaded.Context.YardInstanceName] =
			ports.InstanceInfo{Name: target.Loaded.Context.YardInstanceName,
				Project: target.Loaded.Context.IncusProject, Status: "Running"}
		appendMismatchedHashSteps(t, fake, target.Loaded, "0")
	}
	if !named {
		for _, target := range targets {
			appendMismatchedHashSteps(t, fake, target.Loaded, "0")
		}
	}
	return configApplyRepairFixture{
		cli: program, outcome: outcome, journalPath: journalPath, readyMarker: readyMarker,
	}
}

func completeConfigApplyGateFixture(
	t *testing.T,
	root string,
	environment []string,
	runtimeRoot string,
	journalPath string,
) (string, releasetransition.Outcome) {
	t.Helper()
	identity, err := uninstall.InstallationIdentity(uint32(os.Getuid()), environmentValue(environment, "SUBYARD_HOME"), environmentValue(environment, "SUBYARD_CONFIG_HOME"), runtimeRoot, filepath.Join(environmentValue(environment, "SUBYARD_HOME"), "releases"), true)
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := uninstall.MarshalInstallation(identity)
	if err != nil {
		t.Fatal(err)
	}
	writeProfileTestFile(t, filepath.Join(runtimeRoot, uninstall.ManifestName), inventory, 0600)
	const target = "1.2.3-aaaaaaaaaaaa"
	readyMarker := filepath.Join(root, "config-apply-ready")
	engine := filepath.Join(runtimeRoot, "releases", target, "bin", "yard-engine")
	script := fmt.Sprintf(`#!/bin/sh
case "${1:-}" in
--version) printf 'yard-engine 1.2.3\n' ;;
_release-transition)
  request="$(cat)"
  case "$request" in
    *'"mode":"converge"'*)
      if [ -e %q ]; then
        printf '%%s\n' '{"schemaVersion":1,"activationReconciliationOwned":true,"outcome":{"status":"ready","reachedGoal":true,"active":"%s","previous":"release-a","target":"%s","code":"ready","message":"verified","transaction":"tx-0123456789abcdef"}}'
      else
        printf '%%s\n' '{"schemaVersion":1,"activationReconciliationOwned":true,"outcome":{"status":"operator-action-required","reachedGoal":false,"active":"%s","previous":"release-a","target":"%s","code":"dependency-unavailable","message":"activation drift persists","retry":"repair fixture activation"}}'
      fi
      exit 0 ;;
  esac
  if [ -e %q ]; then
    printf '%%s\n' '{"schemaVersion":1,"activationReconciliationOwned":true,"inspection":{"plan":"plan-v1-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","assessment":{"action":"release.transition.v2","effect":"mutation","changed":false,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible"},"outcome":{"status":"ready","reachedGoal":true,"active":"%s","previous":"release-a","target":"%s","code":"ready","message":"verified","transaction":"tx-0123456789abcdef"}}}'
  else
    printf '%%s\n' '{"schemaVersion":1,"activationReconciliationOwned":true,"inspection":{"plan":"plan-v1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","assessment":{"action":"release.transition.v2","effect":"mutation","changed":true,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible","consequences":["repair release activation"]},"outcome":{"status":"migration-required","reachedGoal":false,"active":"%s","previous":"release-a","target":"%s","code":"transition-required","message":"activation drift","retry":"run yard update"}}}'
  fi ;;
*) exit 64 ;;
esac
`, readyMarker, target, target, target, target, readyMarker, target, target, target, target)
	writeCLIFile(t, engine, script, 0o700)
	enginePayload, err := os.ReadFile(engine)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := os.ReadFile(filepath.Join(runtimeRoot, "releases", target,
		"config", "release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	engineDigest := sha256.Sum256(enginePayload)
	registryDigest := sha256.Sum256(registry)
	manifest := fmt.Sprintf("%x  ./bin/yard-engine\n%x  ./config/release-transition.json\n",
		engineDigest, registryDigest)
	writeCLIFile(t, filepath.Join(runtimeRoot, "releases", target, "runtime-files.sha256"),
		manifest, 0o600)
	manifestDigest := sha256.Sum256([]byte(manifest))
	journalPayload, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := releasetransition.ParseJournal(journalPayload)
	if err != nil {
		t.Fatal(err)
	}
	journal.Checkpoint = releasetransition.JournalComplete
	journal.ArtifactDigest = releasetransition.Fingerprint(fmt.Sprintf("%x", manifestDigest))
	journal.RegistryDigest = releasetransition.Fingerprint(fmt.Sprintf("%x", registryDigest))
	journalPayload, err = releasetransition.MarshalJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, journalPath, string(journalPayload), 0o600)
	parsedRegistry, _, err := releasetransition.ParseRegistryV2(
		registry, releasetransition.BuiltinCapabilityCatalog(),
	)
	if err != nil {
		t.Fatal(err)
	}
	ledger := releasetransition.BaselineLedgerV2(parsedRegistry)
	for _, migration := range parsedRegistry.Migrations {
		ledger, err = ledger.Advance(parsedRegistry, migration)
		if err != nil {
			t.Fatal(err)
		}
	}
	ledgerPayload, _, err := releasetransition.MarshalLedgerV2(ledger, parsedRegistry)
	if err != nil {
		t.Fatal(err)
	}
	store, err := releasetransition.NewPOSIXV2Store(
		environmentValue(environment, "SUBYARD_CONFIG_HOME"),
	)
	if err != nil {
		t.Fatal(err)
	}
	missingLedger, err := store.ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapLedger(missingLedger, ledgerPayload); err != nil {
		t.Fatal(err)
	}
	links, err := releasetransition.NewRuntimeLinkStore(runtimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := links.Activate(journal.Releases); err != nil {
		t.Fatal(err)
	}
	return readyMarker, releasetransition.Outcome{
		Status: releasetransition.StatusMigrationRequired,
		Code:   releasetransition.CodeTransitionRequired,
		Active: target, Previous: releaseIDForConfigApply("release-a"), Target: target,
		Message: "activation drift", Retry: "run yard update",
	}
}

func releaseIDForConfigApply(value releasetransition.ReleaseID) *releasetransition.ReleaseID {
	return &value
}

func TestConfigApplyRepairGateRequiresFreshCompletedActivationShape(t *testing.T) {
	target := releasetransition.ReleaseID("1.2.3-aaaaaaaaaaaa")
	valid := releasetransition.Outcome{
		Status: releasetransition.StatusMigrationRequired,
		Code:   releasetransition.CodeTransitionRequired,
		Active: target, Target: target, Retry: "run yard update",
	}
	if !configApplyRepairGateShape(valid) {
		t.Fatal("fresh same-release activation repair was rejected")
	}
	transaction := releasetransition.TransactionID("tx-0123456789abcdef")
	for name, mutate := range map[string]func(*releasetransition.Outcome){
		"ready": func(value *releasetransition.Outcome) {
			value.Status, value.Code, value.ReachedGoal =
				releasetransition.StatusReady, releasetransition.CodeReady, true
		},
		"unfinished":     func(value *releasetransition.Outcome) { value.Transaction = &transaction },
		"foreign target": func(value *releasetransition.Outcome) { value.Target = "1.2.4-bbbbbbbbbbbb" },
		"blocked": func(value *releasetransition.Outcome) {
			value.Status, value.Code = releasetransition.StatusOperatorActionRequired,
				releasetransition.CodeJournalInvalid
		},
	} {
		t.Run(name, func(t *testing.T) {
			outcome := valid
			mutate(&outcome)
			if configApplyRepairGateShape(outcome) {
				t.Fatalf("unsafe gate was admitted: %#v", outcome)
			}
		})
	}
}

func TestConfigApplyRepairRequiresCompletedMatchingJournal(t *testing.T) {
	target := releasetransition.ReleaseID("1.2.3-aaaaaaaaaaaa")
	outcome := releasetransition.Outcome{Active: target, Target: target}
	journal := releasetransition.JournalRecord{
		Checkpoint: releasetransition.JournalComplete,
		Goal: releasetransition.Goal{
			Target: target, Direction: releasetransition.DirectionActivateTarget,
		},
	}
	if !configApplyRepairJournalMatches(journal, outcome) {
		t.Fatal("completed matching journal was rejected")
	}
	journal.Checkpoint = releasetransition.JournalReconciling
	if configApplyRepairJournalMatches(journal, outcome) {
		t.Fatal("unfinished journal was admitted")
	}
	journal.Checkpoint = releasetransition.JournalComplete
	journal.Goal.Target = "1.2.4-bbbbbbbbbbbb"
	if configApplyRepairJournalMatches(journal, outcome) {
		t.Fatal("foreign completed journal was admitted")
	}
}

func TestConfigApplyRepairReleaseIdentityComparison(t *testing.T) {
	release := releasetransition.ReleaseID("1.2.3-aaaaaaaaaaaa")
	same := release
	other := releasetransition.ReleaseID("1.2.4-bbbbbbbbbbbb")
	if !sameConfigApplyReleaseID(nil, nil) || !sameConfigApplyReleaseID(&release, &same) ||
		sameConfigApplyReleaseID(nil, &release) || sameConfigApplyReleaseID(&release, &other) {
		t.Fatal("release identity equality accepted an ambiguous previous link")
	}
}

func TestConfigApplyRepairFingerprintBindsDesiredScopeOnly(t *testing.T) {
	facts := configApplyRepairFacts{
		Journal: releasetransition.Fingerprint(strings.Repeat("a", 64)), Target: "1.2.3-aaaaaaaaaaaa",
		Direction: releasetransition.DirectionActivateTarget,
		OtherActivations: []configApplyActivationFact{{
			ID: "host-power", Actual: releasetransition.Fingerprint(strings.Repeat("b", 64)),
			Desired: releasetransition.Fingerprint(strings.Repeat("b", 64)), Converged: true,
		}},
		ConfigTargets: []configApplyTargetFact{{
			Name: "default", DesiredFingerprint: strings.Repeat("c", 64),
		}},
	}
	first, err := configApplyFactsFingerprint(facts)
	if err != nil {
		t.Fatal(err)
	}
	second, err := configApplyFactsFingerprint(facts)
	if err != nil || first != second || len(first) != 64 {
		t.Fatalf("stable facts fingerprints = %q and %q, err=%v", first, second, err)
	}
	facts.ConfigTargets[0].DesiredFingerprint = strings.Repeat("d", 64)
	changed, err := configApplyFactsFingerprint(facts)
	if err != nil || changed == first {
		t.Fatalf("desired scope change was not bound: first=%q changed=%q err=%v", first, changed, err)
	}
}

func TestConfigApplyRepairAllowsOnlyPreviouslyAdmittedDriftToRemain(t *testing.T) {
	if !configApplyDriftSubset(nil, []string{"default"}) ||
		!configApplyDriftSubset([]string{"default"}, []string{"default", "named"}) {
		t.Fatal("converged or remaining admitted drift was rejected")
	}
	if configApplyDriftSubset([]string{"named"}, []string{"default"}) ||
		configApplyDriftSubset([]string{"default", "named"}, []string{"default"}) {
		t.Fatal("new all-local drift was admitted")
	}
}

func TestNormalizedCompletedConfigApplyPairDoesNotSwitchLinks(t *testing.T) {
	from := releasetransition.ReleaseID("1.2.2-bbbbbbbbbbbb")
	target := releasetransition.ReleaseID("1.2.3-aaaaaaaaaaaa")
	got := normalizedCompletedConfigApplyPair(releasetransition.ReleasePair{
		From: from, Target: target,
	})
	if got.From != target || got.Target != target || got.Previous == nil || *got.Previous != from {
		t.Fatalf("normalized completed pair = %#v", got)
	}
}

func TestConfigApplyActivationObservationRequiresTypedFixedPoint(t *testing.T) {
	digest := releasetransition.Fingerprint(strings.Repeat("a", 64))
	if !validConfigApplyActivationObservation(releasetransition.V2ActivationObservation{
		Actual: digest, Desired: digest, Converged: true,
	}) {
		t.Fatal("valid activation observation was rejected")
	}
	if validConfigApplyActivationObservation(releasetransition.V2ActivationObservation{
		Actual: "short", Desired: digest, Converged: true,
	}) {
		t.Fatal("malformed activation fingerprint was admitted")
	}
}

// prepareConfigApplyRepair exercises the bounded activation repair core that
// config sync consumers and completed scalar authoring still install.
func (cli *CLI) prepareConfigApplyRepair(ctx context.Context, yard string, allLocal bool, outcome releasetransition.Outcome) (*configApplyRepairPermit, error) {
	return cli.prepareActivationRepair(ctx, yard, allLocal, outcome, activationRepairMode{requireDrift: true})
}
