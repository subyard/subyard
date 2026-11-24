package releasetransition

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/sys/unix"
)

// This helper is a test-binary subprocess, never a production fault switch.
func TestOwnerPrivatePublicationKilledWriter(t *testing.T) {
	home := os.Getenv("SUBYARD_TEST_PRIVATE_RECEIPT_HOME")
	if home == "" {
		t.Skip("child helper")
	}
	payload, err := os.ReadFile(filepath.Join(home, "intended-receipt"))
	if err != nil {
		t.Fatal(err)
	}
	cut, err := strconv.Atoi(os.Getenv("SUBYARD_TEST_PRIVATE_RECEIPT_CUT"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewPOSIXV2Store(home)
	if err != nil {
		t.Fatal(err)
	}
	store.fault = func(point string) error {
		if point != "after-private-create" {
			return nil
		}
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			return err
		}
		for _, entry := range entries {
			fd, err := strconv.Atoi(entry.Name())
			if err != nil {
				continue
			}
			target, err := os.Readlink("/proc/self/fd/" + entry.Name())
			if err != nil || !strings.HasPrefix(target, home+"/") || !strings.HasSuffix(target, " (deleted)") {
				continue
			}
			var st unix.Stat_t
			if err := unix.Fstat(fd, &st); err != nil || st.Nlink != 0 || st.Mode&unix.S_IFMT != unix.S_IFREG {
				continue
			}
			if n, err := unix.Write(fd, payload[:cut]); err != nil || n != cut {
				return fmt.Errorf("private prefix write: %d %v", n, err)
			}
			if err := unix.Fsync(fd); err != nil {
				return err
			}
			return unix.Kill(os.Getpid(), unix.SIGKILL)
		}
		return errors.New("held private inode not found")
	}
	if err := store.createOwnerImmutable(migrationOwnerReceiptParts, "tx-private.json", payload); err != nil {
		t.Fatal(err)
	}
	t.Fatal("child survived selected write cut")
}

func TestMigrationOwnerPrivatePrefixKillFreshRetry(t *testing.T) {
	for _, cutName := range []string{"zero", "early", "late"} {
		t.Run(cutName, func(t *testing.T) {
			candidate, receipt, links := migrationOwnerReaderFixture(t, false)
			payload, err := MarshalMigrationOwnerReceipt(receipt)
			if err != nil {
				t.Fatal(err)
			}
			// The old engine's intended bytes are a direct fixture. A killed
			// private publisher must expose neither a prefix nor a reservation.
			seedMigrationOwnerReceipt(t, candidate, receipt)
			if err := os.Remove(filepath.Join(candidate.options.ConfigHome, filepath.Join(migrationOwnerReceiptParts...), string(receipt.Successor.Transaction)+".json")); err != nil {
				t.Fatal(err)
			}
			cut := 0
			if cutName == "early" {
				cut = 18
			}
			if cutName == "late" {
				cut = len(payload) - 1
			}
			testkit.WriteFile(t, filepath.Join(candidate.options.ConfigHome, "intended-receipt"), payload, 0600)
			before := snapshotStoreV2Tree(t, candidate.options.ConfigHome)
			cmd := exec.Command(os.Args[0], "-test.run=^TestOwnerPrivatePublicationKilledWriter$", "-test.v")
			cmd.Env = append(os.Environ(), "SUBYARD_TEST_PRIVATE_RECEIPT_HOME="+candidate.options.ConfigHome, "SUBYARD_TEST_PRIVATE_RECEIPT_CUT="+strconv.Itoa(cut))
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatalf("expected killed writer: %v %s", err, output)
			}
			entries, err := os.ReadDir(filepath.Join(candidate.options.ConfigHome, filepath.Join(migrationOwnerReceiptParts...)))
			if err != nil || len(entries) != 0 {
				t.Fatalf("private write exposed receipt staging: %v %v", entries, err)
			}
			if !bytes.Equal(before, snapshotStoreV2Tree(t, candidate.options.ConfigHome)) || links.Active != "release-b" {
				t.Fatal("private write changed predecessor")
			}
			if reserved, err := candidate.store.journalIsReserved(receipt.Predecessor.Transaction); err != nil || reserved {
				t.Fatalf("private staging became reservation: %v %v", reserved, err)
			}
			if err := candidate.store.createOwnerImmutable(migrationOwnerReceiptParts, string(receipt.Successor.Transaction)+".json", payload); err != nil {
				t.Fatal(err)
			}
			receipts, err := candidate.store.migrationOwnerReceipts()
			if err != nil || len(receipts) != 1 {
				t.Fatalf("fresh publication: %v %v", receipts, err)
			}
			for _, actual := range receipts {
				if actual.Replacement != receipt.Replacement {
					t.Fatal("wrong predecessor")
				}
				if err := candidate.store.migrationOwnerEvidence(actual.Predecessor); err != nil {
					t.Fatal(err)
				}
			}

		})
	}
}

