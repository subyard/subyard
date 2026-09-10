package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestYardRegistrationRepairMovesFlatRegistrationToRecovery(t *testing.T) {
	configHome, nestedPath, flatPath := registrationRepairFixture(t)
	nestedBefore, err := os.Lstat(nestedPath)
	if err != nil {
		t.Fatal(err)
	}
	flatBefore, err := os.Lstat(flatPath)
	if err != nil {
		t.Fatal(err)
	}

	repair, err := PlanYardRegistrationRepair(configHome, "named")
	if err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(configHome, "recovery", "yard-registrations", "named.env")
	if !repair.Changed() || repair.NestedPath != nestedPath || repair.FlatPath != flatPath ||
		repair.ArchivePath != archivePath {
		t.Fatalf("repair plan = %#v", repair)
	}
	if _, err := os.Lstat(filepath.Join(configHome, "recovery")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only plan created recovery state: %v", err)
	}
	nestedAfterPlan, err := os.Lstat(nestedPath)
	if err != nil || !os.SameFile(nestedBefore, nestedAfterPlan) {
		t.Fatalf("read-only plan changed nested registration: %v", err)
	}
	flatAfterPlan, err := os.Lstat(flatPath)
	if err != nil || !os.SameFile(flatBefore, flatAfterPlan) {
		t.Fatalf("read-only plan changed flat registration: %v", err)
	}

	if err := repair.Apply(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(flatPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("flat registration remained active: %v", err)
	}
	archive, err := os.ReadFile(archivePath)
	if err != nil || !bytes.Equal(archive, []byte("YARD_TEMPLATE=e2e-vms\nSSH_PORT=3333\n")) {
		t.Fatalf("archived registration = %q, err=%v", archive, err)
	}
	archiveInfo, err := os.Lstat(archivePath)
	if err != nil || !os.SameFile(flatBefore, archiveInfo) || archiveInfo.Mode().Perm() != 0o640 {
		t.Fatalf("archive identity/mode = %v, err=%v", archiveInfo, err)
	}
	nestedAfter, err := os.Lstat(nestedPath)
	if err != nil || !os.SameFile(nestedBefore, nestedAfter) {
		t.Fatalf("repair changed nested registration: %v", err)
	}
	for _, directory := range []string{
		filepath.Join(configHome, "recovery"),
		filepath.Join(configHome, "recovery", "yard-registrations"),
	} {
		info, err := os.Lstat(directory)
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("recovery directory %s mode=%v", directory, info.Mode())
		}
	}

	recovered, err := PlanYardRegistrationRepair(configHome, "named")
	if err != nil || recovered.Changed() {
		t.Fatalf("recovered plan = %#v, err=%v", recovered, err)
	}
	if err := recovered.Apply(); err != nil {
		t.Fatalf("recovered no-op apply: %v", err)
	}
}

func TestPlanYardRegistrationRepairRejectsInvalidOrIncompleteRegistration(t *testing.T) {
	configHome := t.TempDir()
	if err := os.Chmod(configHome, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "default", "../escape"} {
		if _, err := PlanYardRegistrationRepair(configHome, name); err == nil {
			t.Fatalf("accepted unsafe yard name %q", name)
		}
	}
	if _, err := PlanYardRegistrationRepair(configHome, "named"); err == nil {
		t.Fatal("accepted a missing nested registration")
	}
}

func TestPlanYardRegistrationRepairRejectsArchiveConflict(t *testing.T) {
	configHome, _, flatPath := registrationRepairFixture(t)
	archivePath := filepath.Join(configHome, "recovery", "yard-registrations", "named.env")
	writeRegistrationRepairFile(t, archivePath, []byte("existing archive\n"), 0o600)

	if _, err := PlanYardRegistrationRepair(configHome, "named"); err == nil {
		t.Fatal("accepted an existing archive for an active flat registration")
	}
	if content, err := os.ReadFile(flatPath); err != nil || len(content) == 0 {
		t.Fatalf("archive conflict changed flat registration: %q, err=%v", content, err)
	}
}

