package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Subyard/Subyard/internal/contracttest"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestProjectStoreConformance(t *testing.T) {
	t.Run("file", func(t *testing.T) { contracttest.ProjectStore(t, newTestStore(t)) })
	t.Run("memory", func(t *testing.T) { contracttest.ProjectStore(t, testkit.NewMemoryState()) })
}

func TestFileStoreAtomicConcurrentWrites(t *testing.T) {
	store := newTestStore(t)
	var wait sync.WaitGroup
	for index := 0; index < 20; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			record := fixtureRecord("project-a")
			record.Name = fmt.Sprintf("project-%02d", index)
			if err := store.Put(context.Background(), record); err != nil {
				t.Errorf("put: %v", err)
			}
		}(index)
	}
	wait.Wait()
	record, err := store.Get(context.Background(), "project-a")
	if err != nil {
		t.Fatal(err)
	}
	if record.Name == "" {
		t.Fatal("published record is incomplete")
	}
}

func TestFileStoreRejectsCorruptAndSymlinkedState(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Directory(), "bad.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(context.Background()); err == nil {
		t.Fatal("corrupt state was accepted")
	}
	if err := os.Remove(filepath.Join(store.Directory(), "bad.json")); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target.json")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(store.Directory(), "linked.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(context.Background()); err == nil {
		t.Fatal("symlinked state was accepted")
	}
}

func TestFileStoreRejectsBroadPermissionsAndOversizedState(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Directory(), "unsafe.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(context.Background()); err == nil {
		t.Fatal("broad state-file permissions were accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, 1024*1024+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(context.Background()); err == nil {
		t.Fatal("oversized state file was accepted")
	}
}

func TestFileStoreRepairsValidatedLegacyPermissions(t *testing.T) {
	store := newTestStore(t)
	record := fixtureRecord("legacy")
	if err := store.Put(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Directory(), "legacy.json")
	if err := os.Chmod(path, 0o664); err != nil {
		t.Fatal(err)
	}
	changed, err := store.RepairLegacyPermissions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("legacy state permissions were not repaired")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("legacy state mode = %o, want 600", info.Mode().Perm())
	}
	if _, err := store.Get(context.Background(), record.ProjectID); err != nil {
		t.Fatal(err)
	}
	changed, err = store.RepairLegacyPermissions(context.Background())
	if err != nil || changed {
		t.Fatalf("canonical state repair = %t, %v", changed, err)
	}
}

func TestFileStoreDoesNotRepairInvalidBroadState(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Directory(), "invalid.json")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Normalize the legacy mode across process umasks.
	if err := os.Chmod(path, 0o664); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o664); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RepairLegacyPermissions(context.Background()); err == nil {
		t.Fatal("invalid broad state was repaired")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o664 {
		t.Fatalf("invalid state mode changed to %o", info.Mode().Perm())
	}
}

func TestFileStoreDoesNotRepairAnomalousLegacyMode(t *testing.T) {
	store := newTestStore(t)
	if err := store.Put(context.Background(), fixtureRecord("executable")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Directory(), "executable.json")
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RepairLegacyPermissions(context.Background()); err == nil {
		t.Fatal("executable state mode was repaired")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("anomalous state mode changed to %o", info.Mode().Perm())
	}
}

func TestFileStoreReadOnlyRejectsUnsafeStateDirectory(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*testing.T, string) string
	}{
		{
			name: "broad permissions",
			setup: func(t *testing.T, directory string) string {
				t.Helper()
				if err := os.Chmod(directory, 0o755); err != nil {
					t.Fatal(err)
				}
				return directory
			},
		},
		{
			name: "symlink",
			setup: func(t *testing.T, directory string) string {
				t.Helper()
				link := filepath.Join(t.TempDir(), "projects")
				if err := os.Symlink(directory, link); err != nil {
					t.Fatal(err)
				}
				return link
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "projects")
			store, err := NewFileStore(directory)
			if err != nil {
				t.Fatal(err)
			}
			record := fixtureRecord("project-a")
			if err := store.Put(context.Background(), record); err != nil {
				t.Fatal(err)
			}
			unsafePath := test.setup(t, directory)
			readOnly, err := NewFileStore(unsafePath)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := readOnly.ListReadOnly(context.Background()); err == nil {
				t.Fatal("ListReadOnly accepted an unsafe state directory")
			}
			if _, err := readOnly.GetReadOnly(context.Background(), record.ProjectID); err == nil {
				t.Fatal("GetReadOnly accepted an unsafe state directory")
			}
		})
	}
}

