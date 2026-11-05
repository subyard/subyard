package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
)

// DeveloperIdentityMarker survives guest/account cuts and fences owner runtime
// replacement until the exact init continuation finishes.
type DeveloperIdentityMarker struct {
	Schema   int    `json:"schema"`
	Yard     string `json:"yard"`
	Inputs   string `json:"inputs"`
	Identity string `json:"identity"`
	Release  string `json:"release"`
	Proof    string `json:"proof"`
}

func DeveloperIdentityMarkerPath(root, yard string) string {
	return filepath.Join(root, ".developer-identity-"+yard+".json")
}

func PendingDeveloperIdentities(root string) ([]DeveloperIdentityMarker, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var result []DeveloperIdentityMarker
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".developer-identity-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		snapshot, err := ReadPersistentFileSnapshot(root, filepath.Join(root, entry.Name()))
		if err != nil {
			return nil, err
		}
		var marker DeveloperIdentityMarker
		if !snapshot.Exists || snapshot.Identity.Mode&0o777 != 0o600 || json.Unmarshal(snapshot.Content, &marker) != nil || marker.Schema != 1 || !domain.SafeName(marker.Yard) || filepath.Base(DeveloperIdentityMarkerPath(root, marker.Yard)) != entry.Name() || len(marker.Inputs) != 64 || len(marker.Identity) != 64 || len(marker.Release) != 64 {
			return nil, errors.New("unsafe pending developer identity authority")
		}
		result = append(result, marker)
	}
	return result, nil
}
