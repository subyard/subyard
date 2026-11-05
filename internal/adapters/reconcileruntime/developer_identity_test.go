package reconcileruntime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

// Execute the actual guest driver on protected disposable files. Account tools
// are never invoked by this observe/begin transport contract.
type identityNativeExecutor struct {
	root      string
	cut       string
	cutDone   bool
	checks    int
	checkFail bool
}

func (fixture *identityNativeExecutor) Exec(ctx context.Context, _, _ string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	if len(request.Command) == 1 && request.Command[0] == "identity-package-check" {
		fixture.checks++
		command := exec.CommandContext(ctx, "python3", "-B", "-c", `import os,sys; assert os.geteuid()==int(sys.argv[1]) and os.getegid()==int(sys.argv[2]); assert os.environ['HOME']=='/home/next'; assert os.environ['USER']=='next'`, strconv.Itoa(int(request.User)), strconv.Itoa(int(request.Group)))
		command.Env = os.Environ()
		for name, value := range request.Environment {
			command.Env = append(command.Env, name+"="+value)
		}
		if os.Getuid() == 0 {
			command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: request.User, Gid: request.Group}}
		}
		output, err := command.CombinedOutput()
		if fixture.checkFail {
			return ports.InstanceExecResult{ExitCode: 1}, &ports.GuestExitError{Code: 1}
		}
		return ports.InstanceExecResult{Stdout: output}, err
	}
	cut := ""
	if ((request.Command[4] == "account" && fixture.cut != "listener-restored") || (request.Command[4] == "receipts" && fixture.cut == "listener-restored")) && !fixture.cutDone {
		cut = fixture.cut
		fixture.cutDone = true
	}
	prefix := fmt.Sprintf(`from pathlib import Path
from contextlib import contextmanager
_fixture_root=Path(%q)
@contextmanager
def _fixture_password_lock():
    yield

def _fixture_listener():
    return json.loads((_fixture_root/'listener.json').read_text())

def _fixture_account_tool(arguments):
    if arguments[0]=='systemctl':
        state=_fixture_listener();state[arguments[2]]=arguments[1]=='start'
        (_fixture_root/'listener.json').write_text(json.dumps(state))
        return
    assert not any(_fixture_listener().values())
    assert not json.loads((_fixture_root/'etc/sample-owner-state.json').read_text())['active']
    with (_fixture_root/'account-tools.log').open('a') as stream:stream.write(arguments[0]+'\n')
    if arguments[0]=='usermod':
        old,new,home=arguments[-1],arguments[2],arguments[4]
        path=_fixture_root/'etc/passwd'
        rows=[line.split(':') for line in path.read_text().splitlines()]
        for row in rows:
            if row[0]==old: row[0],row[5]=new,home
        path.write_text(''.join(':'.join(row)+'\n' for row in rows))
        group=_fixture_root/'etc/group'
        rows=[line.split(':') for line in group.read_text().splitlines()]
        for row in rows:
            row[3]=','.join(new if value==old else value for value in row[3].split(','))
        group.write_text(''.join(':'.join(row)+'\n' for row in rows))
    elif arguments[0]=='groupmod':
        path=_fixture_root/'etc/group'
        rows=[line.split(':') for line in path.read_text().splitlines()]
        for row in rows:
            if row[0]==arguments[-1]:row[0]=arguments[2]
        path.write_text(''.join(':'.join(row)+'\n' for row in rows))
    else: raise ValueError('unexpected fixture account tool')

def _fixture_checkpoint(phase):
    if phase==%q: raise InterruptedError('fixture interruption')
`, fixture.root, cut)
	program := strings.Replace(request.Command[3], "transition = Transition()", fmt.Sprintf("transition = Transition(root=Path(%q), owner=%d, run=_fixture_account_tool, checkpoint=_fixture_checkpoint, subid_lock=_fixture_password_lock, listener=_fixture_listener)", fixture.root, os.Getuid()), 1)
	program = prefix + program
	program = strings.Replace(program, "if os.geteuid() != 0:", "if False:", 1)
	command := exec.CommandContext(ctx, "python3", "-B", "-c", program, request.Command[4])
	command.Stdin = strings.NewReader(string(request.Stdin))
	output, err := command.CombinedOutput()
	result := ports.InstanceExecResult{Stdout: output}
	var failure *exec.ExitError
	if errors.As(err, &failure) {
		result.ExitCode = failure.ExitCode()
	}
	return result, err
}

