package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestWritePersistentAssignmentPreservesUnrelatedRecords(t *testing.T) {
	configHome := filepath.Join(t.TempDir(), "config")
	if err := os.MkdirAll(configHome, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(configHome, "config.env")
	initial := "# keep this comment\nSSH_PORT=2200\nDEV_USER='dev'\n"
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	value := "2299"
	if err := WritePersistentAssignment(
		configHome, path, "SSH_PORT", &value,
	); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "# keep this comment\n") ||
		!strings.Contains(string(content), "DEV_USER='dev'\n") ||
		!strings.Contains(string(content), "SSH_PORT='2299'\n") {
		t.Fatalf("unexpected edited config:\n%s", content)
	}
	if err := WritePersistentAssignment(
		configHome, path, "SSH_PORT", nil,
	); err != nil {
		t.Fatal(err)
	}
	content, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "SSH_PORT") ||
		!strings.Contains(string(content), "DEV_USER='dev'") {
		t.Fatalf("unexpected unset config:\n%s", content)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("persistent config mode = %o", info.Mode().Perm())
	}
}

func TestCreatePersistentFileRefusesExistingTarget(t *testing.T) {
	configHome := filepath.Join(t.TempDir(), "config")
	if err := os.MkdirAll(configHome, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(configHome, "yards", "sample-service", "config.env")
	content := []byte("ENVIRONMENT_PROFILES=sample-service\n")
	if err := CreatePersistentFile(configHome, path, content); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || string(stored) != string(content) {
		t.Fatalf("unexpected persistent file: mode=%v content=%q", info.Mode(), stored)
	}
	if err := CreatePersistentFile(configHome, path, []byte("CODING_TOOL_INTEGRATIONS=claude\n")); err == nil ||
		!strings.Contains(err.Error(), "already exists") {
		t.Fatalf("existing target error=%v", err)
	}
	stored, err = os.ReadFile(path)
	if err != nil || string(stored) != string(content) {
		t.Fatalf("existing target was changed: content=%q err=%v", stored, err)
	}
}

func TestCreatePersistentFileCreatesMissingConfigurationRoot(t *testing.T) {
	configHome := filepath.Join(t.TempDir(), "missing")
	path := filepath.Join(configHome, "yards", "sample-service", "config.env")
	content := []byte("ENVIRONMENT_PROFILES=sample-service\n")
	if err := CreatePersistentFile(configHome, path, content); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(path)
	if err != nil || string(stored) != string(content) {
		t.Fatalf("persistent file content=%q err=%v", stored, err)
	}
	info, err := os.Lstat(configHome)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("configuration root mode=%v err=%v", info.Mode(), err)
	}
}

