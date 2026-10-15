package profile

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestFullCleanupAssessmentUsesOnlyShippedReadOnlyHook(t *testing.T) {
	root := testkit.TempDir(t)
	directory := filepath.Join(root, "config", "profiles", "fixture")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(directory, "inspect.sh"), []byte("#!/bin/sh\n[ \"$1\" = --assess ] && [ \"$ASSUME_YES\" = 0 ] || exit 9\nprintf '%s\\n' '{\"artifacts\":[],\"services\":[],\"preserved\":[\"foreign producer\"]}'\n"), 0700)
	fixture(t, root, "fixture", "", Definition{FullCleanup: "inspect.sh", FullCleanupEnvironment: []string{"FIXTURE_ROOT"}})
	plans, err := AssessFullCleanup(context.Background(), root, map[string]string{"ASSUME_YES": "0"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].Profile != "fixture" || len(plans[0].Preserved) != 1 {
		t.Fatalf("plans=%#v", plans)
	}
}

func TestLifecycleOnlyCleanupCannotBecomeSelected(t *testing.T) {
	root := testkit.TempDir(t)
	directory := filepath.Join(root, "config", "profiles", "fixture")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(directory, "inspect.sh"), []byte("#!/bin/sh\nprintf '%s\\n' '{\"artifacts\":[],\"services\":[],\"preserved\":[]}'\n"), 0700)
	fixture(t, root, "fixture", "", Definition{LifecycleOnly: true, FullCleanup: "inspect.sh", ManagedPaths: []ManagedPath{{Root: "data", Path: "routes/{yard}"}}})
	definitions, err := Load(root)
	if err != nil || len(definitions) != 1 {
		t.Fatalf("load lifecycle owner: %v, %v", definitions, err)
	}
	if definitions[0].Selected("default", map[string]string{"ENVIRONMENT_PROFILES": "fixture"}) {
		t.Fatal("cleanup-only owner became a runtime profile")
	}
	plans, err := AssessFullCleanup(context.Background(), root, map[string]string{})
	if err != nil || len(plans) != 1 {
		t.Fatalf("unselected lifecycle owner was not assessed: %v, %v", plans, err)
	}
}

func TestFullCleanupAssessmentRejectsExpansionAndUnsafeUnitNames(t *testing.T) {
	for _, body := range []string{
		`{"artifacts":[{"Path":"/","Binding":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}],"services":[],"preserved":[]}`,
		`{"artifacts":[],"services":[{"scope":"system","units":["foreign.service"]}],"preserved":[]}`,
		`{"artifacts":[],"services":[],"preserved":[],"unknown":true}`,
	} {
		t.Run(body, func(t *testing.T) {
			root := testkit.TempDir(t)
			directory := filepath.Join(root, "config", "profiles", "fixture")
			if err := os.MkdirAll(directory, 0700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(directory, "inspect.sh"), []byte("#!/bin/sh\nprintf '%s\\n' '"+body+"'\n"), 0700)
			fixture(t, root, "fixture", "", Definition{FullCleanup: "inspect.sh"})
			if _, err := AssessFullCleanup(context.Background(), root, map[string]string{}); err == nil {
				t.Fatal("unsafe metadata accepted")
			}
		})
	}
}
