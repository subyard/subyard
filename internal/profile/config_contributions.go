package profile

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ConfigContribution locates a protected, completed field receipt from a
// release that predates contribution references.
type ConfigContribution struct {
	Destination string `json:"destination"`
	Receipt     string `json:"receipt"`
	Path        string `json:"path"`
	Section     string `json:"section"`
}

func (definition *Definition) loadConfigContributions() error {
	const name = "config-contributions.json"
	path, err := definition.sourcePath(name, false)
	if err != nil {
		if _, statErr := os.Lstat(filepath.Join(definition.Root, name)); os.IsNotExist(statErr) {
			return nil
		}
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	const limit = 4096
	payload, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return err
	}
	if len(payload) > limit {
		return errors.New("configuration contributions exceed size bound")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var entries []ConfigContribution
	if decoder.Decode(&entries) != nil || decoder.Decode(new(any)) != io.EOF {
		return errors.New("invalid configuration contributions")
	}
	for _, entry := range entries {
		if !filepath.IsLocal(entry.Destination) || filepath.Clean(entry.Destination) != entry.Destination ||
			!strings.HasPrefix(entry.Receipt, "/var/lib/subyard/") || filepath.Clean(entry.Receipt) != entry.Receipt ||
			!strings.HasPrefix(entry.Path, "/") || entry.Section == "" {
			return errors.New("invalid configuration contribution")
		}
	}
	definition.ConfigContributions = entries
	definition.validationInputs = append(definition.validationInputs, path)
	return nil
}
