package incusclient

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
)

func TestInstallationYardsUsesVersionedAdoptedProvenance(t *testing.T) {
	for _, scenario := range []string{"adopted", "created", "missing-uuid", "missing-context", "unknown-marker", "mixed", "mixed-empty", "foreign-context"} {
		t.Run(scenario, func(t *testing.T) {
			fake := newNetworkIncusServer(t)
			owner := domain.Context{YardName: "fixture", AccessKind: domain.AccessLocal, YardKind: domain.YardContainer, IncusProject: "project-a", YardInstanceName: "yard-a", SSHHost: "yard-a", SSHPort: 2222, DevUser: "dev", DevUID: 1000, ShiftMode: "shift", Paths: domain.RuntimePaths{OperatorHome: "/fixture/home", ConfigHome: "/fixture/config", DataHome: "/fixture/data", HostBase: "/fixture/host"}}
			contextJSON, err := json.Marshal(owner)
			if err != nil {
				t.Fatal(err)
			}
			values := map[string]string{"user.subyard.installation": domain.AdoptedInstallation, "user.subyard.adopted_uuid": "original-uuid", "user.subyard.context": string(contextJSON), "user.subyard.data_home": owner.Paths.DataHome, "user.subyard.config_home": owner.Paths.ConfigHome}
			switch scenario {
			case "created":
				values["user.subyard.installation"] = "1"
				delete(values, "user.subyard.adopted_uuid")
			case "missing-uuid":
				delete(values, "user.subyard.adopted_uuid")
			case "missing-context":
				delete(values, "user.subyard.context")
			case "unknown-marker":
				values["user.subyard.installation"] = "adopted-v99"
			case "mixed":
				values["user.subyard.installation"] = "1"
			case "mixed-empty":
				values["user.subyard.installation"] = "1"
				values["user.subyard.adopted_uuid"] = ""
			case "foreign-context":
				values["user.subyard.data_home"] = "/foreign/data"
			}
			fake.projects[owner.IncusProject] = fakeProject{ETag: "project-v1", Config: values}
			yards, err := New(fake.socket, "projects").InstallationYards(context.Background())
			if scenario == "adopted" || scenario == "created" {
				if err != nil || len(yards) != 1 || yards[0].Adopted != (scenario == "adopted") {
					t.Fatal("versioned provenance misclassified", yards, err)
				}
			} else if err == nil {
				t.Fatal("incomplete or conflicting provenance admitted", yards)
			}
		})
	}
}

func TestProjectAdoptionUsesSameReadETagAndPreservesPolicy(t *testing.T) {
	for _, scenario := range []string{"valid", "missing-etag", "before-drift", "race", "retry"} {
		t.Run(scenario, func(t *testing.T) {
			fake := newNetworkIncusServer(t)
			before := map[string]string{"features.images": "false", "restricted": "true", "foreign.description": "preserve"}
			metadata := map[string]string{"user.subyard.installation": domain.AdoptedInstallation, "user.subyard.adopted_uuid": "original-uuid"}
			if scenario == "retry" {
				for key, value := range metadata {
					before[key] = value
				}
			}
			fake.projects["project-a"] = fakeProject{ETag: "assessed-v1", Description: "preserve description", Config: maps.Clone(before)}
			if scenario == "missing-etag" {
				p := fake.projects["project-a"]
				p.ETag = ""
				fake.projects["project-a"] = p
			}
			if scenario == "before-drift" {
				fake.projects["project-a"].Config["restricted"] = "false"
			}
			if scenario == "race" {
				fake.server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					fake.ServeHTTP(w, r)
					if r.Method == http.MethodGet && r.URL.Path == "/1.0/projects/project-a" {
						fake.mu.Lock()
						p := fake.projects["project-a"]
						p.ETag = "replacement-v1"
						p.Config["concurrent"] = "preserve"
						fake.projects["project-a"] = p
						fake.mu.Unlock()
					}
				})
			}
			err := New(fake.socket, "projects").AdoptProject(context.Background(), "project-a", before, metadata)
			fake.mu.Lock()
			defer fake.mu.Unlock()
			project := fake.projects["project-a"]
			if scenario == "valid" || scenario == "retry" {
				if err != nil {
					t.Fatal(err)
				}
				for key, value := range metadata {
					if project.Config[key] != value {
						t.Fatal("incomplete ownership publication")
					}
				}
				if project.Config["restricted"] != "true" || project.Config["foreign.description"] != "preserve" || project.Description != "preserve description" || project.Config[ownerCreateTokenKey] != "" {
					t.Fatal("adoption changed policy or invented creation")
				}
				if scenario == "valid" && fake.projectIfMatch != "assessed-v1" {
					t.Fatal("PUT lost same-read ETag")
				}
				if scenario == "retry" && fake.projectIfMatch != "" {
					t.Fatal("retry rewrote converged project")
				}
			} else {
				if err == nil || project.Config["user.subyard.installation"] != "" {
					t.Fatal("stale adoption published ownership", err)
				}
				if scenario == "race" && (fake.projectIfMatch != "assessed-v1" || project.Config["concurrent"] != "preserve") {
					t.Fatal("concurrent replacement was overwritten")
				}
			}
		})
	}
}

func TestTeardownInventoryProvesEnabledBucketNamespaceEmpty(t *testing.T) {
	for _, scenario := range []string{"empty", "present", "unreadable"} {
		t.Run(scenario, func(t *testing.T) {
			fake := newNetworkIncusServer(t)
			fake.projects["project-a"] = fakeProject{ETag: "project-v1", Config: map[string]string{"features.images": "false", "features.profiles": "false", "features.storage.volumes": "false", "features.storage.buckets": "true"}}
			listed := false
			fake.server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/1.0":
					writeNetworkIncusSync(w, map[string]any{"api_extensions": []string{"projects", "instances", "storage", "storage_buckets"}, "environment": map[string]string{"server": "incus", "server_version": "6.23"}})
				case "/1.0/storage-pools":
					writeNetworkIncusSync(w, []map[string]string{{"name": "fixture", "driver": "dir"}})
				case "/1.0/storage-pools/fixture/buckets":
					listed = r.URL.Query().Get("project") == "project-a"
					if scenario == "unreadable" {
						writeNetworkIncusError(w, http.StatusForbidden, "unreadable")
						return
					}
					buckets := []string{}
					if scenario == "present" {
						buckets = append(buckets, "/1.0/storage-pools/fixture/buckets/unproven?project=project-a")
					}
					writeNetworkIncusSync(w, buckets)
				default:
					fake.ServeHTTP(w, r)
				}
			})
			resources, err := New(fake.socket).TeardownInventory(context.Background(), "project-a")
			if !listed {
				t.Fatal("bucket namespace was not inventoried in the selected project")
			}
			if scenario == "unreadable" {
				if err == nil {
					t.Fatal("unreadable namespace inferred empty")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, r := range resources {
				if r.Kind == "bucket" {
					found = r.Name == "unproven" && r.Pool == "fixture" && r.Binding != ""
				}
			}
			if found != (scenario == "present") {
				t.Fatal(fmt.Sprintf("incorrect bucket inventory: %s", scenario))
			}
		})
	}
}
