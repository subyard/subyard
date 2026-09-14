package state

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
)

func TestCopyAdmissionFinalizesIndependentCopiesWithSharedProvenance(t *testing.T) {
	for _, mode := range []domain.ProjectMode{domain.ProjectSync, domain.ProjectGit} {
		t.Run(string(mode), func(t *testing.T) {
			store := newTestStore(t)
			ctx := context.Background()
			source := "/work/exact/Demo"
			legacy := fixtureRecord("legacy-demo-12345678")
			legacy.Name, legacy.HostPath, legacy.SourceKey = "LegacyDemo", source, ""
			legacy.Mode = mode
			putLegacyRecord(t, store, legacy)

			wantNames := []string{"Demo", "Demo-2", "Demo-3"}
			for index, want := range wantNames {
				admission, err := store.Admit(
					ctx, "sync-copy-op-"+string(rune('1'+index)), source,
					mode, "Demo", false,
				)
				if err != nil {
					t.Fatalf("admit copy %d: %v", index+1, err)
				}
				if admission.ProjectID != want || admission.Name != want || admission.Existing != nil {
					t.Fatalf("copy %d admission = %#v, want name %q", index+1, admission, want)
				}
				record := admittedRecord(admission, source, mode)
				if err := store.FinalizeOperation(ctx, admission.Reservation.OperationID, record); err != nil {
					t.Fatalf("finalize copy %d: %v", index+1, err)
				}
			}

			for _, name := range wantNames {
				record, err := store.Get(ctx, name)
				if err != nil {
					t.Fatalf("get %q: %v", name, err)
				}
				if record.ProjectID != name || record.Name != name || record.HostPath != source ||
					record.SourceKey != SourceKey(source) {
					t.Fatalf("copy %q lost identity or provenance: %#v", name, record)
				}
			}
			gotLegacy, err := store.Get(ctx, legacy.ProjectID)
			if err != nil || gotLegacy != legacy {
				t.Fatalf("legacy same-source record changed: got=%#v err=%v", gotLegacy, err)
			}
		})
	}
}

func TestSyncAdmissionSuffixesLongAndNumericBaseNames(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	longName := strings.Repeat("a", 50)
	long, err := store.Admit(
		ctx, "long-name-op", "/work/long", domain.ProjectSync, longName, false, longName,
	)
	if err != nil {
		t.Fatal(err)
	}
	wantLong := strings.Repeat("a", 48) + "-2"
	if long.Name != wantLong || len(long.Name) != 50 || !domain.SafeProjectName(long.Name) {
		t.Fatalf("long suffix = %q (%d bytes), want %q", long.Name, len(long.Name), wantLong)
	}

	numeric, err := store.Admit(
		ctx, "numeric-name-op", "/work/numeric", domain.ProjectSync, "Demo-2", false, "Demo-2",
	)
	if err != nil || numeric.Name != "Demo-2-2" {
		t.Fatalf("numeric base suffix = %#v, %v", numeric, err)
	}
}

func TestSyncAdmissionTreatsSuppliedWorkspaceNamesAsOccupied(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	physical := []string{"Demo", "Demo-2"}
	preview, err := store.PreviewAdmission(
		ctx, "/work/Demo", domain.ProjectSync, "Demo", false, physical...,
	)
	if err != nil || preview.Name != "Demo-3" || preview.Reservation != nil {
		t.Fatalf("preview with retained workspaces = %#v, %v", preview, err)
	}
	admission, err := store.Admit(
		ctx, "workspace-op", "/work/Demo", domain.ProjectSync, "Demo", false, physical...,
	)
	if err != nil || admission.Name != "Demo-3" {
		t.Fatalf("admit with retained workspaces = %#v, %v", admission, err)
	}
}

func TestPreviewAdmissionDoesNotPruneExpiredReservations(t *testing.T) {
	store := newTestStore(t)
	expired := ProjectReservation{
		Schema: 1, OperationID: "expired-op", ProjectID: "Demo", Name: "Demo",
		Source: "/work/Demo", Mode: domain.ProjectSync, Requested: "Demo",
		CreatedAt: time.Now().UTC().Add(-reservationTTL - time.Minute),
	}
	if err := store.writeReservation(expired); err != nil {
		t.Fatal(err)
	}
	path := store.reservationPath(expired.OperationID)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := store.PreviewAdmission(
		context.Background(), expired.Source, domain.ProjectSync, "Demo", false,
	)
	if err != nil || preview.Name != "Demo" {
		t.Fatalf("preview with expired reservation = %#v, %v", preview, err)
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("read-only preview mutated expired reservation: %v", err)
	}
}

