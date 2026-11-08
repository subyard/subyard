package statusruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

// ReadUnmanagedIntegrations reports profile-owned optional software as state.
// Probe failure is unknown; it never becomes a readiness refusal or warning.
func (runtime Runtime) ReadUnmanagedIntegrations(ctx context.Context, yard domain.Context, running bool) map[string]string {
	result := map[string]string{}
	if !running || runtime.Executor == nil {
		return result
	}
	selected := strings.Fields(runtime.Environment["CODING_TOOL_INTEGRATIONS"])
	for _, definition := range runtime.Profiles {
		if !definition.Selected(yard.YardName, runtime.Environment) {
			continue
		}
		for _, observation := range definition.UnmanagedIntegrations {
			if slices.Contains(selected, observation.ID) {
				continue
			}
			command := append(slices.Clone(observation.Command), "/home/"+yard.DevUser, strconv.Itoa(yard.DevUID))
			probeCtx, cancel := context.WithTimeout(ctx, runtime.probeTimeout())
			probe, err := runtime.Executor.Exec(probeCtx, yard.IncusProject, yard.YardInstanceName, ports.InstanceExecRequest{Command: command})
			var value struct {
				State string `json:"state"`
			}
			decoder := json.NewDecoder(bytes.NewReader(probe.Stdout))
			decoder.DisallowUnknownFields()
			if err == nil && probe.ExitCode == 0 && probeCtx.Err() == nil && len(probe.Stdout) <= 1024 && decoder.Decode(&value) == nil && decoder.Decode(new(any)) == io.EOF && value.State == "unmanaged" {
				result[observation.ID] = observation.Detail
			}
			cancel()
		}
	}
	return result
}
