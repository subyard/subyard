package reconcileruntime

import (
	"context"
	"errors"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestPreparedHostPowerSeparatesImmutableSourceFromInstalledBaseline(t *testing.T) {
	root := testkit.TempDir(t)
	for _, name := range []string{"config", "scripts"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	source, installed, unit := filepath.Join(root, "source-engine"), filepath.Join(root, "installed-engine"), filepath.Join(root, "installed-unit")
	testkit.WriteFile(t, source, []byte("approved native engine"), 0o700)
	runtime := Runtime{RepositoryRoot: root, Environment: []string{"SUBYARD_USER=" + account.Username, "SUBYARD_POWER_ENGINE_SOURCE=" + source, "SUBYARD_POWER_RECONCILER_PATH=" + installed, "SUBYARD_POWER_UNIT_PATH=" + unit}}
	first, err := runtime.PrepareHostPowerRuntime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	testkit.WriteFile(t, installed, []byte("installed approved engine"), 0o755)
	testkit.WriteFile(t, unit, []byte("installed approved unit"), 0o644)
	second, err := runtime.PrepareHostPowerRuntime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if first.Binding() != second.Binding() {
		t.Fatal("own installed effects changed immutable source binding")
	}
	if first.NativeBefore() == second.NativeBefore() {
		t.Fatal("changed native baseline was hidden")
	}
	if err := runtime.ApplyPreparedHostPowerRuntime(context.Background(), first); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("changed native baseline reached effects", err)
	}
	testkit.WriteFile(t, source, []byte("changed engine after consent"), 0o700)
	approved, err := io.ReadAll(io.NewSectionReader(second.engine, 0, 128<<20))
	if err != nil || string(approved) != "approved native engine" {
		t.Fatal("mutable original source changed sealed approved bytes", err)
	}
	if err := runtime.ApplyPreparedHostPowerRuntime(context.Background(), second); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("changed original engine reached effects", err)
	}
}
