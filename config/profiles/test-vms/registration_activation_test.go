package testvms_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/ownerservice"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestBrokerRegistrationActivation(t *testing.T) {
	for _, scenario := range []string{"one", "zero", "unselected", "several", "UUID", "ownership", "several+proof", "existing", "save-failure", "publish-failure"} {
		t.Run(scenario, func(t *testing.T) {
			body, err := os.ReadFile("profile.json")
			if err != nil {
				t.Fatal(err)
			}
			metadata, err := profile.Decode("test-vms", body)
			if err != nil {
				t.Fatal(err)
			}
			data, _ := metadata.Data()
			service := data.InfrastructureService
			if service.RegistrationActivation != "test-vm-broker" {
				t.Fatal("registration must extend the declared broker owner")
			}
			root := testkit.TempDir(t)
			registry := ownerservice.Registry{Root: filepath.Join(root, "services")}
			publication := filepath.Join(root, "routes")
			if scenario != "publish-failure" {
				if err := os.Mkdir(publication, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(publication, 0o755); err != nil {
					t.Fatal(err)
				}
			} else {
				publication = filepath.Join(root, "missing-parent", "routes")
			}
			record := ownerservice.Registration{Public: ownerservice.Public{Owner: "fixture-owner", Yard: service.DefaultYard, Project: "broker-project", Instance: "broker-instance", UUID: "broker-uuid"}, Ownership: strings.Repeat("a", 64)}
			candidates := []ownerservice.ActivationCandidate{{Registration: record}}
			if scenario == "zero" || scenario == "unselected" {
				candidates = nil
			}
			if scenario == "several" || scenario == "several+proof" {
				candidates = append(candidates, candidates[0])
				candidates[0].Yard, candidates[1].Yard = "z-broker", "a-broker"
			}
			if scenario == "UUID" || scenario == "ownership" || scenario == "several+proof" {
				candidates[0].Err = errors.New("service runtime " + scenario + " is unproven")
			}
			proofYard := record.Yard
			if len(candidates) != 0 {
				proofYard = candidates[0].Yard
			}
			path := filepath.Join(registry.Root, service.ID, "default.json")
			var before []byte
			if scenario == "existing" {
				record.SchemaVersion, record.Service, record.Pool = ownerservice.SchemaVersion, service.ID, "default"
				record.Generation, err = ownerservice.NewGeneration()
				if err != nil {
					t.Fatal(err)
				}
				if err := registry.Save(record); err != nil {
					t.Fatal(err)
				}
				before, err = os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "save-failure" {
				if err := os.Mkdir(registry.Root, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(registry.Root, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			var output bytes.Buffer
			run := func() {
				registry.RegisterActivation(service.ID, publication, func() ([]ownerservice.ActivationCandidate, bool, error) {
					if scenario == "existing" {
						t.Fatal("existing entry rediscovered runtimes")
					}
					if scenario == "save-failure" {
						if err := os.Chmod(registry.Root, 0o755); err != nil {
							t.Fatal(err)
						}
					}
					return candidates, scenario != "unselected", nil
				}, &output)
			}
			run()
			after, readErr := os.ReadFile(path)
			switch scenario {
			case "one", "publish-failure":
				saved, err := registry.Resolve(record.Owner, service.ID, "default")
				if err != nil || saved.UUID != record.UUID || saved.Generation == "" {
					t.Fatalf("registration: %+v %v", saved, err)
				}
				if scenario == "one" {
					if err := registry.Published(saved, publication); err != nil || !strings.Contains(output.String(), "Registered service test-vms pool default: yard test-yard, project broker-project, instance broker-instance, UUID broker-uuid") {
						t.Fatalf("published result: %v %s", err, &output)
					}
				} else {
					if err := registry.Published(saved, publication); err == nil || !strings.Contains(output.String(), "yard service register test-vms --yard test-yard") {
						t.Fatalf("missing publication failure guidance: %v %s", err, &output)
					}
					output.Reset()
					run()
					retried, err := os.ReadFile(path)
					if err != nil || !bytes.Equal(after, retried) || output.Len() != 0 {
						t.Fatalf("retry changed interrupted registration: %v %s", err, &output)
					}
				}
			case "existing":
				if readErr != nil || !bytes.Equal(before, after) || output.Len() != 0 {
					t.Fatalf("existing registration changed: %v %s", readErr, &output)
				}
			default:
				if readErr == nil || strings.Contains(output.String(), "Registered service") {
					t.Fatalf("unexpected registration: %v %s", readErr, &output)
				}
				if scenario == "save-failure" && !strings.Contains(output.String(), "yard service register test-vms --yard test-yard") {
					t.Fatalf("missing save failure guidance: %s", &output)
				}
				if scenario == "unselected" && output.Len() != 0 {
					t.Fatalf("unselected diagnostic: %s", &output)
				}
				if scenario == "zero" && output.String() != "Diagnostic: no owned runtime to register; run yard service register test-vms --yard NAME\n" {
					t.Fatalf("zero-candidate diagnostic: %s", &output)
				}
				if scenario == "several" || scenario == "several+proof" {
					text := output.String()
					if strings.Count(text, "multiple candidate runtimes") != 2 || !strings.Contains(text, "yard service register test-vms --yard a-broker") || !strings.Contains(text, "yard service register test-vms --yard z-broker") || strings.Index(text, "a-broker") > strings.Index(text, "z-broker") {
						t.Fatalf("unstable ambiguity guidance: %s", &output)
					}
				}
				if strings.Contains(scenario, "proof") || scenario == "UUID" || scenario == "ownership" {
					if !strings.Contains(output.String(), "is unproven; run yard service register test-vms --yard "+proofYard) {
						t.Fatalf("missing proof guidance: %s", &output)
					}
				}
			}
		})
	}
}