func TestFileStoreReadOnlyDoesNotCreateMissingStateDirectory(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "missing", "projects")
	store, err := NewFileStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	if records, err := store.ListReadOnly(context.Background()); err != nil || len(records) != 0 {
		t.Fatalf("missing read-only list=%#v err=%v", records, err)
	}
	if _, err := store.GetReadOnly(context.Background(), "missing-id"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing read-only get error=%v", err)
	}
	if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only access created state directory: %v", err)
	}
}

func TestFileStoreDeleteIsIdempotent(t *testing.T) {
	store := newTestStore(t)
	if err := store.Put(context.Background(), fixtureRecord("gone")); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(context.Background(), "gone"); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(context.Background(), "gone"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), "gone"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
}

func TestProjectAdmissionUsesCanonicalNamesAndSerializesSources(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	first, err := store.Admit(ctx, "op-one", "/work/Subyard", domain.ProjectSync, "Subyard", false)
	if err != nil {
		t.Fatal(err)
	}
	if first.ProjectID != "Subyard" || first.Name != "Subyard" || first.Reservation == nil {
		t.Fatalf("first admission = %#v", first)
	}
	if _, err := store.Admit(
		ctx, "op-two", "/work/Subyard", domain.ProjectSync, "Subyard", false,
	); !errors.Is(err, ErrAdmissionPending) {
		t.Fatalf("concurrent same-source admission = %v", err)
	}
	if _, err := store.Admit(
		ctx, "op-one", "/work/Subyard", domain.ProjectSync, "Other", false,
	); err == nil || !strings.Contains(err.Error(), "different admission request") {
		t.Fatalf("operation ID replay changed its request: %v", err)
	}
	record := fixtureRecord(first.ProjectID)
	record.IdentityVersion = 2
	record.Name = first.Name
	record.HostPath = "/work/Subyard"
	record.SourceKey = SourceKey(record.HostPath)
	record.YardPath = YardPath(record.ProjectID)
	mismatched := record
	mismatched.HostPath = "/other/source"
	mismatched.SourceKey = SourceKey(mismatched.HostPath)
	if err := store.FinalizeOperation(
		ctx, first.Reservation.OperationID, mismatched,
	); err == nil {
		t.Fatal("owner finalize accepted a result outside its reservation")
	}
	if err := store.FinalizeOperation(ctx, first.Reservation.OperationID, record); err != nil {
		t.Fatal(err)
	}
	repeat, err := store.Admit(
		ctx, "op-three", "/work/Subyard", domain.ProjectSync, "Subyard", false,
	)
	if err != nil || repeat.Existing == nil || repeat.ProjectID != "Subyard" {
		t.Fatalf("repeat admission = %#v, %v", repeat, err)
	}
	conflicting := fixtureRecord("legacy-other")
	conflicting.Name = "subyard"
	if err := store.Put(ctx, conflicting); err == nil {
		t.Fatal("direct project store write bypassed name uniqueness")
	}
	second, err := store.Admit(
		ctx, "op-four", "/other/Subyard", domain.ProjectSync, "Subyard", false,
	)
	if err != nil || second.ProjectID != "Subyard-2" {
		t.Fatalf("colliding admission = %#v, %v", second, err)
	}
	if _, err := store.Admit(
		ctx, "op-five", "/third/Subyard", domain.ProjectSync, "subyard", true,
	); err == nil {
		t.Fatal("case-insensitive explicit collision was accepted")
	}
	if err := store.AbortAdmission(ctx, second.Reservation.OperationID); err != nil {
		t.Fatal(err)
	}
	retried, err := store.Admit(
		ctx, "op-six", "/other/Subyard", domain.ProjectSync, "Subyard", false,
	)
	if err != nil || retried.ProjectID != "Subyard-2" {
		t.Fatalf("admission after abort = %#v, %v", retried, err)
	}
}

func TestProjectAdmissionPreviewDoesNotPublishReservation(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "projects")
	store, err := NewFileStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.PreviewAdmission(
		context.Background(), "/work/Demo", domain.ProjectSync, "Demo", false,
	)
	if err != nil || first.ProjectID != "Demo" || first.Name != "Demo" ||
		first.Existing != nil || first.Reservation != nil {
		t.Fatalf("preview=%#v err=%v", first, err)
	}
	second, err := store.PreviewAdmission(
		context.Background(), "/work/Demo", domain.ProjectSync, "Demo", false,
	)
	if err != nil || second.ProjectID != first.ProjectID || second.Reservation != nil {
		t.Fatalf("repeat preview=%#v err=%v", second, err)
	}
	if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preview created project state: %v", err)
	}
}

