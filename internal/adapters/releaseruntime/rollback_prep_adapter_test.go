package releaseruntime

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

// Step 1 runs the shipped rollback-apply through the reconcile adapter, which
// resolves its handler from the loaded catalog of the active release: that
// root stays open after the admission reruns until the conversions finish.
func TestRollbackPreparationRunsShippedHooksFromAnOpenRoot(t *testing.T) {
	handler := `#!/bin/sh
current=2
[ -f "$SAMPLE_STATE" ] && read -r current <"$SAMPLE_STATE"
case "$1" in
rollback-observe)
	if [ "$current" = 1 ]; then
		printf '{"state":"current","actual":"%s","desired":"%s"}\n' ` + prepOld + ` ` + prepOld + `
	else
		printf '{"state":"stale","actual":"%s","desired":"%s"}\n' ` + prepNew + ` ` + prepOld + `
	fi ;;
rollback-apply)
	printf '1\n' >"$SAMPLE_STATE"
	printf '{"state":"current","actual":"%s","desired":"%s"}\n' ` + prepOld + ` ` + prepOld + ` ;;
*) exit 9 ;;
esac
`
	fixture := newPreparedRollbackFixture(t, preparedRollbackOptions{active: preparedSample.active, seed: func(active, _ string) {
		testkit.WriteFile(t, filepath.Join(active, "config/profiles/sample/runtime.sh"), []byte(handler), 0o700)
	}})
	states := testkit.TempDir(t)
	adapter := func(loaded config.Loaded) reconcileruntime.Runtime {
		instance := loaded.Context.IncusProject + "/" + loaded.Context.YardInstanceName
		return reconcileruntime.Runtime{
			RepositoryRoot: loaded.Context.Paths.RepositoryRoot, Profiles: loaded.Catalog.Profiles(), Yard: loaded.Context,
			Environment: []string{"SAMPLE_STATE=" + filepath.Join(states, loaded.Context.YardName)},
			Incus:       &testkit.Incus{Instances: map[string]ports.InstanceInfo{instance: {Status: "Running"}}},
		}
	}
	prepared, err := fixture.prepare(t)
	if err != nil {
		t.Fatal(err)
	}
	apply := fixture.runtime.config.ProfileRuntimeRollbackApply
	fixture.runtime.config.ProfileRuntimeRollbackApply = func(ctx context.Context, root string, loaded config.Loaded, id, operation, actual, desired, consequence string) error {
		if root != loaded.Context.Paths.RepositoryRoot {
			t.Errorf("hook root %s, yard settings read with %s", root, loaded.Context.Paths.RepositoryRoot)
		}
		if err := adapter(loaded).ApplyProfileRuntimeRollback(ctx, id, operation, actual, desired, consequence); err != nil {
			return err
		}
		return apply(ctx, root, loaded, id, operation, actual, desired, consequence)
	}
	if err := prepared.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state, err := os.ReadFile(filepath.Join(states, "default")); err != nil || string(state) != "1\n" ||
		!slices.Equal(fixture.applies, []string{"default sample-runtime yard " + fixture.activeID}) {
		t.Fatalf("shipped conversion = %q, %v, applies %v", state, err, fixture.applies)
	}
}
