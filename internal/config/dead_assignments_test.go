package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func deadAssignmentFixture(t *testing.T) (string, string, Catalog, string) {
	t.Helper()
	root := integrationCatalogFixture(t, `{"schema_version":2,"command":"sample","artifacts":[{"id":"config","kind":"config","source":"settings.json","destination":".sample/settings.json","format":"destination"},{"id":"rules","kind":"config","source":"rules/repo.rules","destination":".sample/rules","format":"raw"}]}`)
	home := testkit.TempDir(t)
	for relative, content := range map[string]string{"sample/settings.json": "{}\n", "sample/rules/repo.rules": "allow()\n"} {
		for _, path := range []string{filepath.Join(root, "config/agents", relative), filepath.Join(home, "overrides/host/agents", relative)} {
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, path, []byte(content), 0600)
		}
	}
	catalog, err := LoadCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	return root, home, catalog, filepath.Join(home, "config.env")
}

func TestDeadAssignmentsLoadPreviewAndAtomicApply(t *testing.T) {
	root, home, catalog, path := deadAssignmentFixture(t)
	before := []byte("# keep this comment\r\nAGENT_sample_CONFIG=\"$SUBYARD_CONFIG_DIR/../private/agents/sample/settings.json\"\r\nexport AGENT_sample_RULES=${SUBYARD_CONFIG_DIR}/../private/agents/sample/rules/repo.rules\nDEV_SUDO=prompt")
	testkit.WriteFile(t, path, before, 0640)
	plan, err := catalog.PlanDeadAssignments(home, filepath.Join(root, "config"), nil)
	if err != nil || len(plan.Removed) != 2 {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	if plan.Removed[0].Line != 2 || plan.Removed[1].Line != 3 {
		t.Fatal(plan.Removed)
	}
	expected := []byte("# keep this comment\r\nDEV_SUDO=prompt")
	if !bytes.Equal(plan.Desired, expected) {
		t.Fatalf("desired=%q", plan.Desired)
	}
	for _, a := range plan.Removed {
		if !strings.Contains(a.String(), "is dead") || !strings.Contains(a.String(), "update will remove this line") || !strings.Contains(a.String(), a.Source) {
			t.Fatal(a.String())
		}
	}
	loaded, err := Load(LoadOptions{RepositoryRoot: root, OperatorHome: testkit.TempDir(t), Environment: map[string]string{"SUBYARD_CONFIG_HOME": home, "CODING_TOOL_INTEGRATIONS": "sample"}})
	if err != nil || len(loaded.DeadAssignments()) != 2 {
		t.Fatalf("load: dead=%v err=%v", loaded.DeadAssignments(), err)
	}
	if loaded.Environment["AGENT_sample_CONFIG"] != plan.Removed[0].Source {
		t.Fatal("host file override is not effective")
	}
	current, _ := os.ReadFile(path)
	if !bytes.Equal(current, before) {
		t.Fatal("preview changed settings")
	}
	if err := plan.Apply(home); err != nil {
		t.Fatal(err)
	}
	after, err := ReadPersistentFileSnapshot(home, path)
	if err != nil || !bytes.Equal(after.Content, expected) {
		t.Fatalf("after=%+v err=%v", after, err)
	}
	if after.Identity.Mode != plan.Expected.Identity.Mode || after.Identity.UID != plan.Expected.Identity.UID || after.Identity.GID != plan.Expected.Identity.GID || after.Identity.Inode == plan.Expected.Identity.Inode {
		t.Fatal("atomic edit did not preserve mode/owner")
	}
	repeat, err := catalog.PlanDeadAssignments(home, filepath.Join(root, "config"), nil)
	if err != nil || len(repeat.Removed) != 0 {
		t.Fatalf("repeat=%+v err=%v", repeat, err)
	}
}

func TestDeadAssignmentsKeepUncertainReferencesAndLiteralRefusals(t *testing.T) {
	for _, name := range []string{"live", "other variable", "parameter operator", "missing override", "symlink override", "hardlink override", "writable override", "symlink private", "duplicate", "single quote", "command substitution", "config dir reassignment", "wrong artifact", "multiline"} {
		t.Run(name, func(t *testing.T) {
			root, home, catalog, path := deadAssignmentFixture(t)
			assignment := `AGENT_sample_CONFIG="$SUBYARD_CONFIG_DIR/../private/agents/sample/settings.json"` + "\n"
			override := filepath.Join(home, "overrides/host/agents/sample/settings.json")
			private := filepath.Join(root, "private/agents/sample/settings.json")
			switch name {
			case "live":
				if err := os.MkdirAll(filepath.Dir(private), 0700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, private, []byte("{}"), 0600)
			case "other variable":
				assignment = `AGENT_sample_CONFIG="$NAME/settings.json"` + "\n"
			case "parameter operator":
				assignment = `AGENT_sample_CONFIG="${SUBYARD_CONFIG_DIR:-unused}/../private/agents/sample/settings.json"` + "\n"
			case "missing override":
				if err := os.Rename(override, override+".saved"); err != nil {
					t.Fatal(err)
				}
			case "symlink override":
				if err := os.Rename(override, override+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(override+".saved", override); err != nil {
					t.Fatal(err)
				}
			case "hardlink override":
				if err := os.Link(override, override+".link"); err != nil {
					t.Fatal(err)
				}
			case "writable override":
				if err := os.Chmod(override, 0660); err != nil {
					t.Fatal(err)
				}
			case "symlink private":
				if err := os.Symlink(home, filepath.Join(root, "private")); err != nil {
					t.Fatal(err)
				}
			case "duplicate":
				assignment += assignment
			case "single quote":
				assignment = `AGENT_sample_CONFIG='$SUBYARD_CONFIG_DIR/../private/agents/sample/settings.json'` + "\n"
			case "command substitution":
				assignment = `AGENT_sample_CONFIG="$(echo nope)/../private/agents/sample/settings.json"` + "\n"
			case "config dir reassignment":
				assignment = "SUBYARD_CONFIG_DIR=/unknown\n" + assignment
			case "wrong artifact":
				assignment = `AGENT_sample_CONFIG="$SUBYARD_CONFIG_DIR/../private/agents/sample/other.json"` + "\n"
			case "multiline":
				assignment = "AGENT_sample_CONFIG=\"$SUBYARD_CONFIG_DIR/../private/agents/sample/\nsettings.json\"\n"
			}
			testkit.WriteFile(t, path, []byte(assignment), 0600)
			plan, err := catalog.PlanDeadAssignments(home, filepath.Join(root, "config"), nil)
			if err != nil || len(plan.Removed) != 0 || string(plan.Desired) != assignment {
				t.Fatalf("uncertain assignment changed: %+v err=%v", plan, err)
			}
			_, err = ParsePersistentAssignments(path, plan.Desired)
			if name != "single quote" && name != "command substitution" && !errors.Is(err, errSettingExpansion) {
				t.Fatalf("literal refusal lost: %v", err)
			}
		})
	}
}

func TestDeadAssignmentsRefuseChangedProofBeforePublication(t *testing.T) {
	for _, change := range []string{"host", "same bytes new inode", "override", "missing path now exists"} {
		t.Run(change, func(t *testing.T) {
			root, home, catalog, path := deadAssignmentFixture(t)
			before := []byte("# retained\nAGENT_sample_CONFIG=\"$SUBYARD_CONFIG_DIR/../private/agents/sample/settings.json\"\n")
			testkit.WriteFile(t, path, before, 0644)
			plan, err := catalog.PlanDeadAssignments(home, filepath.Join(root, "config"), nil)
			if err != nil || len(plan.Removed) != 1 {
				t.Fatalf("plan=%+v err=%v", plan, err)
			}
			switch change {
			case "host":
				testkit.WriteFile(t, path, append(before, []byte("DEV_SUDO=prompt\n")...), 0644)
			case "same bytes new inode":
				testkit.WriteFile(t, path+".replacement", before, 0644)
				if err := os.Rename(path+".replacement", path); err != nil {
					t.Fatal(err)
				}
			case "override":
				testkit.WriteFile(t, plan.Removed[0].Source, []byte("{\"changed\":true}"), 0600)
			default:
				if err := os.MkdirAll(filepath.Dir(plan.Missing[0]), 0700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, plan.Missing[0], []byte("{}"), 0600)
			}
			current, _ := os.ReadFile(path)
			if err := plan.Apply(home); !errors.Is(err, ErrPersistentTargetStale) {
				t.Fatalf("apply=%v", err)
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(current, after) {
				t.Fatal("stale plan changed host")
			}
		})
	}
}
