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
	"github.com/Subyard/Subyard/internal/safefs"
)

var ErrAdmissionPending = errors.New("project admission is already in progress")

var errReservationNotRegular = errors.New("project reservation is not a regular file")

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
	workspaceNames ...string,
) (Admission, error) {
	if err := validateAdmissionRequest(source, mode, requestedName); err != nil {
		return Admission{}, err
	}
	records, err := store.ListReadOnly(ctx)
	if err != nil {
		return Admission{}, err
	}
	if admission, found, err := existingAdmission(records, source, mode, requestedName, explicit); err != nil || found {
		return admission, err
	}
	reservations, err := store.readReservations(time.Now().UTC(), false)
	if err != nil {
		return Admission{}, err
	}
	name, err := admissionName(records, reservations, workspaceNames, requestedName, explicit)
	if err != nil {
		return Admission{}, err
	}
	return Admission{ProjectID: name, Name: name}, nil
}

func (store *FileStore) Admit(
	ctx context.Context,
	operationID, source string,
	mode domain.ProjectMode,
	requestedName string,
	explicit bool,
	workspaceNames ...string,
) (Admission, error) {
	if operationID == "" || !domain.SafeID(operationID) {
		return Admission{}, errors.New("project admission requires a safe operation ID")
	}
	if err := validateAdmissionRequest(source, mode, requestedName); err != nil {
		return Admission{}, err
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
	if admission, found, err := existingAdmission(records, source, mode, requestedName, explicit); err != nil || found {
		return admission, err
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
			if mode == domain.ProjectBind || reservation.Mode != mode {
				return Admission{}, fmt.Errorf("%w for this source; retry shortly", ErrAdmissionPending)
			}
		}
	}

	name, err := admissionName(records, reservations, workspaceNames, requestedName, explicit)
	if err != nil {
		return Admission{}, err
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

func validateAdmissionRequest(source string, mode domain.ProjectMode, requestedName string) error {
	if source == "" {
		return errors.New("project source is required")
	}
	if mode != domain.ProjectSync && mode != domain.ProjectBind && mode != domain.ProjectGit {
		return fmt.Errorf("invalid project mode %q", mode)
	}
	if !domain.SafeProjectName(requestedName) {
		return fmt.Errorf(
			"invalid project name %q; use 1-50 ASCII letters, digits, '.', '_' or '-' and do not start with '-'",
			requestedName,
		)
	}
	return nil
}

// existingAdmission returns the registered bind project for source. Sync and
// Git sources may be added again under a new identity.
func existingAdmission(
	records []domain.ProjectRecord,
	source string,
	mode domain.ProjectMode,
	requestedName string,
	explicit bool,
) (Admission, bool, error) {
	for index := range records {
		record := records[index]
		if record.SourceKey != SourceKey(source) &&
			(record.SourceKey != "" || record.HostPath != source) {
			continue
		}
		if record.Mode != mode {
			return Admission{}, false, fmt.Errorf(
				"%q is already in the yard as %q; remove it before re-adding as %s",
				record.Name, record.Mode, mode,
			)
		}
		if mode == domain.ProjectSync || mode == domain.ProjectGit {
			continue
		}
		if explicit && record.Name != requestedName {
			return Admission{}, false, fmt.Errorf(
				"source is already registered as %q; implicit rename to %q is not allowed",
				record.Name, requestedName,
			)
		}
		current := record
		return Admission{ProjectID: record.ProjectID, Name: record.Name, Existing: &current}, true, nil
	}
	return Admission{}, false, nil
}

// admissionName keeps an explicit name or derives the next free implicit one.
func admissionName(
	records []domain.ProjectRecord,
	reservations []ProjectReservation,
	workspaceNames []string,
	requestedName string,
	explicit bool,
) (string, error) {
	occupied := make(map[string]string, len(records)+len(reservations))
	for _, record := range records {
		occupied[domain.ProjectNameKey(record.ProjectID)] = record.ProjectID
		occupied[domain.ProjectNameKey(record.Name)] = record.ProjectID
	}
	for _, reservation := range reservations {
		occupied[domain.ProjectNameKey(reservation.ProjectID)] = reservation.ProjectID
	}
	for _, workspaceName := range workspaceNames {
		occupied[domain.ProjectNameKey(workspaceName)] = workspaceName
	}
	owner, found := occupied[domain.ProjectNameKey(requestedName)]
	if !found {
		return requestedName, nil
	}
	if explicit {
		return "", fmt.Errorf(
			"project name %q conflicts with project %q; choose another explicit name such as %q",
			requestedName, owner, nextProjectName(requestedName, occupied),
		)
	}
	return nextProjectName(requestedName, occupied), nil
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
		record.HostPath != reservation.Source || record.Mode != reservation.Mode ||
		record.SourceKey != SourceKey(reservation.Source) {
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

func (store *FileStore) readReservations(now time.Time, prune ...bool) ([]ProjectReservation, error) {
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
		// A concurrent finalize or abort may remove an entry after a read-only listing.
		if len(prune) != 0 && !prune[0] && errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if now.Sub(reservation.CreatedAt) >= reservationTTL {
			if len(prune) == 0 || prune[0] {
				if err := store.removeReservation(operationID); err != nil {
					return nil, err
				}
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
	err = writeStateFile(path, payload, ".reservation.tmp.*", true)
	if errors.Is(err, os.ErrExist) {
		current, readErr := store.readReservation(reservation.OperationID)
		if readErr == nil && current == reservation {
			return nil
		}
		if errors.Is(readErr, errReservationNotRegular) {
			return readErr
		}
		return errors.New("project operation ID already has a different reservation")
	}
	return err
}

func (store *FileStore) readReservation(operationID string) (ProjectReservation, error) {
	if !domain.SafeID(operationID) {
		return ProjectReservation{}, errors.New("invalid project reservation operation ID")
	}
	path := store.reservationPath(operationID)
	// A reservation is private state: a final symlink or special file is never read through.
	payload, err := safefs.ReadRegular(path, safefs.ReadOptions{})
	if errors.Is(err, safefs.ErrSymlink) || errors.Is(err, safefs.ErrNotRegular) {
		return ProjectReservation{}, fmt.Errorf("%w: %s", errReservationNotRegular, path)
	}
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
