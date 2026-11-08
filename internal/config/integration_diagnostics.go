package config

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
)

func (catalog Catalog) IntegrationDiagnosticFile(name string) bool {
	definition, ok := catalog.LookupSetting(name)
	return ok && catalog.IntegrationDiagnosticSetting(name) && (definition.Kind == SettingFile || definition.Type == SettingRegularFilePath || name == "HOST_CLAUDE_MD" || name == "HOST_CODEX_AGENTS_MD" || name == "HOST_OPENCODE_AGENTS_MD")
}

// IntegrationDiagnosticDigests computes keyed evidence without retaining the
// values or file contents. Unreadable files have no evidence and remain unknown.
func (catalog Catalog) IntegrationDiagnosticDigests(values map[string]string, key []byte, read func(MaterializedAsset) ([]byte, error)) map[string]string {
	result := map[string]string{}
	for name, value := range values {
		definition, known := catalog.LookupSetting(name)
		if !known || !catalog.IntegrationDiagnosticSetting(name) || (!definition.Sensitive && !catalog.IntegrationDiagnosticFile(name)) {
			continue
		}
		payload := []byte(value)
		if catalog.IntegrationDiagnosticFile(name) {
			asset := MaterializedAsset{Source: value, FollowSymlinks: strings.HasPrefix(name, "HOST_")}
			var err error
			if read == nil {
				payload, err = asset.ReadSource()
			} else {
				payload, err = read(asset)
			}
			if err != nil {
				continue
			}
		}
		result[name] = ConfigurationInputDigest(name, definition.Kind, key, payload)
	}
	return result
}

func ConfigurationInputDigest(name string, kind SettingKind, key, payload []byte) string {
	mac := hmac.New(sha256.New, key)
	_, _ = io.WriteString(mac, name+"\x00"+string(kind)+"\x00")
	_, _ = mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

// ValidIntegrationDiagnosticEvidence checks the closed digest transport before
// any retained key or digest is used as evidence.
func (catalog Catalog) ValidIntegrationDiagnosticEvidence(key string, digests map[string]string) bool {
	decoded, err := hex.DecodeString(key)
	if err != nil || len(decoded) != 32 || hex.EncodeToString(decoded) != key || len(digests) > 4096 {
		return false
	}
	for name, digest := range digests {
		definition, ok := catalog.LookupSetting(name)
		if !ok || !catalog.IntegrationDiagnosticSetting(name) || (!definition.Sensitive && !catalog.IntegrationDiagnosticFile(name)) || len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" {
			return false
		}
	}
	return true
}
