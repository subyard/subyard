package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/incusclient"
	"github.com/Subyard/Subyard/internal/adapters/networkruntime"
	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/audit"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerpublication"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/previewroute"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
	"golang.org/x/sys/unix"
)

// Separate commands are required: sh -e does not stop for the first operand
// of an && list, which could hide an earlier retained resource.
const publicationPhysicalRuntimeArtifactsAbsent = `for path do test ! -e "$path"; test ! -L "$path"; done`

func TestPublicationPhysicalRuntimeArtifactsAbsent(t *testing.T) {
	roles := []string{"engine", "power-unit", "passive-unit"}
	cases := []struct {
		name    string
		role    int
		symlink bool
	}{
		{"all-absent", -1, false},
		{"engine-present", 0, false},
		{"engine-dangling-symlink", 0, true},
		{"power-unit-present", 1, false},
		{"power-unit-dangling-symlink", 1, true},
		{"passive-unit-present", 2, false},
		{"passive-unit-dangling-symlink", 2, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := testkit.TempDir(t)
			paths := make([]string, len(roles))
			for index, role := range roles {
				paths[index] = filepath.Join(root, role)
			}
			if tc.role >= 0 {
				if tc.symlink {
					if err := os.Symlink(filepath.Join(root, "absent-target"), paths[tc.role]); err != nil {
						t.Fatal(err)
					}
				} else {
					testkit.WriteFile(t, paths[tc.role], []byte("retained runtime artifact\n"), 0o644)
				}
			}
			args := append([]string{"-eu", "-c", publicationPhysicalRuntimeArtifactsAbsent, "sh"}, paths...)
			output, err := exec.Command("sh", args...).CombinedOutput()
			if tc.role < 0 {
				if err != nil {
					t.Fatalf("all absent: %v: %s", err, output)
				}
			} else {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 1 {
					t.Fatalf("retained %s was not refused with exit1: %v: %s", roles[tc.role], err, output)
				}
			}
		})
	}
}

// Host-free controls model manager/journal observations, including the race in
// which an inactive unit is unloaded between successful exit and systemctl show.
func TestPublicationPhysicalJournalWitness(t *testing.T) {
	boot := strings.Repeat("a", 32)
	invocation := strings.Repeat("b", 32)
	old := strings.Repeat("c", 32)
	event := func(id string, number int) map[string]string {
		r := map[string]string{"__CURSOR": fmt.Sprintf("cursor-%d", number), "__MONOTONIC_TIMESTAMP": strconv.Itoa(number), "__REALTIME_TIMESTAMP": strconv.Itoa(number + 100), "_BOOT_ID": boot, "_PID": "1", "UNIT": ownerpublication.HandoffService, "INVOCATION_ID": invocation, "MESSAGE_ID": id}
		if id == publicationStarted {
			r["JOB_TYPE"], r["JOB_RESULT"] = "start", "done"
		}
		return r
	}
	anchor := event("", 0)
	good := func() []map[string]string {
		return []map[string]string{event(publicationStarted, 1), event(publicationSuccess, 2)}
	}
	encode := func(records []map[string]string) []byte {
		var data []byte
		for _, r := range records {
			b, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			data = append(append(data, b...), '\n')
		}
		return data
	}
	cases := []struct {
		name                   string
		alter                  func([]map[string]string) []map[string]string
		quiet, pending, refuse bool
	}{
		{name: "successful-completion-with-collected-manager"},
		{name: "starting-without-new-invocation", alter: func(r []map[string]string) []map[string]string {
			e := event(publicationStarting, 0)
			e["__CURSOR"] = "starting"
			delete(e, "INVOCATION_ID")
			return append([]map[string]string{e}, r...)
		}},
		{name: "starting-with-old-invocation", alter: func(r []map[string]string) []map[string]string {
			e := event(publicationStarting, 0)
			e["__CURSOR"] = "starting"
			e["INVOCATION_ID"] = old
			return append([]map[string]string{e}, r...)
		}},
		{name: "no-events-pending", pending: true, alter: func(r []map[string]string) []map[string]string { return nil }},
		{name: "started-only-pending", pending: true, alter: func(r []map[string]string) []map[string]string { return r[:1] }},
		{name: "missing-invocation", refuse: true, alter: func(r []map[string]string) []map[string]string { delete(r[1], "INVOCATION_ID"); return r }},
		{name: "different-invocation", refuse: true, alter: func(r []map[string]string) []map[string]string { r[1]["INVOCATION_ID"] = old; return r }},
		{name: "reused-invocation", refuse: true, alter: func(r []map[string]string) []map[string]string {
			for _, e := range r {
				e["INVOCATION_ID"] = old
			}
			return r
		}},
		{name: "success-without-start", refuse: true, alter: func(r []map[string]string) []map[string]string { return r[1:] }},
		{name: "forged-pid", refuse: true, alter: func(r []map[string]string) []map[string]string { r[1]["_PID"] = "2"; return r }},
		{name: "wrong-boot", refuse: true, alter: func(r []map[string]string) []map[string]string { r[1]["_BOOT_ID"] = old; return r }},
		{name: "wrong-unit", refuse: true, alter: func(r []map[string]string) []map[string]string { r[1]["UNIT"] = "foreign.service"; return r }},
		{name: "duplicate-cursor", refuse: true, alter: func(r []map[string]string) []map[string]string { r[1]["__CURSOR"] = r[0]["__CURSOR"]; return r }},
		{name: "reverse-monotonic", refuse: true, alter: func(r []map[string]string) []map[string]string { r[1]["__MONOTONIC_TIMESTAMP"] = "0"; return r }},
		{name: "missing-cursor", refuse: true, alter: func(r []map[string]string) []map[string]string { delete(r[1], "__CURSOR"); return r }},
		{name: "success-then-restart", refuse: true, alter: func(r []map[string]string) []map[string]string { return append(r, event(publicationRestart, 3)) }},
		{name: "success-then-new-start", refuse: true, alter: func(r []map[string]string) []map[string]string { return append(r, event(publicationStarted, 3)) }},
		{name: "stop-cancelled-retry", refuse: true, alter: func(r []map[string]string) []map[string]string {
			e := event("9d1aaa27d60140bd96365438aad20286", 3)
			e["JOB_TYPE"] = "stop"
			return append(r, e)
		}},
		{name: "failed-unit", refuse: true, alter: func(r []map[string]string) []map[string]string {
			r[1]["MESSAGE_ID"] = "d9b373ed55a64feb8242e02dbe79a49c"
			return r
		}},
		{name: "failed-exit", refuse: true, alter: func(r []map[string]string) []map[string]string { r[1]["EXIT_STATUS"] = "75"; return r }},
		{name: "teardown-new-activation-despite-reset-counter", quiet: true, refuse: true},
		{name: "teardown-starting-without-completion", quiet: true, refuse: true, alter: func(r []map[string]string) []map[string]string {
			return []map[string]string{event(publicationStarting, 1)}
		}},
		{name: "teardown-idle", quiet: true, pending: true, alter: func(r []map[string]string) []map[string]string { return nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := good()
			if tc.alter != nil {
				r = tc.alter(r)
			}
			parsed, err := publicationJournalRecords(encode(r), nil, boot)
			if err == nil {
				err = publicationJournalSequence(anchor, nil, parsed)
			}
			complete := false
			if err == nil {
				complete, err = publicationJournalCompletion(parsed, old, tc.quiet)
			}
			if (err != nil) != tc.refuse || !tc.refuse && complete != !tc.pending {
				t.Fatalf("complete=%v error=%v", complete, err)
			}
		})
	}
	t.Run("malformed-and-failed-readers", func(t *testing.T) {
		for name, data := range map[string][]byte{"malformed": []byte("{"), "trailing-json": append(encode(good()), []byte("{")...), "null": []byte(`{"__CURSOR":null}`), "array": []byte(`{"__CURSOR":["a","b"]}`), "duplicate-field": []byte(`{"__CURSOR":"a","__CURSOR":"b"}`), "overflow-bytes": bytes.Repeat([]byte(" "), publicationJournalBytes+1)} {
			t.Run(name, func(t *testing.T) {
				if _, err := publicationJournalRecords(data, nil, boot); err == nil {
					t.Fatal("invalid reader accepted")
				}
			})
		}
		if _, err := publicationJournalRecords(encode(good()), errors.New("reader failed"), boot); err == nil {
			t.Fatal("failed reader accepted")
		}
		var overflow []map[string]string
		for n := 1; n <= 257; n++ {
			overflow = append(overflow, event("", n))
		}
		if _, err := publicationJournalRecords(encode(overflow), nil, boot); err == nil {
			t.Fatal("overflow accepted")
		}
		if err := publicationJournalSequence(anchor, good(), good()[:1]); err == nil {
			t.Fatal("shrinking history accepted")
		}
		changed := good()
		changed[0]["JOB_RESULT"] = "failed"
		if err := publicationJournalSequence(anchor, good(), changed); err == nil {
			t.Fatal("changed history accepted")
		}
	})
	t.Run("old-failed-queued-fresh-success", func(t *testing.T) {
		m := map[string]string{"MainPID": "0", "ActiveState": "failed", "SubState": "failed", "Job": "0", "ExecMainCode": "1", "ExecMainStatus": "75", "InvocationID": old}
		if idle, err := publicationPassiveManager(m, old, nil); err != nil || idle {
			t.Fatalf("old failure must remain pending: idle=%v err=%v", idle, err)
		}
		m["Job"] = "12"
		if idle, err := publicationPassiveManager(m, old, nil); err != nil || idle {
			t.Fatalf("queued fresh job must remain pending: idle=%v err=%v", idle, err)
		}
		m["InvocationID"], m["ActiveState"], m["SubState"], m["ExecMainStatus"] = invocation, "activating", "start", "0"
		if idle, err := publicationPassiveManager(m, old, nil); err != nil || idle {
			t.Fatalf("fresh running job must remain pending: idle=%v err=%v", idle, err)
		}
		m["ActiveState"], m["SubState"], m["Job"] = "inactive", "dead", "0"
		if idle, err := publicationPassiveManager(m, old, good()); err != nil || !idle {
			t.Fatalf("fresh success refused: idle=%v err=%v", idle, err)
		}
		m["ExecMainStatus"] = "75"
		if _, err := publicationPassiveManager(m, old, nil); err == nil {
			t.Fatal("fresh manager exit75 accepted before Started ingestion")
		}
		m["InvocationID"] = ""
		if _, err := publicationPassiveManager(m, old, good()[:1]); err == nil {
			t.Fatal("fresh Started exit75 accepted after invocation collection")
		}
	})
	t.Run("manager-reset-and-retry", func(t *testing.T) {
		m := map[string]string{"MainPID": "0", "ActiveState": "inactive", "SubState": "dead", "Job": "0", "ExecMainCode": "0", "ExecMainStatus": "0", "InvocationID": "", "NRestarts": "0"}
		if idle, err := publicationPassiveManager(m, old, good()); err != nil || !idle {
			t.Fatalf("collected manager refused: %v", err)
		}
		m["ExecMainCode"] = "1" // CLD_EXITED is the normal recorded successful-exit code.
		if idle, err := publicationPassiveManager(m, old, good()); err != nil || !idle {
			t.Fatalf("retained successful manager refused: %v", err)
		}
		m["ActiveState"], m["SubState"] = "activating", "auto-restart"
		if idle, _ := publicationPassiveManager(m, old, good()); idle {
			t.Fatal("force75 retry accepted")
		}
		m["ExecMainStatus"] = "75"
		if _, err := publicationPassiveManager(m, old, good()); err == nil {
			t.Fatal("nonzero status accepted")
		}
		m["ExecMainStatus"], m["ExecMainCode"] = "0", "2"
		if _, err := publicationPassiveManager(m, old, good()); err == nil {
			t.Fatal("signalled exit accepted")
		}
	})
}

