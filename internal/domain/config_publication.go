package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
)

const ConfigPublicationPendingPrefix = "contribution-publication-pending-v1 "

type ConfigPublicationPending struct {
	Destination string          `json:"destination"`
	Journal     string          `json:"journal"`
	Prior       json.RawMessage `json:"prior"`
}

func (failure ConfigPublicationPending) Error() string {
	return fmt.Sprintf("configuration publication pending for %q (restore prior fields %s, then clear %q manually)", failure.Destination, failure.Prior, failure.Journal)
}

func ConfigPublicationRefusals(output []byte, destination string) []error {
	var failures []error
	for _, line := range bytes.Split(output, []byte{'\n'}) {
		data, ok := bytes.CutPrefix(line, []byte(ConfigPublicationPendingPrefix))
		if !ok || len(data) > 24<<10 || strings.ContainsFunc(string(data), unicode.IsControl) {
			continue
		}
		var failure ConfigPublicationPending
		if json.Unmarshal(data, &failure) != nil || !filepath.IsAbs(failure.Destination) ||
			!filepath.IsAbs(failure.Journal) || len(failure.Prior) == 0 ||
			(failure.Prior[0] != '{' && string(failure.Prior) != "null") ||
			(destination != "" && failure.Destination != destination) {
			continue
		}
		failures = append(failures, failure)
	}
	return failures
}
