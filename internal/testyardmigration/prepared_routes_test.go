package testyardmigration

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
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