func TestCreatePersistentFileGuardHoldsExclusiveRootWithoutPublishing(t *testing.T) {
	root := testkit.TempDir(t)
	path := filepath.Join(root, "yards", "new", "config.env")
	rejected := errors.New("approved inputs changed")
	called := false
	err := CreatePersistentFile(root, path, []byte("SSH_PORT=2444\n"), func() error {
		called = true
		probe, err := os.Open(root)
		if err != nil {
			return err
		}
		defer probe.Close()
		if err := syscall.Flock(int(probe.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = syscall.Flock(int(probe.Fd()), syscall.LOCK_UN)
			return fmt.Errorf("create guard lacks exclusive configuration root: %v", err)
		}
		return rejected
	})
	if !called || !errors.Is(err, rejected) {
		t.Fatalf("guard called=%v err=%v", called, err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected create guard published a target: %v", err)
	}
}

func TestReadPersistentFileSnapshotIsStrictlyReadOnlyAndProtected(t *testing.T) {
	configHome := t.TempDir()
	if err := os.Chmod(configHome, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(configHome, "yards", "sample-service", "config.env")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("YARD_TEMPLATE=test-vms\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := ReadPersistentFileSnapshot(configHome, path)
	if err != nil || !snapshot.Exists || string(snapshot.Content) != "YARD_TEMPLATE=test-vms\n" {
		t.Fatalf("snapshot = %#v, err=%v", snapshot, err)
	}
	if snapshot.Identity == (PersistentFileIdentity{}) {
		t.Fatal("protected snapshot did not bind file identity and metadata")
	}
	after, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || before.Mode() != after.Mode() {
		t.Fatal("read-only snapshot changed file identity or mode")
	}

	missing := filepath.Join(configHome, "yards", "sample-service", "missing.env")
	missingSnapshot, err := ReadPersistentFileSnapshot(configHome, missing)
	if err != nil || missingSnapshot.Exists {
		t.Fatalf("missing snapshot = %#v, err=%v", missingSnapshot, err)
	}
	if _, err := os.Lstat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only snapshot created missing path: %v", err)
	}
	missingTree := filepath.Join(configHome, "overrides", "shared", "config.env")
	missingSnapshot, err = ReadPersistentFileSnapshot(configHome, missingTree)
	if err != nil || missingSnapshot.Exists {
		t.Fatalf("missing-tree snapshot = %#v, err=%v", missingSnapshot, err)
	}
	if _, err := os.Lstat(filepath.Join(configHome, "overrides")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only snapshot created missing directory: %v", err)
	}
}

func TestCompareAndSwapPersistentFileUsesProtectedIdentity(t *testing.T) {
	configHome := t.TempDir()
	if err := os.Chmod(configHome, 0o700); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(configHome, "yards", "sample-service")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "config.env")
	before := []byte("YARD_TEMPLATE=e2e-vms\n")
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	expected, err := ReadPersistentFileSnapshot(configHome, path)
	if err != nil {
		t.Fatal(err)
	}

	replacement := filepath.Join(directory, "replacement")
	if err := os.WriteFile(replacement, before, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if err := CompareAndSwapPersistentFile(
		configHome, path, expected, []byte("YARD_TEMPLATE=test-vms\n"),
	); !errors.Is(err, ErrPersistentTargetStale) {
		t.Fatalf("same-bytes identity drift error = %v", err)
	}
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, before) {
		t.Fatalf("identity drift was overwritten: %q, err=%v", actual, err)
	}

	expected, err = ReadPersistentFileSnapshot(configHome, path)
	if err != nil {
		t.Fatal(err)
	}
	desired := []byte("YARD_TEMPLATE=test-vms\n")
	if err := CompareAndSwapPersistentFile(configHome, path, expected, desired); err != nil {
		t.Fatal(err)
	}
	actual, err = os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, desired) {
		t.Fatalf("CAS result = %q, err=%v", actual, err)
	}
}

func TestCompareAndSwapPersistentFileConvergesAcrossPublicationFaults(t *testing.T) {
	for _, point := range []string{"after-pending-fsync", "after-publish-before-dir-fsync"} {
		t.Run(point, func(t *testing.T) {
			configHome := t.TempDir()
			if err := os.Chmod(configHome, 0o700); err != nil {
				t.Fatal(err)
			}
			directory := filepath.Join(configHome, "yards", "sample-service")
			if err := os.MkdirAll(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(directory, "config.env")
			if err := os.WriteFile(path, []byte("YARD_TEMPLATE=e2e-vms\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			expected, err := ReadPersistentFileSnapshot(configHome, path)
			if err != nil {
				t.Fatal(err)
			}
			desired := []byte("YARD_TEMPLATE=test-vms\n")
			injected := errors.New("injected persistent CAS fault")
			err = compareAndSwapPersistentFile(
				configHome, path, expected, desired,
				func(actual string) error {
					if actual == point {
						return injected
					}
					return nil
				},
			)
			if !errors.Is(err, injected) {
				t.Fatalf("fault error = %v", err)
			}
			actual, err := ReadPersistentFileSnapshot(configHome, path)
			if err != nil {
				t.Fatal(err)
			}
			switch point {
			case "after-pending-fsync":
				if !samePersistentFileSnapshotExact(actual, expected) {
					t.Fatalf("pre-publication state = %#v", actual)
				}
				if err := CompareAndSwapPersistentFile(configHome, path, expected, desired); err != nil {
					t.Fatalf("resume pending CAS: %v", err)
				}
			case "after-publish-before-dir-fsync":
				if !bytes.Equal(actual.Content, desired) {
					t.Fatalf("post-publication state = %#v", actual)
				}
				if err := CompareAndSwapPersistentFile(configHome, path, expected, desired); !errors.Is(err, ErrPersistentTargetStale) {
					t.Fatalf("post-publication retry error = %v", err)
				}
			}
		})
	}
}

func TestReadPersistentFileSnapshotRejectsUnsafePaths(t *testing.T) {
	configHome := t.TempDir()
	if err := os.Chmod(configHome, 0o700); err != nil {
		t.Fatal(err)
	}
	safeDirectory := filepath.Join(configHome, "yards", "safe")
	if err := os.MkdirAll(safeDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	safe := filepath.Join(safeDirectory, "config.env")
	if err := os.WriteFile(safe, []byte("SSH_PORT=2222\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("escape", func(t *testing.T) {
		if _, err := ReadPersistentFileSnapshot(configHome, filepath.Join(configHome, "..", "foreign")); err == nil {
			t.Fatal("accepted path outside config home")
		}
	})
	t.Run("symlinked parent", func(t *testing.T) {
		link := filepath.Join(configHome, "linked")
		if err := os.Symlink(safeDirectory, link); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadPersistentFileSnapshot(configHome, filepath.Join(link, "config.env")); err == nil {
			t.Fatal("accepted symlinked parent")
		}
	})
	t.Run("group writable", func(t *testing.T) {
		if err := os.Chmod(safe, 0o660); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(safe, 0o600)
		if _, err := ReadPersistentFileSnapshot(configHome, safe); err == nil {
			t.Fatal("accepted group-writable file")
		}
	})
	t.Run("hard linked", func(t *testing.T) {
		alias := filepath.Join(safeDirectory, "alias.env")
		if err := os.Link(safe, alias); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(alias)
		if _, err := ReadPersistentFileSnapshot(configHome, safe); err == nil {
			t.Fatal("accepted hard-linked file")
		}
	})
	t.Run("symlinked file", func(t *testing.T) {
		link := filepath.Join(safeDirectory, "link.env")
		if err := os.Symlink(safe, link); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadPersistentFileSnapshot(configHome, link); err == nil {
			t.Fatal("accepted symlinked file")
		}
	})
}

func TestParsePersistentAssignmentsPreservesDuplicateAndDynamicProvenance(t *testing.T) {
	content := []byte("# retained\nYARD_TEMPLATE=e2e-vms\n" +
		"export YARD_TEMPLATE='test-vms'\n" +
		": ${NESTED_E2E_VMS:=0}\nNESTED_E2E_VMS=\"$NESTED_E2E_VMS\"\n")
	assignments, err := ParsePersistentAssignments("fixture.env", content)
	if err != nil {
		t.Fatal(err)
	}
	if len(assignments) != 4 {
		t.Fatalf("assignments = %#v", assignments)
	}
	want := []PersistentAssignment{
		{Name: "YARD_TEMPLATE", Value: "e2e-vms", Line: 2, Direct: true},
		{Name: "YARD_TEMPLATE", Value: "test-vms", Line: 3, Direct: true},
		{Name: "NESTED_E2E_VMS", Value: "0", Line: 4, Dynamic: true},
		{Name: "NESTED_E2E_VMS", Value: "0", Line: 5, Direct: true, Dynamic: true},
	}
	for index := range want {
		if assignments[index] != want[index] {
			t.Fatalf("assignment %d = %#v, want %#v", index, assignments[index], want[index])
		}
	}
}

func TestReadPersistentFileSnapshotRejectsForeignOwnershipWhenSupported(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to construct a foreign-owned fixture")
	}
	configHome := t.TempDir()
	if err := os.Chmod(configHome, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(configHome, "config.env")
	if err := os.WriteFile(path, []byte("NESTED_E2E_VMS=0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(path, 1, -1); err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skip("filesystem does not permit ownership fixture")
		}
		t.Fatal(err)
	}
	defer os.Chown(path, os.Getuid(), -1)
	if _, err := ReadPersistentFileSnapshot(configHome, path); err == nil {
		t.Fatal("accepted foreign-owned file")
	}
}

func TestSelectionAuthoringReplacesLegacyAlias(t *testing.T) {
	value := "paseo"
	for _, initial := range []string{"AGENTS=codex\n", "AGENTS=none\nCODING_TOOL_INTEGRATIONS=\n"} {
		content, err := EditPersistentAssignmentContent("config.env", []byte("SSH_PORT=2222\n"+initial), "CODING_TOOL_INTEGRATIONS", &value)
		if err != nil {
			t.Fatal(err)
		}
		if string(content) != "SSH_PORT=2222\nCODING_TOOL_INTEGRATIONS='paseo'\n" {
			t.Fatalf("canonical content = %s", content)
		}
		content, err = EditPersistentAssignmentContent("config.env", []byte(initial), "CODING_TOOL_INTEGRATIONS", nil)
		if err != nil || len(content) != 0 {
			t.Fatalf("unset legacy selection = %q, %v", content, err)
		}
	}
}

func TestPersistentAuthoringCanonicalizesEveryAlias(t *testing.T) {
	for legacy, canonical := range legacySettingNames {
		for _, mixed := range []bool{false, true} {
			t.Run(legacy+fmt.Sprint(mixed), func(t *testing.T) {
				initial := "# retained comment\nexport " + legacy + "='before'\n"
				if mixed {
					initial += canonical + "='before'\n"
				}
				initial += "DEV_USER='retained'\n"
				value := "after"
				content, err := EditPersistentAssignmentContent("fixture.env", []byte(initial), canonical, &value)
				if err != nil {
					t.Fatal(err)
				}
				if string(content) != "# retained comment\n"+canonical+"='after'\nDEV_USER='retained'\n" {
					t.Fatalf("set: %s", content)
				}
				content, err = EditPersistentAssignmentContent("fixture.env", []byte(initial), canonical, nil)
				if err != nil {
					t.Fatal(err)
				}
				if string(content) != "# retained comment\nDEV_USER='retained'\n" {
					t.Fatalf("unset: %s", content)
				}
			})
		}
	}
}

func TestPersistentAuthoringPreservesExistingReadPermissions(t *testing.T) {
	root := testkit.TempDir(t)
	path := filepath.Join(root, "config.env")
	testkit.WriteFile(t, path, []byte("YARD_PROFILES=default\n"), 0o640)
	value := "sample"
	before, err := ReadPersistentFileSnapshot(root, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := WritePersistentAssignmentIfUnchanged(root, path, "ENVIRONMENT_PROFILES", &value, before); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("mode: %v, %v", info, err)
	}
}

func TestWritePersistentAssignmentCanonicalizesLegacyAliases(t *testing.T) {
	for _, setting := range []struct {
		legacy, canonical, before, after string
	}{
		{"YARD_TYPE", "ACCESS_KIND", "local", "remote"},
		{"INSTANCE_TYPE", "YARD_KIND", "container", "vm"},
		{"INSTANCE_NAME", "YARD_INSTANCE_NAME", "yard-old", "yard-new"},
		{"REMOTE_DEST", "OWNER_ENDPOINT", "dev@old.example", "dev@new.example"},
		{"REMOTE_YARD", "OWNER_YARD_NAME", "old", "new"},
		{"BASE_IMAGE", "YARD_IMAGE", "images:debian/12", "images:debian/13"},
		{"BASE_IMAGE_FALLBACK", "YARD_IMAGE_FALLBACK", "images:debian/12", "images:debian/13"},
		{"YARD_PROFILES", "ENVIRONMENT_PROFILES", "neighbor", "neighbor fixture"},
		{"AGENTS", "CODING_TOOL_INTEGRATIONS", "codex", "paseo"},
	} {
		for _, mixed := range []bool{false, true} {
			initial := "export " + setting.legacy + "='" + setting.before + "'\n"
			if mixed {
				initial += setting.canonical + "='" + setting.before + "'\n"
			}
			t.Run(initial, func(t *testing.T) {
				configHome := testkit.TempDir(t)
				path := filepath.Join(configHome, "config.env")
				prefix, suffix := "# keep this comment\nSSH_PORT=2222\n", "\nDEV_USER='dev'\n"
				for _, value := range []*string{&setting.after, nil} {
					testkit.WriteFile(t, path, []byte(prefix+initial+suffix), 0o600)
					before, err := ReadPersistentFileSnapshot(configHome, path)
					if err != nil {
						t.Fatal(err)
					}
					if err := WritePersistentAssignmentIfUnchanged(configHome, path, setting.canonical, value, before); err != nil {
						t.Fatal(err)
					}
					content, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					want := prefix + suffix
					if value != nil {
						want = prefix + setting.canonical + "='" + *value + "'\n" + suffix
					}
					if string(content) != want {
						t.Fatalf("canonical content = %q, want %q", content, want)
					}
					loaded := environment{}
					if err := applyEnvFileValidated(path, loaded, ScopeHost, false, nil); err != nil {
						t.Fatal(err)
					}
					if value != nil && loaded[setting.canonical] != *value {
						t.Fatalf("reloaded value = %q, want %q", loaded[setting.canonical], *value)
					}
					if value == nil {
						if _, exists := loaded[setting.canonical]; exists {
							t.Fatal("unset retained the selection")
						}
					}
				}
			})
		}
	}
}

func TestWritePersistentAssignmentAliasRejectsStaleAndInvalidTargets(t *testing.T) {
	for _, condition := range []string{"stale content", "stale identity", "invalid record", "unsafe mode"} {
		t.Run(condition, func(t *testing.T) {
			configHome := testkit.TempDir(t)
			path := filepath.Join(configHome, "config.env")
			initial := []byte("YARD_PROFILES='neighbor'\n")
			if condition == "invalid record" {
				initial = append(initial, []byte("SSH_PORT=$(false)\n")...)
			}
			testkit.WriteFile(t, path, initial, 0o600)
			before, err := ReadPersistentFileSnapshot(configHome, path)
			if err != nil {
				t.Fatal(err)
			}
			switch condition {
			case "stale content":
				initial = append(initial, []byte("# operator edit\n")...)
				testkit.WriteFile(t, path, initial, 0o600)
			case "stale identity":
				replacement := filepath.Join(configHome, "replacement")
				testkit.WriteFile(t, replacement, initial, 0o600)
				if err := os.Rename(replacement, path); err != nil {
					t.Fatal(err)
				}
			case "unsafe mode":
				if err := os.Chmod(path, 0o660); err != nil {
					t.Fatal(err)
				}
			}
			value := "neighbor fixture"
			err = WritePersistentAssignmentIfUnchanged(configHome, path, "ENVIRONMENT_PROFILES", &value, before)
			if err == nil || strings.HasPrefix(condition, "stale") && !errors.Is(err, ErrPersistentTargetStale) {
				t.Fatalf("%s accepted: %v", condition, err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, initial) {
				t.Fatalf("rejected write changed content: %q, %v", after, err)
			}
		})
	}
}

func TestWritePersistentAssignmentRejectsPreservedConditionalAlias(t *testing.T) {
	selection := "neighbor fixture"
	empty := ""
	for _, setting := range []struct {
		initial string
		values  []*string
	}{
		{"YARD_PROFILES=neighbor\n: ${YARD_PROFILES:=neighbor}\n", []*string{&selection, nil}},
		{"YARD_PROFILES=neighbor\nDEV_USER=${YARD_PROFILES:=neighbor}\n", []*string{&selection, nil}},
		{"ENVIRONMENT_PROFILES=neighbor\n: ${ENVIRONMENT_PROFILES:=neighbor}\n", []*string{nil, &empty}},
		{"YARD_PROFILES=neighbor\nENVIRONMENT_PROFILES=neighbor\n: ${ENVIRONMENT_PROFILES:=neighbor}\n", []*string{&empty}},
	} {
		for _, value := range setting.values {
			configHome := testkit.TempDir(t)
			path := filepath.Join(configHome, "config.env")
			testkit.WriteFile(t, path, []byte(setting.initial), 0o600)
			if err := WritePersistentAssignment(configHome, path, "ENVIRONMENT_PROFILES", value); err == nil || !strings.Contains(err.Error(), "preserved record") {
				t.Fatalf("conditional alias accepted: %v", err)
			}
			content, err := os.ReadFile(path)
			if err != nil || string(content) != setting.initial {
				t.Fatalf("rejected write changed content: %q, %v", content, err)
			}
		}
	}
}
