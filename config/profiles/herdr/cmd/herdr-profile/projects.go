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

type registration struct {
	ID      string `json:"id"`
	Pending string `json:"pending,omitempty"`
}

// storedProject is one projects.json record. Releases before nested checkouts read
// only id and pending; retired keeps the checkout records of a project whose own
// space is gone without reading as a registered project space.
type storedProject struct {
	ID        string                  `json:"id"`
	Pending   string                  `json:"pending,omitempty"`
	Retired   bool                    `json:"retired,omitempty"`
	Checkouts map[string]registration `json:"checkouts,omitempty"`
}

type ownership struct {
	Schema    int    `json:"schema"`
	ProjectID string `json:"projectId"`
	Checkout  string `json:"checkout,omitempty"`
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
	var stored map[string]storedProject
	if json.Unmarshal(data, &stored) != nil || stored == nil {
		return nil, errors.New("invalid Herdr registration state")
	}
	for id, entry := range stored {
		if !domain.SafeID(id) || entry.ID != spaceID(id) {
			return nil, errors.New("invalid Herdr registration identity")
		}
		if !entry.Retired {
			state[id] = registration{ID: entry.ID, Pending: entry.Pending}
		}
		for path, record := range entry.Checkouts {
			key := checkoutKey(id, path)
			if !validCheckoutPath(path) || record.ID != spaceID(key) {
				return nil, errors.New("invalid Herdr registration identity")
			}
			state[key] = record
		}
	}
	return state, nil
}

func saveRegistrations(dir string, state map[string]registration) error {
	stored := make(map[string]storedProject)
	for key, record := range state {
		id, path := splitKey(key)
		entry, exists := stored[id]
		if !exists {
			entry = storedProject{ID: spaceID(id), Retired: true}
		}
		if path == "" {
			entry.Pending, entry.Retired = record.Pending, false
		} else {
			if entry.Checkouts == nil {
				entry.Checkouts = make(map[string]registration)
			}
			entry.Checkouts[path] = record
		}
		stored[id] = entry
	}
	data, err := json.Marshal(stored)
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

func (owner ownership) key() string { return checkoutKey(owner.ProjectID, owner.Checkout) }

func (entry space) owned() bool {
	return entry.Subyard.Schema == 1 && domain.SafeID(entry.Subyard.ProjectID) &&
		(entry.Subyard.Checkout == "" || validCheckoutPath(entry.Subyard.Checkout)) && entry.ID == spaceID(entry.Subyard.key())
}

// retirable reports whether a proven-absent checkout's space may go: a nested
// checkout's space goes with its layout, as Orca removes its record; the project
// space goes only while untouched.
func (entry space) retirable() bool {
	return entry.Subyard.Checkout != "" || entry.pristine()
}

func (entry space) pristine() bool {
	return entry.Tabs != nil && len(entry.Tabs) == 0 &&
		entry.Name == entry.Subyard.Name && entry.Folder == entry.Subyard.Folder
}

func jsonValue(value any) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}

