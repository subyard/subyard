package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/safefs"
)

const appDocument = "herdr-app"
const documentLimit = 1 << 20

type project struct {
	Name, Folder string
}

type registration struct {
	ID      string `json:"id"`
	Pending string `json:"pending,omitempty"`
}

type ownership struct {
	Schema    int    `json:"schema"`
	ProjectID string `json:"projectId"`
	Name      string `json:"name"`
	Folder    string `json:"folder"`
}

type space struct {
	ID, Name, Folder string
	Tabs             []json.RawMessage
	Subyard          ownership
	fields           map[string]json.RawMessage
}

type document struct {
	Value   json.RawMessage `json:"value"`
	Version uint64          `json:"version"`
}

func projectInventory(root string, known map[string]registration) (map[string]project, error) {
	projects := make(map[string]project)
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil || resolved != root {
		return nil, errors.New("cannot inspect canonical Herdr workspaces")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var refusals []error
	for _, entry := range entries {
		id := entry.Name()
		if !domain.SafeID(id) {
			continue
		}
		workspace := filepath.Join(root, id)
		if !entry.IsDir() {
			if _, exists := known[id]; exists {
				refusals = append(refusals, fmt.Errorf("workspace %q is no longer a directory", id))
			}
			continue
		}
		data, err := safefs.ReadRegular(filepath.Join(workspace, ".subyard-meta.json"), safefs.ReadOptions{MaxBytes: 65536, SingleLink: true})
		if errors.Is(err, os.ErrNotExist) && known[id].ID == "" {
			continue
		}
		var metadata struct {
			Schema          int
			ProjectID, Name string
		}
		if err != nil || json.Unmarshal(data, &metadata) != nil || metadata.Schema != 1 || metadata.ProjectID != id || strings.TrimSpace(metadata.Name) == "" {
			refusals = append(refusals, fmt.Errorf("invalid project metadata for %q", id))
			continue
		}
		folder := filepath.Join(workspace, "src")
		info, err := os.Lstat(folder)
		if err != nil || !info.IsDir() {
			refusals = append(refusals, fmt.Errorf("project %q has no canonical checkout directory", id))
			continue
		}
		projects[id] = project{Name: metadata.Name, Folder: folder}
	}
	return projects, domain.CollectRefusals(refusals...)
}

func stateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "subyard-herdr"), nil
}

func loadRegistrations(dir string) (map[string]registration, error) {
	state := make(map[string]registration)
	uid := uint32(os.Getuid())
	_ = safefs.RepairPath(filepath.Join(dir, "projects.json"), 0o077)
	data, err := safefs.ReadRegular(filepath.Join(dir, "projects.json"), safefs.ReadOptions{MaxBytes: documentLimit, OwnerUID: &uid, DenyPerm: 0o077, SingleLink: true})
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return nil, err
	}
	if json.Unmarshal(data, &state) != nil || state == nil {
		return nil, errors.New("invalid Herdr registration state")
	}
	for id, entry := range state {
		if !domain.SafeID(id) || entry.ID != spaceID(id) {
			return nil, errors.New("invalid Herdr registration identity")
		}
	}
	return state, nil
}

func saveRegistrations(dir string, state map[string]registration) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if len(data) > documentLimit {
		return errors.New("Herdr registration state exceeds 1 MiB")
	}
	return safefs.WriteFile(filepath.Join(dir, "projects.json"), data, safefs.Options{Mode: 0o600, ParentMode: 0o700})
}

func reportPending() error {
	dir, err := stateDir()
	if err != nil {
		return err
	}
	state, err := loadRegistrations(dir)
	if err != nil {
		return err
	}
	for _, id := range slices.Sorted(maps.Keys(state)) {
		if pending := state[id].Pending; pending != "" {
			fmt.Printf("Pending space cleanup for %s: %s\n", id, pending)
		}
	}
	return nil
}

func spaceID(id string) string {
	key := sha256.Sum256([]byte(id))
	return fmt.Sprintf("subyard-%x", key[:16])
}

