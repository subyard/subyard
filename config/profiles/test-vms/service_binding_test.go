package testvms_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/testvmsruntime"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestServicePoolFenceIsAtomicWithAdmissionAndDoesNotRevokeBeforeStop(t *testing.T) {
	store := testvmsruntime.LeaseStore{Path: filepath.Join(testkit.TempDir(t), "leases.json"), SlotCount: 2}
	original, err := store.AcquireSlot("original", "SHA256:key", "", "service-fence", "slot-001")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkHeld(original); err != nil {
		t.Fatal(err)
	}
	pool, err := store.Status()
	if err != nil {
		t.Fatal(err)
	}
	approved := testvmsruntime.PoolOperationBinding(pool)
	// A grant after the outer observation but before the native fence invalidates it.
	replacement, err := store.AcquireSlot("replacement", "SHA256:key", "", "service-fence", "slot-002")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FenceExpectedPool(approved); !errors.Is(err, testvmsruntime.ErrLeaseTargetStale) {
		t.Fatalf("late grant admitted to old drain: %v", err)
	}
	pool, err = store.Status()
	if err != nil {
		t.Fatal(err)
	}
	if pool.AdmissionClosed || pool.Slots[0].State != testvmsruntime.SlotHeld || pool.Slots[1].LeaseEpoch != replacement.LeaseEpoch {
		t.Fatal("stale fence changed allocations")
	}
	if err := store.FenceExpectedPool(testvmsruntime.PoolOperationBinding(pool)); err != nil {
		t.Fatal(err)
	}
	fenced, err := store.Status()
	if err != nil {
		t.Fatal(err)
	}
	if fenced.SchemaVersion != testvmsruntime.LeaseFencedSchemaVersion || !fenced.AdmissionClosed {
		t.Fatal("fence was not published through the incompatible-reader boundary")
	}
	if _, err := store.Renew(original); err != nil {
		t.Fatalf("fence revoked existing holder before native stop: %v", err)
	}
	if err := store.BeginDrain(replacement); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishDrain(replacement.SlotID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireSlot("new", "SHA256:key", "", "service-fence", "slot-002"); !errors.Is(err, testvmsruntime.ErrLeaseBusy) {
		t.Fatalf("slot reused after native fence: %v", err)
	}
	if err := store.BeginDrainAll("native service stop"); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishDrain(original.SlotID, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.OpenAdmission(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireSlot("restarted", "SHA256:key", "", "service-fence", "slot-002"); err != nil {
		t.Fatalf("explicit restart did not reopen admission: %v", err)
	}
}

func TestServiceProfileAndNativePoolBindingAgree(t *testing.T) {
	store := testvmsruntime.LeaseStore{Path: filepath.Join(testkit.TempDir(t), "leases.json"), SlotCount: 2}
	if _, err := store.AcquireSlot("fixture", "SHA256:key", "", "service-binding", "slot-001"); err != nil {
		t.Fatal(err)
	}
	pool, err := store.Status()
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{"status": "ok", "pool": pool})
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("python3", "-B", "-c", `import json,runpy,sys; module=runpy.run_path('service-binding.py');print(module['project_pool'](json.load(sys.stdin))['binding'])`)
	command.Stdin = bytes.NewReader(payload)
	actual, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(actual)) != testvmsruntime.PoolOperationBinding(pool) {
		t.Fatal("profile/native binding encoding differs")
	}
}
