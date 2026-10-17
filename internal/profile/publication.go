package profile

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// RevisionedSettingsPublication describes a disabled owner's published recipe.
// It grants no writer, device, lifecycle or deletion authority.
type RevisionedSettingsPublication struct {
	SchemaKey           string               `json:"schema_key"`
	SchemaValue         string               `json:"schema_value"`
	RevisionKey         string               `json:"revision_key"`
	DisabledSetting     string               `json:"disabled_setting"`
	Settings            []PublicationSetting `json:"settings"`
	LegacySettingsCount int                  `json:"legacy_settings_count"`
}

type PublicationSetting struct {
	Name string `json:"name"`
	// FallbackSetting is an identity alias: an explicit value must match it.
	FallbackSetting string  `json:"fallback_setting,omitempty"`
	LegacyDefault   *string `json:"legacy_default,omitempty"`
}

var publicationSettingName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
var publicationKey = regexp.MustCompile(`^[a-z0-9_.]+$`)
var publicationRevision = regexp.MustCompile(`^[a-f0-9]{64}$`)

func (publication RevisionedSettingsPublication) validate(owner string) error {
	prefix := "user.subyard." + strings.ReplaceAll(owner, "-", "_") + "_"
	for _, key := range []string{publication.SchemaKey, publication.RevisionKey} {
		if !strings.HasPrefix(key, prefix) || len(key) > 128 || !publicationKey.MatchString(key) || reservedPublicationKey(key) {
			return errors.New("invalid or reserved profile native publication key")
		}
	}
	schema, err := strconv.ParseUint(publication.SchemaValue, 10, 16)
	if err != nil || schema == 0 || strconv.FormatUint(schema, 10) != publication.SchemaValue || publication.SchemaKey == publication.RevisionKey || !publicationSettingName.MatchString(publication.DisabledSetting) || len(publication.Settings) < 2 || len(publication.Settings) > 16 || publication.LegacySettingsCount < 1 || publication.LegacySettingsCount >= len(publication.Settings) {
		return errors.New("invalid revisioned settings publication")
	}
	seen := map[string]bool{}
	for i, setting := range publication.Settings {
		if !publicationSettingName.MatchString(setting.Name) || seen[setting.Name] || setting.Name == publication.DisabledSetting || setting.FallbackSetting != "" && (!publicationSettingName.MatchString(setting.FallbackSetting) || setting.FallbackSetting == setting.Name) {
			return errors.New("invalid publication setting")
		}
		seen[setting.Name] = true
		if i < publication.LegacySettingsCount && setting.LegacyDefault != nil || i >= publication.LegacySettingsCount && (setting.LegacyDefault == nil || *setting.LegacyDefault == "" || !textSafe(*setting.LegacyDefault)) {
			return errors.New("invalid legacy publication defaults")
		}
	}
	return nil
}

func reservedPublicationKey(key string) bool {
	switch key {
	case "user.subyard.managed", "user.subyard.name", "user.subyard.bridge", "user.subyard.initialized", "user.subyard.desired_power", "user.subyard.installation", "user.subyard.operator_uid", "user.subyard.data_home", "user.subyard.config_home", "user.subyard.context", "user.subyard.yard_name", "user.subyard.instance_name", "user.subyard.host_base", "user.subyard.srv_pool", "user.subyard.srv_volume", "user.subyard.storage_pool", "user.subyard.storage_path", "user.subyard.ccusage_version", "user.subyard.codex_version", "user.subyard.preview_sha256", "user.subyard.preview_endpoint_sha256", "user.subyard.preview_proxy", "user.subyard.host_memory":
		return true
	}
	return strings.HasPrefix(key, "user.subyard.resource.") || strings.HasPrefix(key, "user.subyard.deferred_publication.")
}

// ProjectInstancePublication validates observations against trusted resolved
// settings. Missing pairs confer no metadata authority. Only the release digest
// and an explicitly compatible old serialization are publication-equivalent.
func (definition Definition) ProjectInstancePublication(local, effective, environment map[string]string) (map[string]string, error) {
	publication := definition.InstancePublication
	if publication == nil {
		return nil, nil
	}
	if err := publication.validate(definition.Name); err != nil {
		return nil, err
	}
	schema, hasSchema := local[publication.SchemaKey]
	revision, hasRevision := local[publication.RevisionKey]
	effectiveSchema, effectiveHasSchema := effective[publication.SchemaKey]
	effectiveRevision, effectiveHasRevision := effective[publication.RevisionKey]
	if !hasSchema && !hasRevision && !effectiveHasSchema && !effectiveHasRevision {
		return nil, nil
	}
	if !hasSchema || !hasRevision || !effectiveHasSchema || !effectiveHasRevision || schema != effectiveSchema || revision != effectiveRevision || schema != publication.SchemaValue || environment[publication.DisabledSetting] != "0" || len(revision) > 8192 {
		return nil, errors.New("native publication is partial, inconsistent or not supported while disabled")
	}
	parts := strings.SplitN(revision, ":", 3)
	if len(parts) != 3 || parts[0] != "0" || !publicationRevision.MatchString(parts[1]) {
		return nil, errors.New("native publication revision is malformed or enabled")
	}
	values := make([]string, 0, len(publication.Settings))
	stable := map[string]string{}
	legacyCompatible := true
	for _, setting := range publication.Settings {
		value := environment[setting.Name]
		if setting.FallbackSetting != "" {
			identity := environment[setting.FallbackSetting]
			if value != "" && value != identity {
				return nil, fmt.Errorf("native publication setting %s differs from its protected identity", setting.Name)
			}
			value = identity
		}
		if value == "" || len(value) > 1024 || !textSafe(value) {
			return nil, fmt.Errorf("native publication setting %s is unavailable", setting.Name)
		}
		values = append(values, value)
		stable[setting.Name] = value
		if setting.LegacyDefault != nil && value != *setting.LegacyDefault {
			legacyCompatible = false
		}
	}
	current := strings.Join(values, ":")
	legacy := strings.Join(values[:publication.LegacySettingsCount], ":")
	if parts[2] != current && !(legacyCompatible && parts[2] == legacy) {
		return nil, errors.New("native publication settings differ from the protected context")
	}
	canonical, _ := json.Marshal(struct {
		Disabled, Schema string
		Settings         map[string]string
	}{"0", schema, stable})
	return map[string]string{publication.SchemaKey: schema, publication.RevisionKey: string(canonical)}, nil
}
