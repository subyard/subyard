package profile

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
)

// DeveloperIdentityContract is a source-package capability. Its separate file
// preserves the strict profile.json wire schema understood by retained engines.
func (definition Definition) DeveloperIdentityContract() (string, error) {
	path, err := definition.sourcePath("developer-identity.json", false)
	if err != nil {
		if _, statErr := os.Lstat(definition.Root + "/developer-identity.json"); os.IsNotExist(statErr) {
			return "", nil
		}
		return "", err
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if len(payload) > 1024 {
		return "", errors.New("developer identity capability exceeds size bound")
	}
	var value struct {
		Schema int    `json:"schema"`
		Mode   string `json:"mode"`
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF || value.Schema != 1 || definition.Runtime == nil || (value.Mode != "rebind" && value.Mode != "independent") {
		return "", errors.New("invalid developer identity capability")
	}
	return value.Mode, nil
}