type publicationPhysical struct {
	t                                                                                       *testing.T
	ctx                                                                                     context.Context
	cli                                                                                     *CLI
	loaded                                                                                  config.Loaded
	runtime                                                                                 reconcileruntime.Runtime
	client                                                                                  *incusclient.Client
	root, marker, project, instance, foreignProject, foreign, iface, address, port, preview string
	workerGroup                                                                             string
	contracts                                                                               []ownerpublication.Contract
	consumer                                                                                ownerpublication.Consumer
	plan                                                                                    *reconcileruntime.PublicationConsumerPlan
	uuid                                                                                    string
}

// This is an explicitly selected Go callback fixture. Its checked observer is
// test-owned; it establishes real SDK/PID1/OFD coupling, never Rust admission.
func TestOwnerPublicationHandoffPhysical(t *testing.T) {
	root := os.Getenv("SUBYARD_PUBLICATION_PHYSICAL_ROOT")
	if root == "" {
		if os.Getenv("SUBYARD_PUBLICATION_PHYSICAL_REQUIRED") == "1" {
			t.Fatal("required physical publication fixture: SUBYARD_PUBLICATION_PHYSICAL_ROOT is missing")
		}
		t.Skip("opt-in actual Incus/PID1 publication lane; --publication-handoff makes this case required")
	}
	if os.Getenv("SUBYARD_E2E_VM") != "1" && os.Getenv("SUBYARD_E2E_VM") != "2" {
		t.Fatal("physical publication fixture requires a leased disposable VM")
	}
	if os.Geteuid() == 0 {
		t.Fatal("run as the fixture operator so the original owner OFD belongs to that account")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
	t.Cleanup(cancel)
	f := &publicationPhysical{t: t, ctx: ctx, root: root}
	for key, target := range map[string]*string{"MARKER": &f.marker, "PROJECT": &f.project, "INSTANCE": &f.instance, "FOREIGN_PROJECT": &f.foreignProject, "FOREIGN": &f.foreign, "INTERFACE": &f.iface, "ADDRESS": &f.address, "PORT": &f.port, "PREVIEW": &f.preview} {
		*target = os.Getenv("SUBYARD_PUBLICATION_PHYSICAL_" + key)
		if *target == "" {
			t.Fatalf("required physical publication fixture: %s is missing", key)
		}
	}
	if !filepath.IsAbs(root) || !domain.SafeName(f.project) || !domain.SafeName(f.instance) || !domain.SafeName(f.foreign) || !domain.SafeName(f.foreignProject) || f.foreignProject == f.project {
		t.Fatal("invalid physical fixture scope")
	}
	f.command("sudo", "-n", "true")
	if strings.TrimSpace(string(f.command("ps", "-p", "1", "-o", "comm="))) != "systemd" {
		t.Fatal("real PID1 systemd is required")
	}
	if strings.TrimSpace(string(f.command("ip", "-j", "link", "show", "dev", f.iface))) == "" {
		t.Fatal("marker-owned dummy interface is required")
	}
	var links []struct {
		Alias string `json:"ifalias"`
	}
	if json.Unmarshal(f.command("ip", "-j", "link", "show", "dev", f.iface), &links) != nil || len(links) != 1 || links[0].Alias != f.marker {
		t.Fatal("foreign dummy interface")
	}
	if strings.TrimSpace(string(f.command("incus", "project", "get", f.project, "user.subyard.p0-power-systemd"))) != f.marker {
		t.Fatal("foreign Incus project")
	}
	if strings.TrimSpace(string(f.command("incus", "project", "get", f.foreignProject, "user.subyard.p0-publication-foreign"))) != f.marker {
		t.Fatal("foreign control project is not fixture-owned")
	}
	f.client = incusclient.New("", "projects")
	var err error
	f.cli, err = New(Options{RepositoryRoot: root, DispatcherPath: filepath.Join(root, "bin/yard-engine"), WorkingDir: root, Environment: os.Environ(), Incus: f.client, Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: os.Stderr})
	f.check(err)
	f.loaded, err = f.cli.loadContext("default")
	f.check(err)
	if f.loaded.Context.AccessKind != domain.AccessLocal || f.loaded.Context.YardKind != domain.YardContainer || f.loaded.Context.IncusProject != f.project || f.loaded.Context.YardInstanceName != f.instance {
		t.Fatal("fixture requires its actual registered local container; no QEMU CPU claim")
	}
	yards, err := f.cli.powerYardContexts(f.loaded)
	f.check(err)
	var ok bool
	f.check(f.cli.prepareSudoPrivileges(ctx, os.Stderr, f.cli.effectiveUID(), "start"))
	f.runtime, ok = f.cli.initPlatform(f.loaded, yards).(reconcileruntime.Runtime)
	if !ok {
		t.Fatal("production CLI runtime unavailable")
	}
	f.runtime.LogicalRepositoryRoot = root
	f.contracts, err = ownerpublication.Load(root)
	f.check(err)
	p, err := f.runtime.PrepareHostPowerRuntime(ctx)
	f.check(err)
	defer p.Close()
	f.plan, err = f.runtime.PreparePublicationConsumer(p)
	f.check(err)
	f.consumer, err = f.runtime.CapturePublicationConsumer(f.plan)
	f.check(err)
	if len(f.consumer.Generations) != 3 {
		t.Fatal("preconverged three-resource installation is required")
	}
	f.check(f.runtime.CheckPublicationConsumer(ctx, f.consumer))
	properties := f.manager()
	if properties["RestartUSec"] != "30s" || properties["RuntimeMaxUSec"] != "30s" || properties["FragmentPath"] != "/etc/systemd/system/"+ownerpublication.HandoffService || properties["DropInPaths"] != "" {
		t.Fatal("installed passive manager contract differs", properties)
	}
	f.command("sudo", "-n", "systemctl", "stop", "subyard-power-reconcile.service")
	t.Cleanup(func() { f.t = t; f.stopWorker() })
	i := f.instanceInfo()
	f.uuid = i.LocalConfig["volatile.uuid"]
	if f.uuid == "" || i.LocalConfig["user.subyard.managed"] != "true" || i.Type != domain.YardContainer {
		t.Fatal("unowned physical instance")
	}
	for _, phase := range []struct {
		name string
		run  func()
	}{
		{"ack-tail", func() { f.resetReceipt(); f.capsule(false, false) }},
		{"coalesced-retry", func() { f.resetReceipt(); f.capsule(true, false) }},
		{"exact-restore", func() { f.restore() }},
		{"temporary-stop", func() { f.temporaryStop() }},
		{"serialized-stop-retirement", func() { f.resetReceipt(); f.prepareSerializedStop(); f.capsule(false, true) }},
		{"teardown-waiting", func() { f.teardown() }},
	} {
		if !t.Run(phase.name, func(t *testing.T) { f.t = t; phase.run() }) {
			t.FailNow()
		}
	}
}

func (f *publicationPhysical) check(err error) {
	f.t.Helper()
	if err != nil {
		f.t.Fatal(err)
	}
}
func (f *publicationPhysical) command(argv ...string) []byte {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(f.ctx, 45*time.Second)
	defer cancel()
	if len(argv) == 3 && argv[0] == filepath.Join(f.root, "bin/yard-engine") && argv[1] == "migrate" && argv[2] == "--yes" {
		// Readiness preparation shares the fixture budget; its full activation
		// observations and repair are not a short native command.
		ctx = f.ctx
	}
	b, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
	if err != nil {
		f.t.Fatalf("physical command %q: %v: %s", argv, err, b)
	}
	return b
}
func (f *publicationPhysical) instanceInfo() ports.InstanceInfo {
	f.t.Helper()
	i, e := f.client.Instance(f.ctx, f.project, f.instance)
	f.check(e)
	return i
}
func (f *publicationPhysical) poll(budget time.Duration, check func() bool) {
	f.t.Helper()
	deadline := time.NewTimer(budget)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if check() {
			return
		}
		select {
		case <-deadline.C:
			f.t.Fatal("physical postcondition deadline")
		case <-f.ctx.Done():
			f.t.Fatal(f.ctx.Err())
		case <-tick.C:
		}
	}
}
func (f *publicationPhysical) manager() map[string]string {
	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, "sudo", "-n", "systemctl", "show", ownerpublication.HandoffService, "-p", "LoadState", "-p", "MainPID", "-p", "ActiveState", "-p", "SubState", "-p", "Job", "-p", "ControlGroup", "-p", "InvocationID", "-p", "NRestarts", "-p", "FragmentPath", "-p", "DropInPaths", "-p", "RestartUSec", "-p", "RuntimeMaxUSec", "-p", "ExecMainStartTimestampMonotonic", "-p", "ExecMainStatus", "-p", "ExecMainCode").CombinedOutput()
	r := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			r[k] = v
		}
	}
	if err != nil && !(r["LoadState"] == "not-found" && r["MainPID"] == "0") {
		f.t.Fatalf("passive manager observation: %v: %s", err, b)
	}
	if r["ControlGroup"] != "" {
		f.workerGroup = r["ControlGroup"]
	}
	return r
}
func (f *publicationPhysical) stopWorker() {
	f.manager() // Retain the last actual cgroup even after the unit is removed.
	ctx, cancel := context.WithTimeout(f.ctx, 15*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "sudo", "-n", "systemctl", "stop", ownerpublication.HandoffService).CombinedOutput()
	if err != nil {
		m := f.manager()
		if m["LoadState"] != "not-found" || m["MainPID"] != "0" || (m["Job"] != "" && m["Job"] != "0") {
			f.t.Fatalf("passive stop: %v: %s: %v", err, output, m)
		}
	}
	f.poll(10*time.Second, func() bool {
		m := f.manager()
		return m["MainPID"] == "0" && (m["ActiveState"] == "inactive" || m["ActiveState"] == "failed") && (m["Job"] == "" || m["Job"] == "0")
	})
	if f.workerGroup != "" {
		f.command("sudo", "-n", "sh", "-eu", "-c", `test ! -e /sys/fs/cgroup"$1"/cgroup.procs || test -z "$(cat /sys/fs/cgroup"$1"/cgroup.procs)"`, "sh", f.workerGroup)
	}
}
func (f *publicationPhysical) addressPresent() bool {
	var devices []struct {
		Addresses []struct {
			Local string `json:"local"`
		} `json:"addr_info"`
	}
	f.check(json.Unmarshal(f.command("ip", "-j", "address", "show", "dev", f.iface), &devices))
	for _, d := range devices {
		for _, a := range d.Addresses {
			if a.Local == f.address {
				return true
			}
		}
	}
	return false
}
func (f *publicationPhysical) setAddress(present bool) {
	if f.addressPresent() == present {
		return
	}
	verb := "del"
	if present {
		verb = "add"
	}
	f.command("sudo", "-n", "ip", "address", verb, f.address+"/32", "dev", f.iface)
}
func (f *publicationPhysical) owner() func() {
	f.t.Helper()
	release, err := f.cli.admitOwnerWrite(f.ctx, f.loaded)
	f.check(err)
	return release
}

