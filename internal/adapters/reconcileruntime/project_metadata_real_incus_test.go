//go:build realincus

package reconcileruntime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/incusclient"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

type metadataNativeExecutor struct {
	*incusclient.Client
	calls []ports.InstanceExecRequest
}

func (executor *metadataNativeExecutor) Exec(ctx context.Context, project, instance string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	executor.calls = append(executor.calls, request)
	return executor.Client.Exec(ctx, project, instance, request)
}

// This fixture owns a disposable nested container and exercises actual Incus
// user/group execution, including the privileged publisher and nonzero exits.
func TestRealIncusProjectMetadataLegacyBindRepair(t *testing.T) {
	const marker = "project-metadata-repair-v1"
	if os.Getenv("SUBYARD_REAL_METADATA_REPAIR") != marker {
		t.Skip("set SUBYARD_REAL_METADATA_REPAIR=" + marker + " on a leased test VM")
	}
	if os.Getenv("SUBYARD_E2E_VM") == "" {
		t.Fatal("real metadata repair requires a leased disposable VM")
	}
	const project, instance = "subyard-metadata-repair", "metadata-repair"
	cli := func(args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, "incus", args...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("fixture Incus %s: %v: %s", args[0], err, output)
		}
		return strings.TrimSpace(string(output))
	}
	for _, name := range strings.Split(cli("project", "list", "-f", "csv", "-c", "n"), "\n") {
		if name == project {
			t.Fatal("refusing an existing metadata repair project")
		}
	}
	cli("project", "create", project, "-c", "features.images=false", "-c", "features.profiles=false", "-c", "user.subyard.acceptance_fixture="+marker)
	t.Cleanup(func() {
		if cli("project", "get", project, "user.subyard.acceptance_fixture") != marker {
			t.Fatal("metadata repair project ownership changed")
		}
		if cli("list", "--project", project, "-f", "csv", "-c", "n") != "" {
			if cli("config", "get", instance, "--project", project, "user.subyard.acceptance_fixture") != marker {
				t.Fatal("metadata repair instance ownership changed")
			}
			cli("delete", instance, "--project", project, "--force")
		}
		cli("project", "delete", project)
	})
	cli("launch", "subyard-e2e-debian-13-cloud-container", instance, "--project", project, "--storage", "default", "-c", "user.subyard.acceptance_fixture="+marker)
	client := incusclient.New("/var/lib/incus/unix.socket")
	ctx := context.Background()
	guest := func(program string, payload []byte) []byte {
		t.Helper()
		call, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		result, err := client.Exec(call, project, instance, ports.InstanceExecRequest{Command: []string{"python3", "-B", "-c", program}, Stdin: payload})
		if err != nil || result.ExitCode != 0 {
			t.Fatalf("native metadata fixture failed: exit=%d err=%v stderr=%s", result.ExitCode, err, result.Stderr)
		}
		return result.Stdout
	}
	guest("import os; os.makedirs('/srv/workspaces', exist_ok=True); os.chmod('/srv/workspaces',0o755)", nil)
	for _, scenario := range []struct {
		name                       string
		rootUID, rootGID, rootMode int
		srcUID, srcGID, srcMode    int
		mode                       domain.ProjectMode
		reason                     string
		stale                      bool
	}{
		{"shared-root-source", 0, 1000, 0o2775, 0, 1000, 0o2775, domain.ProjectBind, "", false},
		{"readonly-wrapper", 0, 1000, 0o2755, 1000, 1000, 0o775, domain.ProjectBind, "", false},
		{"shared-wrapper", 0, 1000, 0o2775, 1000, 1000, 0o2755, domain.ProjectBind, "", false},
		{"non-bind-root", 0, 1000, 0o2755, 1000, 1000, 0o775, domain.ProjectSync, "unsafe_root", false},
		{"non-bind-source", 1000, 1000, 0o755, 0, 1000, 0o2775, domain.ProjectSync, "unsafe_source", false},
		{"foreign-root-owner", 1001, 1000, 0o2755, 1000, 1000, 0o775, domain.ProjectBind, "unsafe_root", false},
		{"foreign-root-group", 0, 1001, 0o2755, 1000, 1000, 0o775, domain.ProjectBind, "unsafe_root", false},
		{"foreign-source-owner", 0, 1000, 0o2755, 1001, 1000, 0o775, domain.ProjectBind, "unsafe_source", false},
		{"foreign-source-group", 0, 1000, 0o2755, 0, 1001, 0o2775, domain.ProjectBind, "unsafe_source", false},
		{"foreign-dev-source-group", 0, 1000, 0o2755, 1000, 1001, 0o775, domain.ProjectBind, "unsafe_source", false},
		{"world-write-root", 0, 1000, 0o2777, 1000, 1000, 0o775, domain.ProjectBind, "unsafe_root", false},
		{"world-write-source", 0, 1000, 0o2755, 0, 1000, 0o2777, domain.ProjectBind, "unsafe_source", false},
		{"world-write-dev-source", 0, 1000, 0o2755, 1000, 1000, 0o777, domain.ProjectBind, "unsafe_source", false},
		{"stale-wrapper", 0, 1000, 0o2755, 1000, 1000, 0o775, domain.ProjectBind, "stale_inputs", true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			root := "/srv/workspaces/" + scenario.name
			payload, err := json.Marshal([]any{root, scenario.rootUID, scenario.rootGID, scenario.rootMode, scenario.srcUID, scenario.srcGID, scenario.srcMode})
			if err != nil {
				t.Fatal(err)
			}
			guest(`import json,os,sys
r,ru,rg,rm,su,sg,sm=json.load(sys.stdin)
os.makedirs(r+'/src')
os.chown(r,ru,rg); os.chmod(r,rm)
os.chown(r+'/src',su,sg); os.chmod(r+'/src',sm)
with open(r+'/src/sentinel','w') as f: f.write('preserve source and sessions')
os.chown(r+'/src/sentinel',1000,1000); os.chmod(r+'/src/sentinel',0o600)
`, payload)
			rootPayload, _ := json.Marshal(root)
			const snapshot = `import hashlib,json,os,sys
r=json.load(sys.stdin)
def fact(p):
 s=os.stat(p); return [s.st_mode,s.st_uid,s.st_gid,s.st_dev,s.st_ino]
print(json.dumps([fact(r),fact(r+'/src'),fact(r+'/src/sentinel'),hashlib.sha256(open(r+'/src/sentinel','rb').read()).hexdigest()]))
`
			before := guest(snapshot, rootPayload)
			executor := &metadataNativeExecutor{Client: client}
			runtime := Runtime{Executor: executor, Yard: domain.Context{YardName: "selected", DevUID: 1000, IncusProject: project, YardInstanceName: instance}, HookPlan: &ProjectHookPlan{
				ownerProjects: []domain.ProjectRecord{{Schema: 1, ProjectID: scenario.name, Name: scenario.name, HostPath: "/owner/source", YardPath: root + "/src", Mode: scenario.mode, SSHHost: "yard"}},
			}}
			observed, err := runtime.metadataRequest(ctx, "inspect", &projectMetadataPlan{Targets: []projectMetadataTarget{}})
			if err != nil {
				t.Fatal(err)
			}
			runtime.HookPlan.approved.Projects = observed.Projects
			err = runtime.PrepareProjectMetadata(ctx)
			for _, call := range executor.calls {
				if call.User != 1000 || call.Group != 1000 {
					t.Fatal("inspection used privileged execution")
				}
			}
			if scenario.stale {
				if err != nil {
					t.Fatal(err)
				}
				guest("import json,os,sys; os.chmod(json.load(sys.stdin),0o2775)", rootPayload)
				before = guest(snapshot, rootPayload)
			}
			if err == nil {
				err = runtime.repairProjectMetadata(ctx)
			}
			if scenario.reason != "" {
				var failure *ProjectMetadataError
				if !errors.As(err, &failure) || failure.Reason != scenario.reason || failure.ProjectID != scenario.name || errors.Is(err, domain.ErrPlanStale) != scenario.stale {
					t.Fatalf("native refusal: %v", err)
				}
				guest("import json,os,sys; assert not os.path.lexists(json.load(sys.stdin)+'/.subyard-meta.json')", rootPayload)
			} else {
				if err != nil {
					t.Fatal(err)
				}
				wantUser := uint32(1000)
				if scenario.rootMode&0o020 == 0 {
					wantUser = 0
				}
				if last := executor.calls[len(executor.calls)-1]; last.User != wantUser || last.Group != 1000 {
					t.Fatalf("publication user/group=%d/%d want=%d/1000", last.User, last.Group, wantUser)
				}
				metadataBefore := guest("import json,os,sys; p=json.load(sys.stdin)+'/.subyard-meta.json'; s=os.stat(p); assert s.st_uid==1000 and s.st_gid==1000 and s.st_mode&0o777==0o600; print(json.dumps([s.st_ino,s.st_mtime_ns,open(p).read()]))", rootPayload)
				if err := runtime.PrepareProjectMetadata(ctx); err != nil {
					t.Fatal(err)
				}
				runtime.HookPlan.metadataCompleted = false
				if err := runtime.repairProjectMetadata(ctx); err != nil {
					t.Fatal(err)
				}
				metadataAfter := guest("import json,os,sys; p=json.load(sys.stdin)+'/.subyard-meta.json'; s=os.stat(p); print(json.dumps([s.st_ino,s.st_mtime_ns,open(p).read()]))", rootPayload)
				if string(metadataBefore) != string(metadataAfter) || executor.calls[len(executor.calls)-1].User != 1000 {
					t.Fatal("repeat repair changed metadata or retained privileged execution")
				}
			}
			if after := guest(snapshot, rootPayload); string(before) != string(after) {
				t.Fatal("metadata repair changed root/source identity, permissions or source data")
			}
		})
	}
}
