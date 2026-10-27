package testvmsruntime

// Owner observations are optional evidence. They never authorize lifecycle work.
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type ResourceObservation struct {
	SlotID             string    `json:"slot_id"`
	ResourceGeneration uint64    `json:"resource_generation"`
	LeaseEpoch         uint64    `json:"lease_epoch"`
	Selector           string    `json:"selector"`
	State              SlotState `json:"state"`
	Phase              string    `json:"phase"`
	SourceTime         time.Time `json:"source_time"`
	BootID             string    `json:"boot_id"`
	MonotonicNS        *uint64   `json:"monotonic_ns"`
	CounterGeneration  string    `json:"counter_generation"`
	CPU                *uint64   `json:"cpu_ns"`
	Memory             *uint64   `json:"memory_bytes"`
	MemoryPeak         *uint64   `json:"memory_peak_bytes"`
	Swap               *uint64   `json:"swap_bytes"`
	IORead             *uint64   `json:"io_read_bytes"`
	IOWrite            *uint64   `json:"io_write_bytes"`
	IOReadOps          *uint64   `json:"io_read_ops"`
	IOWriteOps         *uint64   `json:"io_write_ops"`
	Missing            []string  `json:"missing"`
	FinalComplete      bool      `json:"final_complete"`
	Accounting         string    `json:"accounting"`
	RSS                *uint64   `json:"rss_bytes"`
	PSS                *uint64   `json:"pss_bytes"`
	RSSPeak            *uint64   `json:"rss_peak_bytes"`
}

type ResourceAllocation struct {
	SlotID             string           `json:"slot_id"`
	ResourceGeneration uint64           `json:"resource_generation"`
	LeaseEpoch         uint64           `json:"lease_epoch"`
	State              SlotState        `json:"state"`
	Environment        *EnvironmentSpec `json:"environment"`
	Context            *LeaseContext    `json:"context"`
	AcquiredAt         time.Time        `json:"acquired_at"`
	ReadyAt            time.Time        `json:"ready_at"`
	ExpiresAt          time.Time        `json:"expires_at"`
}

func publicResourceAllocation(slot LeaseSlot) *ResourceAllocation {
	if slot.Environment == nil {
		return nil
	}
	return &ResourceAllocation{SlotID: slot.SlotID, ResourceGeneration: slot.ResourceGeneration, LeaseEpoch: slot.LeaseEpoch, State: slot.State, Environment: slot.Environment, Context: leaseContextFromSlot(slot), AcquiredAt: slot.AcquiredAt, ReadyAt: slot.ReadyAt, ExpiresAt: slot.ExpiresAt}
}

type ObservationPage struct {
	ObservedAt   time.Time             `json:"observed_at"`
	Allocations  []*ResourceAllocation `json:"allocations"`
	OwnerID      string                `json:"owner_id"`
	Events       []BrokerEvent         `json:"events"`
	NextCursor   string                `json:"next_cursor"`
	Gap          bool                  `json:"gap"`
	More         bool                  `json:"more"`
	Observations []ResourceObservation `json:"observations"`
}