func TestPublicationPhysicalOwnerRetainsAuditedChildBarrier(t *testing.T) {
	publicationPhysicalOwnerRetainsAuditedChildBarrier(t, false)
}

func TestPublicationPhysicalNativeOwnerRetainsAuditedChildBarrier(t *testing.T) {
	publicationPhysicalOwnerRetainsAuditedChildBarrier(t, true)
}

func publicationPhysicalOwnerRetainsAuditedChildBarrier(t *testing.T, native bool) {
	root, environment, _ := nativeFixture(t)
	values := environmentMap(environment)
	delete(values, "SUBYARD_NO_AUDIT")
	environment = environmentList(values, nil)
	nativeInstallationFixture(t, environment)
	runner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Result: domain.AdapterResult{Status: "ok"}}}}
	program, err := New(Options{RepositoryRoot: root, WorkingDir: root, Environment: environment, AdapterRunner: runner, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	f := &publicationPhysical{t: t, ctx: ctx, cli: program, loaded: loaded}
	release := f.owner()
	defer func() {
		if release != nil {
			release()
		}
	}()
	home := loaded.Context.Paths.DataHome
	held, err := uninstall.BorrowOwnerLock(home)
	if err != nil || held == nil {
		t.Fatal("fixture owner barrier unavailable", err)
	}
	defer held.Close()
	before, err := held.Stat()
	f.check(err)
	f.check(uninstall.ValidateOwnerLockDescriptor(home, uint32(os.Getuid()), held))
	if native {
		// A native session registers the borrowed OFD without admitting a CLI command.
		child := *program
		child.ownerMutationLock = nil
		program = &child
	}
	orchestrator, plan, err := ownerChildOrchestrator(ctx, program, loaded, "start", root)
	f.check(err)
	log, ok := orchestrator.Audit.(audit.OperationLog)
	if !ok {
		t.Fatal("actual child auditing is disabled")
	}
	// Refuse the old self-deadlock under a short deadline before the audit
	// writer's ordinary Background admission can wait on the same held OFD.
	probe, stop := context.WithTimeout(ctx, 150*time.Millisecond)
	nested, allowed, err := log.BeforeWrite(probe)
	stop()
	if err != nil || !allowed {
		t.Fatal("child audit did not reuse the admitted owner barrier", allowed, err)
	}
	nested()
	result, _, err := orchestrator.RunAdapter(ctx, plan, domain.AdapterRequest{OperationID: plan.OperationID}, nil)
	if err != nil || result.Status != "ok" || len(runner.Requests) != 1 {
		t.Fatal("audited child did not finish", result, err)
	}
	raw, err := os.ReadFile(filepath.Join(home, "logs/yard.log"))
	if err != nil || !strings.Contains(string(raw), " -- operation.started\n") || !strings.Contains(string(raw), " -- operation.finished\n") {
		t.Fatal("actual child audit records are missing", err)
	}
	borrowed, err := uninstall.BorrowOwnerLock(home)
	if err != nil || borrowed == nil {
		t.Fatal("child audit released the parent barrier", err)
	}
	after, err := borrowed.Stat()
	f.check(err)
	f.check(uninstall.ValidateOwnerLockDescriptor(home, uint32(os.Getuid()), borrowed))
	f.check(borrowed.Close())
	if !os.SameFile(before, after) {
		t.Fatal("child audit replaced the held owner identity")
	}
	wait, stop := context.WithTimeout(ctx, 150*time.Millisecond)
	_, unlock, err := uninstall.OwnerLock(wait, home, nil)
	stop()
	if err == nil {
		unlock()
		t.Fatal("independent writer entered while fixture owner was held")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	// The inspection borrow also owns the OFD; close it before testing the
	// fixture's final release, without unlocking any accepted child's borrow.
	f.check(held.Close())
	release()
	release = nil
	_, unlock, err = uninstall.OwnerLock(ctx, home, nil)
	f.check(err)
	unlock()
	// The physical cases reuse this same helper after their first audit.
	again := f.owner()
	again()
}

func (f *publicationPhysical) network() func() {
	f.t.Helper()
	release, err := (networkruntime.HostLock{}).Acquire(f.ctx)
	f.check(err)
	return release
}
func (f *publicationPhysical) resetReceipt() {
	f.stopWorker()
	release := f.owner()
	defer release()
	unlock := f.network()
	defer unlock()
	i := f.instanceInfo()
	// Fixture preparation uses the original live UUID; production reconciliation
	// creates the exact v1 receipt from the approved preview definition.
	f.setAddress(true)
	f.check(f.client.SetInstanceConfig(f.ctx, f.project, f.instance, map[string]string{"user.subyard.desired_power": "running", previewroute.Key: "v1:" + f.address + ":" + f.port}))
	if !strings.EqualFold(i.Status, "running") {
		f.check(f.client.SetInstancePower(f.ctx, f.project, f.instance, "start", false))
	}
	if i.LocalConfig[ownerpublication.Prefix+previewroute.DeviceName] == "" && len(i.LocalDevices[previewroute.DeviceName]) == 0 {
		f.command("bash", "-eu", "-c", `. "$1/scripts/lib/preview-proxy.sh"; YARD_INSTANCE_NAME="$2" INCUS_PROJECT="$3" WEB_PREVIEW_HOST_PORT="$4" YARD_KIND=container; PROJ=(--project "$INCUS_PROJECT"); subyard_preview_route "$5"`, "sh", f.root, f.instance, f.project, f.port, f.address)
	}
	f.setAddress(false)
	_, err := f.client.ReconcileOriginalPublicationsIf(f.ctx, f.project, f.instance, f.uuid, domain.YardContainer, f.contracts, nil)
	f.check(err)
	i = f.instanceInfo()
	raw := i.LocalConfig[ownerpublication.Prefix+previewroute.DeviceName]
	var receipt ownerpublication.Receipt
	f.check(json.Unmarshal([]byte(raw), &receipt))
	if receipt.Version != 1 || receipt.Ownership != "v1:"+f.address+":"+f.port || !maps.Equal(receipt.Device, previewroute.Device(f.address, f.port)) || len(i.LocalDevices[previewroute.DeviceName]) != 0 {
		f.t.Fatal("missing-address receipt is not exact v1")
	}
}

// Ordinary stop must pass its real read-only migration gate before it can
// wait on the owner OFD. Establish that premise before capturing this capsule.
func (f *publicationPhysical) prepareSerializedStop() {
	inspect := func() releasetransition.PublicStatus {
		var report struct {
			Outcome  releasetransition.Outcome   `json:"outcome"`
			Blockers []releasetransition.Blocker `json:"blockers"`
		}
		f.check(json.Unmarshal(f.command(filepath.Join(f.root, "bin/yard-engine"), "migrate", "--check", "--json"), &report))
		f.t.Logf("serialized stop readiness: status=%s code=%s blockers=%v", report.Outcome.Status, report.Outcome.Code, report.Blockers)
		return report.Outcome.Status
	}
	if inspect() != releasetransition.StatusReady {
		f.command(filepath.Join(f.root, "bin/yard-engine"), "migrate", "--yes")
		if inspect() != releasetransition.StatusReady {
			f.t.Fatal("ordinary migration did not establish serialized stop readiness")
		}
	}
	var err error
	f.loaded, err = f.cli.loadContext("default")
	f.check(err)
	if f.loaded.Environment["WEB_PREVIEW_HOST_PORT"] != f.port {
		f.t.Fatal("serialized stop preview port differs from persisted fixture setting")
	}
	yards, err := f.cli.powerYardContexts(f.loaded)
	f.check(err)
	f.runtime = f.cli.initPlatform(f.loaded, yards).(reconcileruntime.Runtime)
	f.runtime.LogicalRepositoryRoot = f.root
	p, err := f.runtime.PrepareHostPowerRuntime(f.ctx)
	f.check(err)
	defer p.Close()
	f.plan, err = f.runtime.PreparePublicationConsumer(p)
	f.check(err)
	f.consumer, err = f.runtime.CapturePublicationConsumer(f.plan)
	f.check(err)
	f.check(f.runtime.CheckPublicationConsumer(f.ctx, f.consumer))
}

type publicationPhysicalObserver struct {
	handoffLifecycleObserver
	before  func(ports.NativeOperation) error
	after   func(ports.NativeOperation) error
	command func(string) error
}

func (o *publicationPhysicalObserver) Before(v ports.NativeOperation) error {
	if o.before != nil {
		if e := o.before(v); e != nil {
			return e
		}
	}
	return o.physicalStartupObserver.Before(v)
}
func (o *publicationPhysicalObserver) Terminal(v ports.NativeOperation) error {
	if e := o.physicalStartupObserver.Terminal(v); e != nil {
		return e
	}
	if o.after != nil {
		return o.after(v)
	}
	return nil
}
func (o *publicationPhysicalObserver) CommandStarted(kind, binding string) (uint64, error) {
	if o.command != nil {
		if e := o.command(kind); e != nil {
			return 0, e
		}
	}
	return o.ownerGraphObserver.CommandStarted(kind, binding)
}

// The installed worker opens policy.lock only in Restore -> WithStart, after
// Inventory and ACK. Matching its FD while this producer retains the exclusive
// lock proves that specific progress; LOCK_NB polling has no /proc/locks waiter.
func (f *publicationPhysical) workerWaiting() map[string]string {
	var proof map[string]string
	f.poll(8*time.Second, func() bool {
		m := f.manager()
		pid, err := strconv.Atoi(m["MainPID"])
		if err != nil || pid <= 1 || m["InvocationID"] == "" || m["ControlGroup"] == "" {
			return false
		}
		const probe = `import os,sys,json
p=sys.argv[1];lock='/run/lock/subyard-network/policy.lock';engine=sys.argv[2];cg=sys.argv[3]
try:
 a=os.stat(lock);e=os.stat(engine);x=os.stat('/proc/'+p+'/exe')
 assert (e.st_dev,e.st_ino)==(x.st_dev,x.st_ino)
 assert cg in [line.rsplit(':',1)[-1] for line in open('/proc/'+p+'/cgroup').read().splitlines()]
 start=open('/proc/'+p+'/stat').read().rsplit(')',1)[1].split()[19]
 for fd in os.listdir('/proc/'+p+'/fd'):
  try:
   b=os.stat('/proc/'+p+'/fd/'+fd)
   if (a.st_dev,a.st_ino)==(b.st_dev,b.st_ino):
    assert 'lock:' not in open('/proc/'+p+'/fdinfo/'+fd).read()
    print(json.dumps({'pid':p,'start':start,'lock':str(a.st_dev)+':'+str(a.st_ino),'fd':fd}));sys.exit(0)
  except FileNotFoundError:pass
except (FileNotFoundError,AssertionError,ProcessLookupError):pass
sys.exit(1)`
		b, e := exec.CommandContext(f.ctx, "sudo", "-n", "python3", "-c", probe, strconv.Itoa(pid), "/usr/local/libexec/subyard/yard-boot-reconcile", m["ControlGroup"]).CombinedOutput()
		if e != nil {
			return false
		}
		if json.Unmarshal(b, &proof) != nil {
			return false
		}
		again := f.manager()
		if again["MainPID"] != m["MainPID"] || again["InvocationID"] != m["InvocationID"] {
			return false
		}
		proof["invocation"] = m["InvocationID"]
		proof["cgroup"] = m["ControlGroup"]
		return true
	})
	file, err := os.OpenFile("/run/lock/subyard-network/policy.lock", os.O_RDWR, 0)
	f.check(err)
	defer file.Close()
	err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		f.t.Fatal("producer lost exclusive real network lock")
	}
	if !errors.Is(err, syscall.EWOULDBLOCK) {
		f.t.Fatal(err)
	}
	f.t.Logf("installed worker inventory-to-lock progress: %v", proof)
	return proof
}

// An owned verified child runs a sealed memfd copy, with a different inode
// from the installed engine. Its bytes and all mutation-preventing seals bind it.
func publicationStopExecutable(image *os.File, expected [sha256.Size]byte, installedRoot bool) (bool, int, bool, error) {
	hash := sha256.New()
	if _, err := io.Copy(hash, image); err != nil {
		return false, 0, false, err
	}
	matched := bytes.Equal(hash.Sum(nil), expected[:])
	seals, err := unix.FcntlInt(image.Fd(), unix.F_GET_SEALS, 0)
	if err != nil {
		seals = 0
	}
	required := unix.F_SEAL_WRITE | unix.F_SEAL_GROW | unix.F_SEAL_SHRINK | unix.F_SEAL_SEAL
	return matched, seals, matched && (installedRoot || seals&required == required), nil
}

// publicationStopWaiter inspects only the launched process and its current owned
// descendants. Go may launch its verified mutation inspector on another thread.
func publicationStopWaiter(root int, start, lock string, engineDigest [sha256.Size]byte) (bool, []string) {
	expected, err := os.Stat(lock)
	if err != nil {
		return false, []string{"lock stat: " + err.Error()}
	}
	identity := func(pid int) (string, int, error) {
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return "", 0, err
		}
		end := strings.LastIndexByte(string(raw), ')')
		if end < 0 {
			return "", 0, errors.New("invalid process stat")
		}
		fields := strings.Fields(string(raw)[end+1:])
		if len(fields) < 20 {
			return "", 0, errors.New("short process stat")
		}
		parent, err := strconv.Atoi(fields[1])
		return fields[19], parent, err
	}
	current, _, err := identity(root)
	executable, executableErr := os.Stat(fmt.Sprintf("/proc/%d/exe", root))
	if err != nil || executableErr != nil || current != start {
		return false, []string{"launched process identity unavailable"}
	}
	type member struct {
		pid, parent int
		parentStart string
	}
	queue := []member{{pid: root}}
	seen := map[int]bool{}
	var census []string
	for len(queue) > 0 && len(seen) < 128 {
		item := queue[0]
		queue = queue[1:]
		if seen[item.pid] {
			continue
		}
		seen[item.pid] = true
		before, parent, e := identity(item.pid)
		if e != nil {
			census = append(census, fmt.Sprintf("pid=%d stat: %v", item.pid, e))
			continue
		}
		if item.pid != root {
			ps, _, e := identity(item.parent)
			if e != nil || ps != item.parentStart || parent != item.parent {
				continue
			}
		}
		exe, e := os.Readlink(fmt.Sprintf("/proc/%d/exe", item.pid))
		if e != nil {
			census = append(census, fmt.Sprintf("pid=%d exe: %v", item.pid, e))
			continue
		}
		matched := false
		rawFD, unlocked, imageAccepted, digestMatch := false, false, false, false
		seals := 0
		var fdFacts []string
		entries, e := os.ReadDir(fmt.Sprintf("/proc/%d/fd", item.pid))
		if e != nil {
			census = append(census, fmt.Sprintf("pid=%d fd: %v", item.pid, e))
		}
		for _, entry := range entries {
			path := fmt.Sprintf("/proc/%d/fd/%s", item.pid, entry.Name())
			actual, e := os.Stat(path)
			if e != nil || !os.SameFile(actual, expected) {
				if e != nil && !os.IsNotExist(e) {
					census = append(census, fmt.Sprintf("pid=%d fd=%s stat: %v", item.pid, entry.Name(), e))
				}
				continue
			}
			rawFD = true
			info, e := os.ReadFile(fmt.Sprintf("/proc/%d/fdinfo/%s", item.pid, entry.Name()))
			if e != nil {
				census = append(census, fmt.Sprintf("pid=%d fd=%s info: %v", item.pid, entry.Name(), e))
				continue
			}
			flags := uint64(0)
			for _, line := range strings.Split(string(info), "\n") {
				if strings.HasPrefix(line, "flags:") {
					flags, _ = strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, "flags:")), 8, 64)
				}
			}
			locked := strings.Contains(string(info), "lock:")
			native, _ := actual.Sys().(*syscall.Stat_t)
			if native != nil {
				fdFacts = append(fdFacts, fmt.Sprintf("fd=%s dev=%d ino=%d flags=%o lock-line=%t", entry.Name(), native.Dev, native.Ino, flags, locked))
			}
			if flags&syscall.O_ACCMODE != syscall.O_RDWR || strings.Contains(string(info), "lock:") {
				continue
			}
			unlocked = true
			image, imageErr := os.Open(fmt.Sprintf("/proc/%d/exe", item.pid))
			if imageErr != nil {
				census = append(census, fmt.Sprintf("pid=%d image: %v", item.pid, imageErr))
				continue
			}
			imageBefore, beforeErr := image.Stat()
			digestMatch, seals, imageAccepted, imageErr = publicationStopExecutable(image, engineDigest, item.pid == root && beforeErr == nil && os.SameFile(imageBefore, executable))
			_ = image.Close()
			if imageErr != nil {
				census = append(census, fmt.Sprintf("pid=%d image digest: %v", item.pid, imageErr))
				continue
			}
			after, ppid, e := identity(item.pid)
			again, aerr := os.Stat(path)
			currentExe, exeErr := os.Stat(fmt.Sprintf("/proc/%d/exe", item.pid))
			if e == nil && aerr == nil && exeErr == nil && beforeErr == nil && os.SameFile(imageBefore, currentExe) && imageAccepted && before == after && parent == ppid && os.SameFile(again, expected) {
				matched = true
			}
		}
		census = append(census, fmt.Sprintf("pid=%d ppid=%d start=%s exe=%s owner-fd=%t unlocked=%t digest-match=%t seals=%x executable-admitted=%t witness=%t %v", item.pid, parent, before, exe, rawFD, unlocked, digestMatch, seals, imageAccepted, matched, fdFacts))
		if !matched {
			for _, leaf := range []string{"wchan", "syscall"} {
				raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/%s", item.pid, leaf))
				value := strings.TrimSpace(string(raw))
				if len(value) > 256 {
					value = value[:256]
				}
				census = append(census, fmt.Sprintf("pid=%d %s=%s read-error=%v", item.pid, leaf, value, err))
			}
		}
		if matched {
			rootNow, _, rootErr := identity(root)
			if rootErr == nil && rootNow == start {
				return true, census
			}
			return false, census
		}
		tasks, e := os.ReadDir(fmt.Sprintf("/proc/%d/task", item.pid))
		if e != nil {
			census = append(census, fmt.Sprintf("pid=%d tasks: %v", item.pid, e))
			continue
		}
		for _, task := range tasks {
			raw, e := os.ReadFile(fmt.Sprintf("/proc/%d/task/%s/children", item.pid, task.Name()))
			if e != nil {
				continue
			}
			for _, word := range strings.Fields(string(raw)) {
				pid, e := strconv.Atoi(word)
				if e == nil {
					queue = append(queue, member{pid, item.pid, before})
				}
			}
		}
	}
	return false, census
}