func loadLayout(source json.RawMessage) (map[string]json.RawMessage, []space, error) {
	fields := make(map[string]json.RawMessage)
	var rows []json.RawMessage
	if string(source) != "null" {
		if len(source) == 0 || len(source) > documentLimit || json.Unmarshal(source, &fields) != nil ||
			json.Unmarshal(fields["spaces"], &rows) != nil || rows == nil {
			return nil, nil, errors.New("unsupported or oversized herdr-app document")
		}
	}
	spaces := make([]space, 0, len(rows))
	ids := make(map[string]bool)
	for _, row := range rows {
		var entry space
		if json.Unmarshal(row, &entry) != nil || json.Unmarshal(row, &entry.fields) != nil || entry.ID == "" || ids[entry.ID] {
			return nil, nil, errors.New("invalid or duplicate space in herdr-app document")
		}
		ids[entry.ID] = true
		spaces = append(spaces, entry)
	}
	return fields, spaces, nil
}

func (entry space) owned() bool {
	return entry.Subyard.Schema == 1 && domain.SafeID(entry.Subyard.ProjectID) && entry.ID == spaceID(entry.Subyard.ProjectID)
}

func (entry space) pristine() bool {
	return entry.Tabs != nil && len(entry.Tabs) == 0 &&
		entry.Name == entry.Subyard.Name && entry.Folder == entry.Subyard.Folder
}

func jsonValue(value any) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}

func projectSpace(id string, project project) map[string]json.RawMessage {
	return map[string]json.RawMessage{
		"id": jsonValue(spaceID(id)), "name": jsonValue(project.Name), "folder": jsonValue(project.Folder),
		"tabs": json.RawMessage("[]"), "subyard": jsonValue(ownership{1, id, project.Name, project.Folder}),
	}
}

func liveSpaces(socket string) (map[string]bool, error) {
	var result struct {
		Terminals []struct {
			Address string
			State   string
			Labels  map[string]string
		}
	}
	// The subscription snapshot does not prune terminal history like terminal.list.
	if err := call(socket, "events.subscribe", nil, &result); err != nil {
		return nil, err
	}
	if result.Terminals == nil {
		return nil, errors.New("Core returned no terminal inventory")
	}
	live := make(map[string]bool)
	for _, terminal := range result.Terminals {
		if terminal.Address == "" {
			return nil, errors.New("Core returned a terminal without an address")
		}
		if terminal.State == "live" && !strings.Contains(terminal.Address, "/") && terminal.Labels["herdr.space"] != "" {
			live[terminal.Labels["herdr.space"]] = true
		}
	}
	return live, nil
}

func planSpaces(spaces []space, projects map[string]project, state map[string]registration,
	live map[string]bool, restore bool,
) ([]map[string]json.RawMessage, bool, error) {
	rows := make([]map[string]json.RawMessage, 0, len(spaces))
	present := make(map[string]space)
	changed := false
	for _, entry := range spaces {
		present[entry.ID] = entry
		if entry.owned() {
			id := entry.Subyard.ProjectID
			record := registration{ID: entry.ID}
			if project, exists := projects[id]; exists {
				if entry.Subyard.Name != project.Name || entry.Subyard.Folder != project.Folder {
					desired := projectSpace(id, project)
					entry.fields["name"], entry.fields["folder"], entry.fields["subyard"] = desired["name"], desired["folder"], desired["subyard"]
					changed = true
				}
			} else {
				switch {
				case !entry.pristine():
					record.Pending = "space contains user layout"
				case live[entry.ID]:
					record.Pending = "space has live terminals"
				default:
					delete(state, id)
					changed = true
					continue
				}
			}
			state[id] = record
		}
		rows = append(rows, entry.fields)
	}
	for id, record := range state {
		if entry, exists := present[record.ID]; !exists || !entry.owned() {
			if _, active := projects[id]; !active {
				delete(state, id)
			} else if record.Pending != "" {
				record.Pending = ""
				state[id] = record
			}
		}
	}
	var refusals []error
	for _, id := range slices.Sorted(maps.Keys(projects)) {
		spaceID := spaceID(id)
		if entry, exists := present[spaceID]; exists {
			if !entry.owned() {
				refusals = append(refusals, fmt.Errorf("space identity for project %q is already in use", id))
			}
			continue
		}
		if _, seen := state[id]; seen && !restore {
			continue
		}
		state[id] = registration{ID: spaceID}
		rows = append(rows, projectSpace(id, projects[id]))
		changed = true
	}
	return rows, changed, domain.CollectRefusals(refusals...)
}