// readObservationPage reads the independent owner archive. It does not acknowledge,
// delete, create state, or reconstruct archive ownership from reusable counters.
func (recorder EventRecorder) readObservationPage(client, cursor string, count int) (ObservationPage, error) {
	page := ObservationPage{ObservedAt: recorder.now(), Allocations: []*ResourceAllocation{}, OwnerID: resourceOwnerID(recorder.Source), Events: []BrokerEvent{}, NextCursor: cursor, Observations: []ResourceObservation{}}
	if !agentLogClientID.MatchString(client) || (cursor != "-" && !brokerRecordID.MatchString(cursor)) || count < 1 || count > maxAgentLogEvents {
		return page, errors.New("invalid observer selection")
	}
	entries, err := os.ReadDir(recorder.agentLogDirectory(client, "events"))
	if os.IsNotExist(err) {
		page.Gap = cursor != "-"
		return page, nil
	}
	if err != nil {
		return page, err
	}
	// The archive has a fixed byte/age cap. Keep only the earliest bounded page.
	var selected []BrokerEvent
	found := cursor == "-"
	var after uint64
	if cursor != "-" {
		body, readErr := readAgentLogRecord(filepath.Join(recorder.agentLogDirectory(client, "events"), cursor+".json"), maxBrokerEventBytes)
		var event BrokerEvent
		if readErr == nil && json.Unmarshal(body, &event) == nil && validateBrokerEvent(event) == nil && event.EventID == cursor && recorder.now().Sub(event.Timestamp) < agentLogRetention {
			after = event.Sequence
			found = true
		}
	}
	page.Gap = !found
	for _, entry := range entries {
		id := strings.TrimSuffix(entry.Name(), ".json")
		if entry.Name() != id+".json" || !brokerRecordID.MatchString(id) {
			continue
		}
		body, readErr := readAgentLogRecord(filepath.Join(recorder.agentLogDirectory(client, "events"), entry.Name()), maxBrokerEventBytes)
		if os.IsNotExist(readErr) {
			continue
		}
		if readErr != nil {
			return page, readErr
		}
		var event BrokerEvent
		if err := json.Unmarshal(body, &event); err != nil {
			return page, err
		}
		if err := validateBrokerEvent(event); err != nil {
			return page, err
		}
		if event.EventID != id || recorder.now().Sub(event.Timestamp) >= agentLogRetention || event.Sequence <= after {
			continue
		}
		event.Error = strings.ReplaceAll(event.Error, client, "[redacted]")
		selected = append(selected, event)
		sortBrokerEvents(selected)
		if len(selected) > count+1 {
			selected = selected[:count+1]
		}
	}
	total := 2
	for _, event := range selected {
		body, _ := json.Marshal(event)
		if len(page.Events) == count || total+len(body)+1 > maxAgentLogBytes {
			page.More = true
			break
		}
		total += len(body) + 1
		page.Events = append(page.Events, event)
		page.NextCursor = event.EventID
	}
	return page, nil
}

