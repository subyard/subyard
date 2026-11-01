package cli

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releaseowner"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/testyardmigration"
)

type ownerPhysicalResultObserver struct {
	ownerGraphObserver
	reads int
}

func (o *ownerPhysicalResultObserver) OwnerCreatedResults() ([]ports.NativeOwnerCreated, error) {
	o.reads++
	return nil, nil
}

func TestOwnerPhysicalReceiptRestoresExactObservationAndRefusesDrift(t *testing.T) {
	if os.Getenv("SUBYARD_NATIVE_OWNER_GRAPH_ENGINE") == "" {
		t.Skip("original owner constructors require a matching normal engine")
	}
	c, _, _ := ownerOriginalGraphFixture(t)
	ctx := context.Background()
	if _, _, err := c.CaptureInventory(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := c.CaptureOwner(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A failed native start retains its stage. Read-only physical capture must
	// reconstruct sources without claiming that invocation's mutation stage.
	observer := &ownerPhysicalResultObserver{}
	ctx = ports.WithNativeInvocationKey(ports.WithNativeOperationObserver(ctx, observer), nativeValueDigest(testkit.TempDir(t)))
	child := c.ownerChildren.inits[testyardmigration.LegacyYard]
	sources, err := c.ownerChildren.restoreChildSources(ctx, testyardmigration.LegacyYard)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := child.runtime.OwnerSourceRuntime(ctx, sources)
	if err != nil {
		t.Fatal(err)
	}
	stage := runtime.RepositoryRoot
	t.Cleanup(func() {
		_ = filepath.WalkDir(stage, func(path string, entry fs.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				return os.Chmod(path, 0700)
			}
			return err
		})
		_ = os.RemoveAll(stage)
	})
	if err := sources.FinishOwnerSources(errors.New("simulated native start failure")); err != nil {
		t.Fatal(err)
	}
	retained, err := os.Stat(stage)
	if err != nil {
		t.Fatal("failed invocation stage was not retained", err)
	}
	token := strings.Repeat("1", 32)
	action := releaseowner.OwnerActionReference{Program: strings.Repeat("a", 64), Action: releaseowner.OwnerAction{Operation: "validate-original"}}
	reader := c.cli.options.Incus.(*ownerGraphReader)
	key := "subyard-e2e-yard/yard-e2e-yard"
	instance := reader.Instances[key]
	instance.Status = "Running"
	reader.Instances[key] = instance
	if _, err := c.CaptureOwnerPhysical(ctx, before, token, action); err != nil {
		t.Fatal("original lifecycle convergence was refused", err)
	}
	if current, err := os.Stat(stage); err != nil || !os.SameFile(retained, current) {
		t.Fatal("physical observation replaced the failed invocation stage", err)
	}
	if observer.reads != 2 || observer.commands != 0 {
		t.Fatal("physical observation lost created-resource authority or executed an observed command")
	}
	instance.Status = "Stopped"
	reader.Instances[key] = instance
	physical, err := c.CaptureOwnerPhysical(ctx, before, token, action)
	if err != nil {
		t.Fatal("original physical output", err)
	}
	c.ownerResultsRecipe = before.PreparedRecipe
	if err := c.BindOwnerPhysical(ctx, before, token, action, physical); err != nil {
		t.Fatal(err)
	}
	if err := c.restoreOwnerPhysical(ctx, before, token, action); err != nil {
		t.Fatal("unchanged receipt did not reconstruct", err)
	}
	instance.Status = "Running"
	reader.Instances[key] = instance
	// Model the output boundary of a closed native start, then reconstruct
	// through the original serialized capsule rather than the old handles.
	action.Action.Operation = "start"
	started, err := c.CaptureOwnerPhysical(ctx, before, token, action)
	if err != nil || started == physical {
		t.Fatal("native output did not bind the changed physical state", err)
	}
	if err := c.bindOwnerChildrenForApply(ctx, before.PreparedRecipe, c.ownerChildren.raw, token); err != nil {
		t.Fatal("original capsule could not reopen", err)
	}
	if err := c.BindOwnerPhysical(ctx, before, token, action, started); err != nil {
		t.Fatal(err)
	}
	if err := c.restoreOwnerPhysical(ctx, before, token, action); err != nil || c.ownerChildren.binding() != before.PreparedRecipe {
		t.Fatal("physical successor changed the original source capsule", err)
	}
	instance.Config["volatile.uuid"] = "foreign-replacement"
	reader.Instances[key] = instance
	if err := c.restoreOwnerPhysical(ctx, before, token, action); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("replacement UUID adopted", err)
	}
	instance.Config["volatile.uuid"] = "original-legacy-uuid"
	reader.Instances[key] = instance
	// This managed output did not exist in the closed producer observation.
	// Its expected pathname alone must not authorize adopting new contents.
	state := c.ownerChildren.teardowns[testyardmigration.CurrentYard].loaded.Context.Paths.StateDir
	if err := os.MkdirAll(state, 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(state, "foreign"), []byte("unrelated data"), 0600)
	if err := c.restoreOwnerPhysical(ctx, before, token, action); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("foreign artifact adopted", err)
	}
}
