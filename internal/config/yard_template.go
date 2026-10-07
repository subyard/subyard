package config

import (
	"os"
	"path/filepath"

	"github.com/Subyard/Subyard/internal/domain"
)

// IsSupportedYardTemplate reports whether template is a shipped template that
// the candidate loader can apply from configDir.
func IsSupportedYardTemplate(configDir, template string) bool {
	if configDir == "" || !filepath.IsAbs(configDir) || !domain.SafeName(template) {
		return false
	}
	path := filepath.Join(configDir, "yards", "profiles", template+".env")
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	settings, err := LoadCatalog(filepath.Dir(configDir))
	if err != nil {
		return false
	}
	return settings.applyEnvFileValidated(path, environment{}, ScopeShipped, false, nil) == nil
}
