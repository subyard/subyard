package audit

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
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
	Home string
	Now  func() time.Time
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
	if history.Home == "" {
		return nil, errors.New("update history home is required")
	}
	if limit < 1 {
		return nil, errors.New("update history limit must be positive")
	}
	directory := filepath.Join(history.Home, "logs", "updates")
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return []UpdateRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	records := make([]UpdateRecord, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		record, err := readUpdateRecord(filepath.Join(directory, entry.Name()))
		if err != nil {
			return nil, err
		}
		if entry.Name() != record.AttemptID+".json" {
			return nil, errors.New("update history filename does not match attempt ID")
		}
		records = append(records, record)
	}
	sort.Slice(records, func(left, right int) bool {
		if records[left].StartedAt.Equal(records[right].StartedAt) {
			return records[left].AttemptID > records[right].AttemptID
		}
		return records[left].StartedAt.After(records[right].StartedAt)
	})
	if limit > len(records) {
		limit = len(records)
	}
	return records[:limit], nil
}

func (history UpdateHistory) write(record UpdateRecord) error {
	logs := filepath.Join(history.Home, "logs")
	if err := ensurePrivateDirectory(logs); err != nil {
		return err
	}
	directory := filepath.Join(logs, "updates")
	if err := ensurePrivateDirectory(directory); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(logs, ".updates.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck
	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	if len(payload) > maxUpdateRecordBytes {
		return errors.New("update history record is too large")
	}
	path := filepath.Join(directory, record.AttemptID+".json")
	temporary, err := os.CreateTemp(directory, ".update-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	_, err = temporary.Write(payload)
	if err == nil {
		err = temporary.Sync()
	}
	closeErr := temporary.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	directoryHandle, err := os.Open(directory)
	if err != nil {
		return err
	}
	err = directoryHandle.Sync()
	closeErr = directoryHandle.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return pruneUpdateHistory(directory)
}

func pruneUpdateHistory(directory string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	type retained struct {
		name    string
		started time.Time
	}
	items := make([]retained, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		record, readErr := readUpdateRecord(filepath.Join(directory, entry.Name()))
		if readErr != nil {
			return readErr
		}
		if entry.Name() != record.AttemptID+".json" {
			return errors.New("update history filename does not match attempt ID")
		}
		items = append(items, retained{name: entry.Name(), started: record.StartedAt})
	}
	sort.Slice(items, func(left, right int) bool {
		if items[left].started.Equal(items[right].started) {
			return items[left].name > items[right].name
		}
		return items[left].started.After(items[right].started)
	})
	if len(items) <= UpdateHistoryRetention {
		return nil
	}
	for _, item := range items[UpdateHistoryRetention:] {
		if err := os.Remove(filepath.Join(directory, item.name)); err != nil {
			return err
		}
	}
	return nil
}

func readUpdateRecord(path string) (UpdateRecord, error) {
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return UpdateRecord{}, err
	}
	if !pathInfo.Mode().IsRegular() || pathInfo.Mode()&os.ModeSymlink != 0 || pathInfo.Size() > maxUpdateRecordBytes {
		return UpdateRecord{}, errors.New("update history record is not a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return UpdateRecord{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return UpdateRecord{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxUpdateRecordBytes {
		return UpdateRecord{}, errors.New("update history record is not a bounded regular file")
	}
	var record UpdateRecord
	decoder := json.NewDecoder(io.LimitReader(file, maxUpdateRecordBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return UpdateRecord{}, fmt.Errorf("decode update history record: %w", err)
	}
	if !validUpdateRecord(record) {
		return UpdateRecord{}, errors.New("update history record is invalid")
	}
	return record, nil
}

func ensurePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("update history directory is not a directory")
	}
	return os.Chmod(path, 0o700)
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
