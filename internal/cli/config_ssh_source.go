package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

// Call only after validating the isolated source candidate. External source
// publication remains owned by the prepared Git operation and its fresh consent.
func sanitizeLegacySSHSource(candidate, hostID string) ([]string, error) {
	if !domain.SafeName(hostID) {
		return nil, errors.New("legacy SSH source cleanup requires a safe owner identity")
	}
	host := filepath.Join(candidate, "hosts", hostID)
	paths := []string{filepath.Join(host, "config.env")}
	yards, err := os.ReadDir(filepath.Join(host, "yards"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, yard := range yards {
		if !yard.IsDir() || !domain.SafeName(yard.Name()) {
			return nil, errors.New("legacy SSH source cleanup requires safe yard definitions")
		}
		paths = append(paths, filepath.Join(host, "yards", yard.Name(), "config.env"))
	}
	changed := []string{}
	for _, path := range paths {
		before, err := readConfigAuthoringTarget(path)
		if err != nil {
			return nil, err
		}
		if !before.Exists {
			continue
		}
		if err := config.LegacyGitSSHPort(path, before.Content); err != nil {
			return nil, err
		}
		content, err := config.EditPersistentAssignmentContent(path, before.Content, "SSH_PORT", nil)
		if err != nil {
			return nil, err
		}
		if bytes.Equal(content, before.Content) {
			continue
		}
		// Keep the definition, even when SSH_PORT was its only record.
		if err := writeExportedConfigFile(path, content, os.FileMode(before.Identity.Mode&0o777)); err != nil {
			return nil, err
		}
		relative, err := filepath.Rel(candidate, path)
		if err != nil {
			return nil, err
		}
		changed = append(changed, filepath.ToSlash(relative))
	}
	return changed, nil
}
