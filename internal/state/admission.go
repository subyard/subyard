package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
)

var ErrAdmissionPending = errors.New("project admission is already in progress")

const reservationTTL = 6 * time.Hour

type ProjectReservation struct {
	Schema      int                `json:"schema"`
	OperationID string             `json:"operationId"`
	ProjectID   string             `json:"projectId"`
	Name        string             `json:"name"`
	Source      string             `json:"source"`
	Mode        domain.ProjectMode `json:"mode"`
	Requested   string             `json:"requestedName"`
	Explicit    bool               `json:"explicit"`
	CreatedAt   time.Time          `json:"createdAt"`
}

type Admission struct {
	ProjectID   string
	Name        string
	Existing    *domain.ProjectRecord
	Reservation *ProjectReservation
}

// PreviewAdmission derives the current canonical project identity without
// creating state, a lock file, or a blocking reservation. Callers must still
// use Admit after consent and reject a different result as a stale plan.
func (store *FileStore) PreviewAdmission(
	ctx context.Context,
	source string,
	mode domain.ProjectMode,
	requestedName string,
	explicit bool,
) (Admission, error) {
	if source == "" {
		return Admission{}, errors.New("project source is required")
	}
	if mode != domain.ProjectSync && mode != domain.ProjectBind && mode != domain.ProjectGit {
		return Admission{}, fmt.Errorf("invalid project mode %q", mode)
	}
	if !domain.SafeProjectName(requestedName) {
		return Admission{}, fmt.Errorf(
			"invalid project name %q; use 1-50 ASCII letters, digits, '.', '_' or '-' and do not start with '-'",
			requestedName,
		)
	}
	records, err := store.ListReadOnly(ctx)
	if err != nil {
		return Admission{}, err
	}
	for index := range records {
		record := records[index]
		if record.SourceKey != SourceKey(source) &&
			(record.SourceKey != "" || record.HostPath != source) {
			continue
		}
		if record.Mode != mode {
			return Admission{}, fmt.Errorf(
				"%q is already in the yard as %q; remove it before re-adding as %s",
				record.Name, record.Mode, mode,
			)
		}
		if explicit && record.Name != requestedName {
			return Admission{}, fmt.Errorf(
				"source is already registered as %q; implicit rename to %q is not allowed",
				record.Name, requestedName,
			)
		}
		current := record
		return Admission{ProjectID: record.ProjectID, Name: record.Name, Existing: &current}, nil
	}
	occupied := make(map[string]string, len(records))
	for _, record := range records {
		occupied[domain.ProjectNameKey(record.ProjectID)] = record.ProjectID
		occupied[domain.ProjectNameKey(record.Name)] = record.ProjectID
	}
	name := requestedName
	if owner, found := occupied[domain.ProjectNameKey(name)]; found {
		if explicit {
			return Admission{}, fmt.Errorf(
				"project name %q conflicts with project %q; choose another explicit name such as %q",
				name, owner, nextProjectName(requestedName, occupied),
			)
		}
		name = nextProjectName(requestedName, occupied)
	}
	return Admission{ProjectID: name, Name: name}, nil
}

func (store *FileStore) Admit(
	ctx context.Context,
	operationID, source string,
	mode domain.ProjectMode,
	requestedName string,
	explicit bool,
) (Admission, error) {
	if operationID == "" || !domain.SafeID(operationID) {
		return Admission{}, errors.New("project admission requires a safe operation ID")
	}
	if source == "" {
		return Admission{}, errors.New("project source is required")
	}
	if mode != domain.ProjectSync && mode != domain.ProjectBind && mode != domain.ProjectGit {
		return Admission{}, fmt.Errorf("invalid project mode %q", mode)
	}
	if !domain.SafeProjectName(requestedName) {
		return Admission{}, fmt.Errorf(
			"invalid project name %q; use 1-50 ASCII letters, digits, '.', '_' or '-' and do not start with '-'",
			requestedName,
		)
	}
	lock, err := store.lock(ctx, true)
	if err != nil {
		return Admission{}, err
	}
	defer unlock(lock)

	records, err := store.listUnlocked()
	if err != nil {
		return Admission{}, err
	}
	for index := range records {
		record := records[index]
		if record.SourceKey != SourceKey(source) &&
			(record.SourceKey != "" || record.HostPath != source) {
			continue
		}
		if record.Mode != mode {
			return Admission{}, fmt.Errorf(
				"%q is already in the yard as %q; remove it before re-adding as %s",
				record.Name, record.Mode, mode,
			)
		}
		if explicit && record.Name != requestedName {
			return Admission{}, fmt.Errorf(
				"source is already registered as %q; implicit rename to %q is not allowed",
				record.Name, requestedName,
			)
		}
		current := record
		return Admission{ProjectID: record.ProjectID, Name: record.Name, Existing: &current}, nil
	}

	reservations, err := store.readReservations(time.Now().UTC())
	if err != nil {
		return Admission{}, err
	}
	for _, reservation := range reservations {
		if reservation.Source == source {
			if reservation.OperationID == operationID {
				if reservation.Mode != mode || reservation.Requested != requestedName ||
					reservation.Explicit != explicit {
					return Admission{}, errors.New(
						"project operation ID already has a different admission request",
					)
				}
				current := reservation
				return Admission{
					ProjectID: reservation.ProjectID, Name: reservation.Name,
					Reservation: &current,
				}, nil
			}
			return Admission{}, fmt.Errorf("%w for this source; retry shortly", ErrAdmissionPending)
		}
	}

	occupied := make(map[string]string, len(records)+len(reservations))
	for _, record := range records {
		occupied[domain.ProjectNameKey(record.ProjectID)] = record.ProjectID
		occupied[domain.ProjectNameKey(record.Name)] = record.ProjectID
	}
	for _, reservation := range reservations {
		occupied[domain.ProjectNameKey(reservation.ProjectID)] = reservation.ProjectID
	}
	name := requestedName
	if owner, found := occupied[domain.ProjectNameKey(name)]; found {
		if explicit {
			return Admission{}, fmt.Errorf(
				"project name %q conflicts with project %q; choose another explicit name such as %q",
				name, owner, nextProjectName(requestedName, occupied),
			)
		}
		name = nextProjectName(requestedName, occupied)
	}
	reservation := ProjectReservation{
		Schema: 1, OperationID: operationID, ProjectID: name, Name: name,
		Source: source, Mode: mode, Requested: requestedName, Explicit: explicit,
		CreatedAt: time.Now().UTC(),
	}
	if err := store.writeReservation(reservation); err != nil {
		return Admission{}, err
	}
	return Admission{ProjectID: name, Name: name, Reservation: &reservation}, nil
}