func (f *publicationPhysical) capsule(coalesced, queuedStop bool) {
	f.stopWorker()
	release := f.owner()
	defer func() {
		if release != nil {
			release()
		}
	}()
	original, err := captureOwnerStart(f.ctx, f.cli, f.loaded, f.root)
	f.check(err)
	original.Publication = f.plan
	sources, err := f.runtime.PrepareOwnerRuntimeSources(f.ctx)
	f.check(err)
	f.check(sources.Close())
	observer := &publicationPhysicalObserver{}
	ctx, cancel := context.WithTimeout(ports.WithNativeOwnerHome(ports.WithNativeInvocationKey(ports.WithNativeOperationObserver(f.ctx, observer), strings.Repeat("c", 64)), f.loaded.Context.Paths.DataHome), 80*time.Second)
	defer cancel()
	var old map[string]string
	var prior ownerpublication.Handoff
	posted := false
	var stop *exec.Cmd
	var stopDone chan error
	stopDrained := false
	sawRetry := false
	observer.before = func(v ports.NativeOperation) error {
		if v.Kind != "instance-publication-handoff" || posted {
			return nil
		}
		posted = true
		if coalesced {
			if e := f.runtime.StartPublicationConsumer(ports.WithoutNativeOperationObserver(ctx), f.consumer); e != nil {
				return e
			}
			old = f.workerWaiting()
		}
		if queuedStop {
			engineBytes, e := os.ReadFile(filepath.Join(f.root, "bin/yard-engine"))
			if e != nil {
				return e
			}
			engineDigest := sha256.Sum256(engineBytes)
			stop = exec.CommandContext(f.ctx, filepath.Join(f.root, "bin/yard-engine"), "stop", "--force", "--yes")
			stop.Env = os.Environ()
			stop.Stdout = os.Stderr
			stop.Stderr = os.Stderr
			if e := stop.Start(); e != nil {
				return e
			}
			stopDone = make(chan error, 1)
			go func() { stopDone <- stop.Wait() }()
			f.t.Cleanup(func() {
				if !stopDrained {
					_ = stop.Process.Kill()
					select {
					case <-stopDone:
					case <-time.After(5 * time.Second):
						f.t.Error("queued stop did not drain")
					}
				}
			})
			path := uninstall.OwnerLockPath(f.loaded.Context.Paths.DataHome)
			processStart := facadeProcessStart(f.t, stop.Process.Pid)
			var census []string
			defer func() { f.t.Logf("ordinary queued stop owned process census: %v", census) }()
			deadline, _ := ctx.Deadline()
			f.poll(time.Until(deadline), func() bool {
				select {
				case e := <-stopDone:
					stopDrained = true
					err = fmt.Errorf("ordinary stop exited before owner barrier: %v", e)
					return true
				default:
				}
				var waiting bool
				waiting, census = publicationStopWaiter(stop.Process.Pid, processStart, path, engineDigest)
				return waiting
			})
			if stopDrained {
				return err
			}
			// The waiter has an independent, unlocked descriptor; the original
			// producer must still exclude a separate acquisition.
			probe, e := os.OpenFile(path, os.O_RDWR, 0)
			if e != nil {
				return e
			}
			e = syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			if e == nil {
				_ = syscall.Flock(int(probe.Fd()), syscall.LOCK_UN)
			}
			_ = probe.Close()
			if !errors.Is(e, syscall.EWOULDBLOCK) {
				return fmt.Errorf("queued stop producer exclusion: %v", e)
			}
			select {
			case e := <-stopDone:
				stopDrained = true
				return fmt.Errorf("ordinary stop passed original owner barrier early: %v", e)
			default:
			}
		}
		return nil
	}
	observer.command = func(kind string) error {
		if coalesced && kind == "publication-delivery-start" {
			m := f.manager()
			if m["SubState"] == "auto-restart" {
				sawRetry = true
			}
		}
		return nil
	}
	observer.after = func(v ports.NativeOperation) error {
		if v.Kind == "instance-publication-handoff" {
			i := f.instanceInfo()
			if value := i.LocalConfig[ownerpublication.HandoffPrefix+previewroute.DeviceName]; value != "" && prior.ID == "" {
				h, e := ownerpublication.DecodeHandoff(value)
				if e != nil {
					return e
				}
				prior = h
			} else if value == "" && coalesced {
				fresh := f.workerWaiting()
				if fresh["pid"] == old["pid"] || fresh["invocation"] == old["invocation"] {
					return errors.New("coalesced delivery did not use a new installed worker")
				}
			}
		}
		return nil
	}
	children := &preparedOwnerChildren{operation: f.cli, inits: map[string]preparedOwnerInitChild{"default": {loaded: f.loaded, runtime: f.runtime, start: original, sources: sources, publicationConsumer: &f.consumer}}}
	started := time.Now()
	f.check(children.runStart(ctx, "default", os.Stderr))
	elapsed := time.Since(started)
	i := f.instanceInfo()
	if !posted || prior.ID == "" || i.LocalConfig[ownerpublication.HandoffPrefix+previewroute.DeviceName] != "" || i.LocalConfig[ownerpublication.Prefix+previewroute.DeviceName] == "" || observer.unknown || len(observer.started) != len(observer.terminal) {
		f.t.Fatal("original callback/ACK/own-sidecar CAS tail did not complete", observer)
	}
	// Explicit starts of the same unit expedite systemd's automatic restart, so there is no minimum delay.
	if coalesced && (elapsed > 75*time.Second || !sawRetry) {
		f.t.Fatal("coalesced retry did not complete within delivery budget; explicit same-unit starts may expedite restart", elapsed, sawRetry)
	}
	f.t.Logf("original Go callback completed with real owner OFD held: elapsed=%s checked_fixture_observer=true rust_admission=false", elapsed)
	f.stopWorker()
	if queuedStop {
		release()
		release = nil
		select {
		case e := <-stopDone:
			stopDrained = true
			f.check(e)
		case <-time.After(35 * time.Second):
			f.t.Fatal("serialized ordinary stop did not finish")
		}
		i = f.instanceInfo()
		if !strings.EqualFold(i.Status, "stopped") || i.LocalConfig["user.subyard.desired_power"] != "stopped" {
			f.t.Fatal("ordinary stop did not commit")
		}
		unlockOwner := f.owner()
		defer unlockOwner()
		unlock := f.network()
		defer unlock()
		if e := f.client.ExchangePublicationHandoff(f.ctx, prior, f.contracts, "", prior.Encode(), nil); e == nil {
			f.t.Fatal("delayed old delivery admitted after serialized stop")
		}
		// Metadata retirement is fixture-owned under both real barriers. It is
		// not a claim to have exercised whole-installation Rust retirement.
		f.check(f.client.SetInstanceConfig(f.ctx, f.project, f.instance, map[string]string{previewroute.Key: "", ownerpublication.Prefix + previewroute.DeviceName: ""}))
		if e := f.client.ExchangePublicationHandoff(f.ctx, prior, f.contracts, "", prior.Encode(), nil); e == nil {
			f.t.Fatal("delayed old delivery admitted after receipt retirement")
		}
		f.setAddress(true)
		f.passiveTurn()
		i = f.instanceInfo()
		if !strings.EqualFold(i.Status, "stopped") || i.LocalConfig[previewroute.Key] != "" || i.LocalConfig[ownerpublication.Prefix+previewroute.DeviceName] != "" || len(i.LocalDevices[previewroute.DeviceName]) != 0 {
			f.t.Fatal("late passive delivery resurrected retired receipt")
		}
	}
}