func TestDeveloperIdentityDeferredProfileAndHookFactsPassOperationPolicy(t *testing.T) {
	runtime := Runtime{Profiles: []profile.Definition{{Name: "synthetic", Runtime: &profile.RuntimeHook{ActivationID: "synthetic-runtime"}}}}
	plan, err := runtime.DeferredIdentityRuntimes()
	if err != nil {
		t.Fatal(err)
	}
	hooks := &ProjectHookPlan{conditional: true, allowedHooks: []string{"/usr/local/libexec/subyard/synthetic"}, metadata: &projectMetadataPlan{Targets: []projectMetadataTarget{{Root: "/srv/workspaces/project"}}}}
	steps := append(plan.OperationSteps("project/yard"), hooks.OperationStep("project/yard"), *hooks.MetadataOperationStep("project/yard"))
	if len(steps) != 3 {
		t.Fatal("conditional profile or project scope omitted")
	}
	if err := domain.ValidateOperationSteps(steps); err != nil {
		t.Fatal(err)
	}
}

func TestDeveloperIdentityNativePlanAndBeginTransport(t *testing.T) {
	for _, scenario := range []string{"owner-refusals", "begin", "changed-source", "foreign-home", "advance", "package-receipt", "package-hash", "package-pending", "package-source", "package-check-fails", "host-boot-cut", "cut-reboot-home", "cut-home", "cut-account", "cut-subuid", "cut-listener-restored", "owner-failure"} {
		t.Run(scenario, func(t *testing.T) {
			base := testkit.TempDir(t)
			repository, guest := filepath.Join(base, "repository"), filepath.Join(base, "guest")
			for _, path := range []string{filepath.Join(repository, "config/identity"), filepath.Join(guest, "etc"), filepath.Join(guest, "proc/self"), filepath.Join(guest, "proc/sys/kernel/random"), filepath.Join(guest, "home/dev"), filepath.Join(guest, "var/lib/subyard/integrations")} {
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			program, err := os.ReadFile("../../../config/identity/transition.py")
			if err != nil {
				t.Fatal(err)
			}
			module, err := os.ReadFile("../../../config/identity/subids.py")
			if err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(repository, "config/identity/subids.py"), module, 0o600)
			accounts, err := os.ReadFile("../../../config/identity/accounts.py")
			if err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(repository, "config/identity/accounts.py"), accounts, 0o600)
			authority, err := os.ReadFile("../../../config/identity/authority.py")
			if err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(repository, "config/identity/authority.py"), authority, 0o600)
			sourcePath := filepath.Join(repository, developerIdentitySource)
			testkit.WriteFile(t, sourcePath, program, 0o600)
			uid, gid := os.Getuid(), os.Getgid()
			if uid == 0 {
				uid = 12345
			}
			if gid == 0 {
				gid = 12345
			}
			if err := os.Chown(filepath.Join(guest, "home/dev"), uid, gid); err != nil {
				t.Fatal(err)
			}
			write := func(name, content string, mode os.FileMode) {
				testkit.WriteFile(t, filepath.Join(guest, name), []byte(content), mode)
			}
			write("etc/passwd", fmt.Sprintf("dev:x:%d:%d:private:/home/dev:/bin/bash\n", uid, gid), 0o644)
			write("etc/group", fmt.Sprintf("dev:x:%d:\n", gid), 0o644)
			write("etc/machine-id", strings.Repeat("a", 32), 0o644)
			write("proc/self/mountinfo", "", 0o644)
			write("proc/sys/kernel/random/boot_id", "11111111-1111-1111-1111-111111111111", 0444)
			write("listener.json", `{"ssh.service":true,"ssh.socket":false}`, 0o600)
			write("etc/subuid", "dev:100000:65536\n", 0o644)
			write("etc/subgid", "dev:100000:65536\n", 0o644)
			write("var/lib/subyard/integrations/inventory.json", fmt.Sprintf(`{"schema":1,"entries":[],"applied_settings":{"DEV_USER":"dev","DEV_UID":"%d","DEV_SUDO":"1","AGENT_claude_COMMAND":"old-command"}}`, uid), 0o600)
			write("home/dev/data", "preserved", 0o600)
			runtime := Runtime{RepositoryRoot: repository, Executor: &identityNativeExecutor{root: guest}, Profiles: []profile.Definition{}, Environment: []string{"DEV_USER=next", fmt.Sprintf("DEV_UID=%d", uid)}, Yard: domain.Context{YardName: "sample", IncusProject: "sample-project", YardInstanceName: "sample-yard", DevUser: "next", DevUID: uid}}
			var wantedPackage integrationArtifact
			if strings.HasPrefix(scenario, "package-") {
				if err := os.Chown(filepath.Join(guest, "home/dev/data"), -1, uid); err != nil {
					t.Fatal(err)
				}
				runtime.Integrations = []profile.IntegrationDefinition{}
				runtime.Environment = append(runtime.Environment, "CODING_TOOL_INTEGRATIONS=sample", "AGENT_sample_CHECK=identity-package-check")
				original := runtime
				original.Yard.DevUser = "dev"
				before, err := original.integrationPackageArtifacts("inspect", nil)
				if err != nil || len(before) != 1 {
					t.Fatalf("package baseline: %v", err)
				}
				after, err := runtime.integrationPackageArtifacts("inspect", nil)
				if err != nil || len(after) != 1 || before[0].Digest == after[0].Digest {
					t.Fatalf("identity package source: %v", err)
				}
				wantedPackage = after[0]
				entries := []integrationArtifact{before[0], {ID: "sample", Kind: "file", Path: "/home/dev/data", Digest: fmt.Sprintf("%x", sha256.Sum256([]byte("preserved"))), Mode: 0600}}
				payload, _ := json.Marshal(map[string]any{"schema": 1, "entries": entries, "applied_settings": map[string]string{"DEV_USER": "dev", "DEV_UID": strconv.Itoa(uid), "DEV_SUDO": "1", "AGENT_claude_COMMAND": "old-command"}})
				write("var/lib/subyard/integrations/inventory.json", string(payload), 0600)
				if scenario == "package-hash" || scenario == "package-pending" || scenario == "package-source" {
					var inventory map[string]any
					json.Unmarshal(payload, &inventory)
					entry := inventory["entries"].([]any)[0].(map[string]any)
					if scenario == "package-pending" {
						entry["pending_digest"] = entry["digest"]
					} else if scenario == "package-hash" {
						entry["digest"] = wantedPackage.Digest
					} else {
						entry["digest"] = strings.Repeat("e", 64)
					}
					data, _ := json.Marshal(inventory)
					write("var/lib/subyard/integrations/inventory.json", string(data), 0600)
				}

			}
			profileRoot := filepath.Join(repository, "config/profiles/sample")
			if err := os.MkdirAll(profileRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			ownerFile := filepath.Join(guest, "etc/sample-owner")
			ownerState := filepath.Join(guest, "etc/sample-owner-state.json")
			write("etc/sample-owner", "user=dev\nhome=/home/dev\n", 0o644)
			write("etc/sample-owner-state.json", `{"active":true}`, 0o600)
			handler := fmt.Sprintf(`#!/usr/bin/python3
import json,os,sys
from pathlib import Path
sys.path.insert(0,%q)
import authority
root=Path(%q)
path=Path(%q)
state=Path(%q)
old=b'user=dev\nhome=/home/dev\n'
new=b'user=next\nhome=/home/next\n'
recipe=json.loads(%q)
action=sys.argv[1]
source=sys.argv[-1]
assert os.environ['DEV_USER']=='dev'
journal=root/'var/lib/subyard/developer-identity/transition.json'
if journal.exists():
    saved,recipe=authority.load(source,'sample',path=journal,machine=root/'etc/machine-id',owner=os.getuid(),root=root)
    phase=saved['phase']
else:
    assert action=='identity-observe' and path.read_bytes()==old
    recipe['active']=json.loads(state.read_text())['active']
    phase=None
assert path.read_bytes() in ([old,new] if phase=='account-ready' else [old])
if action=='identity-observe':
    print(json.dumps(recipe))
elif phase in ('prepared','quiesced','home','account'):
    assert not any(json.loads((root/'listener.json').read_text()).values())
    state.write_text(json.dumps({'active':False}))
elif phase=='account-ready':
    assert json.loads((root/'listener.json').read_text())=={'ssh.service':True,'ssh.socket':False}
    assert json.loads((root/'var/lib/subyard/integrations/inventory.json').read_text())['applied_settings']['DEV_USER']=='dev'
    if (root/'fail-owner').exists():
        (root/'fail-owner').unlink()
        raise ValueError('injected owner restoration failure')
    path.write_bytes(new)
    state.write_text(json.dumps({'active':recipe['active']}))
else: raise ValueError('unexpected owner phase')
`, filepath.Join(repository, "config/identity"), guest, ownerFile, ownerState, fmt.Sprintf(`{"schema":1,"source":"%s","original":{"user":"dev","uid":%d,"gid":%d,"home":"/home/dev","group":"dev"},"target":{"user":"next","uid":%d,"gid":%d,"home":"/home/next","group":"next"},"marker":"synthetic"}`, strings.Repeat("f", 64), uid, gid, uid, gid))
			testkit.WriteFile(t, filepath.Join(profileRoot, "runtime.sh"), []byte(handler), 0o700)
			testkit.WriteFile(t, filepath.Join(profileRoot, "developer-identity.json"), []byte(`{"schema":1,"mode":"rebind"}`), 0o600)
			runtime.Profiles = []profile.Definition{{Name: "sample", Root: profileRoot, Runtime: &profile.RuntimeHook{ActivationID: "sample-runtime", Handler: "runtime.sh"}}}
			if scenario == "owner-refusals" {
				testkit.WriteFile(t, filepath.Join(profileRoot, "runtime.sh"), []byte("#!/bin/sh\nexit 64\n"), 0o700)
				second := runtime.Profiles[0]
				second.Name = "second"
				runtime.Profiles = append(runtime.Profiles, second)
			}
			source, err := runtime.developerIdentityInputs()
			if err != nil {
				t.Fatal(err)
			}
			plan, err := runtime.prepareDeveloperIdentity(context.Background(), source, AppliedIntegrationConfiguration{Identity: map[string]string{"DEV_USER": "dev", "DEV_UID": strconv.Itoa(uid)}}, developerIdentityObservation{})
			if scenario == "package-hash" || scenario == "package-pending" || scenario == "package-source" {
				if err == nil {
					t.Fatal("unapproved package receipt accepted")
				}
				return
			}
			if scenario == "owner-refusals" {
				if err == nil || !strings.Contains(err.Error(), "profile sample:") || !strings.Contains(err.Error(), "profile second:") {
					t.Fatalf("independent owner refusals not aggregated: %v", err)
				}
				if strings.Index(err.Error(), "profile sample:") > strings.Index(err.Error(), "profile second:") {
					t.Fatal("owner refusal order changed")
				}
				return
			}
			if err != nil || plan == nil || plan.Binding() == "" || len(plan.core.Owners) != 1 {
				t.Fatalf("prepare identity: %v", err)
			}
			journal := filepath.Join(guest, "var/lib/subyard/developer-identity/transition.json")
			if _, err := os.Lstat(journal); !os.IsNotExist(err) {
				t.Fatal("assessment published authority")
			}
			if scenario == "changed-source" {
				testkit.WriteFile(t, sourcePath, append(program, []byte("\n# changed\n")...), 0o600)
			}
			if scenario == "foreign-home" {
				if err := os.Mkdir(filepath.Join(guest, "home/next"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "advance" || scenario == "host-boot-cut" || strings.HasPrefix(scenario, "package-") || strings.HasPrefix(scenario, "cut-") || scenario == "owner-failure" {
				if scenario == "owner-failure" {
					write("fail-owner", "fixture", 0o600)
				}
				initialProof := plan.CompletionProof()
				if scenario == "host-boot-cut" {
					before := plan.Binding()
					if err := os.Remove(filepath.Join(guest, "proc/sys/kernel/random/boot_id")); err != nil {
						t.Fatal(err)
					}
					write("proc/sys/kernel/random/boot_id", "22222222-2222-2222-2222-222222222222", 0444)
					plan, err = runtime.PrepareDeveloperIdentityProof(context.Background(), plan.CompletionProof(), before)
					if err != nil || plan == nil || plan.Binding() != before {
						t.Fatalf("host-marker-only boot recovery: %v", err)
					}
				}

				executor := runtime.Executor.(*identityNativeExecutor)
				executor.cut = strings.TrimPrefix(scenario, "cut-")
				if scenario == "cut-reboot-home" {
					executor.cut = "home"
				}
				executor.checkFail = scenario == "package-check-fails"
				err := runtime.AdvanceDeveloperIdentity(context.Background(), plan)
				if scenario == "package-check-fails" {
					if err == nil {
						t.Fatal("failed target package check published identity")
					}
					data, _ := os.ReadFile(filepath.Join(guest, "var/lib/subyard/integrations/inventory.json"))
					var receipt struct {
						Applied map[string]string     `json:"applied_settings"`
						Entries []integrationArtifact `json:"entries"`
					}
					json.Unmarshal(data, &receipt)
					if receipt.Applied["DEV_USER"] != "dev" || receipt.Entries[0].Digest == wantedPackage.Digest {
						t.Fatal("failed check published package or applied identity")
					}
					executor.checkFail = false
					err = runtime.AdvanceDeveloperIdentity(context.Background(), plan)
				}

				if strings.HasPrefix(scenario, "cut-") || scenario == "owner-failure" {
					if err == nil {
						t.Fatal("interruption did not reach account boundary")
					}
					if scenario == "cut-reboot-home" {
						if err := os.Remove(filepath.Join(guest, "proc/sys/kernel/random/boot_id")); err != nil {
							t.Fatal(err)
						}
						write("proc/sys/kernel/random/boot_id", "22222222-2222-2222-2222-222222222222", 0444)
						write("listener.json", `{"ssh.service":true,"ssh.socket":false}`, 0600)
						write("etc/sample-owner-state.json", `{"active":true}`, 0600)
					}
					observed, readErr := runtime.identityNative(context.Background(), "pending", map[string]any{"source": source, "yard": plan.core.Yard, "new_user": "next", "uid": uid})
					if readErr != nil {
						t.Fatal(readErr)
					}
					plan, readErr = runtime.prepareDeveloperIdentity(context.Background(), source, AppliedIntegrationConfiguration{Identity: map[string]string{"DEV_USER": "dev", "DEV_UID": strconv.Itoa(uid)}}, observed)
					if readErr != nil {
						t.Fatal(readErr)
					}
					err = runtime.AdvanceDeveloperIdentity(context.Background(), plan)
				}
				if err != nil {
					t.Fatal(err)
				}
				if plan.CompletionProof() != initialProof {
					t.Fatal("completion proof changed across canonical journal recovery")
				}
				if payload, readErr := os.ReadFile(filepath.Join(guest, "account-tools.log")); readErr != nil || string(payload) != "usermod\ngroupmod\n" {
					t.Fatalf("account tools repeated across owner/listener recovery: %q %v", payload, readErr)
				}
				if plan.phase != "ready" {
					t.Fatal("account prerequisite did not complete")
				}
				if data, err := os.ReadFile(filepath.Join(guest, "home/next/data")); err != nil || string(data) != "preserved" {
					t.Fatal("account prerequisite lost data")
				}
				if data, err := os.ReadFile(ownerFile); err != nil || string(data) != "user=next\nhome=/home/next\n" {
					t.Fatal("owner was not rebound")
				}
				for _, name := range []string{"subuid", "subgid"} {
					data, err := os.ReadFile(filepath.Join(guest, "etc", name))
					if err != nil || string(data) != "next:100000:65536\n" {
						t.Fatal("subordinate allocation was not retained")
					}
				}
				payload, readErr := os.ReadFile(filepath.Join(guest, "var/lib/subyard/integrations/inventory.json"))
				if readErr != nil {
					t.Fatal(readErr)
				}
				var inventory struct {
					Applied map[string]string `json:"applied_settings"`
				}
				if err := json.Unmarshal(payload, &inventory); err != nil {
					t.Fatal(err)
				}
				if inventory.Applied["DEV_USER"] != "next" || inventory.Applied["DEV_SUDO"] != "1" || inventory.Applied["AGENT_claude_COMMAND"] != "old-command" {
					t.Fatal("identity commit published unrelated desired fields")
				}
				if strings.HasPrefix(scenario, "package-") {
					var receipt struct {
						Entries []integrationArtifact `json:"entries"`
					}
					if err := json.Unmarshal(payload, &receipt); err != nil {
						t.Fatal(err)
					}
					if executor.checks == 0 {
						t.Fatal("target account package check was omitted")
					}
					if len(receipt.Entries) != 2 || receipt.Entries[0] != wantedPackage || receipt.Entries[1].Path != "/home/next/data" {
						t.Fatalf("identity left source-derived package or file receipt stale: %#v", receipt.Entries)
					}
				}
				if strings.HasPrefix(scenario, "package-") {
					program := strings.NewReplacer("@STATE_ROOT@", filepath.Join(guest, "var/lib/subyard/integrations"), "@STATE_UID@", strconv.Itoa(os.Getuid())).Replace(integrationInventoryProgram)
					// Map only physical filesystem access into the disposable guest;
					// request and receipt paths remain canonical across the actual producer.
					at := strings.Index(program, "def main(")
					if at < 0 {
						t.Fatal("inventory entrypoint unavailable")
					}
					physical := fmt.Sprintf(`
_original_actual=actual
_original_metadata=actual_metadata
def _physical(entry):
    result=dict(entry)
    if result.get('path','').startswith('/home/'):result['path']=%q+result['path']
    return result
def actual(entry):return _original_actual(_physical(entry))
def actual_metadata(entry):return _original_metadata(_physical(entry))
`, guest)
					program = program[:at] + physical + program[at:]
					program = strings.Replace(program, "except Exception:\n    print('integration ownership", "except Exception:\n    import traceback; traceback.print_exc()\n    print('integration ownership", 1)
					f := configInventoryFixture{t: t, home: "/home/next", state: filepath.Join(guest, "var/lib/subyard/integrations"), program: program}
					entries := []integrationArtifact{wantedPackage, {ID: "sample", Kind: "file", Path: "/home/next/data", Digest: fmt.Sprintf("%x", sha256.Sum256([]byte("preserved"))), Mode: 0600}}
					data, _ := json.Marshal(entries)
					var values []map[string]any
					json.Unmarshal(data, &values)
					f.run("apply", values, false) // Later ordinary hook fails, leaving intent.
					before, _ := os.ReadFile(f.state + "/inventory.json")
					beforeInfo, _ := os.Stat(f.state + "/inventory.json")
					consumer := Runtime{Executor: f, appliedIntegrationInspection: true}
					observed, err := consumer.integrationInventory(context.Background(), "observe", entries, "")
					if err != nil || observed.Changed || len(observed.Fingerprint) != 64 {
						t.Fatalf("rename then failed hook prevents passive admission: %#v %v", observed, err)
					}
					after, _ := os.ReadFile(f.state + "/inventory.json")
					afterInfo, _ := os.Stat(f.state + "/inventory.json")
					if string(before) != string(after) || !sameInventoryMetadata(beforeInfo, afterInfo) {
						t.Fatal("passive admission mutated pending/applied receipts")
					}
					if f.read()["applied_settings"].(map[string]any)["AGENT_claude_COMMAND"] != "old-command" {
						t.Fatal("failed hook published pending command")
					}
					consumer.appliedIntegrationInspection = false
					observed, err = consumer.integrationInventory(context.Background(), "observe", entries, "")
					if err != nil || !observed.Changed {
						t.Fatal("ordinary init lost pending hook work")
					}
				}
				if err := runtime.FinishDeveloperIdentity(context.Background(), plan); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Lstat(journal); !os.IsNotExist(err) {
					t.Fatal("finished identity retained authority")
				}
				if _, err := runtime.identityNative(context.Background(), "finished", map[string]any{"plan": plan.core}); err != nil {
					t.Fatal(err)
				}
				if err := runtime.VerifyCompletedDeveloperIdentity(context.Background(), plan.CompletionProof(), plan.Binding()); err != nil {
					t.Fatal(err)
				}
				foreign := runtime
				foreign.Yard.IncusProject = "foreign-project"
				if err := foreign.VerifyCompletedDeveloperIdentity(context.Background(), plan.CompletionProof(), plan.Binding()); !errors.Is(err, domain.ErrPlanStale) {
					t.Fatal("changed completion native scope accepted")
				}

				changed := plan.core
				changed.Machine = strings.Repeat("b", 32)
				if _, err := runtime.identityNative(context.Background(), "finished", map[string]any{"plan": changed}); err == nil {
					t.Fatal("foreign completion machine accepted")
				}

				return
			}
			err = runtime.BeginDeveloperIdentity(context.Background(), plan)
			if scenario != "begin" {
				if err == nil {
					t.Fatal("changed native/source state accepted")
				}
				if scenario == "changed-source" && !errors.Is(err, domain.ErrPlanStale) {
					t.Fatalf("wrong source refusal: %v", err)
				}
				if _, err := os.Lstat(journal); !os.IsNotExist(err) {
					t.Fatal("refused transition published authority")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Lstat(journal)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("authority mode: %v", err)
			}
			if payload, err := os.ReadFile(filepath.Join(guest, "home/dev/data")); err != nil || string(payload) != "preserved" {
				t.Fatal("begin changed home data")
			}
		})
	}
}

func TestDeveloperIdentityNativeRefusalsSurviveExecutorExitError(t *testing.T) {
	transport := errors.New("fixture transport failed")
	for _, scenario := range []struct {
		name, payload, want string
		cause               error
	}{
		{"completed", `{"errors":["target account exists","developer processes remain"]}`, "developer identity: target account exists; developer processes remain", &ports.GuestExitError{Code: 1}},
		{"unknown-output", `not a typed refusal`, "developer account transition cannot verify native ownership", &ports.GuestExitError{Code: 1}},
		{"success-on-failure", `{"pending":false}`, "developer account transition cannot verify native ownership", &ports.GuestExitError{Code: 1}},
		{"control-text", `{"errors":["bad\ntext"]}`, "developer account transition cannot verify native ownership", &ports.GuestExitError{Code: 1}},
		{"transport", `{"errors":["must not mask transport"]}`, "", transport},
		{"cancelled", `{"errors":["must not mask cancellation"]}`, "", context.Canceled},
		{"deadline", `{"errors":["must not mask deadline"]}`, "", context.DeadlineExceeded},
		{"wrong-exit", `{"errors":["must not mask mismatched exit"]}`, "", &ports.GuestExitError{Code: 2}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			incus := &testkit.Incus{ExecSteps: []testkit.IncusExecStep{{Result: ports.InstanceExecResult{ExitCode: 1, Stdout: []byte(scenario.payload)}, Err: scenario.cause}}}
			runtime := Runtime{RepositoryRoot: "../../..", Executor: incus}
			_, err := runtime.identityNative(context.Background(), "account", map[string]any{})
			if scenario.want == "" {
				if !errors.Is(err, scenario.cause) {
					t.Fatalf("transport/cancellation lost: %v", err)
				}
				return
			}
			if err == nil || err.Error() != scenario.want {
				t.Fatalf("native refusal: %v, want %s", err, scenario.want)
			}
		})
	}
}

func TestIdentityOwnerRefusalKeepsOnlyItsFixedLine(t *testing.T) {
	for _, scenario := range []struct {
		stderr, want string
	}{
		{"Orca identity: agent terminals are running; close them\n", "Orca identity: agent terminals are running; close them"},
		{"noise\nGit-fetch identity: custody changed\nmore\n", "Git-fetch identity: custody changed"},
		{"Traceback: secret=value\n", ""},
		{"prefix Orca identity: not at line start\n", ""},
		{"Orca identity: \n", ""},
	} {
		if got := string(identityOwnerRefusal.Find([]byte(scenario.stderr))); got != scenario.want {
			t.Fatalf("%q: got %q, want %q", scenario.stderr, got, scenario.want)
		}
	}
}