func TestPlanYardRegistrationRepairRejectsNonPrivateRecoveryDirectory(t *testing.T) {
	configHome, _, _ := registrationRepairFixture(t)
	recovery := filepath.Join(configHome, "recovery")
	if err := os.Mkdir(recovery, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(recovery, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := PlanYardRegistrationRepair(configHome, "named"); err == nil {
		t.Fatal("plan accepted a non-private recovery directory")
	}
}

func TestYardRegistrationRepairRejectsSnapshotDrift(t *testing.T) {
	for _, target := range []string{"nested", "flat"} {
		t.Run(target, func(t *testing.T) {
			configHome, nestedPath, flatPath := registrationRepairFixture(t)
			repair, err := PlanYardRegistrationRepair(configHome, "named")
			if err != nil {
				t.Fatal(err)
			}
			path := map[string]string{"nested": nestedPath, "flat": flatPath}[target]
			replacement := path + ".replacement"
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(replacement, content, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(replacement, path); err != nil {
				t.Fatal(err)
			}

			if err := repair.Apply(); !errors.Is(err, ErrPersistentTargetStale) {
				t.Fatalf("snapshot drift error = %v", err)
			}
			if _, err := os.Lstat(flatPath); err != nil {
				t.Fatalf("snapshot drift removed flat registration: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(configHome, "recovery")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("snapshot drift created recovery state: %v", err)
			}
		})
	}
}

func TestYardRegistrationRepairRejectsArchiveRaceAndSymlinkTraversal(t *testing.T) {
	t.Run("archive appeared", func(t *testing.T) {
		configHome, _, flatPath := registrationRepairFixture(t)
		repair, err := PlanYardRegistrationRepair(configHome, "named")
		if err != nil {
			t.Fatal(err)
		}
		archivePath := filepath.Join(configHome, "recovery", "yard-registrations", "named.env")
		writeRegistrationRepairFile(t, archivePath, []byte("raced archive\n"), 0o600)
		if err := repair.Apply(); !errors.Is(err, ErrPersistentTargetStale) {
			t.Fatalf("archive race error = %v", err)
		}
		if _, err := os.Lstat(flatPath); err != nil {
			t.Fatalf("archive race removed flat registration: %v", err)
		}
	})

	t.Run("recovery symlink appeared", func(t *testing.T) {
		configHome, _, flatPath := registrationRepairFixture(t)
		repair, err := PlanYardRegistrationRepair(configHome, "named")
		if err != nil {
			t.Fatal(err)
		}
		external := t.TempDir()
		if err := os.Symlink(external, filepath.Join(configHome, "recovery")); err != nil {
			t.Fatal(err)
		}
		if err := repair.Apply(); err == nil {
			t.Fatal("repair followed a recovery-directory symlink")
		}
		if _, err := os.Lstat(flatPath); err != nil {
			t.Fatalf("symlink traversal removed flat registration: %v", err)
		}
		entries, err := os.ReadDir(external)
		if err != nil || len(entries) != 0 {
			t.Fatalf("symlink traversal wrote outside config home: entries=%v err=%v", entries, err)
		}
	})

	t.Run("non-private recovery appeared", func(t *testing.T) {
		configHome, _, flatPath := registrationRepairFixture(t)
		repair, err := PlanYardRegistrationRepair(configHome, "named")
		if err != nil {
			t.Fatal(err)
		}
		recovery := filepath.Join(configHome, "recovery")
		if err := os.Mkdir(recovery, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(recovery, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := repair.Apply(); err == nil {
			t.Fatal("repair accepted a non-private recovery directory")
		}
		if _, err := os.Lstat(flatPath); err != nil {
			t.Fatalf("unsafe recovery directory removed flat registration: %v", err)
		}
	})
}

func TestYardRegistrationRepairRechecksSourcesAfterPreparingRecovery(t *testing.T) {
	configHome, nestedPath, flatPath := registrationRepairFixture(t)
	repair, err := PlanYardRegistrationRepair(configHome, "named")
	if err != nil {
		t.Fatal(err)
	}
	err = repair.apply(func() error {
		replacement := nestedPath + ".replacement"
		content, err := os.ReadFile(nestedPath)
		if err != nil {
			return err
		}
		if err := os.WriteFile(replacement, content, 0o600); err != nil {
			return err
		}
		return os.Rename(replacement, nestedPath)
	})
	if !errors.Is(err, ErrPersistentTargetStale) {
		t.Fatalf("late source drift error = %v", err)
	}
	if _, err := os.Lstat(flatPath); err != nil {
		t.Fatalf("late source drift removed flat registration: %v", err)
	}
}

func registrationRepairFixture(t *testing.T) (string, string, string) {
	t.Helper()
	configHome := t.TempDir()
	if err := os.Chmod(configHome, 0o700); err != nil {
		t.Fatal(err)
	}
	nestedPath := filepath.Join(configHome, "yards", "named", "config.env")
	flatPath := filepath.Join(configHome, "yards", "named.env")
	writeRegistrationRepairFile(t, nestedPath, []byte("YARD_TEMPLATE=test-vms\nSSH_PORT=2223\n"), 0o600)
	writeRegistrationRepairFile(t, flatPath, []byte("YARD_TEMPLATE=e2e-vms\nSSH_PORT=3333\n"), 0o640)
	return configHome, nestedPath, flatPath
}

func writeRegistrationRepairFile(t *testing.T, path string, content []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, mode); err != nil {
		t.Fatal(err)
	}
}