func (f *publicationPhysical) identity() string {
	return string(f.command("incus", "exec", f.instance, "--project", f.project, "--", "sh", "-eu", "-c", "cat /proc/sys/kernel/random/boot_id; systemctl show \"$1\" -p MainPID --value", "sh", f.preview))
}
func (f *publicationPhysical) restore() {
	before := f.identity()
	f.setAddress(true)
	f.check(f.runtime.StartPublicationConsumer(f.ctx, f.consumer))
	f.poll(40*time.Second, func() bool {
		i := f.instanceInfo()
		return i.LocalConfig[ownerpublication.Prefix+previewroute.DeviceName] == "" && maps.Equal(i.LocalDevices[previewroute.DeviceName], previewroute.Device(f.address, f.port)) && maps.Equal(i.Devices[previewroute.DeviceName], previewroute.Device(f.address, f.port))
	})
	if f.identity() != before {
		f.t.Fatal("passive restore restarted container or local preview")
	}
	body := f.command("curl", "-q", "--noproxy", "*", "--max-time", "5", "--fail", "--silent", "http://"+f.address+":"+f.port+"/")
	if strings.TrimSpace(string(body)) != f.marker {
		f.t.Fatal("exact restored preview is not alive")
	}
	f.stopWorker()
}
func (f *publicationPhysical) temporaryStop() {
	f.resetReceipt()
	release := f.owner()
	unlock := f.network()
	f.check(f.client.SetInstancePower(f.ctx, f.project, f.instance, "stop", false))
	unlock()
	release()
	f.setAddress(true)
	f.passiveTurn()
	i := f.instanceInfo()
	if !strings.EqualFold(i.Status, "stopped") || i.LocalConfig["user.subyard.desired_power"] != "running" || i.LocalConfig[ownerpublication.Prefix+previewroute.DeviceName] == "" || len(i.LocalDevices[previewroute.DeviceName]) != 0 {
		f.t.Fatal("passive consumer changed a temporary stop")
	}
	f.stopWorker()
}

