package reconcileruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

type afterOwnerConfigCut struct {
	*testkit.Incus
	calls int
}

func (e *afterOwnerConfigCut) Exec(ctx context.Context, project, name string, r ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	e.calls++
	if e.calls == 3 {
		return ports.InstanceExecResult{}, errors.New("original partial config commit interrupted")
	}
	return e.Incus.Exec(ctx, project, name, r)
}
func TestAfterOwnerMaterializedFilesRetainsPhysicalRecordAcrossFreshProcess(t *testing.T) {
	ctx := context.Background()
	if root := os.Getenv("SUBYARD_AFTER_OWNER_FILES_CHILD"); root != "" {
		runtime, _ := materializedConfigFixture(t, root)
		raw, err := os.ReadFile(filepath.Join(root, "resolved.recipe"))
		if err != nil {
			t.Fatal(err)
		}
		physical, err := os.ReadFile(filepath.Join(root, "physical.record"))
		if err != nil {
			t.Fatal(err)
		}
		p, err := runtime.BindAfterOwnerMaterializedConfigs(ctx, raw, physical)
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		originalBinding := p.configs.binding
		if err := runtime.ApplyPreparedMaterializedConfigs(ctx, p); err != nil {
			t.Fatal(err)
		}
		if p.configs.binding != originalBinding {
			t.Fatal("partial apply replaced original physical baseline")
		}
		incus := runtime.Incus.(*testkit.Incus)
		if len(incus.ExecCalls) != 3 || !bytes.Contains(incus.ExecCalls[1].Request.Stdin, []byte("approved before owner\n")) {
			t.Fatal("fresh helper lost retained payload")
		}
		return
	}
	root := testkit.TempDir(t)
	runtime, source := materializedConfigFixture(t, root)
	testkit.WriteFile(t, source, []byte("approved before owner\n"), 0600)
	p, err := runtime.PrepareAfterOwnerMaterializedConfigs(ctx, []byte(`{"version":1,"phase":"template","metadataBytes":4096,"descriptorBytes":4096}`))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if len(runtime.Incus.(*testkit.Incus).ExecCalls) != 0 {
		t.Fatal("source capture observed guest")
	}
	resolved, physical, err := runtime.ResolveAfterOwnerMaterializedConfigs(ctx, p.Recipe())
	if err != nil {
		t.Fatal(err)
	}
	defer resolved.Close()
	derived, err := resolved.WithAfterOwnerMetadata([]byte(`{"version":1,"phase":"resolved"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer derived.Close()
	cut := &afterOwnerConfigCut{Incus: runtime.Incus.(*testkit.Incus)}
	runtime.Executor = cut
	if err := runtime.ApplyPreparedMaterializedConfigs(ctx, derived); err == nil || cut.calls != 3 {
		t.Fatal("partial apply was not interrupted", err, cut.calls)
	}
	testkit.WriteFile(t, filepath.Join(root, "resolved.recipe"), derived.Recipe(), 0600)
	testkit.WriteFile(t, filepath.Join(root, "physical.record"), physical, 0600)
	child := exec.Command(os.Args[0], "-test.run=^TestAfterOwnerMaterializedFilesRetainsPhysicalRecordAcrossFreshProcess$")
	child.Env = append(os.Environ(), "SUBYARD_AFTER_OWNER_FILES_CHILD="+root)
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("fresh process: %v\n%s", err, output)
	}
	for _, change := range []string{"uuid", "source", "ordinary-kind"} {
		t.Run(change, func(t *testing.T) {
			current, _ := materializedConfigFixture(t, root)
			switch change {
			case "uuid":
				current.Incus.(*testkit.Incus).Reconcile.Instance.Config["volatile.uuid"] = "foreign-instance"
			case "source":
				testkit.WriteFile(t, source, []byte("unapproved source B\n"), 0600)
			case "ordinary-kind":
				var other installedRecipe
				_ = json.Unmarshal(derived.Recipe(), &other)
				other.Owner = "materialized-integrations"
				raw, _ := json.Marshal(other)
				if _, err := current.BindAfterOwnerMaterializedConfigs(ctx, raw, physical); !errors.Is(err, domain.ErrPlanStale) {
					t.Fatal("wrong kind accepted", err)
				}
				return
			}
			if _, err := current.BindAfterOwnerMaterializedConfigs(ctx, derived.Recipe(), physical); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatal("drift replaced original physical plan", err)
			}
			if len(current.Incus.(*testkit.Incus).ExecCalls) != 0 {
				t.Fatal("refusal made guest effect")
			}
		})
	}
}