func TestOwnerPrivatePublicationImmutableAndLegacyCanonicalPending(t *testing.T) {
	for _, namespace := range [][]string{migrationOwnerReceiptParts, migrationOwnerCompletedParts} {
		t.Run(namespace[len(namespace)-1], func(t *testing.T) {
			home := protectedConfigHome(t)
			store, err := NewPOSIXV2Store(home)
			if err != nil {
				t.Fatal(err)
			}
			payload := []byte("canonical owner record\n")
			for _, point := range []string{"after-private-create", "after-pending-fsync", "before-publish", "after-publish-before-dir-fsync"} {
				store.fault = func(actual string) error {
					if actual == point {
						return errors.New("selected private publication cut")
					}
					return nil
				}
				if err := store.createOwnerImmutable(namespace, "tx-private.json", payload); err == nil {
					t.Fatal("fault not reached")
				}
				dir := filepath.Join(home, filepath.Join(namespace...))
				entries, _ := os.ReadDir(dir)
				if point != "after-publish-before-dir-fsync" && len(entries) != 0 {
					t.Fatalf("visible partial record after %s: %v", point, entries)
				}
				store.fault = nil
				if err := store.createOwnerImmutable(namespace, "tx-private.json", payload); err != nil {
					t.Fatal(err)
				}
				assertProtectedFile(t, filepath.Join(dir, "tx-private.json"))
				if err := store.createOwnerImmutable(namespace, "tx-private.json", []byte("foreign\n")); !errors.Is(err, ErrProtectedStoreExists) {
					t.Fatalf("conflicting immutable write: %v", err)
				}
				if err := os.Remove(filepath.Join(dir, "tx-private.json")); err != nil {
					t.Fatal(err)
				}
			}
			dir := filepath.Join(home, filepath.Join(namespace...))
			pending := filepath.Join(dir, ".tx-private.json.pending")
			testkit.WriteFile(t, pending, payload, 0600)
			if err := store.createOwnerImmutable(namespace, "tx-private.json", payload); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(pending); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("legacy canonical pending not retired: %v", err)
			}
		})
	}
}

func TestOwnerPrivatePublicationAdmissionFailureNoProtectedWrites(t *testing.T) {
	home := protectedConfigHome(t)
	store, err := NewPOSIXV2Store(home)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	store.fault = func(point string) error {
		if point == "before-private-create" {
			return unix.EOPNOTSUPP
		}
		return nil
	}
	if err := store.createOwnerImmutable(migrationOwnerReceiptParts, "tx-private.json", []byte("canonical\n")); !errors.Is(err, unix.EOPNOTSUPP) {
		t.Fatalf("unsupported admission: %v", err)
	}
	after, err := os.ReadDir(home)
	if err != nil || len(before) != len(after) {
		t.Fatalf("unsupported admission created protected records: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(home, filepath.Join(migrationOwnerReceiptParts...))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsupported admission created namespace: %v", err)
	}
}