// These PID1 events survive unit garbage collection; cached execution properties do
// not. IDs and fields follow systemd v257 sd-messages.h, core/job.c and core/unit.c.
const (
	publicationStarting      = "7d4958e842da4a758f6c1cdc7b36dcc5"
	publicationStarted       = "39f53479d3a045ac8e11786248231fbf"
	publicationSuccess       = "7ad2d189f7e94e70a38c781354912448"
	publicationRestart       = "5eb03494b6584870a536b337290809b3"
	publicationJournalFields = "_PID,UNIT,INVOCATION_ID,MESSAGE_ID,JOB_TYPE,JOB_RESULT,UNIT_RESULT,EXIT_CODE,EXIT_STATUS"
	publicationJournalBytes  = 512 * 1024
)

type publicationJournalBuffer struct{ bytes.Buffer }

func (b *publicationJournalBuffer) Write(p []byte) (int, error) {
	if len(p) > publicationJournalBytes-b.Len() {
		return 0, fmt.Errorf("journal byte limit")
	}
	return b.Buffer.Write(p)
}

// Refuse duplicate keys and non-scalar fields, including journalctl's array
// representation for repeated or binary values. Never read MESSAGE or process args.
func publicationJournalRecords(data []byte, readErr error, boot string) ([]map[string]string, error) {
	if readErr != nil {
		return nil, fmt.Errorf("journal read: %w", readErr)
	}
	if len(data) > publicationJournalBytes {
		return nil, fmt.Errorf("journal byte limit")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	var records []map[string]string
	for {
		token, err := d.Token()
		if err == io.EOF {
			return records, nil
		}
		if err != nil || token != json.Delim('{') {
			return nil, fmt.Errorf("invalid journal object")
		}
		r := map[string]string{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return nil, fmt.Errorf("invalid journal field")
			}
			k, ok := key.(string)
			if !ok {
				return nil, fmt.Errorf("invalid journal key")
			}
			if _, exists := r[k]; exists {
				return nil, fmt.Errorf("duplicate journal field")
			}
			raw, err := d.Token()
			value, scalar := raw.(string)
			if err != nil || !scalar || len(value) > 4096 {
				return nil, fmt.Errorf("non-scalar or oversized journal field")
			}
			r[k] = value
		}
		if token, err = d.Token(); err != nil || token != json.Delim('}') {
			return nil, fmt.Errorf("invalid journal end")
		}
		if r["__CURSOR"] == "" || strings.ContainsAny(r["__CURSOR"], " \t\r\n") || r["_BOOT_ID"] != boot {
			return nil, fmt.Errorf("journal cursor/boot mismatch")
		}
		for _, key := range []string{"__MONOTONIC_TIMESTAMP", "__REALTIME_TIMESTAMP"} {
			if _, err := strconv.ParseUint(r[key], 10, 64); err != nil {
				return nil, fmt.Errorf("invalid journal timestamp")
			}
		}
		records = append(records, r)
		if len(records) > 256 {
			return nil, fmt.Errorf("journal record limit")
		}
	}
}
func publicationJournalID(value string) bool {
	return len(value) == 32 && strings.Trim(value, "0123456789abcdef") == "" && value != strings.Repeat("0", 32)
}