// Observe reads exact managed QEMU processes, including their threads. Incus
// instance-state CPU/memory may be guest-agent metrics and are not host usage.
// RSS/PSS and process block-I/O bytes retain their narrower native semantics.
func (rt *Runtime) Observe(ctx context.Context, slots []LeaseSlot) []ResourceObservation {
	rt.prepareDefaults()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var result []ResourceObservation
	for _, slot := range slots {
		if slot.Environment == nil {
			continue
		}
		index, err := slotNumber(slot.SlotID, rt.Config.SlotCount)
		if err != nil {
			continue
		}
		child := rt.slotRuntime(index, "")
		child.useLeaseSlot(slot)
		for number := 1; number <= slot.Environment.Count; number++ {
			sample := ResourceObservation{SlotID: slot.SlotID, ResourceGeneration: slot.ResourceGeneration, LeaseEpoch: slot.LeaseEpoch, Selector: strconv.Itoa(number), State: slot.State, Phase: "periodic", SourceTime: rt.Now().UTC(), Missing: []string{}}
			boot, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
			sample.BootID = strings.TrimSpace(string(boot))
			if uptime, err := os.ReadFile("/proc/uptime"); err == nil {
				fields := strings.Fields(string(uptime))
				if len(fields) > 0 {
					parts := strings.SplitN(fields[0], ".", 2)
					sec, err := strconv.ParseUint(parts[0], 10, 64)
					if err == nil && sec <= ^uint64(0)/1000000000 {
						ns := sec * 1000000000
						if len(parts) == 2 && len(parts[1]) <= 9 {
							frac, e := strconv.ParseUint(parts[1]+strings.Repeat("0", 9-len(parts[1])), 10, 64)
							if e == nil {
								ns += frac
							}
						}
						sample.MonotonicNS = &ns
					}
				}
			}
			vm := child.Config.vm(number)
			metadata, err := child.incus(ctx, "query", "/1.0/instances/"+vm+"?project="+child.Config.Project)
			var instance struct {
				Type   string            `json:"type"`
				Config map[string]string `json:"config"`
			}
			valid := err == nil && json.Unmarshal([]byte(metadata), &instance) == nil && instance.Type == "virtual-machine" && instance.Config["user.subyard.managed"] == managedMarker && instance.Config["user.subyard.generation"] == strconv.FormatUint(slot.ResourceGeneration, 10) && instance.Config["user.subyard.lease-epoch"] == strconv.FormatUint(slot.LeaseEpoch, 10)
			if valid {
				body, stateErr := child.incus(ctx, "query", "/1.0/instances/"+vm+"/state?project="+child.Config.Project)
				var state struct {
					PID int `json:"pid"`
				}
				if stateErr == nil && json.Unmarshal([]byte(body), &state) == nil && state.PID > 1 {
					ticksBody, _, ticksErr := rt.Runner.Run(ctx, "getconf", []string{"CLK_TCK"}, nil, nil)
					ticks, parseErr := strconv.ParseUint(strings.TrimSpace(string(ticksBody)), 10, 64)
					pid := strconv.Itoa(state.PID)
					uuid := instance.Config["volatile.uuid"]
					if ticksErr == nil && parseErr == nil && ticks > 0 && ownedQEMUProcess("/proc", pid, uuid) {
						_ = sample.readProcess("/proc", pid, ticks)
						if !ownedQEMUProcess("/proc", pid, uuid) {
							sample.CPU = nil
							sample.RSS = nil
							sample.PSS = nil
							sample.RSSPeak = nil
							sample.Swap = nil
							sample.IORead = nil
							sample.IOWrite = nil
							sample.CounterGeneration = ""
						}
					}
				}
			}
			for name, value := range map[string]*uint64{"cpu_ns": sample.CPU, "memory_bytes": sample.Memory, "memory_peak_bytes": sample.MemoryPeak, "swap_bytes": sample.Swap, "io_read_bytes": sample.IORead, "io_write_bytes": sample.IOWrite, "io_read_ops": sample.IOReadOps, "io_write_ops": sample.IOWriteOps, "rss_bytes": sample.RSS, "pss_bytes": sample.PSS, "rss_peak_bytes": sample.RSSPeak} {
				if value == nil {
					sample.Missing = append(sample.Missing, name)
				}
			}
			sort.Strings(sample.Missing)
			result = append(result, sample)
		}
	}
	return result
}

