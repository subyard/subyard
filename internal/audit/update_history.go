package audit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
)

const (
	UpdateHistorySchema    = "yard.update-history.v1"
	UpdateHistoryRetention = 30
	maxUpdateRecordBytes   = 64 << 10
)

type UpdateRecord struct {
	SchemaVersion string        `json:"schema"`
	AttemptID     string        `json:"attemptId"`
	OperationID   string        `json:"operationId"`
	Direction     string        `json:"direction"`
	Phase         string        `json:"phase"`
	Status        string        `json:"status"`
	SourceRelease string        `json:"sourceRelease,omitempty"`
	SourceVersion string        `json:"sourceVersion,omitempty"`
	TargetRelease string        `json:"targetRelease,omitempty"`
	TargetVersion string        `json:"targetVersion,omitempty"`
	ErrorCode     string        `json:"errorCode,omitempty"`
	StartedAt     time.Time     `json:"startedAt"`
	UpdatedAt     time.Time     `json:"updatedAt"`
	FinishedAt    time.Time     `json:"finishedAt,omitempty"`
	Events        []UpdateEvent `json:"events"`
}

type UpdateEvent struct {
	Phase     string    `json:"phase"`
	At        time.Time `json:"at"`
	Status    string    `json:"status"`
	ErrorCode string    `json:"errorCode,omitempty"`
}

type UpdateHistory struct {
	BeforeWrite func(context.Context) (func(), bool, error)
	Home        string
	Now         func() time.Time
}

type UpdateRecorder struct {
	history UpdateHistory
	record  UpdateRecord
}

func (history UpdateHistory) Begin(record UpdateRecord) (*UpdateRecorder, error) {
	if history.Home == "" {
		return nil, errors.New("update history home is required")
	}
	if !safeHistoryID(record.OperationID) {
		return nil, errors.New("update history operation ID is invalid")
	}
	if record.Direction != "activate" && record.Direction != "rollback" {
		return nil, errors.New("update history direction is invalid")
	}
	now := history.now()
	attemptID, err := newUpdateAttemptID()
	if err != nil {
		return nil, err
	}
	record.AttemptID = attemptID
	record.SchemaVersion = UpdateHistorySchema
	record.Phase = "execute"
	record.Status = "unfinished"
	record.ErrorCode = ""
	record.StartedAt = now
	record.UpdatedAt = now
	record.FinishedAt = time.Time{}
	record.Events = []UpdateEvent{
		{Phase: "prepare", At: now, Status: "success"},
		{Phase: "execute", At: now, Status: "running"},
	}
	recorder := &UpdateRecorder{history: history, record: record}
	if err := history.write(record); err != nil {
		return nil, err
	}
	return recorder, nil
}

func (recorder *UpdateRecorder) Advance(phase, targetRelease, targetVersion string) error {
	if recorder == nil {
		return errors.New("update recorder is required")
	}
	if phase != "execute" && phase != "refresh" {
		return errors.New("update history phase is invalid")
	}
	record := recorder.record
	record.Phase = phase
	record.UpdatedAt = recorder.history.now()
	setUpdateTarget(&record, targetRelease, targetVersion)
	record.Events = append(record.Events, UpdateEvent{Phase: phase, At: record.UpdatedAt, Status: "running"})
	recorder.record = record
	if err := recorder.history.write(record); err != nil {
		return err
	}
	return nil
}

func (recorder *UpdateRecorder) Finish(phase, status, errorCode, targetRelease, targetVersion string) error {
	if recorder == nil {
		return errors.New("update recorder is required")
	}
	switch status {
	case "success", "failure", "interrupted", "declined":
	default:
		return errors.New("update history status is invalid")
	}
	if errorCode != "" && !safeHistoryToken(errorCode) {
		return errors.New("update history error code is invalid")
	}
	record := recorder.record
	if phase != "execute" && phase != "refresh" {
		return errors.New("update history terminal phase is invalid")
	}
	record.Status = status
	record.ErrorCode = errorCode
	record.UpdatedAt = recorder.history.now()
	record.FinishedAt = record.UpdatedAt
	setUpdateTarget(&record, targetRelease, targetVersion)
	record.Events = append(record.Events, UpdateEvent{
		Phase: phase, At: record.UpdatedAt, Status: status, ErrorCode: errorCode,
	})
	record.Phase = "result"
	record.Events = append(record.Events, UpdateEvent{
		Phase: "result", At: record.UpdatedAt, Status: status, ErrorCode: errorCode,
	})
	if err := recorder.history.write(record); err != nil {
		return err
	}
	recorder.record = record
	return nil
}

