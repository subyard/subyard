package testyardmigration

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

type fixtureRouteDevicePreparer struct{ calls []ports.PreparedReadOnlyDisk }

func (p *fixtureRouteDevicePreparer) PrepareReadOnlyDisk(_ context.Context, project, instance, device, source, destination string) (ports.PreparedReadOnlyDisk, error) {
	result := ports.PreparedReadOnlyDisk{Project: project, Instance: instance, UUID: "original-uuid", Device: device, Source: source, Destination: destination}
	p.calls = append(p.calls, result)
	return result, nil
}

func TestPreparedRouteDevicesUseOnlyOriginalResourceAuthority(t *testing.T) {
	for _, change := range []string{"none", "before", "source", "duplicate", "missing"} {
		t.Run(change, func(t *testing.T) {
			options, state := routeConsumerFixture(t, StateCurrent, true)
			before, err := PrepareRouteConsumersActivation(context.Background(), options)
			if err != nil {
				t.Fatal(err)
			}
			native := &fixtureRouteDevicePreparer{}
			p, err := PrepareRouteDevices(context.Background(), options, before, native)
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Devices) != 2 || p.Devices[1].Project != "subyard" || p.Devices[1].Instance != "yard" || p.Devices[1].UUID != "original-uuid" {
				t.Fatal("unexpected original resource scope")
			}
			switch change {
			case "before":
				p.Before += " "
			case "source":
				p.Devices[0].Source += "-foreign"
			case "duplicate":
				p.Devices = append(p.Devices, p.Devices[0])
			case "missing":
				p.Devices = nil
			}
			applied := 0
			options.RouteDevices = p
			options.CheckRouteDevices = func(context.Context, *PreparedRouteDevices) error { return nil }
			options.ApplyRouteDevice = func(_ context.Context, actual ports.PreparedReadOnlyDisk) error {
				matched := false
				for _, original := range native.calls {
					matched = matched || reflect.DeepEqual(actual, original)
				}
				if !matched {
					t.Fatal("recaptured device authority")
				}
				applied++
				path := state.defaultDevice
				if actual.Instance == "yard-test-yard" {
					for _, entry := range options.Environment {
						if value, ok := strings.CutPrefix(entry, "ROUTE_CURRENT_DEVICE="); ok {
							path = value
						}
					}
				}
				write(t, path, "correct\n")
				return nil
			}
			err = CommitRouteConsumersActivation(context.Background(), options, before)
			if change == "none" {
				if err != nil || applied != 1 {
					t.Fatal("original prepared device did not execute", err)
				}
				if strings.Contains(read(t, state.calls), "config device add") {
					t.Fatal("native device callback fell back to Incus CLI")
				}
			} else if !errors.Is(err, domain.ErrPlanStale) || applied != 0 {
				t.Fatal("changed recipe reached effect", err)
			}
		})
	}
}

func TestPreparedOwnerRoutesBeforeOriginalRemoval(t *testing.T) {
	for _, mode := range []string{"allowed", "denied", "denied-mounted", "foreign", "foreign-owner", "inherited", "inventory-drift", "registration-absent"} {
		t.Run(mode, func(t *testing.T) {
			options, state := routeConsumerFixture(t, StateLegacyDirectory, true)
			write(t, state.extra, "1\n")
			writeIsolatedRouteTemplate(t, options)
			template := ""
			if strings.HasPrefix(mode, "denied") {
				template = "isolated"
			}
			write(t, filepath.Join(options.ConfigHome, "yards/demo/config.env"), "YARD_TEMPLATE="+template+"\nSSH_PORT=2233\n")
			before, err := PrepareRouteConsumers(context.Background(), options)
			if err != nil {
				t.Fatal(err)
			}
			registration := filepath.Join(options.ConfigHome, "yards/e2e-yard/config.env")
			original, err := os.ReadFile(registration)
			if err != nil {
				t.Fatal(err)
			}
			options.ProspectiveFiles = &config.ProspectiveFiles{Root: options.ConfigHome, Yards: []string{"default", CurrentYard, "demo"}, ReadSnapshot: func(path string) (config.PersistentFileSnapshot, error) {
				return config.ReadPersistentFileSnapshot(options.ConfigHome, path)
			}, ReadPreparedInput: func(path string) (config.PersistentFileSnapshot, bool, error) {
				switch path {
				case registration:
					return config.PersistentFileSnapshot{}, true, nil
				case filepath.Join(options.ConfigHome, "yards/test-yard/config.env"):
					if mode == "registration-absent" {
						return config.PersistentFileSnapshot{}, true, nil
					}
					return config.PersistentFileSnapshot{Exists: true, Content: original}, true, nil
				}
				return config.PersistentFileSnapshot{}, false, nil
			}}
			switch mode {
			case "denied-mounted":
				write(t, state.extraDevice, "correct\n")
			case "foreign":
				write(t, state.extraDevice, "wrong\n")
			case "foreign-owner":
				raw, err := os.ReadFile(options.Incus)
				if err != nil {
					t.Fatal(err)
				}
				original := []byte(`instance_json subyard-e2e-yard yard-e2e-yard e2e-yard "$(cat "$ROUTE_OWNER_STATUS")" '{}'`)
				foreign := []byte(`instance_json subyard-e2e-yard yard-e2e-yard e2e-yard "$(cat "$ROUTE_OWNER_STATUS")" '{"subyard-e2e-routes":{"type":"disk","source":"/foreign","path":"/var/lib/subyard/e2e-routes","readonly":"true"}}'`)
				if !bytes.Contains(raw, original) {
					t.Fatal("original owner fixture unavailable")
				}
				testkit.WriteFile(t, options.Incus, bytes.Replace(raw, original, foreign, 1), 0700)
			case "inherited":
				write(t, state.extraDevice, "inherited\n")
			case "inventory-drift":
				write(t, state.extra, "0\n")
			}
			native := &fixtureRouteDevicePreparer{}
			p, err := PrepareOwnerRouteDevices(context.Background(), options, before, native)
			if mode != "allowed" && mode != "denied" {
				if err == nil || len(native.calls) != 0 {
					t.Fatal("changed or unsafe original census reached device preparation", err)
				}
				return
			}
			if err != nil {
				t.Fatal("pre-owner source preparation required post-owner state", err)
			}
			if err := ValidatePreparedRouteDevices(options, p.Before, p); err != nil {
				t.Fatal(err)
			}
			want := 3
			if mode == "denied" {
				want = 2
			}
			if len(p.Devices) != want {
				t.Fatalf("device count=%d want=%d", len(p.Devices), want)
			}
			for _, d := range native.calls {
				if d.Project == "subyard-e2e-yard" || mode == "denied" && d.Project == "subyard-demo" {
					t.Fatal("excluded owner or consumer received preparation")
				}
			}
			if _, err := PrepareRouteDevices(context.Background(), options, p.Before, native); err == nil {
				t.Fatal("ordinary post-owner validator admitted original legacy owner")
			}
		})
	}
}