// A success record means successful deactivation, not numeric exit0. With the
// sealed RestartForceExitStatus=75 policy, a retry remains activating; a stop job
// could cancel it, so passive completion refuses stops, failures and new starts.
func publicationJournalCompletion(records []map[string]string, before string, quiet bool) (bool, error) {
	invocation, starting, started, complete := "", false, false, false
	for _, r := range records {
		id := r["MESSAGE_ID"]
		activation := id == publicationStarting || id == publicationStarted || id == publicationRestart
		if quiet {
			if activation || r["JOB_TYPE"] == "start" || strings.Contains(r["JOB_TYPE"], "restart") {
				return false, fmt.Errorf("new activation after teardown")
			}
			continue
		}
		if r["JOB_TYPE"] != "" && r["JOB_TYPE"] != "start" || r["JOB_RESULT"] != "" && r["JOB_RESULT"] != "done" || r["UNIT_RESULT"] != "" && r["UNIT_RESULT"] != "success" || r["EXIT_STATUS"] != "" && r["EXIT_STATUS"] != "0" || r["EXIT_CODE"] != "" && r["EXIT_CODE"] != "exited" {
			return false, fmt.Errorf("conflicting passive job/result")
		}
		switch id {
		case publicationStarting:
			// Job-start messages need not carry the newly assigned invocation.
			if starting || started || complete {
				return false, fmt.Errorf("another activation")
			}
			starting = true
		case publicationStarted, publicationSuccess:
			v := r["INVOCATION_ID"]
			if !publicationJournalID(v) || v == before || invocation != "" && invocation != v {
				return false, fmt.Errorf("missing or conflicting fresh invocation")
			}
			invocation = v
			if id == publicationStarted {
				if started || complete || r["JOB_TYPE"] != "start" || r["JOB_RESULT"] != "done" {
					return false, fmt.Errorf("invalid Started event")
				}
				started = true
			}
			if id == publicationSuccess {
				if !started || complete {
					return false, fmt.Errorf("success without unique Started")
				}
				complete = true
			}
		case publicationRestart, "de5b426a63be47a7b6ac3eaac82e2f6f", "9d1aaa27d60140bd96365438aad20286", "be02cf6855d2428ba40df7e9d022f03d", "d9b373ed55a64feb8242e02dbe79a49c", "0e4284a0caca4bfc81c0bb6786972673":
			return false, fmt.Errorf("passive restart/stop/failure/skip event")
		}
	}
	return complete, nil
}

type publicationJournal struct {
	boot    string
	anchor  map[string]string
	records []map[string]string
}

