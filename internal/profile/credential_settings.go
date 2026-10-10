package profile

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// CredentialSettings keeps a key and its declared setup fields in one revision.
// Paths and other owner-local options are deliberately excluded.
type CredentialSettings struct {
	SchemaVersion int            `json:"schema_version"`
	Settings      map[string]any `json:"settings"`
	PrivateKey    string         `json:"private_key"`
}

func (setup Setup) SharedSettings(values map[string]any) (map[string]any, error) {
	shared := map[string]any{}
	for _, field := range setup.Fields {
		shared[field.Name] = values[field.Name]
	}
	data, err := json.Marshal(shared)
	if err != nil {
		return nil, err
	}
	return setup.Decode(data)
}

func (setup Setup) DecodeCredentialSettings(data []byte) (CredentialSettings, error) {
	var bundle CredentialSettings
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if decoder.Decode(&bundle) != nil || decoder.Decode(new(any)) != io.EOF || bundle.SchemaVersion != 1 || bundle.PrivateKey == "" {
		return bundle, errors.New("invalid credential settings bundle")
	}
	settings, err := setup.SharedSettings(bundle.Settings)
	if err != nil || len(settings) != len(bundle.Settings) {
		return bundle, errors.New("invalid shared credential fields")
	}
	bundle.Settings = settings
	return bundle, nil
}

func (setup Setup) EncodeCredentialSettings(settings map[string]any, key []byte) ([]byte, error) {
	shared, err := setup.SharedSettings(settings)
	if err != nil {
		return nil, err
	}
	return json.Marshal(CredentialSettings{SchemaVersion: 1, Settings: shared, PrivateKey: string(key)})
}
