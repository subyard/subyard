package ownerapi

import "github.com/Subyard/Subyard/internal/config"

type SettingProvenance struct {
	Scope  string  `json:"scope"`
	Role   string  `json:"role"`
	Status string  `json:"status"`
	Value  *string `json:"value,omitempty"`
}

func settingProvenance(trace config.SettingTrace, includeValues bool) []SettingProvenance {
	result := make([]SettingProvenance, 0, len(trace.Resolutions))
	for _, source := range trace.Resolutions {
		entry := SettingProvenance{Scope: source.Scope, Role: source.Role, Status: source.Status}
		if includeValues && source.Status != "unset" && len(source.Value) <= 8192 && config.ValidateNonSecretContent(trace.Name, source.Value) == nil {
			value := source.Value
			entry.Value = &value
		}
		result = append(result, entry)
	}
	return result
}
