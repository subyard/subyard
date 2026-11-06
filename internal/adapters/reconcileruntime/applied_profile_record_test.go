package reconcileruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

// ownedFactsExecutor runs the shipped facts program against a fixture root
// owned by the test user instead of root.
type ownedFactsExecutor struct{ appliedIdentityExecutor }

func (fixture ownedFactsExecutor) Exec(ctx context.Context, project, instance string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	request.Command = append([]string(nil), request.Command...)
	request.Command[3] = strings.ReplaceAll(request.Command[3], "os.fchown(stream.fileno(), 0, 0)", fmt.Sprintf("os.fchown(stream.fileno(), %d, %d)", os.Getuid(), os.Getgid()))
	return fixture.appliedIdentityExecutor.Exec(ctx, project, instance, request)
}

// A profile operation applies only its own runtime. The applied selection must
// follow that profile and keep every other entry, including unapplied intent.
func TestRecordAppliedProfileChangesOnlyThatProfile(t *testing.T) {
	for _, scenario := range []struct {
		name, profile    string
		selected         bool
		applied, want    []string
		status, yard     string
		missing, written bool
		refused          bool
	}{
		{name: "disable", profile: "github", applied: []string{"a", "github", "b"}, want: []string{"a", "b"}, written: true},
		{name: "enable", profile: "github", selected: true, applied: []string{"a"}, want: []string{"a", "github"}, written: true},
		{name: "enable-present", profile: "github", selected: true, applied: []string{"github", "a"}, want: []string{"github", "a"}},
		{name: "disable-absent", profile: "github", applied: []string{"a"}, want: []string{"a"}},
		{name: "incomplete", profile: "github", applied: []string{"github"}, status: "incomplete", refused: true},
		{name: "foreign-yard", profile: "github", applied: []string{"github"}, yard: "foreign"},
		{name: "missing", profile: "github", missing: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			root := testkit.TempDir(t)
			for _, path := range []string{"etc/subyard", filepath.Dir(guestFactsSource)} {
				if err := os.MkdirAll(filepath.Join(root, path), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			program, err := os.ReadFile(filepath.Join("../../..", guestFactsSource))
			if err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(root, guestFactsSource), program, 0o600)
			testkit.WriteFile(t, filepath.Join(root, "etc/machine-id"), []byte(strings.Repeat("a", 32)), 0o644)
			// The desired selection carries unapplied intent that a profile operation must not record.
			runtime := Runtime{RepositoryRoot: root, Executor: ownedFactsExecutor{appliedIdentityExecutor{root}}, Yard: domain.Context{YardName: "default", IncusProject: "subyard", YardInstanceName: "yard", DevUser: "dev", DevUID: 1001},
				Environment: []string{"YARD_VERSION=1.2.3", "ENVIRONMENT_PROFILES=a b c"}}
			var target map[string]any
			if err := json.Unmarshal(runtime.guestRuntimeFacts(), &target); err != nil {
				t.Fatal(err)
			}
			target["selected_profiles"] = scenario.applied
			if scenario.applied == nil {
				target["selected_profiles"] = []string{}
			}
			if scenario.yard != "" {
				target["yard"].(map[string]any)["name"] = scenario.yard
			}
			status := "current"
			if scenario.status != "" {
				status = scenario.status
			}
			facts := map[string]any{"schema": 1, "machine_id": strings.Repeat("a", 32), "status": status, "target": target, "last_success": target, "capabilities": "unknown", "readiness": "unknown"}
			content, _ := json.Marshal(facts)
			path := filepath.Join(root, "etc/subyard/guest-runtime.json")
			if !scenario.missing {
				testkit.WriteFile(t, path, content, 0o644)
			}
			err = runtime.RecordAppliedProfile(context.Background(), scenario.profile, scenario.selected)
			// An unfinished refresh or record must fail visibly instead of looking recorded.
			if scenario.refused != (err != nil) || scenario.refused && !strings.Contains(err.Error(), "run yard init") {
				t.Fatalf("record err=%v, want refused=%v", err, scenario.refused)
			}
			after, readErr := os.ReadFile(path)
			if scenario.missing {
				if !os.IsNotExist(readErr) {
					t.Fatalf("missing facts were created: %v", readErr)
				}
				return
			}
			if readErr != nil {
				t.Fatal(readErr)
			}
			if written := !bytes.Equal(after, content); written != scenario.written {
				t.Fatalf("facts rewritten=%v, want %v: %s", written, scenario.written, after)
			}
			if scenario.want == nil {
				return
			}
			actual, known, err := runtime.AppliedEnvironmentProfiles(context.Background())
			if err != nil || !known || strings.Join(actual, " ") != strings.Join(scenario.want, " ") {
				t.Fatalf("applied selection=%v known=%v err=%v, want %v", actual, known, err, scenario.want)
			}
		})
	}
	if err := (Runtime{RepositoryRoot: "../../..", Executor: ownedFactsExecutor{}}).RecordAppliedProfile(context.Background(), "../github", true); err == nil {
		t.Fatal("unsafe profile name accepted")
	}
}
