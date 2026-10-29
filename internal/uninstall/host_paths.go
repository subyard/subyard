package uninstall

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Subyard/Subyard/internal/config"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type HostPathRecord struct {
	Schema     int             `json:"schema"`
	Root       string          `json:"root"`
	DataHome   string          `json:"dataHome"`
	ConfigHome string          `json:"configHome"`
	UID        uint32          `json:"uid"`
	Paths      []HostPathEntry `json:"paths"`
}

type HostPathEntry struct {
	Name     string `json:"name"`
	Created  bool   `json:"created"`
	Identity *Entry `json:"identity"`
}

func ReadHostPaths(installation Installation) ([]HostPathRecord, error) {
	directory := filepath.Join(installation.DataHome, "ownership", "host-paths")
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(entries) > 256 {
		return nil, errors.New("host ownership ledger exceeds bounded roots")
	}
	result := []HostPathRecord{}
	for _, entry := range entries {
		path := filepath.Join(directory, entry.Name())
		tree, err := Capture(path)
		if err != nil {
			return nil, err
		}
		if len(tree.Entries) != 1 || tree.Entries[0].Mode&syscall.S_IFMT != syscall.S_IFREG || tree.Entries[0].Mode&0777 != 0600 || tree.Entries[0].UID != installation.OperatorUID {
			return nil, protectedEntryError(path, "host ownership ledger", tree.Entries, installation.OperatorUID)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if len(body) > 65536 {
			return nil, errors.New("host ownership ledger is oversized")
		}
		var record HostPathRecord
		if err = json.Unmarshal(body, &record); err != nil {
			return nil, config.OwnerMetadataError(path, "host ownership ledger", "invalid host ownership ledger (JSON format)", "", err)
		}
		var differences []string
		if record.Schema != 1 {
			differences = append(differences, fmt.Sprintf("schema: stored=%d, required=1", record.Schema))
		}
		if record.UID != installation.OperatorUID {
			differences = append(differences, fmt.Sprintf("uid: stored=%d, current=%d", record.UID, installation.OperatorUID))
		}
		if record.DataHome != installation.DataHome {
			differences = append(differences, pathDifference("dataHome", record.DataHome, installation.DataHome))
		}
		if installation.ConfigHome != "" && record.ConfigHome != installation.ConfigHome {
			differences = append(differences, pathDifference("configHome", record.ConfigHome, installation.ConfigHome))
		}
		if len(record.Paths) > 256 {
			differences = append(differences, fmt.Sprintf("paths: observed count=%d, required maximum=256", len(record.Paths)))
		}
		if len(differences) != 0 {
			return nil, config.OwnerMetadataError(path, "host ownership ledger", "foreign host ownership ledger", strings.Join(differences, "; "), nil)
		}
		if err = safePath(record.Root); err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		for _, item := range record.Paths {
			if item.Name == "" || item.Name == "." || item.Name == ".." || filepath.Base(item.Name) != item.Name || seen[item.Name] {
				return nil, errors.New("invalid host ownership descendant")
			}
			seen[item.Name] = true
			if item.Created && item.Identity == nil {
				return nil, errors.New("host namespace installation is incomplete; repeat mount setup before full removal")
			}
			if item.Created {
				current, err := CaptureRoot(filepath.Join(record.Root, item.Name))
				if errors.Is(err, os.ErrPermission) {
					continue
				}
				if err != nil {
					return nil, err
				}
				if len(current.Entries) > 0 {
					native := current.Entries[0]
					old := *item.Identity
					native.Path, old.Path = "", ""
					native.Size, old.Size = 0, 0
					native.Modified, old.Modified = 0, 0
					native.Changed, old.Changed = 0, 0
					if native != old {
						return nil, config.OwnerMetadataError(filepath.Join(record.Root, item.Name), "host namespace", "identity differs from its installation owner", entryIdentityFacts(old, native), nil)
					}
				}
			}
		}
		if err = tree.Check(); err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, nil
}

// protectedEntryError formats the already-captured inventory; it does not
// perform another filesystem check or change the exact-mode policy.
func protectedEntryError(path, role string, entries []Entry, uid uint32) error {
	observed := fmt.Sprintf("observed entries=%d", len(entries))
	if len(entries) > 0 {
		entry := entries[0]
		observed += fmt.Sprintf(", type=%#o, mode=%04o, uid=%d", entry.Mode&syscall.S_IFMT, entry.Mode&0777, entry.UID)
	}
	return config.OwnerMetadataError(path, role, fmt.Sprintf("must be an operator-owned regular mode 0600 file; %s; required entries=1, type=%#o, mode=0600, uid=%d", observed, syscall.S_IFREG, uid), "", nil)
}

func entryIdentityFacts(stored, current Entry) string {
	return fmt.Sprintf("stored mode=%#o, uid=%d, gid=%d, device=%d, inode=%d; current mode=%#o, uid=%d, gid=%d, device=%d, inode=%d", stored.Mode, stored.UID, stored.GID, stored.Device, stored.Inode, current.Mode, current.UID, current.GID, current.Device, current.Inode)
}