func TestAdmissionPrunesExpiredReservationBeforePendingModeCheck(t *testing.T) {
	for _, mode := range []domain.ProjectMode{domain.ProjectBind, domain.ProjectGit} {
		t.Run(string(mode), func(t *testing.T) {
			store := newTestStore(t)
			expired := ProjectReservation{
				Schema: 1, OperationID: "expired-op", ProjectID: "Demo", Name: "Demo",
				Source: "/work/Demo", Mode: mode, Requested: "Demo",
				CreatedAt: time.Now().UTC().Add(-reservationTTL - time.Minute),
			}
			if err := store.writeReservation(expired); err != nil {
				t.Fatal(err)
			}
			admission, err := store.Admit(
				context.Background(), "fresh-op", expired.Source, mode, "Demo", false,
			)
			if err != nil || admission.Name != "Demo" {
				t.Fatalf("expired reservation blocked admission: %#v, %v", admission, err)
			}
			if _, err := os.Stat(store.reservationPath(expired.OperationID)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("mutating admission retained expired reservation: %v", err)
			}
		})
	}
}

func TestRepeatedBindAdmissionKeepsExistingSemantics(t *testing.T) {
	for _, test := range []struct {
		name   string
		mode   domain.ProjectMode
		source string
	}{
		{name: "bind", mode: domain.ProjectBind, source: "/work/bound"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newTestStore(t)
			ctx := context.Background()
			first, err := store.Admit(ctx, "first-op", test.source, test.mode, "Demo", false)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Admit(
				ctx, "pending-op", test.source, test.mode, "Demo", false,
			); !errors.Is(err, ErrAdmissionPending) {
				t.Fatalf("pending repeated admission = %v", err)
			}
			record := admittedRecord(first, test.source, test.mode)
			if err := store.FinalizeOperation(ctx, first.Reservation.OperationID, record); err != nil {
				t.Fatal(err)
			}
			repeated, err := store.Admit(ctx, "second-op", test.source, test.mode, "Demo", false)
			if err != nil || repeated.Existing == nil || repeated.Reservation != nil ||
				repeated.ProjectID != first.ProjectID {
				t.Fatalf("repeated admission = %#v, %v", repeated, err)
			}
		})
	}
}

func TestFinalizeAdmissionRejectsMismatchedSourceFingerprint(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	admission, err := store.Admit(
		ctx, "fingerprint-op", "/work/Demo", domain.ProjectSync, "Demo", false,
	)
	if err != nil {
		t.Fatal(err)
	}
	record := admittedRecord(admission, "/work/Demo", domain.ProjectSync)
	record.SourceKey = strings.Repeat("0", 64)
	if err := store.FinalizeOperation(ctx, admission.Reservation.OperationID, record); err == nil {
		t.Fatal("finalize accepted a fingerprint unrelated to the reserved source")
	}
}

func TestAdmissionOperationIDReplayIsStableAndRejectsChangedRequest(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	first, err := store.Admit(ctx, "replay-op", "/work/Demo", domain.ProjectSync, "Demo", false)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := store.Admit(ctx, "replay-op", "/work/Demo", domain.ProjectSync, "Demo", false)
	if err != nil || first.Reservation == nil || replayed.Reservation == nil ||
		*replayed.Reservation != *first.Reservation {
		t.Fatalf("operation replay = %#v, %v", replayed, err)
	}
	if _, err := store.Admit(
		ctx, "replay-op", "/work/Other", domain.ProjectSync, "Other", false,
	); err == nil || !strings.Contains(err.Error(), "different reservation") {
		t.Fatalf("changed operation replay was accepted: %v", err)
	}
	if _, err := store.Admit(
		ctx, "replay-op", "/work/Demo", domain.ProjectSync, "Demo", true,
	); err == nil || !strings.Contains(err.Error(), "different admission request") {
		t.Fatalf("changed same-source replay was accepted: %v", err)
	}
}

func admittedRecord(admission Admission, source string, mode domain.ProjectMode) domain.ProjectRecord {
	record := fixtureRecord(admission.ProjectID)
	record.IdentityVersion = 2
	record.Name = admission.Name
	record.HostPath = source
	record.SourceKey = SourceKey(source)
	record.YardPath = YardPath(admission.ProjectID)
	record.Mode = mode
	return record
}
