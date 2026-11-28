package audit

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
)

const (
	SetupHistorySchema    = "yard.setup-history.v1"
	SetupHistoryRetention = 30
	// MaxSetupRefusals bounds the refusals of one attempt; OmittedRefusals
	// counts the rest.
	MaxSetupRefusals     = 32
	maxSetupRefusalNames = 16
	maxSetupRecordBytes  = 32 << 10
)

// SetupRecord is one `yard init` (`yard setup`) attempt on this owner. It
// holds value-free facts only: phase, status, a code and the typed refusals
// with the validated names they refer to, never error text or values.
type SetupRecord struct {
	SchemaVersion   string         `json:"schema"`
	AttemptID       string         `json:"attemptId"`
	OperationID     string         `json:"operationId"`
	Yard            string         `json:"yard"`
	Phase           string         `json:"phase"`
	Status          string         `json:"status"`
	ErrorCode       string         `json:"errorCode,omitempty"`
	Refusals        []SetupRefusal `json:"refusals,omitempty"`
	OmittedRefusals int            `json:"omittedRefusals,omitempty"`
	// Incomplete means the failure had causes that are not typed refusals.
	Incomplete bool      `json:"incomplete,omitempty"`
	StartedAt  time.Time `json:"startedAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
	FinishedAt time.Time `json:"finishedAt,omitzero"`
}

// SetupRefusal is one typed refusal: its source, code and the validated
// setting, profile, integration or yard-kind names it names.
type SetupRefusal struct {
	Source string   `json:"source"`
	Code   string   `json:"code"`
	Names  []string `json:"names,omitempty"`
}

// SetupOutcome is the terminal result of an attempt.
type SetupOutcome struct {
	Status     string
	ErrorCode  string
	Refusals   []SetupRefusal
	Incomplete bool
}

type SetupHistory struct {
	BeforeWrite func(context.Context) (func(), bool, error)
	Home        string
	Now         func() time.Time
}

type SetupRecorder struct {
	history SetupHistory
	record  SetupRecord
}

// Begin records an unfinished execution, so an interrupted process leaves it
// unfinished rather than absent.
func (history SetupHistory) Begin(operationID, yard string) (*SetupRecorder, error) {
	record, err := history.newRecord(operationID, yard)
	if err != nil {
		return nil, err
	}
	record.Phase, record.Status = "execute", "unfinished"
	if err := history.store().write(record); err != nil {
		return nil, err
	}
	return &SetupRecorder{history: history, record: record}, nil
}

// Finish records the execution's success, failure or interruption.
func (recorder *SetupRecorder) Finish(outcome SetupOutcome) error {
	if recorder == nil {
		return errors.New("setup recorder is required")
	}
	if !slices.Contains([]string{"success", "failure", "interrupted"}, outcome.Status) {
		return errors.New("setup history status is invalid")
	}
	record := recorder.record
	if err := applySetupOutcome(&record, outcome, recorder.history.now()); err != nil {
		return err
	}
	if err := recorder.history.store().write(record); err != nil {
		return err
	}
	recorder.record = record
	return nil
}

// Terminal records an attempt that ended before its execution began.
func (history SetupHistory) Terminal(operationID, yard, phase string, outcome SetupOutcome) error {
	if phase != "prepare" && phase != "confirmation" && phase != "execute" {
		return errors.New("setup history terminal phase is invalid")
	}
	if !slices.Contains([]string{"failure", "interrupted", "declined"}, outcome.Status) {
		return errors.New("setup history terminal status is invalid")
	}
	record, err := history.newRecord(operationID, yard)
	if err != nil {
		return err
	}
	record.Phase = phase
	if err := applySetupOutcome(&record, outcome, record.StartedAt); err != nil {
		return err
	}
	return history.store().write(record)
}

// Read returns the newest attempts first, at most limit of them.
func (history SetupHistory) Read(limit int) ([]SetupRecord, error) {
	return history.store().read(limit)
}

func (history SetupHistory) newRecord(operationID, yard string) (SetupRecord, error) {
	if history.Home == "" {
		return SetupRecord{}, errors.New("setup history home is required")
	}
	if !safeHistoryID(operationID) {
		return SetupRecord{}, errors.New("setup history operation ID is invalid")
	}
	if !safeSetupYard(yard) {
		return SetupRecord{}, errors.New("setup history yard is invalid")
	}
	attemptID, err := newUpdateAttemptID()
	if err != nil {
		return SetupRecord{}, err
	}
	now := history.now()
	return SetupRecord{
		SchemaVersion: SetupHistorySchema, AttemptID: attemptID, OperationID: operationID, Yard: yard,
		StartedAt: now, UpdatedAt: now,
	}, nil
}

// applySetupOutcome keeps only refusals whose source, code and names are
// value-free tokens; a dropped refusal makes the record incomplete.
func applySetupOutcome(record *SetupRecord, outcome SetupOutcome, now time.Time) error {
	if outcome.ErrorCode != "" && !safeHistoryToken(outcome.ErrorCode) {
		return errors.New("setup history error code is invalid")
	}
	if outcome.Status == "success" && (outcome.ErrorCode != "" || len(outcome.Refusals) != 0 || outcome.Incomplete) {
		return errors.New("setup history success has failure facts")
	}
	record.Status, record.ErrorCode = outcome.Status, outcome.ErrorCode
	record.Refusals, record.OmittedRefusals, record.Incomplete = nil, 0, outcome.Incomplete
	for _, refusal := range outcome.Refusals {
		if !validSetupRefusal(refusal) {
			record.Incomplete = true
			continue
		}
		if len(record.Refusals) == MaxSetupRefusals {
			record.OmittedRefusals++
			continue
		}
		record.Refusals = append(record.Refusals, SetupRefusal{
			Source: refusal.Source, Code: refusal.Code, Names: slices.Clone(refusal.Names),
		})
	}
	record.UpdatedAt, record.FinishedAt = now, now
	return nil
}

func (history SetupHistory) store() attemptStore[SetupRecord] {
	return attemptStore[SetupRecord]{
		home: history.Home, beforeWrite: history.BeforeWrite, label: "setup history",
		directory: "setup", lock: ".setup.lock", temp: ".setup-*.tmp",
		retention: SetupHistoryRetention, maxBytes: maxSetupRecordBytes,
		identity: func(record SetupRecord) (string, time.Time) { return record.AttemptID, record.StartedAt },
		valid:    validSetupRecord,
	}
}

func (history SetupHistory) now() time.Time {
	if history.Now != nil {
		return history.Now().UTC()
	}
	return time.Now().UTC()
}

// setupName matches the validated names a typed refusal may carry.
var setupName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

func safeSetupYard(yard string) bool {
	return len(yard) <= 63 && domain.SafeName(yard)
}

func validSetupRefusal(refusal SetupRefusal) bool {
	if (refusal.Source != "config" && refusal.Source != "release-gate") || !safeHistoryToken(refusal.Code) ||
		len(refusal.Names) > maxSetupRefusalNames {
		return false
	}
	for _, name := range refusal.Names {
		if !setupName.MatchString(name) {
			return false
		}
	}
	return true
}

func validSetupRecord(record SetupRecord) bool {
	if record.SchemaVersion != SetupHistorySchema || !safeHistoryID(record.AttemptID) ||
		!safeHistoryID(record.OperationID) || !safeSetupYard(record.Yard) ||
		record.StartedAt.IsZero() || record.UpdatedAt.IsZero() ||
		len(record.Refusals) > MaxSetupRefusals || record.OmittedRefusals < 0 {
		return false
	}
	switch record.Phase {
	case "prepare", "confirmation", "execute":
	default:
		return false
	}
	switch record.Status {
	case "unfinished", "success", "failure", "interrupted", "declined":
	default:
		return false
	}
	if record.ErrorCode != "" && !safeHistoryToken(record.ErrorCode) {
		return false
	}
	for _, refusal := range record.Refusals {
		if !validSetupRefusal(refusal) {
			return false
		}
	}
	return true
}
