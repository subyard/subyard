package ownerinventory

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"golang.org/x/crypto/ssh"
)

const sshHostTrustSchema = 1

// SSHHostTrust is the concrete OpenSSH server key accepted when an OwnerHost
// connection is registered. The display fingerprint is persisted and checked
// against the key material so neither value can be substituted independently.
type SSHHostTrust struct {
	SchemaVersion  int    `json:"schemaVersion"`
	KnownHostsLine string `json:"knownHostsLine"`
	Fingerprint    string `json:"fingerprint"`
}

func NewSSHHostTrust(knownHostsLine string) (SSHHostTrust, error) {
	knownHostsLine = strings.TrimSpace(knownHostsLine)
	marker, hosts, key, _, rest, err := ssh.ParseKnownHosts([]byte(knownHostsLine + "\n"))
	if err != nil || marker != "" || len(hosts) == 0 || key == nil || len(strings.TrimSpace(string(rest))) != 0 {
		return SSHHostTrust{}, errors.New("SSH host trust requires one concrete known_hosts key")
	}
	if _, certificate := key.(*ssh.Certificate); certificate {
		return SSHHostTrust{}, errors.New("SSH host certificates are not supported as concrete owner trust")
	}
	return SSHHostTrust{
		SchemaVersion: sshHostTrustSchema, KnownHostsLine: knownHostsLine,
		Fingerprint: ssh.FingerprintSHA256(key),
	}, nil
}

