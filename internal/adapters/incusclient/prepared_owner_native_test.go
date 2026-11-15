package incusclient

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/lxc/incus/v6/shared/api"
)

func TestOrdinarySharedProjectRequiresCanonicalInitialConfig(t *testing.T) {
	for _, change := range []string{"valid", "nil", "empty", "malformed", "duplicate", "foreign-context", "appeared", "conflict"} {
		t.Run(change, func(t *testing.T) {
			mode := "ordinary"
			if change == "conflict" {
				mode = "ordinary-conflict"
			}
			client, fixture := ownerCreateClientFixture(t, mode)
			p := ports.PreparedImageProject{Project: "subyard-test-yard", Absent: true, SharedImages: true}
			p.InitialConfig, _ = renderOwnerProjectConfig(t, domain.Context{YardName: "test-yard", IncusProject: p.Project, YardInstanceName: "yard-test-yard"}, []string{"SUBYARD_HOME=/fixture/data", "SUBYARD_CONFIG_HOME=/fixture/config", "YARD_NAME=test-yard", "YARD_INSTANCE_NAME=yard-test-yard", "HOST_BASE=/fixture/host", "SRV_POOL=default", "SRV_VOLUME=yard-test-yard", "STORAGE_PATH=/fixture/storage"})
			switch change {
			case "nil":
				p.InitialConfig = nil
			case "empty":
				p.InitialConfig = []byte{}
			case "malformed":
				p.InitialConfig = []byte("user.subyard.installation=1")
			case "duplicate":
				p.InitialConfig = append(p.InitialConfig, []byte("user.subyard.installation=1\x00")...)
			case "foreign-context":
				p.InitialConfig = []byte(strings.Replace(string(p.InitialConfig), `"incusProject":"subyard-test-yard"`, `"incusProject":"foreign"`, 1))
			case "appeared":
				fixture.mu.Lock()
				fixture.project = &api.Project{Name: p.Project, ProjectPut: api.ProjectPut{Config: map[string]string{"foreign": "keep"}}}
				fixture.mu.Unlock()
			}
			err := client.ApplyPreparedImageProject(context.Background(), p)
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			if change == "valid" {
				if err != nil || fixture.posts != 1 || fixture.project.Config["user.subyard.installation"] != "1" || fixture.project.Config["features.images"] != "false" {
					t.Fatal("canonical atomic create failed", err)
				}
				if _, exists := fixture.project.Config[ownerCreateTokenKey]; exists {
					t.Fatal("ordinary project acquired native token")
				}
			} else {
				if err == nil {
					t.Fatal("invalid or raced creation admitted")
				}
				wantPosts := 0
				if change == "conflict" {
					wantPosts = 1
				}
				if fixture.posts != wantPosts {
					t.Fatal("invalid configuration submitted")
				}
				if change == "appeared" || change == "conflict" {
					if len(fixture.project.Config) != 1 || fixture.project.Config["foreign"] != "keep" {
						t.Fatal("foreign project changed")
					}
				}
			}
		})
	}
}

func TestOrdinaryProjectInitialPOSTUsesActualRenderer(t *testing.T) {
	for _, reporting := range []string{"0", "1"} {
		t.Run(reporting, func(t *testing.T) {
			client, fixture := ownerCreateClientFixture(t, "ordinary")
			p := ports.PreparedImageProject{Project: "subyard-test-yard", Absent: true, SharedImages: true}
			initialConfig, contextValue := renderOwnerProjectConfig(t, domain.Context{YardName: "test-yard", IncusProject: p.Project, YardInstanceName: "yard-test-yard"}, []string{"SUBYARD_HOME=/original/data", "SUBYARD_CONFIG_HOME=/original/config", "YARD_NAME=test-yard", "YARD_INSTANCE_NAME=yard-test-yard", "HOST_BASE=/original/host-\xff", "SRV_POOL=original-pool", "SRV_VOLUME=original-srv", "STORAGE_PATH=/original/storage", "YARD_KIND=vm", "VM_FREE_PAGE_REPORTING=" + reporting, "NESTED_E2E_VMS=1"})
			p.InitialConfig = initialConfig
			if err := client.ApplyPreparedImageProject(context.Background(), p); err != nil {
				t.Fatal("actual initial project config refused", err)
			}
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			config := fixture.project.Config
			if fixture.posts != 1 || config["user.subyard.installation"] != "1" || config["user.subyard.data_home"] != "/original/data" || config["user.subyard.context"] != contextValue || config["restricted.containers.interception"] != "allow" || config["restricted.virtual-machines.lowlevel"] != "allow" || config["features.images"] != "false" {
				t.Fatal("initial actual POST omitted ownership or policy")
			}
			if _, exists := config[ownerCreateTokenKey]; exists {
				t.Fatal("ordinary project acquired native token")
			}
		})
	}
}

func TestPreparedOwnerImageProjectRefusesOriginalScopeDrift(t *testing.T) {
	for _, change := range []string{"none", "config", "name", "absent-unshared"} {
		t.Run(change, func(t *testing.T) {
			fake, err := testkit.NewIncusServer(testkit.TempDir(t))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { fake.Close() })
			project := map[string]any{"name": "subyard-e2e-yard", "config": map[string]string{"features.images": "false", "foreign": "keep"}}
			if change != "absent-unshared" {
				fake.SetProject("subyard-e2e-yard", project)
			}
			client := New(fake.SocketPath, "projects")
			prepared, err := client.PrepareImageProject(context.Background(), "subyard-e2e-yard", change != "absent-unshared")
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "config":
				project["config"] = map[string]string{"features.images": "false", "foreign": "changed"}
			case "name":
				project["name"] = "foreign-project"
			}
			if change != "absent-unshared" {
				fake.SetProject("subyard-e2e-yard", project)
			}
			err = client.ApplyPreparedImageProject(context.Background(), prepared)
			if change == "none" || change == "absent-unshared" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, domain.ErrPlanStale) {
				t.Fatal("changed project admitted", err)
			}
		})
	}
}
