package cli

import (
	"context"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
	"os"
	"strings"
)

type filePowerOracle struct {
	*testkit.Incus
	path string
}

func (oracle filePowerOracle) Instance(ctx context.Context, project, name string) (ports.InstanceInfo, error) {
	instance, err := oracle.Incus.Instance(ctx, project, name)
	if err != nil {
		return instance, err
	}
	payload, err := os.ReadFile(oracle.path)
	if err != nil {
		return instance, err
	}
	instance.Status = strings.TrimSpace(string(payload))
	return instance, nil
}

// A successful scripted physical boundary must update the independent Incus
// oracle. Failure steps deliberately leave physical state unchanged.
func powerScriptedIncus(adapter *testkit.ScriptedAdapter, incus *testkit.Incus) *testkit.Incus {
	if incus == nil {
		incus = lifecycleIncus()
	}
	for index := range adapter.Steps {
		previous := adapter.Steps[index].Apply
		adapter.Steps[index].Apply = func(request domain.AdapterRequest) {
			if previous != nil {
				previous(request)
			}
			if request.Adapter != "lifecycle" {
				return
			}
			key := request.Context["INCUS_PROJECT"] + "/" + request.Context["YARD_INSTANCE_NAME"]
			instance := incus.Instances[key]
			if request.Action == "start" {
				instance.Status = "Running"
			} else if request.Action == "stop" {
				instance.Status = "Stopped"
			}
			incus.Instances[key] = instance
		}
	}
	return incus
}
