// Package hosterrors builds the value-free snapshot of owner-side failures
// that the owner publishes into a yard for its agents: failed RPC requests,
// release updates and setup attempts. It carries no error text, request or
// operation IDs, values or paths, and never another yard's records.
package hosterrors

import (
	"encoding/json"
	"regexp"
	"time"

	"github.com/Subyard/Subyard/internal/audit"
	"github.com/Subyard/Subyard/internal/rpc"
)

const (
	Schema = 1
	// MaxBytes bounds the encoded snapshot; the guest reader enforces it too.
	MaxBytes = 32 << 10
	// SectionEntries bounds each section; omitted counts the older entries.
	SectionEntries = 10
	// SnapshotRefusals bounds the refusals shown for one setup attempt.
	SnapshotRefusals = 8
	// rpcRecordLimit bounds the RPC records read from the two log generations.
	rpcRecordLimit = 100000
	// GeneratedLayout orders snapshots by text; other times are in seconds.
	GeneratedLayout = "2006-01-02T15:04:05.000000000Z"
	timeLayout      = "2006-01-02T15:04:05Z"
)

// Snapshot is the published document. Every key is always present, newest
// entries first; the guest adds its machine ID when it stores the snapshot.
type Snapshot struct {
	Schema      int           `json:"schema"`
	Yard        string        `json:"yard"`
	GeneratedAt string        `json:"generated_at"`
	RPC         []RPCEntry    `json:"rpc"`
	Updates     []UpdateEntry `json:"updates"`
	Setup       []SetupEntry  `json:"setup"`
	Omitted     Omitted       `json:"omitted"`
	// Unavailable names the sections whose host source could not be read.
	Unavailable []string `json:"unavailable"`
}

// RPCEntry is one failure, or a run of identical consecutive failures.
type RPCEntry struct {
	At     string `json:"at"`
	Last   string `json:"last"`
	Count  int    `json:"count"`
	Method string `json:"method"`
	Code   string `json:"code"`
}

type UpdateEntry struct {
	Attempt   string `json:"attempt"`
	At        string `json:"at"`
	Direction string `json:"direction"`
	Phase     string `json:"phase"`
	Status    string `json:"status"`
	Code      string `json:"code"`
	Source    string `json:"source"`
	Target    string `json:"target"`
}

type SetupEntry struct {
	Attempt         string    `json:"attempt"`
	At              string    `json:"at"`
	Phase           string    `json:"phase"`
	Status          string    `json:"status"`
	Code            string    `json:"code"`
	Refusals        []Refusal `json:"refusals"`
	OmittedRefusals int       `json:"omitted_refusals"`
	Incomplete      bool      `json:"incomplete"`
}

type Refusal struct {
	Source string   `json:"source"`
	Code   string   `json:"code"`
	Names  []string `json:"names"`
}

type Omitted struct {
	RPC     int `json:"rpc"`
	Updates int `json:"updates"`
	Setup   int `json:"setup"`
}

// token is the shared value rule of the guest reader; anything else becomes
// "unknown" here, so one odd value never makes the guest refuse the snapshot.
var token = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

func safeToken(value string) string {
	if token.MatchString(value) {
		return value
	}
	return "unknown"
}

// optionalToken keeps an absent value empty.
func optionalToken(value string) string {
	if value == "" {
		return ""
	}
	return safeToken(value)
}

func stamp(at time.Time) string { return at.UTC().Format(timeLayout) }

// Build reads the owner's histories under home and returns the snapshot for
// yard: its own RPC and setup records, RPC records without a yard, and every
// update attempt. A source that cannot be read is named in Unavailable.
func Build(home, yard string, now time.Time) Snapshot {
	snapshot := Snapshot{
		Schema: Schema, Yard: yard, GeneratedAt: now.UTC().Format(GeneratedLayout),
		RPC: []RPCEntry{}, Updates: []UpdateEntry{}, Setup: []SetupEntry{}, Unavailable: []string{},
	}
	if records, err := (audit.RPCErrorLog{Home: home}).Read(rpcRecordLimit); err != nil {
		snapshot.Unavailable = append(snapshot.Unavailable, "rpc")
	} else {
		snapshot.RPC, snapshot.Omitted.RPC = rpcEntries(records, yard)
	}
	if records, err := (audit.UpdateHistory{Home: home}).Read(audit.UpdateHistoryRetention); err != nil {
		snapshot.Unavailable = append(snapshot.Unavailable, "updates")
	} else {
		for _, record := range records {
			snapshot.Updates = append(snapshot.Updates, updateEntry(record))
		}
		snapshot.Updates, snapshot.Omitted.Updates = newest(snapshot.Updates)
	}
	if records, err := (audit.SetupHistory{Home: home}).Read(audit.SetupHistoryRetention); err != nil {
		snapshot.Unavailable = append(snapshot.Unavailable, "setup")
	} else {
		for _, record := range records {
			if record.Yard == yard {
				snapshot.Setup = append(snapshot.Setup, setupEntry(record))
			}
		}
		snapshot.Setup, snapshot.Omitted.Setup = newest(snapshot.Setup)
	}
	snapshot.fit()
	return snapshot
}