func checkoutSpace(key string, checkout checkout) map[string]json.RawMessage {
	id, path := splitKey(key)
	return map[string]json.RawMessage{
		"id": jsonValue(spaceID(key)), "name": jsonValue(checkout.Name), "folder": jsonValue(checkout.Folder),
		"tabs": json.RawMessage("[]"), "subyard": jsonValue(ownership{1, id, path, checkout.Name, checkout.Folder}),
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

func planSpaces(spaces []space, inventory inventory, state map[string]registration,
	live map[string]bool, restore bool,
) ([]map[string]json.RawMessage, bool, error) {
	rows := make([]map[string]json.RawMessage, 0, len(spaces))
	present := make(map[string]space)
	changed := false
	for _, entry := range spaces {
		present[entry.ID] = entry
		if entry.owned() {
			key := entry.Subyard.key()
			record := registration{ID: entry.ID}
			if checkout, exists := inventory.Checkouts[key]; exists {
				if entry.Subyard.Name != checkout.Name || entry.Subyard.Folder != checkout.Folder {
					desired := checkoutSpace(key, checkout)
					entry.fields["name"], entry.fields["folder"], entry.fields["subyard"] = desired["name"], desired["folder"], desired["subyard"]
					changed = true
				}
			} else if inventory.Gone[key] {
				switch {
				case !entry.retirable():
					record.Pending = "space contains user layout"
				case live[entry.ID]:
					record.Pending = "space has live terminals"
				default:
					delete(state, key)
					changed = true
					continue
				}
			}
			// A checkout that is neither present nor proven absent keeps its space
			// and any pending cleanup reason.
			if _, desired := inventory.Checkouts[key]; !desired && !inventory.Gone[key] {
				record.Pending = state[key].Pending
			}
			state[key] = record
		}
		rows = append(rows, entry.fields)
	}
	for key, record := range state {
		if entry, exists := present[record.ID]; !exists || !entry.owned() {
			if _, active := inventory.Checkouts[key]; !active {
				if !inventory.retains(key) {
					delete(state, key)
				}
			} else if record.Pending != "" {
				record.Pending = ""
				state[key] = record
			}
		}
	}
	var refusals []error
	for _, key := range slices.Sorted(maps.Keys(inventory.Checkouts)) {
		spaceID := spaceID(key)
		if entry, exists := present[spaceID]; exists {
			if !entry.owned() {
				kind := "project"
				if strings.Contains(key, "/") {
					kind = "checkout"
				}
				refusals = append(refusals, fmt.Errorf("space identity for %s %q is already in use", kind, key))
			}
			continue
		}
		if _, seen := state[key]; seen && !restore {
			continue
		}
		state[key] = registration{ID: spaceID}
		rows = append(rows, checkoutSpace(key, inventory.Checkouts[key]))
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
	var stateErr, inventoryErr error
	if dirErr == nil {
		var state map[string]registration
		state, stateErr = loadRegistrations(dir)
		// A running Core is reconciled under the project lock, which scans again
		// unless the state is unreadable.
		if core == nil || coreErr != nil || stateErr != nil {
			inventory, err := scanCheckouts(workspaces, dir, slices.Sorted(maps.Keys(state)))
			inventoryErr = domain.CollectRefusals(err, inventory.refusal())
		}
	}
	return dir, core != nil, domain.CollectRefusals(coreErr, dirErr, stateErr, inventoryErr)
}

func ensureStateDir(dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	return safefs.EnsurePrivateDir(dir)
}

func syncProjects(socket, workspaces string, restore, hook bool) error {
	dir, running, err := inspectRegistration(socket, workspaces, hook)
	if err != nil || !running {
		return err
	}
	if err := ensureStateDir(dir); err != nil {
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
	state, err := loadRegistrations(dir)
	if err != nil {
		return err
	}
	return syncLayout(socket, workspaces, dir, state, restore)
}

// discoverCheckouts advances the periodic nested checkout scan, then reconciles
// spaces as a project hook does.
func discoverCheckouts(socket, workspaces string) error {
	dir, err := stateDir()
	if err != nil {
		return err
	}
	if err := ensureStateDir(dir); err != nil {
		return err
	}
	return domain.CollectRefusals(advanceDiscovery(workspaces, dir), syncProjects(socket, workspaces, false, true))
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

func inspectLayout(socket string, inventory inventory, next map[string]registration, restore bool) (document, bool, error) {
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
		if space.owned() && inventory.Gone[space.Subyard.key()] && space.retirable() {
			live, err = liveSpaces(socket)
			if err != nil {
				return document{}, false, err
			}
			break
		}
	}
	rows, changed, err := planSpaces(spaces, inventory, next, live, restore)
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

type registrationPlan struct {
	document  document
	next      map[string]registration
	inventory inventory
	known     []string
	changed   bool
}

// planRegistration scans the checkouts and plans the layout. Owned spaces that the
// state does not list, such as after a rollback dropped checkout records, are
// scanned once more as known checkouts so their absence can be proven. Readiness
// leaves unregistered nested checkouts to the discovery timer, which adds them
// right after its scan caches them.
func planRegistration(socket, workspaces, dir string, state map[string]registration, restore, readiness bool) (registrationPlan, error) {
	known := slices.Sorted(maps.Keys(state))
	for pass := 0; ; pass++ {
		inventory, err := scanCheckouts(workspaces, dir, known)
		if err != nil {
			return registrationPlan{}, err
		}
		if readiness {
			for key := range inventory.Checkouts {
				if _, recorded := state[key]; !recorded && strings.Contains(key, "/") {
					delete(inventory.Checkouts, key)
				}
			}
		}
		next := maps.Clone(state)
		current, changed, err := inspectLayout(socket, inventory, next, restore)
		if err != nil {
			return registrationPlan{}, err
		}
		var added []string
		for key := range next {
			if _, recorded := state[key]; !recorded {
				if _, desired := inventory.Checkouts[key]; !desired {
					added = append(added, key)
				}
			}
		}
		if len(added) == 0 || pass > 0 {
			return registrationPlan{current, next, inventory, known, changed}, nil
		}
		known = slices.Sorted(slices.Values(append(known, added...)))
	}
}

func registrationReady(socket, workspaces string) error {
	dir, err := stateDir()
	if err != nil {
		return err
	}
	// Hold the project lock, when sync created it, so the timer's update is never seen half applied.
	if _, err := os.Lstat(filepath.Join(dir, "projects.lock")); err == nil {
		lock, err := openProjectLock(dir)
		if err != nil {
			return err
		}
		defer lock.Release()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := lock.Acquire(ctx, true, 25*time.Millisecond); err != nil {
			return err
		}
	}
	state, err := loadRegistrations(dir)
	if err != nil {
		inventory, inventoryErr := scanCheckouts(workspaces, dir, nil)
		return domain.CollectRefusals(err, inventoryErr, inventory.refusal())
	}
	plan, err := planRegistration(socket, workspaces, dir, state, false, true)
	if err != nil {
		return err
	}
	var pending error
	if plan.changed || !reflect.DeepEqual(state, plan.next) {
		pending = errors.New("Herdr project registration needs reconciliation")
	}
	return domain.CollectRefusals(pending, plan.inventory.refusal())
}

func syncLayout(socket, workspaces, dir string, state map[string]registration, restore bool) error {
	for range 4 {
		plan, err := planRegistration(socket, workspaces, dir, state, restore, false)
		if err != nil {
			return err
		}
		fresh, err := scanCheckouts(workspaces, dir, plan.known)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(plan.inventory, fresh) {
			// Periodic discovery or a project change moved on; plan again.
			continue
		}
		if plan.changed {
			err = call(socket, "doc.set", map[string]any{"name": appDocument, "value": plan.document.Value, "version": plan.document.Version}, nil)
			var rejected *rpcError
			if errors.As(err, &rejected) && rejected.Data.Kind == "conflict" {
				continue
			}
			if err != nil {
				return err
			}
		}
		if !reflect.DeepEqual(state, plan.next) {
			if err := saveRegistrations(dir, plan.next); err != nil {
				return err
			}
		}
		// The present checkouts are reconciled; report what kept the rest unchanged.
		return plan.inventory.refusal()
	}
	return errors.New("Herdr document or project inventory kept changing; retry sync")
}
