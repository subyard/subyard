package testvmsruntime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type hostMemoryEvidence struct {
	ObservedAt time.Time        `json:"observed_at"`
	Timing     string           `json:"timing"`
	OuterState string           `json:"outer_state"`
	Host       *MemoryCapacity  `json:"host_memory,omitempty"`
	Allocation *MemoryCapacity  `json:"allocation_memory,omitempty"`
	KernelOOM  []kernelOOMEvent `json:"kernel_oom"`
	Missing    []string         `json:"unavailable,omitempty"`
}

type kernelOOMEvent struct {
	Timestamp string `json:"realtime_microseconds,omitempty"`
	Kind      string `json:"kind"`
}

// Do not persist kernel messages verbatim: they can contain private cgroup paths
// and command lines. Keep event times and OOM classifications for correlation.
func kernelOOMEvidence(body []byte) ([]kernelOOMEvent, error) {
	if len(body) > 1<<20 {
		return nil, errors.New("kernel evidence exceeds bound")
	}
	result := []kernelOOMEvent{}
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 4096), 64<<10)
	for scanner.Scan() {
		var entry map[string]json.RawMessage
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			return nil, err
		}
		var message, stamp string
		_ = json.Unmarshal(entry["MESSAGE"], &message)
		_ = json.Unmarshal(entry["__REALTIME_TIMESTAMP"], &stamp)
		if _, err := strconv.ParseUint(stamp, 10, 64); err != nil {
			stamp = ""
		}
		lower := strings.ToLower(message)
		kind := ""
		switch {
		case strings.Contains(lower, "killed process"):
			kind = "killed_process"
		case strings.Contains(lower, "oom-kill"):
			kind = "oom_kill"
		case strings.Contains(lower, "out of memory"):
			kind = "out_of_memory"
		}
		if kind != "" {
			result = append(result, kernelOOMEvent{Timestamp: stamp, Kind: kind})
		}
		if len(result) == 50 {
			break
		}
	}
	return result, scanner.Err()
}

func (sink *HostSink) saveHostEvidence(ctx context.Context, instance outerBrokerInstance, incidents []IncidentArtifact) error {
	if len(incidents) == 0 {
		return nil
	}
	directory := filepath.Join(sink.DataHome, "logs", "test-vms-broker-incidents", "host")
	var pending []IncidentArtifact
	for _, incident := range incidents {
		if _, err := os.Stat(filepath.Join(directory, incident.IncidentID+".json")); os.IsNotExist(err) {
			pending = append(pending, incident)
		} else if err != nil {
			return err
		}
	}
	if len(pending) == 0 {
		return nil
	}
	// Best-effort diagnostics must never delay fencing or repeatedly block spool
	// acknowledgement when a crashed VM no longer exposes its cgroup.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	now := time.Now().UTC()
	if sink.Now != nil {
		now = sink.Now().UTC()
	}
	evidence := hostMemoryEvidence{ObservedAt: now, Timing: "after incident, at host spool collection; kernel window is the preceding 15 minutes", OuterState: strings.ToLower(instance.Status), KernelOOM: []kernelOOMEvent{}}
	if memory, err := memoryCapacity("/proc", "/sys/fs/cgroup"); err == nil {
		memory.OuterHostEvidence = "collected by physical-owner host sink"
		evidence.Host = &memory
	} else {
		evidence.Missing = append(evidence.Missing, "host_memory")
	}
	body, _, err := sink.Runner.Run(ctx, sink.Incus, []string{"query", "/1.0/instances/" + instance.Name + "/state?project=" + instance.Project}, nil, nil)
	var state struct {
		Status string `json:"status"`
		PID    int    `json:"pid"`
	}
	if err == nil && len(body) < 1<<20 && json.Unmarshal(body, &state) == nil && state.PID > 1 {
		if memory, err := memoryCapacityForProcess("/proc", "/sys/fs/cgroup", strconv.Itoa(state.PID)); err == nil {
			memory.OuterHostEvidence = "collected by physical-owner host sink"
			evidence.Allocation = &memory
		} else {
			evidence.Missing = append(evidence.Missing, "allocation_cgroup")
		}
		switch strings.ToLower(state.Status) {
		case "running", "stopped", "frozen", "error":
			evidence.OuterState = strings.ToLower(state.Status)
		}
	} else {
		evidence.Missing = append(evidence.Missing, "allocation_state_and_cgroup")
	}
	body, _, err = sink.Runner.Run(ctx, "journalctl", []string{"--kernel", "--no-pager", "--output=json", "--since", "-15min", "-n", "50", "--grep", "Out of memory|oom-kill|Killed process"}, nil, nil)
	if err == nil {
		evidence.KernelOOM, err = kernelOOMEvidence(body)
	}
	if err != nil {
		evidence.KernelOOM = []kernelOOMEvent{}
		evidence.Missing = append(evidence.Missing, "kernel_oom_log")
	}
	payload, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(directory, 0750); err != nil {
		return err
	}
	if err := sink.setOperatorGroup(directory); err != nil {
		return err
	}
	for _, incident := range pending {
		path := filepath.Join(directory, incident.IncidentID+".json")
		// Concurrent sink retries keep the first observation, whose timestamp is
		// distinct from the original incident timestamp.
		err := withFileLock(filepath.Join(directory, ".host-evidence.lock"), func() error {
			if _, err := os.Stat(path); err == nil {
				return nil
			} else if !os.IsNotExist(err) {
				return err
			}
			return writeAtomicDurable(path, append(payload, '\n'), 0640)
		})
		if err != nil {
			return err
		}
		if err := sink.setOperatorGroup(path); err != nil {
			return err
		}
	}
	return nil
}