func TestOwnerPrivatePublicationEveryPrefixAndNoReplace(t *testing.T) {
	payload := []byte("canonical owner record\n")
	for cut := 0; cut <= len(payload); cut++ {
		t.Run(strconv.Itoa(cut), func(t *testing.T) {
			home := protectedConfigHome(t)
			store, err := NewPOSIXV2Store(home)
			if err != nil {
				t.Fatal(err)
			}
			store.fault = func(point string) error {
				if point != "after-private-create" {
					return nil
				}
				entries, err := os.ReadDir("/proc/self/fd")
				if err != nil {
					return err
				}
				for _, entry := range entries {
					target, err := os.Readlink("/proc/self/fd/" + entry.Name())
					if err != nil || !strings.HasPrefix(target, home+"/") || !strings.HasSuffix(target, " (deleted)") {
						continue
					}
					fd, err := strconv.Atoi(entry.Name())
					if err != nil {
						return err
					}
					if n, err := unix.Write(fd, payload[:cut]); err != nil || n != cut {
						return fmt.Errorf("prefix: %d %v", n, err)
					}
					return errors.New("writer lost at prefix")
				}
				return errors.New("private inode not found")
			}
			if err := store.createOwnerImmutable(migrationOwnerCompletedParts, "tx-private.json", payload); err == nil {
				t.Fatal("selected cut not reached")
			}
			entries, err := os.ReadDir(home)
			if err != nil || len(entries) != 0 {
				t.Fatalf("prefix%d visible: %v %v", cut, entries, err)
			}
			store.fault = nil
			if err := store.createOwnerImmutable(migrationOwnerCompletedParts, "tx-private.json", payload); err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Run("competing-final", func(t *testing.T) {
		home := protectedConfigHome(t)
		store, _ := NewPOSIXV2Store(home)
		final := filepath.Join(home, filepath.Join(migrationOwnerReceiptParts...), "tx-private.json")
		store.fault = func(point string) error {
			if point == "before-publish" {
				testkit.WriteFile(t, final, []byte("other canonical record\n"), 0600)
			}
			return nil
		}
		if err := store.createOwnerImmutable(migrationOwnerReceiptParts, "tx-private.json", payload); !errors.Is(err, ErrProtectedStoreExists) {
			t.Fatalf("no-replace: %v", err)
		}
		actual, err := os.ReadFile(final)
		if err != nil || !bytes.Equal(actual, []byte("other canonical record\n")) {
			t.Fatalf("competing final overwritten: %s %v", actual, err)
		}
	})
}

func TestOwnerPrivatePublicationRejectsUnsafeLegacyStaging(t *testing.T) {
	for _, mutant := range []string{"nonprefix", "unsafe-mode", "symlink", "hardlink"} {
		t.Run(mutant, func(t *testing.T) {
			home := protectedConfigHome(t)
			store, _ := NewPOSIXV2Store(home)
			parent, present, err := store.openParent(migrationOwnerReceiptParts, true)
			if err != nil || !present {
				t.Fatalf("fixture parent: %v", err)
			}
			unix.Close(parent)
			dir := filepath.Join(home, filepath.Join(migrationOwnerReceiptParts...))
			pending := filepath.Join(dir, ".tx-private.json.pending")
			mode := os.FileMode(0600)
			if mutant == "unsafe-mode" {
				mode = 0644
			}
			if mutant == "symlink" || mutant == "hardlink" {
				target := filepath.Join(home, "foreign")
				testkit.WriteFile(t, target, []byte("canonical\n"), 0600)
				if mutant == "symlink" {
					err = os.Symlink(target, pending)
				} else {
					err = os.Link(target, pending)
				}
				if err != nil {
					t.Fatal(err)
				}
			} else {
				testkit.WriteFile(t, pending, []byte("foreign\n"), mode)
			}
			before, err := os.Lstat(pending)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.createOwnerImmutable(migrationOwnerReceiptParts, "tx-private.json", []byte("canonical\n")); err == nil {
				t.Fatal("unsafe legacy staging admitted")
			}
			after, err := os.Lstat(pending)
			wantMode := before.Mode()
			if mutant == "unsafe-mode" {
				wantMode = 0o600 // Only the operator's own broader mode is repaired.
			}
			if err != nil || !os.SameFile(before, after) || after.Mode() != wantMode || before.Size() != after.Size() {
				t.Fatalf("unsafe staging changed: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(dir, "tx-private.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("refusal published final: %v", err)
			}
		})
	}
}