func (history UpdateHistory) Terminal(record UpdateRecord, phase, status, errorCode string) error {
	if history.Home == "" {
		return errors.New("update history home is required")
	}
	if !safeHistoryID(record.OperationID) {
		return errors.New("update history operation ID is invalid")
	}
	if record.Direction != "activate" && record.Direction != "rollback" {
		return errors.New("update history direction is invalid")
	}
	if phase != "prepare" && phase != "confirmation" && phase != "execute" && phase != "refresh" {
		return errors.New("update history phase is invalid")
	}
	switch status {
	case "failure", "interrupted", "declined":
	default:
		return errors.New("update history terminal status is invalid")
	}
	if errorCode != "" && !safeHistoryToken(errorCode) {
		return errors.New("update history error code is invalid")
	}
	now := history.now()
	var err error
	record.AttemptID, err = newUpdateAttemptID()
	if err != nil {
		return err
	}
	record.SchemaVersion = UpdateHistorySchema
	record.Phase = "result"
	record.Status = status
	record.ErrorCode = errorCode
	record.StartedAt = now
	record.UpdatedAt = now
	record.FinishedAt = now
	record.Events = make([]UpdateEvent, 0, 3)
	if phase == "confirmation" {
		record.Events = append(record.Events, UpdateEvent{Phase: "prepare", At: now, Status: "success"})
	}
	record.Events = append(record.Events,
		UpdateEvent{Phase: phase, At: now, Status: status, ErrorCode: errorCode},
		UpdateEvent{Phase: "result", At: now, Status: status, ErrorCode: errorCode},
	)
	return history.write(record)
}

func (history UpdateHistory) Read(limit int) ([]UpdateRecord, error) {
	return history.store().read(limit)
}

func (history UpdateHistory) write(record UpdateRecord) error {
	return history.store().write(record)
}

func (history UpdateHistory) store() attemptStore[UpdateRecord] {
	return attemptStore[UpdateRecord]{
		home: history.Home, beforeWrite: history.BeforeWrite, label: "update history",
		directory: "updates", lock: ".updates.lock", temp: ".update-*.tmp",
		retention: UpdateHistoryRetention, maxBytes: maxUpdateRecordBytes,
		identity: func(record UpdateRecord) (string, time.Time) { return record.AttemptID, record.StartedAt },
		valid:    validUpdateRecord,
	}
}

func (history UpdateHistory) now() time.Time {
	if history.Now != nil {
		return history.Now().UTC()
	}
	return time.Now().UTC()
}

func setUpdateTarget(record *UpdateRecord, release, version string) {
	if release != "" {
		record.TargetRelease = release
	}
	if version != "" {
		record.TargetVersion = version
	}
}

func safeHistoryToken(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, character := range value {
		if character < 'a' || character > 'z' {
			if character < '0' || character > '9' {
				if character != '-' && character != '_' && character != '.' {
					return false
				}
			}
		}
	}
	return true
}

func safeHistoryID(value string) bool {
	return len(value) <= 128 && domain.SafeID(value)
}

func validUpdateRecord(record UpdateRecord) bool {
	if record.SchemaVersion != UpdateHistorySchema || !safeHistoryID(record.AttemptID) ||
		!safeHistoryID(record.OperationID) || (record.Direction != "activate" && record.Direction != "rollback") ||
		len(record.Events) == 0 || len(record.Events) > 32 || record.StartedAt.IsZero() || record.UpdatedAt.IsZero() {
		return false
	}
	if record.Phase != "execute" && record.Phase != "refresh" && record.Phase != "result" {
		return false
	}
	switch record.Status {
	case "unfinished", "success", "failure", "interrupted", "declined":
	default:
		return false
	}
	for _, value := range []string{
		record.SourceRelease, record.SourceVersion, record.TargetRelease, record.TargetVersion,
	} {
		if value != "" && !safeHistoryID(value) {
			return false
		}
	}
	if record.ErrorCode != "" && !safeHistoryToken(record.ErrorCode) {
		return false
	}
	for _, event := range record.Events {
		if event.At.IsZero() || (event.Phase != "prepare" && event.Phase != "confirmation" &&
			event.Phase != "execute" && event.Phase != "refresh" && event.Phase != "result") {
			return false
		}
		switch event.Status {
		case "running", "success", "failure", "interrupted", "declined":
		default:
			return false
		}
		if event.ErrorCode != "" && !safeHistoryToken(event.ErrorCode) {
			return false
		}
	}
	return true
}

func newUpdateAttemptID() (string, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("generate update history attempt ID: %w", err)
	}
	return "attempt-" + hex.EncodeToString(suffix[:]), nil
}

// ValidHistoryCode reports whether value may be stored as a history code.
func ValidHistoryCode(value string) bool { return safeHistoryToken(value) }