// rpcEntries collapses consecutive identical failures of the yard, oldest
// first, and returns the newest entries first.
func rpcEntries(records []audit.RPCError, yard string) ([]RPCEntry, int) {
	methods := rpc.MethodCapabilities()
	entries := []RPCEntry{}
	for _, record := range records {
		if record.Yard != "" && record.Yard != yard {
			continue
		}
		method, code := "unknown", "unknown"
		if _, known := methods[record.Method]; known {
			method = record.Method
		}
		if rpc.ErrorCode(record.Code).Valid() {
			code = record.Code
		}
		at := stamp(record.At)
		if last := len(entries) - 1; last >= 0 && entries[last].Method == method && entries[last].Code == code {
			entries[last].Count++
			entries[last].Last = at
			continue
		}
		entries = append(entries, RPCEntry{At: at, Last: at, Count: 1, Method: method, Code: code})
	}
	for left, right := 0, len(entries)-1; left < right; left, right = left+1, right-1 {
		entries[left], entries[right] = entries[right], entries[left]
	}
	return newest(entries)
}

func updateEntry(record audit.UpdateRecord) UpdateEntry {
	// A finished attempt names the phase that ended it, not the result marker.
	phase := record.Phase
	if phase == "result" {
		for _, event := range record.Events {
			if event.Phase != "result" {
				phase = event.Phase
			}
		}
	}
	return UpdateEntry{
		Attempt: safeToken(record.AttemptID), At: stamp(record.StartedAt),
		Direction: safeToken(record.Direction), Phase: safeToken(phase), Status: safeToken(record.Status),
		Code:   optionalToken(record.ErrorCode),
		Source: optionalToken(firstNonempty(record.SourceVersion, record.SourceRelease)),
		Target: optionalToken(firstNonempty(record.TargetVersion, record.TargetRelease)),
	}
}

func setupEntry(record audit.SetupRecord) SetupEntry {
	entry := SetupEntry{
		Attempt: safeToken(record.AttemptID), At: stamp(record.StartedAt),
		Phase: safeToken(record.Phase), Status: safeToken(record.Status), Code: optionalToken(record.ErrorCode),
		Refusals: []Refusal{}, OmittedRefusals: record.OmittedRefusals, Incomplete: record.Incomplete,
	}
	for _, refusal := range record.Refusals {
		if len(entry.Refusals) == SnapshotRefusals {
			entry.OmittedRefusals++
			continue
		}
		names := make([]string, 0, len(refusal.Names))
		for _, name := range refusal.Names {
			names = append(names, safeToken(name))
		}
		entry.Refusals = append(entry.Refusals, Refusal{Source: safeToken(refusal.Source), Code: safeToken(refusal.Code), Names: names})
	}
	return entry
}

func newest[T any](entries []T) ([]T, int) {
	if len(entries) <= SectionEntries {
		return entries, 0
	}
	return entries[:SectionEntries], len(entries) - SectionEntries
}

// fit drops the oldest entries, setup first, until the encoding is in bounds.
func (snapshot *Snapshot) fit() {
	for {
		payload, err := json.Marshal(snapshot)
		if err == nil && len(payload) <= MaxBytes {
			return
		}
		switch {
		case len(snapshot.Setup) > 0:
			snapshot.Setup = snapshot.Setup[:len(snapshot.Setup)-1]
			snapshot.Omitted.Setup++
		case len(snapshot.RPC) > 0:
			snapshot.RPC = snapshot.RPC[:len(snapshot.RPC)-1]
			snapshot.Omitted.RPC++
		case len(snapshot.Updates) > 0:
			snapshot.Updates = snapshot.Updates[:len(snapshot.Updates)-1]
			snapshot.Omitted.Updates++
		default:
			return
		}
	}
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
