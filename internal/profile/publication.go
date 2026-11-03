package profile

import (
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// RevisionedSettingsPublication describes an owner's published recipe.
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

// ObserveInstancePublication decodes a declared historical observation for an
// explicit adoption. It grants no lifecycle or device authority and never
// requires mutable recipe settings to equal today's desired values. The caller
// supplies the owning setting catalog's validator; identity aliases still bind
// to the protected environment.
func (definition Definition) ObserveInstancePublication(local, effective, environment map[string]string, validate func(string, string) error) (map[string]string, error) {
	publication := definition.InstancePublication
	if publication == nil {
		return nil, nil
	}
	if err := publication.validate(definition.Name); err != nil {
		return nil, err
	}
	schema, hasSchema := local[publication.SchemaKey]
	revision, hasRevision := local[publication.RevisionKey]
	_, effectiveHasSchema := effective[publication.SchemaKey]
	_, effectiveHasRevision := effective[publication.RevisionKey]
	if !hasSchema && !hasRevision && !effectiveHasSchema && !effectiveHasRevision {
		return nil, nil
	}
	mode := environment[publication.DisabledSetting]
	if !hasSchema || !hasRevision || !effectiveHasSchema || !effectiveHasRevision || schema != effective[publication.SchemaKey] || revision != effective[publication.RevisionKey] || schema != publication.SchemaValue || (mode != "0" && mode != "1") || len(revision) > 8192 {
		return nil, errors.New("native publication is partial, inconsistent or differs from its protected mode")
	}
	if validate == nil {
		return nil, errors.New("native publication has no supported observation codec")
	}
	parts := strings.SplitN(revision, ":", 3)
	if len(parts) != 3 || parts[0] != mode || !publicationRevision.MatchString(parts[1]) {
		return nil, errors.New("native publication revision is malformed or differs from its protected mode")
	}
	var observed map[string]string
	for _, count := range []int{len(publication.Settings), publication.LegacySettingsCount} {
		// Only the first setting can contain the delimiter. Fixed trailing
		// fields are decoded from the right and validated by their owner.
		values := make([]string, count)
		remainder := parts[2]
		for index := count - 1; index > 0; index-- {
			boundary := strings.LastIndexByte(remainder, ':')
			if boundary < 0 {
				remainder = ""
				break
			}
			values[index], remainder = remainder[boundary+1:], remainder[:boundary]
		}
		values[0] = remainder
		candidate := map[string]string{}
		valid := true
		for index, setting := range publication.Settings {
			value := ""
			if index < count {
				value = values[index]
			} else {
				value = *setting.LegacyDefault
			}
			name := setting.Name
			if setting.FallbackSetting != "" {
				name = setting.FallbackSetting
				identity := environment[name]
				if value != identity || environment[setting.Name] != "" && environment[setting.Name] != identity {
					valid = false
					break
				}
			}
			if value == "" || len(value) > 1024 || !textSafe(value) || setting.FallbackSetting == "" && validate(name, value) != nil {
				valid = false
				break
			}
			candidate[setting.Name] = value
		}
		if valid {
			if observed != nil {
				return nil, errors.New("native publication serialization is ambiguous")
			}
			observed = candidate
		}
	}
	if observed == nil {
		return nil, errors.New("native publication recipe is malformed or has conflicting protected identity")
	}
	canonical, _ := json.Marshal(struct {
		Disabled, Schema string
		Settings         map[string]string
	}{mode, schema, observed})
	return map[string]string{publication.SchemaKey: schema, publication.RevisionKey: string(canonical)}, nil
}

func reservedPublicationKey(key string) bool {
	switch key {
	case "user.subyard.adopted_uuid", "user.subyard.managed", "user.subyard.name", "user.subyard.bridge", "user.subyard.initialized", "user.subyard.desired_power", "user.subyard.installation", "user.subyard.operator_uid", "user.subyard.data_home", "user.subyard.config_home", "user.subyard.context", "user.subyard.yard_name", "user.subyard.instance_name", "user.subyard.host_base", "user.subyard.srv_pool", "user.subyard.srv_volume", "user.subyard.storage_pool", "user.subyard.storage_path", "user.subyard.ccusage_version", "user.subyard.codex_version", "user.subyard.preview_sha256", "user.subyard.preview_endpoint_sha256", "user.subyard.preview_proxy", "user.subyard.host_memory":
		return true
	}
	return strings.HasPrefix(key, "user.subyard.resource.") || strings.HasPrefix(key, "user.subyard.deferred_publication.") || strings.HasPrefix(key, "user.subyard.retired_integration.")
}

// ProjectInstancePublication validates observations against trusted resolved
// settings. Missing pairs confer no metadata authority. Only the release digest
// and an explicitly compatible old serialization are publication-equivalent.
func (definition Definition) ProjectInstancePublication(local, effective, environment map[string]string) (map[string]string, error) {
	projection, err := definition.ObserveInstancePublication(local, effective, environment, func(name, value string) error {
		if value != environment[name] {
			return errors.New("native publication settings differ from the protected context")
		}
		return nil
	})
	if err == nil && len(projection) != 0 && environment[definition.InstancePublication.DisabledSetting] != "0" {
		return nil, errors.New("native publication is not supported while enabled")
	}
	return projection, err
}
