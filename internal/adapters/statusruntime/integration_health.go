package statusruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

var healthCommand = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// ReadIntegrationHealth is advisory status. Its results never enter an
// integration convergence plan or release activation fingerprint.
func (runtime Runtime) ReadIntegrationHealth(ctx context.Context, yard domain.Context, running bool) map[string]string {
	result := make(map[string]string)
	for _, name := range strings.Fields(runtime.Environment["CODING_TOOL_INTEGRATIONS"]) {
		command := runtime.Environment["AGENT_"+name+"_HEALTH"]
		if command == "" {
			continue
		}
		result[name] = "unknown"
		if !running || runtime.Executor == nil || !healthCommand.MatchString(command) {
			continue
		}
		timeout := runtime.ProbeTimeout
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
		probeCtx, cancel := context.WithTimeout(ctx, timeout)
		probe, err := runtime.Executor.Exec(probeCtx, yard.IncusProject, yard.YardInstanceName,
			ports.InstanceExecRequest{Command: []string{command}})
		if err == nil && probe.ExitCode == 0 && probeCtx.Err() == nil {
			result[name] = decodeIntegrationHealth(probe.Stdout)
		}
		cancel()
	}
	return result
}

func decodeIntegrationHealth(payload []byte) string {
	if len(payload) > 1024 {
		return "unknown"
	}
	var report struct {
		State string `json:"state"`
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&report); err != nil {
		return "unknown"
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return "unknown"
	}
	switch report.State {
	case "ready", "starting", "failed", "unknown":
		return report.State
	default:
		return "unknown"
	}
}