func ReadSSHHostTrust(path string) (SSHHostTrust, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return SSHHostTrust{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return SSHHostTrust{}, errors.New("captured SSH host trust must be a private regular file")
	}
	if info.Size() <= 0 || info.Size() > 64*1024 {
		return SSHHostTrust{}, errors.New("captured SSH host trust has an invalid size")
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return SSHHostTrust{}, err
	}
	lines := make([]string, 0, 1)
	for _, line := range strings.Split(string(payload), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 {
		return SSHHostTrust{}, errors.New("SSH assessment must capture exactly one concrete host key")
	}
	return NewSSHHostTrust(lines[0])
}

func (trust SSHHostTrust) Validate() error {
	if trust.SchemaVersion != sshHostTrustSchema {
		return errors.New("unsupported SSH host trust schema")
	}
	parsed, err := NewSSHHostTrust(trust.KnownHostsLine)
	if err != nil {
		return err
	}
	if trust.Fingerprint != parsed.Fingerprint {
		return errors.New("SSH host trust fingerprint does not match the concrete key")
	}
	return nil
}

type Connection struct {
	HostID      string               `json:"hostId"`
	Destination string               `json:"destination"`
	Trust       *SSHHostTrust        `json:"trust,omitempty"`
	LegacyNames []string             `json:"legacyNames,omitempty"`
	Yards       map[string]YardRoute `json:"yards,omitempty"`
}

type YardRoute struct {
	SSHHost string `json:"sshHost"`
}

func (connection Connection) Validate() error {
	if err := validateHostID(connection.HostID); err != nil {
		return err
	}
	if !domain.SafeSSHTarget(connection.Destination) {
		return fmt.Errorf("invalid owner destination %q", connection.Destination)
	}
	if connection.Trust != nil {
		if err := connection.Trust.Validate(); err != nil {
			return fmt.Errorf("invalid owner SSH host trust: %w", err)
		}
	}
	for _, name := range connection.LegacyNames {
		if !domain.SafeName(name) {
			return fmt.Errorf("invalid legacy remote name %q", name)
		}
	}
	for yard, route := range connection.Yards {
		if !domain.SafeName(yard) || !domain.SafeSSHTarget(route.SSHHost) {
			return fmt.Errorf("invalid transport route for owner yard %q", yard)
		}
	}
	return nil
}

func (connection Connection) RequireTrust() (SSHHostTrust, error) {
	if connection.Trust == nil {
		return SSHHostTrust{}, errors.New("owner connection has no managed SSH host trust")
	}
	if err := connection.Trust.Validate(); err != nil {
		return SSHHostTrust{}, err
	}
	return *connection.Trust, nil
}

type Connections struct {
	Root   string
	failIO func(string) error
	now    func() time.Time
}

const (
	ownerIOJournalWrite    = "journal-write"
	ownerIOConnectionWrite = "connection-write"
	ownerIOCacheWrite      = "cache-write"
	ownerIOStateDelete     = "state-delete"
	ownerIORoutingRename   = "routing-rename"
	ownerIODirectorySync   = "directory-sync"
	ownerIOJournalCleanup  = "journal-cleanup"
)

func (store Connections) hitIO(boundary string) error {
	if store.failIO == nil {
		return nil
	}
	return store.failIO(boundary)
}

func (store Connections) currentTime() time.Time {
	if store.now != nil {
		return store.now()
	}
	return time.Now()
}

func (store Connections) cache() Cache { return Cache{Root: store.Root, failIO: store.failIO} }

func (store Connections) WriteSnapshot(snapshot Snapshot) error {
	connectionsMu.Lock()
	defer connectionsMu.Unlock()
	release, err := store.lock()
	if err != nil {
		return err
	}
	defer release()
	if err := store.recoverPendingLocked(); err != nil {
		return err
	}
	connections, err := store.list()
	if err != nil {
		return err
	}
	found := false
	for _, connection := range connections {
		found = found || connection.HostID == snapshot.Inventory.HostID
	}
	if !found {
		return fmt.Errorf("OwnerHost %q is not registered", snapshot.Inventory.HostID)
	}
	return store.cache().Write(snapshot)
}

var connectionsMu sync.Mutex

func (store Connections) List() ([]Connection, error) {
	connectionsMu.Lock()
	defer connectionsMu.Unlock()
	release, err := store.lock()
	if err != nil {
		return nil, err
	}
	defer release()
	if err := store.recoverPendingLocked(); err != nil {
		return nil, err
	}
	return store.list()
}

func (store Connections) list() ([]Connection, error) {
	directory := filepath.Join(store.Root, "connections")
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result := make([]Connection, 0, len(entries))
	destinations := make(map[string]string)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		payload, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			return nil, err
		}
		var connection Connection
		if err := json.Unmarshal(payload, &connection); err != nil {
			return nil, fmt.Errorf("decode owner connection %q: %w", entry.Name(), err)
		}
		if err := connection.Validate(); err != nil {
			return nil, err
		}
		if entry.Name() != connection.HostID+".json" {
			return nil, errors.New("owner connection filename does not match HostID")
		}
		if other, duplicate := destinations[connection.Destination]; duplicate && other != connection.HostID {
			return nil, fmt.Errorf(
				"owner destination %q is registered for HostID %q and %q",
				connection.Destination, other, connection.HostID,
			)
		}
		destinations[connection.Destination] = connection.HostID
		result = append(result, connection)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].HostID < result[j].HostID })
	return result, nil
}

func (store Connections) Write(connection Connection) error {
	connectionsMu.Lock()
	defer connectionsMu.Unlock()
	release, err := store.lock()
	if err != nil {
		return err
	}
	defer release()
	if err := store.recoverPendingLocked(); err != nil {
		return err
	}
	if err := connection.Validate(); err != nil {
		return err
	}
	existing, err := store.list()
	if err != nil {
		return err
	}
	for _, current := range existing {
		if current.HostID == connection.HostID && current.Destination != connection.Destination {
			return fmt.Errorf(
				"HostID %q is already registered at %q, refusing %q",
				connection.HostID, current.Destination, connection.Destination,
			)
		}
		if current.HostID == connection.HostID && current.Destination == connection.Destination {
			currentTrust, nextTrust := "", ""
			if current.Trust != nil {
				currentTrust = current.Trust.Fingerprint + "\x00" + current.Trust.KnownHostsLine
			}
			if connection.Trust != nil {
				nextTrust = connection.Trust.Fingerprint + "\x00" + connection.Trust.KnownHostsLine
			}
			if currentTrust != nextTrust {
				return errors.New("owner SSH host trust may be changed only by confirmed host repair")
			}
		}
		if current.HostID != connection.HostID && current.Destination == connection.Destination {
			return fmt.Errorf(
				"destination %q is already registered as HostID %q",
				connection.Destination, current.HostID,
			)
		}
	}
	slices.Sort(connection.LegacyNames)
	connection.LegacyNames = slices.Compact(connection.LegacyNames)
	return store.writeConnectionFile(connection)
}