func inspectRegistration(socket, workspaces string, hook bool) (string, bool, error) {
	core, coreErr := readCore(socket)
	if core == nil && coreErr == nil && !hook {
		coreErr = errors.New("Core is stopped. Run yard herdr up first.")
	}
	dir, dirErr := stateDir()
	var state map[string]registration
	var stateErr error
	if dirErr == nil {
		state, stateErr = loadRegistrations(dir)
	}
	_, inventoryErr := projectInventory(workspaces, state)
	return dir, core != nil, domain.CollectRefusals(coreErr, dirErr, stateErr, inventoryErr)
}

func syncProjects(socket, workspaces string, restore, hook bool) error {
	dir, running, err := inspectRegistration(socket, workspaces, hook)
	if err != nil || !running {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	if err := safefs.EnsurePrivateDir(dir); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	lock, err := openProjectLock(dir)
	if err != nil {
		return err
	}
	defer lock.Release()
	if err := lock.Acquire(ctx, false, 25*time.Millisecond); err != nil {
		return err
	}
	state, stateErr := loadRegistrations(dir)
	projects, inventoryErr := projectInventory(workspaces, state)
	if err := domain.CollectRefusals(stateErr, inventoryErr); err != nil {
		return err
	}
	return syncLayout(socket, workspaces, dir, projects, state, restore)
}

func openProjectLock(dir string) (*safefs.LockFile, error) {
	path := filepath.Join(dir, "projects.lock")
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		if err := safefs.WriteFile(path, nil, safefs.Options{Mode: 0o600, ParentMode: 0o700, NoReplace: true}); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	return safefs.OpenLock(path, 0o600)
}

func inspectLayout(socket string, projects map[string]project, next map[string]registration, restore bool) (document, bool, error) {
	var current document
	if err := call(socket, "doc.get", map[string]string{"name": appDocument}, &current); err != nil {
		return document{}, false, err
	}
	fields, spaces, err := loadLayout(current.Value)
	if err != nil {
		return document{}, false, err
	}
	var live map[string]bool
	for _, space := range spaces {
		if _, present := projects[space.Subyard.ProjectID]; space.owned() && space.pristine() && !present {
			live, err = liveSpaces(socket)
			if err != nil {
				return document{}, false, err
			}
			break
		}
	}
	rows, changed, err := planSpaces(spaces, projects, next, live, restore)
	if err != nil {
		return document{}, false, err
	}
	fields["spaces"] = jsonValue(rows)
	current.Value = jsonValue(fields)
	if len(current.Value) > documentLimit {
		return document{}, false, errors.New("Herdr document would exceed 1 MiB")
	}
	return current, changed, nil
}

func registrationReady(socket, workspaces string) error {
	dir, err := stateDir()
	if err != nil {
		return err
	}
	state, stateErr := loadRegistrations(dir)
	projects, inventoryErr := projectInventory(workspaces, state)
	if err := domain.CollectRefusals(stateErr, inventoryErr); err != nil {
		return err
	}
	next := maps.Clone(state)
	_, changed, err := inspectLayout(socket, projects, next, false)
	if err != nil {
		return err
	}
	if changed || !reflect.DeepEqual(state, next) {
		return errors.New("Herdr project registration needs reconciliation")
	}
	return nil
}

func syncLayout(socket, workspaces, dir string, projects map[string]project, state map[string]registration, restore bool) error {
	for range 4 {
		next := maps.Clone(state)
		current, changed, err := inspectLayout(socket, projects, next, restore)
		if err != nil {
			return err
		}
		fresh, err := projectInventory(workspaces, state)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(projects, fresh) {
			return errors.New("project inventory changed during Herdr sync")
		}
		if changed {
			err = call(socket, "doc.set", map[string]any{"name": appDocument, "value": current.Value, "version": current.Version}, nil)
			var rejected *rpcError
			if errors.As(err, &rejected) && rejected.Data.Kind == "conflict" {
				continue
			}
			if err != nil {
				return err
			}
		}
		if !reflect.DeepEqual(state, next) {
			return saveRegistrations(dir, next)
		}
		return nil
	}
	return errors.New("Herdr document kept changing; retry sync")
}
