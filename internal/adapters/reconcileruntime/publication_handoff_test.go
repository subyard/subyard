package reconcileruntime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/ownerpublication"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/sys/unix"
)

func TestPublicationConsumerUsesPreparedBytesAndHistoricalInventory(t *testing.T) {
	root := testkit.TempDir(t)
	for _, p := range []string{"config/systemd", "scripts", "bin"} {
		if e := os.MkdirAll(filepath.Join(root, p), 0700); e != nil {
			t.Fatal(e)
		}
	}
	account, e := user.Current()
	if e != nil {
		t.Fatal(e)
	}
	engine := filepath.Join(root, "source")
	testkit.WriteFile(t, engine, []byte("prepared engine"), 0700)
	runtime := Runtime{RepositoryRoot: root, Environment: []string{"SUBYARD_USER=" + account.Username, "SUBYARD_POWER_ENGINE_SOURCE=" + engine, "SUBYARD_POWER_RECONCILER_PATH=" + filepath.Join(root, "engine"), "SUBYARD_POWER_UNIT_PATH=" + filepath.Join(root, "power.service")}}
	p, e := runtime.PrepareHostPowerRuntime(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	if len(runtime.hostPowerResourcePaths()) != 2 {
		t.Fatal("historical source changed fixed resources")
	}
	absent, e := runtime.PreparePublicationConsumer(p)
	if e != nil || absent != nil {
		t.Fatal(absent, e)
	}
	template, e := os.ReadFile("../../../config/systemd/subyard-publication-reconcile.service.in")
	if e != nil {
		t.Fatal(e)
	}
	testkit.WriteFile(t, filepath.Join(root, "config/systemd/subyard-publication-reconcile.service.in"), template, 0644)
	powerTemplate, e := os.ReadFile("../../../config/systemd/subyard-power-reconcile.service.in")
	if e != nil {
		t.Fatal(e)
	}
	testkit.WriteFile(t, filepath.Join(root, "config/systemd/subyard-power-reconcile.service.in"), powerTemplate, 0644)
	if _, e := runtime.PreparePublicationConsumer(p); e == nil {
		t.Fatal("changed prepared source accepted")
	}
	modern, e := runtime.PrepareHostPowerRuntime(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer modern.Close()
	plan, e := runtime.PreparePublicationConsumer(modern)
	if e != nil {
		t.Fatal(e)
	}
	if len(runtime.hostPowerResourcePaths()) != 3 || len(plan.Before) != 3 || plan.Before[2].Role != "publication" || plan.EngineDigest != ownerpublication.Digest([]byte("prepared engine")) || !ownerHostPowerDigest(plan.Generation) {
		t.Fatal(plan)
	}
	contracts, e := ownerpublication.Load(root)
	if e != nil {
		t.Fatal(e)
	}
	encoded, e := ownerpublication.Encode(contracts)
	if e != nil {
		t.Fatal(e)
	}
	expected := ownerpublication.RenderPublicationUnit(string(template), filepath.Join(root, "engine"), runtime.publicationResourcePaths()[1], encoded)
	if plan.UnitDigest != ownerpublication.Digest([]byte(expected)) || plan.ContractsDigest != ownerpublication.Digest([]byte(encoded)) {
		t.Fatal("planned consumer did not bind complete prepared unit")
	}
	testkit.WriteFile(t, filepath.Join(root, "config/systemd/subyard-publication-reconcile.service.in"), []byte("changed after consent"), 0644)
	if plan.UnitDigest != ownerpublication.Digest([]byte(expected)) {
		t.Fatal("live template changed approved identity")
	}
	testkit.WriteFile(t, filepath.Join(root, "engine"), []byte("prepared engine"), 0755)
	testkit.WriteFile(t, runtime.publicationUnitPath(), []byte(expected), 0644)
	if os.Geteuid() != 0 {
		if _, e := runtime.CapturePublicationConsumer(plan); e == nil {
			t.Fatal("unprivileged files became protected consumer")
		}
	}
}

type publicationInstallResultObserver struct {
	ownerPublicationFinalizerObserver
	result  *ports.NativeHostPowerInstallation
	records int
}

func (*publicationInstallResultObserver) CommandStarted(string, string) (uint64, error) {
	return 7, nil
}
func (o *publicationInstallResultObserver) RecordHostPowerInstallation(command uint64, v ports.NativeHostPowerInstallation) (ports.NativeHostPowerInstallation, error) {
	if command != 7 {
		return v, errors.New("wrong successful installer ordinal")
	}
	v.Producer = strings.Repeat("c", 64)
	v.Intent = strings.Repeat("a", 64)
	v.Recipe = strings.Repeat("b", 64)
	v.Source = strings.Repeat("d", 64)
	v.Command = command
	o.records++
	o.result = &v
	return v, nil
}
func (o *publicationInstallResultObserver) ReadHostPowerInstallation(producer string) (*ports.NativeHostPowerInstallation, error) {
	if o.result == nil || producer != "" && producer != o.result.Producer {
		return nil, errors.New("unselected original producer")
	}
	return o.result, nil
}

func TestPublicationConsumerProtectedNativeProcess(t *testing.T) {
	if root := os.Getenv("SUBYARD_TEST_PUBLICATION_WORKER_CHILD"); root != "" {
		var input struct {
			Environment []string
			Encoded     string
			Consumer    ownerpublication.Consumer
		}
		raw, e := os.ReadFile(filepath.Join(root, "worker-input.json"))
		if e != nil {
			t.Fatal(e)
		}
		if e := json.Unmarshal(raw, &input); e != nil {
			t.Fatal(e)
		}
		runtime := Runtime{RepositoryRoot: root, Environment: input.Environment}
		actual, e := runtime.PublicationWorkerConsumer(context.Background(), input.Encoded)
		if e != nil || !reflect.DeepEqual(actual, input.Consumer) {
			t.Fatal("installed executable self proof", e)
		}
		return
	}
	if os.Getenv("SUBYARD_TEST_PUBLICATION_ROOT_NS") != "1" {
		unshare, e := exec.LookPath("unshare")
		if e != nil {
			t.Skip("user namespace unavailable")
		}
		probe := exec.Command(unshare, "--user", "--map-root-user", "--mount", "true")
		if e := probe.Run(); e != nil {
			t.Skip("user namespace unavailable")
		}
		command := exec.Command(unshare, "--user", "--map-root-user", "--mount", os.Args[0], "-test.v", "-test.run=^TestPublicationConsumerProtectedNativeProcess$", "-test.count=1")
		command.Env = append(os.Environ(), "SUBYARD_TEST_PUBLICATION_ROOT_NS=1")
		if output, e := command.CombinedOutput(); e != nil {
			t.Fatal(e, string(output))
		} else {
			t.Logf("executed private namespace: %s", output)
		}
		return
	}
	root := testkit.TempDir(t)
	if os.Geteuid() != 0 {
		t.Fatal("fixture requires its private mapped root")
	}
	for _, path := range []string{"config/systemd", "scripts", "bin"} {
		if e := os.MkdirAll(filepath.Join(root, path), 0700); e != nil {
			t.Fatal(e)
		}
	}
	template, e := os.ReadFile("../../../config/systemd/subyard-publication-reconcile.service.in")
	if e != nil {
		t.Fatal(e)
	}
	testkit.WriteFile(t, filepath.Join(root, "config/systemd/subyard-publication-reconcile.service.in"), template, 0644)
	powerTemplate, e := os.ReadFile("../../../config/systemd/subyard-power-reconcile.service.in")
	if e != nil {
		t.Fatal(e)
	}
	testkit.WriteFile(t, filepath.Join(root, "config/systemd/subyard-power-reconcile.service.in"), powerTemplate, 0644)
	image, e := os.ReadFile("/proc/self/exe")
	if e != nil {
		t.Fatal(e)
	}
	source := filepath.Join(root, "bin/yard-engine")
	engine := filepath.Join(root, "engine")
	unit := filepath.Join(root, "power.service")
	passive := filepath.Join(root, ownerpublication.HandoffService)
	testkit.WriteFile(t, source, image, 0755)
	manager := filepath.Join(root, "systemctl")
	script := "#!/bin/sh\nprintf 'LoadState=loaded\\nNeedDaemonReload=no\\nFragmentPath=" + passive + "\\nDropInPaths=\\n'\n"
	testkit.WriteFile(t, manager, []byte(script), 0755)
	if e := unix.Mount(manager, "/usr/bin/systemctl", "", unix.MS_BIND, ""); e != nil {
		t.Fatal("private manager mount", e)
	}
	defer unix.Unmount("/usr/bin/systemctl", 0)
	runtime := Runtime{RepositoryRoot: root, Environment: []string{"PATH=" + root + ":/usr/bin:/bin", "SUBYARD_POWER_ENGINE_SOURCE=" + source, "SUBYARD_POWER_RECONCILER_PATH=" + engine, "SUBYARD_POWER_UNIT_PATH=" + unit}}
	prepared, e := runtime.PrepareHostPowerRuntime(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer prepared.Close()
	plan, e := runtime.PreparePublicationConsumer(prepared)
	if e != nil {
		t.Fatal(e)
	}
	contracts, e := ownerpublication.Load(root)
	if e != nil {
		t.Fatal(e)
	}
	encoded, e := ownerpublication.Encode(contracts)
	if e != nil {
		t.Fatal(e)
	}
	body := ownerpublication.RenderPublicationUnit(string(template), engine, unit, encoded)
	testkit.WriteFile(t, engine, image, 0755)
	testkit.WriteFile(t, unit, []byte(strings.ReplaceAll(string(powerTemplate), "@SUBYARD_POWER_RECONCILER@", engine)+"\n[Service]\nEnvironment=SUBYARD_PUBLICATION_CONTRACTS="+encoded+"\n"), 0644)
	testkit.WriteFile(t, passive, []byte(body), 0644)
	prepared.Close()
	prepared, e = runtime.PrepareHostPowerRuntime(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer prepared.Close()
	plan, e = runtime.PreparePublicationConsumer(prepared)
	if e != nil {
		t.Fatal(e)
	}
	consumer, e := runtime.CapturePublicationConsumer(plan)
	if e != nil {
		t.Fatal(e)
	}
	if e := runtime.CheckPublicationConsumer(context.Background(), consumer); e != nil {
		t.Fatal("exact protected consumer refused", e)
	}
	ctx := ports.WithNativeInvocationKey(ports.WithNativeOperationObserver(context.Background(), &ownerPublicationFinalizerObserver{}), strings.Repeat("a", 64))
	record, e := runtime.PrepareOwnerPublication(ctx)
	if e != nil {
		t.Fatal(e)
	}
	original, e := runtime.RestoreOwnerPublication(record)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := original.OwnerPublicationConsumer(ctx); e != nil {
		t.Fatal("original preconsent convergence", e)
	}
	capability := original.OwnerPublicationInstallation()
	child := runtime
	child.Environment = append(append([]string(nil), runtime.Environment...), "SUBYARD_YARD_ROLE=legacy")
	childRecord, e := child.PrepareOwnerPublication(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if string(childRecord.Power.Source) == string(record.Power.Source) || childRecord.OutputDigest != record.OutputDigest {
		t.Fatal("fixture did not bind distinct original source with equal approved output")
	}
	child, e = child.RestoreOwnerPublication(childRecord)
	if e != nil {
		t.Fatal(e)
	}
	child, e = child.WithOwnerPublicationInstallation(ctx, capability)
	if e != nil {
		t.Fatal("same original graph installation result", e)
	}
	if _, e := child.OwnerPublicationConsumer(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e := child.WithOwnerPublicationInstallation(ports.WithNativeInvocationKey(ctx, strings.Repeat("b", 64)), capability); e == nil {
		t.Fatal("installation result replayed across invocation")
	}
	foreign := runtime
	foreign.Environment = append(append([]string(nil), runtime.Environment...), "SUBYARD_HOME="+filepath.Join(root, "foreign-authority"))
	foreignRecord, e := foreign.PrepareOwnerPublication(ctx)
	if e != nil {
		t.Fatal(e)
	}
	foreign, e = foreign.RestoreOwnerPublication(foreignRecord)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := foreign.WithOwnerPublicationInstallation(ctx, capability); e == nil {
		t.Fatal("installation capability crossed data authority")
	}
	lost, e := runtime.RestoreOwnerPublication(record)
	if e != nil {
		t.Fatal(e)
	}
	generation := `{"generation":"` + record.Consumer.Generation + `","owner":"host-power","version":1}` + "\n"
	for _, path := range runtime.publicationResourcePaths() {
		testkit.WriteFile(t, filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".native-custody.json"), []byte(generation), 0644)
	}
	if _, e := lost.OwnerPublicationConsumer(ctx); e == nil {
		t.Fatal("fresh process selected a later installation without causal capability")
	}
	if _, e := child.OwnerPublicationConsumer(ctx); e == nil {
		t.Fatal("captured result survived generation replacement")
	}
	// The original successful command payload is materialized before any cap.
	resultObserver := &publicationInstallResultObserver{}
	resultCtx := ports.WithNativeInvocationKey(ports.WithNativeOperationObserver(context.Background(), resultObserver), strings.Repeat("e", 64))
	installed, e := lost.ownerPublication.original.CapturePublicationConsumer(&record.Consumer)
	if e != nil {
		t.Fatal(e)
	}
	ids, e := runtime.hostPowerIdentities()
	if e != nil {
		t.Fatal(e)
	}
	tail, _ := json.Marshal(hostPowerTail{Version: 1, FilesDigest: installed.FilesDigest, Identities: ids, Generations: installed.Generations})
	completion := &hostPowerCompletion{command: 7, completed: true}
	_, _ = completion.output.Write(append(tail, '\n'))
	if e := lost.recordHostPowerCompletion(resultCtx, completion); e != nil || resultObserver.records != 1 || lost.OwnerPublicationInstallation() != nil {
		t.Fatal("record-before-cap order", e)
	}
	fresh, e := runtime.RestoreOwnerPublication(record)
	if e != nil {
		t.Fatal(e)
	}
	fresh, e = fresh.ImportOwnerPublicationInstallation(resultCtx, *resultObserver.result)
	if e != nil {
		t.Fatal("fresh live import of exact selected producer", e)
	}
	if _, e := fresh.OwnerPublicationConsumer(resultCtx); e != nil {
		t.Fatal("imported installation unavailable", e)
	}
	for _, change := range []string{"foreign-producer", "foreign-source", "foreign-output", "different-generation", "samebytes-newinode", "missing-result"} {
		t.Run(change, func(t *testing.T) {
			candidate := *resultObserver.result
			candidate.Identities = append([]ports.NativeHostPowerIdentity(nil), candidate.Identities...)
			candidate.Consumer.Generations = append([]ports.NativeResourceGeneration(nil), candidate.Consumer.Generations...)
			switch change {
			case "foreign-producer":
				candidate.Producer = strings.Repeat("f", 64)
			case "foreign-source":
				candidate.HostSource = strings.Repeat("f", 64)
			case "foreign-output":
				candidate.Output = strings.Repeat("f", 64)
			case "different-generation":
				candidate.Consumer.Generations[0].Value = strings.Repeat("f", 64)
			case "samebytes-newinode":
				candidate.Identities[0].Inode++
			case "missing-result":
				resultObserver.result = nil
				defer func() { resultObserver.result = &candidate }()
			}
			empty, e := runtime.RestoreOwnerPublication(record)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = empty.ImportOwnerPublicationInstallation(resultCtx, candidate); e == nil {
				t.Fatal("unsafe result imported")
			}
		})
	}
	if _, e := fresh.OwnerPublicationConsumer(ports.WithNativeInvocationKey(resultCtx, strings.Repeat("f", 64))); e == nil {
		t.Fatal("import capability survived fresh invocation")
	}
	for _, path := range runtime.publicationResourcePaths() {
		if e := os.Remove(filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".native-custody.json")); e != nil {
			t.Fatal(e)
		}
	}

	if _, e := runtime.PublicationWorkerConsumer(context.Background(), encoded); e == nil {
		t.Fatal("same-byte foreign executable identity admitted")
	}
	input, e := json.Marshal(struct {
		Environment []string
		Encoded     string
		Consumer    ownerpublication.Consumer
	}{runtime.Environment, encoded, consumer})
	if e != nil {
		t.Fatal(e)
	}
	testkit.WriteFile(t, filepath.Join(root, "worker-input.json"), input, 0600)
	command := exec.Command(engine, "-test.run=^TestPublicationConsumerProtectedNativeProcess$", "-test.count=1")
	command.Env = append(os.Environ(), "SUBYARD_TEST_PUBLICATION_WORKER_CHILD="+root)
	if output, e := command.CombinedOutput(); e != nil {
		t.Fatal("actual installed native worker", e, string(output))
	}
	testkit.WriteFile(t, passive+".next", []byte(body), 0644)
	if e := os.Rename(passive+".next", passive); e != nil {
		t.Fatal(e)
	}
	if e := runtime.CheckPublicationConsumer(context.Background(), consumer); e == nil {
		t.Fatal("same-byte consumer replacement adopted")
	}
	if _, e = runtime.CapturePublicationConsumer(plan); e == nil {
		t.Fatal("post-consent same-byte replacement adopted")
	}
	consumer, e = runtime.CapturePublicationConsumer(&PublicationConsumerPlan{EngineDigest: plan.EngineDigest, UnitDigest: plan.UnitDigest, ContractsDigest: plan.ContractsDigest})
	if e != nil {
		t.Fatal(e)
	}
	testkit.WriteFile(t, manager, []byte(strings.Replace(script, "DropInPaths=", "DropInPaths=/foreign.conf", 1)), 0755)
	if e := runtime.CheckPublicationConsumer(context.Background(), consumer); e == nil {
		t.Fatal("foreign manager drop-in admitted")
	}
	testkit.WriteFile(t, manager, []byte(script), 0755)
	generation = `{"generation":"` + strings.Repeat("c", 64) + `","owner":"host-power","version":1}` + "\n"
	testkit.WriteFile(t, filepath.Join(root, ".engine.native-custody.json"), []byte(generation), 0644)
	if _, e := runtime.CapturePublicationConsumer(plan); e == nil {
		t.Fatal("unapproved later install generation adopted")
	}
	if e := os.Remove(filepath.Join(root, ".engine.native-custody.json")); e != nil {
		t.Fatal(e)
	}
	testkit.WriteFile(t, engine, []byte("foreign image"), 0755)
	if _, e := runtime.PublicationWorkerConsumer(context.Background(), encoded); e == nil {
		t.Fatal("worker executing image mismatch admitted")
	}
}

func TestPublicationInstallationScopePreservesRawAuthorityPaths(t *testing.T) {
	record := &OwnerPublicationRecord{}
	for _, key := range []string{"SUBYARD_HOME", "SUBYARD_CONFIG_HOME", "SUBYARD_POWER_UNIT_PATH"} {
		t.Run(key, func(t *testing.T) {
			first := Runtime{Environment: []string{key + "=/authority-\xff/power.service"}}
			second := Runtime{Environment: []string{key + "=/authority-\xfe/power.service"}}
			if first.ownerPublicationOutput(record) == second.ownerPublicationOutput(record) {
				t.Fatal("byte-distinct authority scopes shared an installation result")
			}
		})
	}
}

func TestPublicationConsumerRefusesUnrepresentableEngineBeforeConsent(t *testing.T) {
	runtime := shippedPreparedSourcesFixture(t)
	source := filepath.Join(runtime.RepositoryRoot, "source-engine")
	testkit.WriteFile(t, source, []byte("approved engine"), 0755)
	runtime.Environment = append(runtime.Environment, "SUBYARD_POWER_ENGINE_SOURCE="+source)
	runtime.Environment = append(runtime.Environment, "SUBYARD_POWER_RECONCILER_PATH="+filepath.Join(runtime.RepositoryRoot, "unrepresentable'engine"))
	prepared, err := runtime.PrepareHostPowerRuntime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if _, err := runtime.PreparePublicationConsumer(prepared); err == nil {
		t.Fatal("unstartable consumer approved before consent")
	}
}
