package uninstall

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
			return nil, errors.New("host ownership ledger must be an operator-owned regular mode 0600 file")
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
			return nil, errors.New("invalid host ownership ledger")
		}
		if record.Schema != 1 || record.UID != installation.OperatorUID || record.DataHome != installation.DataHome || installation.ConfigHome != "" && record.ConfigHome != installation.ConfigHome || len(record.Paths) > 256 {
			return nil, errors.New("foreign host ownership ledger")
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
						return nil, errors.New("host namespace identity differs from its installation owner")
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