func TestProjectAdmissionIgnoresInterruptedReservationCandidate(t *testing.T) {
	store := newTestStore(t)
	if err := os.MkdirAll(store.reservationDirectory(), 0o700); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(store.reservationDirectory(), ".reservation.tmp.interrupted")
	if err := os.WriteFile(candidate, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := store.Admit(
		context.Background(), "op-one", "/work/Demo",
		domain.ProjectSync, "Demo", false,
	)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := store.Admit(
		context.Background(), "op-one", "/work/Demo",
		domain.ProjectSync, "Demo", false,
	)
	if err != nil || first.Reservation == nil || replayed.Reservation == nil ||
		*first.Reservation != *replayed.Reservation {
		t.Fatalf("published reservation replay = %#v, %v", replayed, err)
	}
	if _, err := os.Stat(candidate); err != nil {
		t.Fatalf("interrupted candidate was treated as published state: %v", err)
	}
}

func TestConcurrentDifferentSourcesReceiveDistinctCanonicalNames(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	type result struct {
		admission Admission
		err       error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for index, source := range []string{"/one/Demo", "/two/Demo"} {
		go func(index int, source string) {
			<-start
			admission, err := store.Admit(
				ctx, fmt.Sprintf("op-%d", index), source,
				domain.ProjectSync, "Demo", false,
			)
			results <- result{admission: admission, err: err}
		}(index, source)
	}
	close(start)
	names := make(map[string]bool)
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		names[result.admission.ProjectID] = true
	}
	if !names["Demo"] || !names["Demo-2"] || len(names) != 2 {
		t.Fatalf("concurrent canonical names = %#v", names)
	}
	if _, err := store.Admit(
		ctx, "op-unicode", "/three/Demo", domain.ProjectSync, "Демо", false,
	); err == nil {
		t.Fatal("Unicode automatic project name was accepted")
	}
}

func TestLegacyNameMigrationIsStableAndDoesNotRenameIdentity(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	older := fixtureRecord("legacy-a")
	older.Name = "Demo"
	older.ImportedAt = "2026-01-01T00:00:00Z"
	newer := fixtureRecord("legacy-b")
	newer.Name = "demo"
	newer.ImportedAt = "2026-02-01T00:00:00Z"
	missing := fixtureRecord("legacy-c")
	missing.Name = "DEMO"
	missing.ImportedAt = ""
	invalid := fixtureRecord("legacy-d")
	invalid.Name = "dEmO"
	invalid.ImportedAt = "not-a-time"
	for _, record := range []domain.ProjectRecord{older, newer, missing, invalid} {
		putLegacyRecord(t, store, record)
	}
	changed, err := store.MigrateLegacyNames(ctx)
	if err != nil || !changed {
		t.Fatalf("migration = %t, %v", changed, err)
	}
	gotOlder, _ := store.Get(ctx, older.ProjectID)
	gotNewer, _ := store.Get(ctx, newer.ProjectID)
	gotMissing, _ := store.Get(ctx, missing.ProjectID)
	gotInvalid, _ := store.Get(ctx, invalid.ProjectID)
	if gotOlder.Name != "Demo" || gotNewer.Name != "Demo-2" ||
		gotMissing.Name != "Demo-3" || gotInvalid.Name != "Demo-4" ||
		gotOlder.ProjectID != older.ProjectID || gotNewer.YardPath != newer.YardPath {
		t.Fatalf(
			"migrated records = %#v %#v %#v %#v",
			gotOlder, gotNewer, gotMissing, gotInvalid,
		)
	}
	changed, err = store.MigrateLegacyNames(ctx)
	if err != nil || changed {
		t.Fatalf("second migration = %t, %v", changed, err)
	}
}

func TestLegacyNameMigrationProtectsExistingSuffixAndResumesJournal(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	records := []domain.ProjectRecord{
		fixtureRecord("legacy-a"),
		fixtureRecord("legacy-b"),
		fixtureRecord("legacy-c"),
	}
	records[0].Name, records[0].ImportedAt = "Demo", "2026-01-01T00:00:00Z"
	records[1].Name, records[1].ImportedAt = "demo", "2026-02-01T00:00:00Z"
	records[2].Name, records[2].ImportedAt = "Demo-2", "2026-03-01T00:00:00Z"
	for _, record := range records {
		putLegacyRecord(t, store, record)
	}
	targets := planProjectNameMigration(records)
	journal := projectNameMigration{Schema: projectNameMigrationSchema, Targets: targets}
	if err := store.writeProjectNameMigration(journal); err != nil {
		t.Fatal(err)
	}
	first, err := store.Get(ctx, targets[0].ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	first.Name, first.SourceKey = targets[0].Name, targets[0].SourceKey
	putLegacyRecord(t, store, first)
	changed, err := store.MigrateLegacyNames(ctx)
	if err != nil || !changed {
		t.Fatalf("resume migration = %t, %v", changed, err)
	}
	gotA, _ := store.Get(ctx, "legacy-a")
	gotB, _ := store.Get(ctx, "legacy-b")
	gotC, _ := store.Get(ctx, "legacy-c")
	if gotA.Name != "Demo" || gotB.Name != "Demo-3" || gotC.Name != "Demo-2" {
		t.Fatalf("resumed names = %q %q %q", gotA.Name, gotB.Name, gotC.Name)
	}
	if _, err := os.Stat(store.projectNameMigrationPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("migration journal remains after resume: %v", err)
	}
}

func TestObservedMetadataCannotOverwriteOwnerNameAndAllocatesLegacyCollision(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	known := fixtureRecord("legacy-a")
	known.Name = "Demo"
	known.ImportedAt = "2026-01-01T00:00:00Z"
	if err := store.Put(ctx, known); err != nil {
		t.Fatal(err)
	}
	stale := known
	stale.Name = "Stale"
	if err := store.ConvergeObserved(ctx, stale, "yard"); err != nil {
		t.Fatal(err)
	}
	gotKnown, _ := store.Get(ctx, known.ProjectID)
	if gotKnown.Name != "Demo" {
		t.Fatalf("stale metadata overwrote owner name: %#v", gotKnown)
	}
	discovered := fixtureRecord("legacy-b")
	discovered.Name = "demo"
	discovered.HostPath = ""
	discovered.ImportedAt = "2026-02-01T00:00:00Z"
	if err := store.ConvergeObserved(ctx, discovered, "yard"); err != nil {
		t.Fatal(err)
	}
	gotDiscovered, _ := store.Get(ctx, discovered.ProjectID)
	if gotDiscovered.Name != "Demo-2" || gotDiscovered.RegistrySource != "yard" {
		t.Fatalf("observed collision was not converged: %#v", gotDiscovered)
	}
}

func TestCanonicalTechnicalIdentifiersDoNotCollapseSafeNames(t *testing.T) {
	records := []domain.ProjectRecord{
		{ProjectID: "Demo.Name", IdentityVersion: 2},
		{ProjectID: "Demo_Name", IdentityVersion: 2},
		{ProjectID: "Demo-Name", IdentityVersion: 2},
		{ProjectID: ".leading-and-trailing-", IdentityVersion: 2},
	}
	devices := make(map[string]bool)
	images := make(map[string]bool)
	for _, record := range records {
		device := WorkspaceDeviceFor(record)
		image := ProjectDockerImageID(record)
		if devices[device] || images[image] {
			t.Fatalf("derived identifier collision for %#v: %q %q", record, device, image)
		}
		devices[device] = true
		images[image] = true
		if strings.ToLower(image) != image {
			t.Fatalf("Docker image identifier is not lowercase: %q", image)
		}
		last := image[len(image)-1]
		if image[0] < 'a' || image[0] > 'z' ||
			!((last >= 'a' && last <= 'z') || (last >= '0' && last <= '9')) ||
			strings.IndexFunc(image, func(character rune) bool {
				return !(character >= 'a' && character <= 'z') &&
					!(character >= '0' && character <= '9') &&
					character != '_'
			}) >= 0 {
			t.Fatalf("Docker image identifier is not repository-safe: %q", image)
		}
	}
}

func TestServiceRemoveProjectUsesSourceKey(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	record := fixtureRecord("RemoteProject")
	record.SourceKey = SourceKey(record.HostPath)
	if err := store.Put(ctx, record); err != nil {
		t.Fatal(err)
	}
	service := Service{Store: store}
	if err := service.RemoveProject(ctx, record.ProjectID, SourceKey("/other/source")); err == nil {
		t.Fatal("remove accepted a different source key")
	}
	if err := service.RemoveProject(ctx, record.ProjectID, record.SourceKey); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, record.ProjectID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removed project remains: %v", err)
	}
}

func newTestStore(t *testing.T) *FileStore {
	t.Helper()
	store, err := NewFileStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func putLegacyRecord(t *testing.T, store *FileStore, record domain.ProjectRecord) {
	t.Helper()
	lock, err := store.lock(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock(lock)
	if err := store.putUnlocked(record); err != nil {
		t.Fatal(err)
	}
}

func fixtureRecord(id string) domain.ProjectRecord {
	return domain.ProjectRecord{
		Schema:     1,
		ProjectID:  id,
		Name:       id,
		HostPath:   "/workspace/" + id,
		YardPath:   "/srv/workspaces/" + id + "/src",
		Mode:       domain.ProjectSync,
		SSHHost:    "yard",
		ImportedAt: "2026-07-20T00:00:00Z",
	}
}