func (f *publicationPhysical) journalCommand(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sudo", append([]string{"-n", "journalctl"}, args...)...)
	var output publicationJournalBuffer
	cmd.Stdout, cmd.Stderr = &output, io.Discard
	err := cmd.Run()
	return output.Bytes(), err
}
func (f *publicationPhysical) journalRecords(j *publicationJournal, args ...string) []map[string]string {
	argv := []string{"--boot=" + j.boot, "--no-pager", "--quiet", "--output=json", "--output-fields=" + publicationJournalFields}
	data, err := f.journalCommand(append(argv, args...)...)
	records, err := publicationJournalRecords(data, err, j.boot)
	f.check(err)
	return records
}
func (f *publicationPhysical) journalStart() *publicationJournal {
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	f.check(err)
	j := &publicationJournal{boot: strings.ReplaceAll(strings.TrimSpace(string(boot)), "-", "")}
	if !publicationJournalID(j.boot) {
		f.t.Fatal("invalid current boot ID")
	}
	_, err = f.journalCommand("--sync")
	f.check(err)
	records := f.journalRecords(j, "--lines=1")
	if len(records) != 1 {
		f.t.Fatal("missing initial journal cursor")
	}
	j.anchor = records[0]
	f.journalRead(j) // Check an exact retained cursor before the guarded operation.
	return j
}
func (f *publicationPhysical) journalRead(j *publicationJournal) {
	anchor := f.journalRecords(j, "--cursor="+j.anchor["__CURSOR"], "--lines=+1")
	if len(anchor) != 1 || !maps.Equal(anchor[0], j.anchor) {
		f.t.Fatal("journal anchor missing or changed")
	}
	records := f.journalRecords(j, "--after-cursor="+j.anchor["__CURSOR"], "--lines=+257", "_PID=1", "UNIT="+ownerpublication.HandoffService)
	f.check(publicationJournalSequence(j.anchor, j.records, records))
	j.records = records
}
func publicationJournalSequence(anchor map[string]string, previous, records []map[string]string) error {
	last, _ := strconv.ParseUint(anchor["__MONOTONIC_TIMESTAMP"], 10, 64)
	seen := map[string]bool{anchor["__CURSOR"]: true}
	if len(records) < len(previous) {
		return fmt.Errorf("journal history shrank")
	}
	for index, r := range records {
		stamp, err := strconv.ParseUint(r["__MONOTONIC_TIMESTAMP"], 10, 64)
		if err != nil || stamp < last || seen[r["__CURSOR"]] || r["_PID"] != "1" || r["_BOOT_ID"] != anchor["_BOOT_ID"] || r["UNIT"] != ownerpublication.HandoffService {
			return fmt.Errorf("untrusted or unordered PID1 journal event")
		}
		if index < len(previous) && !maps.Equal(r, previous[index]) {
			return fmt.Errorf("journal history changed")
		}
		seen[r["__CURSOR"]], last = true, stamp
	}
	return nil
}
func publicationPassiveManager(m map[string]string, before string, records []map[string]string) (bool, error) {
	// A no-block start can still expose the prior failed invocation while its
	// fresh job is queued. Apply exit-status refusal only to a fresh execution.
	bound := publicationJournalID(m["InvocationID"]) && m["InvocationID"] != before
	for _, r := range records {
		if r["MESSAGE_ID"] == publicationStarted {
			bound = true
		}
	}
	if !bound {
		return false, nil
	}
	if m["ExecMainStatus"] != "0" || m["ExecMainCode"] != "0" && m["ExecMainCode"] != "1" {
		return false, fmt.Errorf("observed unsuccessful passive execution")
	}
	return m["MainPID"] == "0" && m["ActiveState"] == "inactive" && m["SubState"] == "dead" && (m["Job"] == "" || m["Job"] == "0"), nil
}
func (f *publicationPhysical) passiveTurn() {
	// Every command, including cursor setup and the final flush, consumes the
	// original ten seconds. No second StartPublicationConsumer is permitted.
	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	original := f.ctx
	f.ctx = ctx
	defer func() { cancel(); f.ctx = original }()
	var initial, last map[string]string
	var j *publicationJournal
	defer func() { f.t.Logf("passive witness initial=%v last=%v journal=%+v", initial, last, j) }()
	initial = f.manager()
	j = f.journalStart()
	f.check(f.runtime.StartPublicationConsumer(ctx, f.consumer))
	for {
		f.journalRead(j)
		complete, err := publicationJournalCompletion(j.records, initial["InvocationID"], false)
		f.check(err)
		last = f.manager()
		idle, err := publicationPassiveManager(last, initial["InvocationID"], j.records)
		f.check(err)
		if complete && idle {
			if f.workerGroup == "" {
				f.t.Fatal("missing captured worker cgroup")
			}
			f.command("sudo", "-n", "sh", "-eu", "-c", `test ! -e /sys/fs/cgroup"$1"/cgroup.procs || test -z "$(cat /sys/fs/cgroup"$1"/cgroup.procs)"`, "sh", f.workerGroup)
			_, err := f.journalCommand("--sync")
			f.check(err)
			f.journalRead(j)
			complete, err = publicationJournalCompletion(j.records, initial["InvocationID"], false)
			f.check(err)
			last = f.manager()
			idle, err = publicationPassiveManager(last, initial["InvocationID"], j.records)
			f.check(err)
			if complete && idle && ctx.Err() == nil {
				return
			}
		}
		select {
		case <-ctx.Done():
			f.t.Fatal("passive completion deadline: ", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Signal only the installed PID/starttime/executable/cgroup/InvocationID and
// policy-lock descriptor already observed at the real lock boundary.
func (f *publicationPhysical) signalWorker(proof map[string]string, action string) error {
	ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancel()
	m := f.manager()
	if action == "CONT" {
		if _, err := os.Stat("/proc/" + proof["pid"]); os.IsNotExist(err) {
			return nil
		}
	}
	if m["MainPID"] != proof["pid"] || m["InvocationID"] != proof["invocation"] || m["ControlGroup"] != proof["cgroup"] {
		return fmt.Errorf("worker unit identity changed: %v", m)
	}
	const script = `import os,sys,signal
p,start,cg,fd,lock,action=sys.argv[1:]
stat=open('/proc/'+p+'/stat').read().rsplit(')',1)[1].split()
assert stat[19]==start
assert cg in [line.rsplit(':',1)[-1] for line in open('/proc/'+p+'/cgroup').read().splitlines()]
e=os.stat('/usr/local/libexec/subyard/yard-boot-reconcile');x=os.stat('/proc/'+p+'/exe')
assert (e.st_dev,e.st_ino)==(x.st_dev,x.st_ino)
a=os.stat('/proc/'+p+'/fd/'+fd);b=os.stat('/run/lock/subyard-network/policy.lock')
assert str(a.st_dev)+':'+str(a.st_ino)==lock
assert (a.st_dev,a.st_ino)==(b.st_dev,b.st_ino)
if action=='STOPPED': assert stat[0] in ('T','t')
else: os.kill(int(p),signal.SIGSTOP if action=='STOP' else signal.SIGCONT)
`
	b, err := exec.CommandContext(ctx, "sudo", "-n", "python3", "-c", script, proof["pid"], proof["start"], proof["cgroup"], proof["fd"], proof["lock"], action).CombinedOutput()
	if err != nil {
		return fmt.Errorf("worker %s: %w: %s", action, err, b)
	}
	return nil
}

func (f *publicationPhysical) teardown() {
	f.resetReceipt()
	foreign, err := f.client.Instance(f.ctx, f.foreignProject, f.foreign)
	f.check(err)
	if foreign.LocalConfig["user.subyard.e2e"] != f.marker || foreign.LocalConfig["user.subyard.managed"] == "true" {
		f.t.Fatal("foreign control must be marked but unowned")
	}
	foreignKey := ownerpublication.HandoffPrefix + "foreign-proof"
	foreignValue := "foreign-" + f.marker
	f.check(f.client.SetInstanceConfig(f.ctx, f.foreignProject, f.foreign, map[string]string{foreignKey: foreignValue}))
	foreign, err = f.client.Instance(f.ctx, f.foreignProject, f.foreign)
	f.check(err)
	unlock := f.network()
	defer func() {
		if unlock != nil {
			unlock()
		}
	}()
	f.check(f.runtime.StartPublicationConsumer(f.ctx, f.consumer))
	worker := f.workerWaiting()
	// Freeze this exact installed worker while the real policy lock is held.
	// PID1 must later stop/drain it; the worker cannot win the unlock race.
	teardownDone := false
	defer func() {
		if !teardownDone {
			originalCtx := f.ctx
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			f.ctx = cleanupCtx
			defer func() { cancel(); f.ctx = originalCtx }()
			if unlock != nil {
				unlock()
				unlock = nil
			}
			if err := f.signalWorker(worker, "CONT"); err != nil {
				f.t.Errorf("failure cleanup worker identity/resume: %v", err)
			}
			f.stopWorker()
		}
	}()
	f.check(f.signalWorker(worker, "STOP"))
	f.poll(3*time.Second, func() bool { return f.signalWorker(worker, "STOPPED") == nil })
	var journal *publicationJournal
	var initialManager, lastManager map[string]string
	defer func() {
		f.t.Logf("teardown witness initial=%v last=%v journal=%+v", initialManager, lastManager, journal)
	}()
	initialManager = f.manager()
	journal = f.journalStart()
	cmd := exec.CommandContext(f.ctx, filepath.Join(f.root, "bin/yard-engine"), "teardown", "--yes")
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	f.check(cmd.Start())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	joined := false
	defer func() {
		if !joined {
			_ = cmd.Process.Kill()
			<-done
		}
	}()
	ownerPath := uninstall.OwnerLockPath(f.loaded.Context.Paths.DataHome)
	phaseDeadline, _ := f.ctx.Deadline()
	f.poll(time.Until(phaseDeadline), func() bool {
		select {
		case err := <-done:
			joined = true
			f.t.Fatalf("teardown exited before the two-lock witness: %v", err)
		default:
		}
		// WithRemovals takes the actual policy lock before stopping the service.
		// Both descriptors bind this ordinary teardown to the two real barriers.
		entries, _ := os.ReadDir(fmt.Sprintf("/proc/%d/fd", cmd.Process.Pid))
		found := map[string]bool{}
		for _, path := range []string{ownerPath, "/run/lock/subyard-network/policy.lock"} {
			expected, x := os.Stat(path)
			for _, entry := range entries {
				actual, e := os.Stat(fmt.Sprintf("/proc/%d/fd/%s", cmd.Process.Pid, entry.Name()))
				if e == nil && x == nil && os.SameFile(actual, expected) {
					found[path] = true
				}
			}
		}
		return found[ownerPath] && found["/run/lock/subyard-network/policy.lock"]
	})
	f.check(f.signalWorker(worker, "STOPPED"))
	unlock()
	unlock = nil
	err = <-done
	joined = true
	f.check(err)
	teardownDone = true
	// One original budget covers observations and final synchronization. Reserve
	// its final second after the 30-second retry boundary; commands cannot extend it.
	deadline := time.Now().Add(31 * time.Second)
	windowCtx, cancel := context.WithDeadline(f.ctx, deadline)
	originalCtx := f.ctx
	f.ctx = windowCtx
	defer func() { cancel(); f.ctx = originalCtx }()
	// These are the three captured runtime resources. Custody records are
	// restored by the wrapper and are not a normal teardown deletion contract.
	f.command("sudo", "-n", "sh", "-eu", "-c", publicationPhysicalRuntimeArtifactsAbsent, "sh", "/usr/local/libexec/subyard/yard-boot-reconcile", "/etc/systemd/system/subyard-power-reconcile.service", "/etc/systemd/system/"+ownerpublication.HandoffService)
	finished := f.manager()
	if finished["MainPID"] != "0" || (finished["Job"] != "" && finished["Job"] != "0") || (finished["ActiveState"] != "inactive" && finished["ActiveState"] != "failed") {
		f.t.Fatal("normal teardown did not drain and cancel passive unit", finished)
	}
	f.command("sudo", "-n", "sh", "-eu", "-c", "test ! -d /proc/\"$1\"; test ! -e /sys/fs/cgroup\"$2\"/cgroup.procs || test -z \"$(cat /sys/fs/cgroup\"$2\"/cgroup.procs)\"", "sh", worker["pid"], worker["cgroup"])
	for {
		current, e := f.client.Instance(f.ctx, f.foreignProject, f.foreign)
		f.check(e)
		if !maps.Equal(current.LocalConfig, foreign.LocalConfig) || !equalPhysicalDevices(current.LocalDevices, foreign.LocalDevices) {
			f.t.Fatal("teardown/passive retry mutated foreign sidecar or instance")
		}
		m := f.manager()
		lastManager = m
		f.journalRead(journal)
		_, err := publicationJournalCompletion(journal.records, "", true)
		f.check(err)
		if m["MainPID"] != "0" || (m["Job"] != "" && m["Job"] != "0") {
			f.t.Fatal("passive consumer restarted after teardown", m)
		}
		if time.Now().After(deadline.Add(-time.Second)) {
			break
		}
		select {
		case <-time.After(250 * time.Millisecond):
		case <-f.ctx.Done():
			f.t.Fatal(f.ctx.Err())
		}
	}
	_, err = f.journalCommand("--sync")
	f.check(err)
	f.journalRead(journal)
	_, err = publicationJournalCompletion(journal.records, "", true)
	f.check(err)
	lastManager = f.manager()
	f.check(windowCtx.Err())
	if lastManager["MainPID"] != "0" || (lastManager["Job"] != "" && lastManager["Job"] != "0") {
		f.t.Fatal("passive consumer restarted after teardown flush", lastManager)
	}
}
func equalPhysicalDevices(a, b map[string]map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if !maps.Equal(value, b[key]) {
			return false
		}
	}
	return true
}