func (store *FileStore) FinalizeOperation(
	ctx context.Context,
	operationID string,
	record domain.ProjectRecord,
) error {
	if err := record.Validate(record.ProjectID); err != nil {
		return err
	}
	lock, err := store.lock(ctx, true)
	if err != nil {
		return err
	}
	defer unlock(lock)
	reservation, err := store.readReservation(operationID)
	if err != nil {
		return err
	}
	if record.ProjectID != reservation.ProjectID || record.Name != reservation.Name ||
		record.HostPath != reservation.Source || record.Mode != reservation.Mode {
		return errors.New("project admission result does not match its reservation")
	}
	if err := store.validateProjectNameUnlocked(record, reservation.OperationID); err != nil {
		return err
	}
	if err := store.putUnlocked(record); err != nil {
		return err
	}
	return store.removeReservation(reservation.OperationID)
}

func (store *FileStore) AbortAdmission(ctx context.Context, operationID string) error {
	if operationID == "" {
		return nil
	}
	lock, err := store.lock(ctx, true)
	if err != nil {
		return err
	}
	defer unlock(lock)
	return store.removeReservation(operationID)
}

func (store *FileStore) readReservations(now time.Time) ([]ProjectReservation, error) {
	directory := store.reservationDirectory()
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result := make([]ProjectReservation, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		operationID := strings.TrimSuffix(entry.Name(), ".json")
		reservation, err := store.readReservation(operationID)
		if err != nil {
			return nil, err
		}
		if now.Sub(reservation.CreatedAt) >= reservationTTL {
			if err := store.removeReservation(operationID); err != nil {
				return nil, err
			}
			continue
		}
		result = append(result, reservation)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].OperationID < result[j].OperationID })
	return result, nil
}

func (store *FileStore) reservationDirectory() string {
	return filepath.Join(store.directory, ".reservations")
}

func (store *FileStore) reservationPath(operationID string) string {
	return filepath.Join(store.reservationDirectory(), operationID+".json")
}

func (store *FileStore) writeReservation(reservation ProjectReservation) error {
	if !domain.SafeID(reservation.OperationID) {
		return errors.New("invalid project reservation operation ID")
	}
	if err := os.MkdirAll(store.reservationDirectory(), 0o700); err != nil {
		return err
	}
	if err := syncDirectory(store.directory); err != nil {
		return err
	}
	payload, err := json.Marshal(reservation)
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	path := store.reservationPath(reservation.OperationID)
	file, err := os.CreateTemp(store.reservationDirectory(), ".reservation.tmp.*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(payload); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Link(temporary, path); errors.Is(err, os.ErrExist) {
		current, readErr := store.readReservation(reservation.OperationID)
		if readErr == nil && current == reservation {
			return nil
		}
		return errors.New("project operation ID already has a different reservation")
	} else if err != nil {
		return err
	}
	return syncDirectory(store.reservationDirectory())
}

func (store *FileStore) readReservation(operationID string) (ProjectReservation, error) {
	if !domain.SafeID(operationID) {
		return ProjectReservation{}, errors.New("invalid project reservation operation ID")
	}
	payload, err := os.ReadFile(store.reservationPath(operationID))
	if err != nil {
		return ProjectReservation{}, err
	}
	var reservation ProjectReservation
	if err := json.Unmarshal(payload, &reservation); err != nil {
		return ProjectReservation{}, err
	}
	if reservation.Schema != 1 || reservation.OperationID != operationID ||
		!domain.SafeProjectName(reservation.ProjectID) ||
		reservation.ProjectID != reservation.Name || reservation.Source == "" ||
		!domain.SafeProjectName(reservation.Requested) ||
		reservation.CreatedAt.IsZero() {
		return ProjectReservation{}, errors.New("invalid project reservation")
	}
	return reservation, nil
}

func (store *FileStore) removeReservation(operationID string) error {
	path := store.reservationPath(operationID)
	if err := os.Remove(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return syncDirectory(store.reservationDirectory())
}

func nextProjectName(base string, occupied map[string]string) string {
	return nextAvailableProjectName(base, func(candidate string) bool {
		_, found := occupied[domain.ProjectNameKey(candidate)]
		return !found
	})
}