// readProcess rejects PID reuse and keeps unavailable counters null. Syscall
// counts (syscr/syscw) are deliberately not advertised as block-I/O operations.
func (sample *ResourceObservation) readProcess(proc, pid string, ticks uint64) error {
	before, err := os.ReadFile(filepath.Join(proc, pid, "stat"))
	if err != nil {
		return err
	}
	start := brokerProcessStart(before)
	if start == "" || ticks == 0 {
		return errors.New("process identity unavailable")
	}
	fields := strings.Fields(string(before)[strings.LastIndex(string(before), ")")+1:])
	if len(fields) < 20 {
		return errors.New("process stat unavailable")
	}
	user, uErr := strconv.ParseUint(fields[11], 10, 64)
	system, sErr := strconv.ParseUint(fields[12], 10, 64)
	if uErr == nil && sErr == nil && ^uint64(0)-user >= system && user+system <= ^uint64(0)/1000000000 {
		cpu := (user + system) * 1000000000 / ticks
		sample.CPU = &cpu
	}
	sample.Accounting = "qemu-process-and-threads"
	if body, err := os.ReadFile(filepath.Join(proc, pid, "status")); err == nil {
		for _, line := range strings.Split(string(body), "\n") {
			parts := strings.Fields(line)
			if len(parts) != 3 || parts[2] != "kB" {
				continue
			}
			v, e := strconv.ParseUint(parts[1], 10, 64)
			if e != nil || v > ^uint64(0)/1024 {
				continue
			}
			v *= 1024
			switch parts[0] {
			case "VmRSS:":
				sample.RSS = &v
			case "VmHWM:":
				sample.RSSPeak = &v
			case "VmSwap:":
				sample.Swap = &v
			}
		}
	}
	if body, err := os.ReadFile(filepath.Join(proc, pid, "smaps_rollup")); err == nil {
		for _, line := range strings.Split(string(body), "\n") {
			parts := strings.Fields(line)
			if len(parts) == 3 && parts[0] == "Pss:" && parts[2] == "kB" {
				v, e := strconv.ParseUint(parts[1], 10, 64)
				if e == nil && v <= ^uint64(0)/1024 {
					v *= 1024
					sample.PSS = &v
				}
			}
		}
	}
	if body, err := os.ReadFile(filepath.Join(proc, pid, "io")); err == nil {
		for _, line := range strings.Split(string(body), "\n") {
			parts := strings.Fields(line)
			if len(parts) != 2 {
				continue
			}
			v, e := strconv.ParseUint(parts[1], 10, 64)
			if e != nil {
				continue
			}
			switch parts[0] {
			case "read_bytes:":
				sample.IORead = &v
			case "write_bytes:":
				sample.IOWrite = &v
			}
		}
	}
	after, err := os.ReadFile(filepath.Join(proc, pid, "stat"))
	if err != nil || brokerProcessStart(after) != start {
		sample.CPU = nil
		sample.RSS = nil
		sample.PSS = nil
		sample.RSSPeak = nil
		sample.Swap = nil
		sample.IORead = nil
		sample.IOWrite = nil
		return errors.New("process identity changed")
	}
	boot, _ := os.ReadFile(filepath.Join(proc, "sys/kernel/random/boot_id"))
	sample.BootID = strings.TrimSpace(string(boot))
	digest := sha256.Sum256([]byte(sample.BootID + ":" + pid + ":" + start))
	sample.CounterGeneration = hex.EncodeToString(digest[:])
	return nil
}

func (rt *Runtime) recordObservation(ctx context.Context, slot LeaseSlot, phase string) {
	samples := rt.Observe(ctx, []LeaseSlot{slot})
	for index := range samples {
		samples[index].Phase = phase
	}
	_, _ = rt.eventRecorder().Record(BrokerEvent{Kind: "resource.observation", SlotID: slot.SlotID, ResourceGeneration: slot.ResourceGeneration, LeaseEpoch: slot.LeaseEpoch, Context: leaseContextFromSlot(slot), Allocation: publicResourceAllocation(slot), agentClientID: slot.ClientID, Observations: samples})
}

func resourceOwnerID(source string) string {
	machine, err := os.ReadFile("/etc/machine-id")
	if err != nil || len(strings.TrimSpace(string(machine))) != 32 {
		return ""
	}
	if _, err := hex.DecodeString(strings.TrimSpace(string(machine))); err != nil {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(string(machine)) + ":" + source))
	return hex.EncodeToString(sum[:])
}

// The native instance UUID is used only for ownership validation. Command-line
// bytes and UUIDs never leave this reader or enter the observation journal.
func ownedQEMUProcess(proc, pid, uuid string) bool {
	if len(uuid) != 36 {
		return false
	}
	if _, err := hex.DecodeString(strings.ReplaceAll(uuid, "-", "")); err != nil {
		return false
	}
	body, err := os.ReadFile(filepath.Join(proc, pid, "cmdline"))
	if err != nil || len(body) > 65536 {
		return false
	}
	args := strings.Split(string(body), "\x00")
	if len(args) == 0 || !strings.HasPrefix(filepath.Base(args[0]), "qemu-system-") {
		return false
	}
	for index, arg := range args {
		if arg == "-uuid" && index+1 < len(args) && args[index+1] == uuid {
			return true
		}
	}
	return false
}