// Remove deletes controller-owned registration and inventory cache only. It
// refuses while the last authoritative snapshot still contains projects.
func (store Connections) Remove(hostID string) (Connection, error) {
	connectionsMu.Lock()
	defer connectionsMu.Unlock()
	release, err := store.lock()
	if err != nil {
		return Connection{}, err
	}
	defer release()
	if err := store.recoverPendingLocked(); err != nil {
		return Connection{}, err
	}
	connections, err := store.list()
	if err != nil {
		return Connection{}, err
	}
	var selected Connection
	found := false
	for _, connection := range connections {
		if connection.HostID == hostID {
			selected, found = connection, true
			break
		}
	}
	if !found {
		return Connection{}, fmt.Errorf("OwnerHost %q is not registered", hostID)
	}
	cache := Cache{Root: store.Root}
	if err := ensureNoOwnerProjects(cache, hostID); err != nil {
		return Connection{}, err
	}
	if err := store.writeRemovalJournal(hostID); err != nil {
		return Connection{}, err
	}
	if err := store.applyRemovalLocked(hostID); err != nil {
		return Connection{}, err
	}
	return selected, nil
}

func (store Connections) CanRemove(hostID string) error {
	connectionsMu.Lock()
	defer connectionsMu.Unlock()
	release, err := store.lock()
	if err != nil {
		return err
	}
	defer release()
	if err := store.recoverPendingLocked(); err != nil {
		return err
	}
	found := false
	connections, err := store.list()
	if err != nil {
		return err
	}
	for _, connection := range connections {
		found = found || connection.HostID == hostID
	}
	if !found {
		return fmt.Errorf("OwnerHost %q is not registered", hostID)
	}
	return ensureNoOwnerProjects(Cache{Root: store.Root}, hostID)
}

func ensureNoOwnerProjects(cache Cache, hostID string) error {
	snapshot, err := cache.Read(hostID)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("OwnerHost %q has no authoritative inventory snapshot; refresh it before removal", hostID)
	}
	if err != nil {
		return err
	}
	if age := time.Since(snapshot.FetchedAt); age > Freshness || age < -Freshness {
		return fmt.Errorf("OwnerHost %q inventory snapshot is stale; refresh it before removal", hostID)
	}
	if err := ensureInventoryHasNoProjects(snapshot.Inventory); err != nil {
		return err
	}
	return ensureNoRoutingProjects(cache.Root, hostID)
}

func ensureInventoryHasNoProjects(inventory domain.OwnerInventory) error {
	for _, yard := range inventory.Yards {
		if len(yard.Projects) != 0 {
			return fmt.Errorf(
				"OwnerHost %q still owns %d project reference(s) in yard %q",
				inventory.HostID, len(yard.Projects), yard.Name,
			)
		}
	}
	return nil
}

func ensureNoRoutingProjects(root, hostID string) error {
	routingRoot := filepath.Join(root, "routing", hostID)
	err := filepath.WalkDir(routingRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, relErr := filepath.Rel(routingRoot, path)
		if relErr != nil {
			return relErr
		}
		for _, component := range strings.Split(relative, string(filepath.Separator)) {
			if component == "projects" {
				return fmt.Errorf("OwnerHost %q still has controller project routing state", hostID)
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (store Connections) writeConnectionFile(connection Connection) error {
	if err := store.hitIO(ownerIOConnectionWrite); err != nil {
		return err
	}
	payload, err := json.Marshal(connection)
	if err != nil {
		return err
	}
	directory := filepath.Join(store.Root, "connections")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	path := filepath.Join(directory, connection.HostID+".json")
	temporary, err := os.CreateTemp(directory, ".connection-*")
	if err != nil {
		return err
	}
	tempPath := temporary.Name()
	defer os.Remove(tempPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(payload); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	return store.syncDirectory(directory)
}
