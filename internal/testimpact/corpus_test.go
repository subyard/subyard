package testimpact

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

const validHistoricalChange = `{"status":"M","similarity":null,"old_path":"internal/cli/cli.go","new_path":"internal/cli/cli.go","old_mode":"100644","new_mode":"100644"}`

var historicalCaseIDPattern = regexp.MustCompile(`^history-[0-9]{2}$`)

type historicalCorpus struct {
	SchemaVersion int
	Cases         []historicalCorpusCase
}

type historicalCorpusCase struct {
	ID                string
	Changes           []Change
	Category          string
	ExpectedFull      bool
	RequiredCheckSets []string
	RequiredE2EChecks []string
}

type historicalCorpusWire struct {
	SchemaVersion json.RawMessage `json:"schema_version"`
	Cases         json.RawMessage `json:"cases"`
}

type historicalCorpusCaseWire struct {
	ID                json.RawMessage `json:"id"`
	Changes           json.RawMessage `json:"changes"`
	Category          json.RawMessage `json:"category"`
	ExpectedFull      json.RawMessage `json:"expected_full"`
	RequiredCheckSets json.RawMessage `json:"required_check_sets"`
	RequiredE2EChecks json.RawMessage `json:"required_e2e_checks"`
}

func decodeHistoricalCorpus(reader io.Reader) (historicalCorpus, error) {
	if reader == nil {
		return historicalCorpus{}, errors.New("historical corpus reader is nil")
	}
	source, err := io.ReadAll(reader)
	if err != nil {
		return historicalCorpus{}, fmt.Errorf("read historical corpus: %w", err)
	}
	if !utf8.Valid(source) || bytes.IndexByte(source, 0) >= 0 {
		return historicalCorpus{}, errors.New("historical corpus is not valid UTF-8 JSON")
	}
	if err := rejectDuplicateMembers(source); err != nil {
		return historicalCorpus{}, fmt.Errorf("scan historical corpus: %w", err)
	}

	var wire historicalCorpusWire
	if err := decodeStrict(source, &wire); err != nil {
		return historicalCorpus{}, fmt.Errorf("decode historical corpus: %w", err)
	}
	if !present(wire.SchemaVersion) || !present(wire.Cases) || isNull(wire.SchemaVersion) || isNull(wire.Cases) {
		return historicalCorpus{}, errors.New("historical corpus requires schema_version and cases")
	}
	var schemaVersion int
	if err := decodeStrict(wire.SchemaVersion, &schemaVersion); err != nil || schemaVersion != 1 {
		return historicalCorpus{}, errors.New("historical corpus requires schema_version 1")
	}
	var rawCases []json.RawMessage
	if err := decodeStrict(wire.Cases, &rawCases); err != nil {
		return historicalCorpus{}, fmt.Errorf("decode historical cases: %w", err)
	}
	corpus := historicalCorpus{SchemaVersion: schemaVersion, Cases: make([]historicalCorpusCase, 0, len(rawCases))}
	ids := make(map[string]struct{}, len(rawCases))
	changeSets := make(map[string]string, len(rawCases))
	categories := make(map[string]struct{}, 2)
	for index, rawCase := range rawCases {
		entry, err := decodeHistoricalCorpusCase(rawCase)
		if err != nil {
			return historicalCorpus{}, fmt.Errorf("decode historical case[%d]: %w", index, err)
		}
		if _, exists := ids[entry.ID]; exists {
			return historicalCorpus{}, fmt.Errorf("historical case[%d] duplicates ID %q", index, entry.ID)
		}
		ids[entry.ID] = struct{}{}
		normalizedChanges, err := json.Marshal(entry.Changes)
		if err != nil {
			return historicalCorpus{}, fmt.Errorf("encode historical case[%d] changes: %w", index, err)
		}
		changeSetKey := string(normalizedChanges)
		if previousID, exists := changeSets[changeSetKey]; exists {
			return historicalCorpus{}, fmt.Errorf("duplicate normalized change set in historical cases %q and %q", previousID, entry.ID)
		}
		changeSets[changeSetKey] = entry.ID
		categories[entry.Category] = struct{}{}
		corpus.Cases = append(corpus.Cases, entry)
	}
	if len(rawCases) < 30 || len(rawCases) > 50 {
		return historicalCorpus{}, fmt.Errorf("historical corpus has %d cases, want 30..50", len(rawCases))
	}
	if _, exists := categories["known_high_risk"]; !exists {
		return historicalCorpus{}, errors.New("historical corpus has no known_high_risk case")
	}
	if _, exists := categories["leaf"]; !exists {
		return historicalCorpus{}, errors.New("historical corpus has no leaf case")
	}
	return corpus, nil
}

func decodeHistoricalCorpusCase(source json.RawMessage) (historicalCorpusCase, error) {
	var wire historicalCorpusCaseWire
	if err := decodeStrict(source, &wire); err != nil {
		return historicalCorpusCase{}, err
	}
	fields := []json.RawMessage{
		wire.ID, wire.Changes, wire.Category, wire.ExpectedFull, wire.RequiredCheckSets, wire.RequiredE2EChecks,
	}
	for _, field := range fields {
		if !present(field) || isNull(field) {
			return historicalCorpusCase{}, errors.New("historical case has a missing or null field")
		}
	}

	id, err := requiredString(wire.ID, "id")
	if err != nil || !historicalCaseIDPattern.MatchString(id) {
		return historicalCorpusCase{}, errors.New("historical case has an invalid ID")
	}
	category, err := requiredString(wire.Category, "category")
	if err != nil || (category != "known_high_risk" && category != "leaf") {
		return historicalCorpusCase{}, errors.New("historical case has an invalid category")
	}
	var expectedFull bool
	if err := decodeStrict(wire.ExpectedFull, &expectedFull); err != nil {
		return historicalCorpusCase{}, errors.New("historical case has an invalid expected_full")
	}
	if category == "known_high_risk" && !expectedFull {
		return historicalCorpusCase{}, errors.New("known_high_risk case must expect full P0")
	}

	var rawChanges []json.RawMessage
	if err := decodeStrict(wire.Changes, &rawChanges); err != nil || len(rawChanges) == 0 {
		return historicalCorpusCase{}, errors.New("historical case requires non-empty changes")
	}
	changeDocument, err := json.Marshal(struct {
		SchemaVersion int               `json:"schema_version"`
		Changes       []json.RawMessage `json:"changes"`
	}{SchemaVersion: 1, Changes: rawChanges})
	if err != nil {
		return historicalCorpusCase{}, fmt.Errorf("encode historical changes: %w", err)
	}
	changeSet, err := DecodeChangeSet(bytes.NewReader(changeDocument))
	if err != nil {
		return historicalCorpusCase{}, fmt.Errorf("decode historical changes: %w", err)
	}

	requiredCheckSets, err := decodeUniqueStrings(wire.RequiredCheckSets, "required_check_sets")
	if err != nil {
		return historicalCorpusCase{}, err
	}
	requiredE2EChecks, err := decodeUniqueStrings(wire.RequiredE2EChecks, "required_e2e_checks")
	if err != nil {
		return historicalCorpusCase{}, err
	}
	return historicalCorpusCase{
		ID: id, Changes: changeSet.Changes, Category: category, ExpectedFull: expectedFull,
		RequiredCheckSets: requiredCheckSets, RequiredE2EChecks: requiredE2EChecks,
	}, nil
}

func decodeUniqueStrings(source json.RawMessage, name string) ([]string, error) {
	var values []string
	if err := decodeStrict(source, &values); err != nil || values == nil {
		return nil, fmt.Errorf("historical case has invalid %s", name)
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" {
			return nil, fmt.Errorf("historical case has empty %s entry", name)
		}
		if _, exists := seen[value]; exists {
			return nil, fmt.Errorf("historical case has duplicate %s entry %q", name, value)
		}
		seen[value] = struct{}{}
	}
	return values, nil
}

func TestHistoricalCorpusContract(t *testing.T) {
	path := filepath.Join("..", "..", "tests", "fixtures", "test-impact", "corpus.json")
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open historical corpus: %v", err)
	}
	defer file.Close()

	corpus, err := decodeHistoricalCorpus(file)
	if err != nil {
		t.Fatalf("decodeHistoricalCorpus() error = %v", err)
	}
	if got := len(corpus.Cases); got < 30 || got > 50 {
		t.Fatalf("case count = %d, want 30..50", got)
	}
}

func TestHistoricalCorpusRejectsDuplicateJSONMembers(t *testing.T) {
	validCase := historicalCaseJSON("history-01", validHistoricalChange)
	tests := []struct {
		name  string
		input string
	}{
		{"corpus member", `{"schema_version":1,"schema_version":1,"cases":[` + validCase + `]}`},
		{"case member", `{"schema_version":1,"cases":[` + strings.Replace(validCase, `"id":"history-01"`, `"id":"history-01","id":"history-02"`, 1) + `]}`},
		{"change member", `{"schema_version":1,"cases":[` + historicalCaseJSON("history-01", strings.Replace(validHistoricalChange, `"status":"M"`, `"status":"M","status":"M"`, 1)) + `]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodeHistoricalCorpus(strings.NewReader(test.input)); err == nil {
				t.Fatal("decodeHistoricalCorpus() accepted duplicate JSON member")
			}
		})
	}
}

func TestHistoricalCorpusRejectsDuplicateNormalizedChangeSets(t *testing.T) {
	cases := make([]string, 30)
	highRisk := historicalCaseJSON("history-01", validHistoricalChange)
	highRisk = strings.Replace(highRisk, `"category":"leaf"`, `"category":"known_high_risk"`, 1)
	highRisk = strings.Replace(highRisk, `"expected_full":false`, `"expected_full":true`, 1)
	cases[0] = highRisk
	cases[1] = historicalCaseJSON("history-02", validHistoricalChange)
	for index := 2; index < len(cases); index++ {
		change := strings.ReplaceAll(validHistoricalChange, "internal/cli/cli.go", fmt.Sprintf("internal/cli/case-%02d.go", index+1))
		cases[index] = historicalCaseJSON(fmt.Sprintf("history-%02d", index+1), change)
	}
	input := `{"schema_version":1,"cases":[` + strings.Join(cases, ",") + `]}`

	if _, err := decodeHistoricalCorpus(strings.NewReader(input)); err == nil || !strings.Contains(err.Error(), "duplicate normalized change set") {
		t.Fatalf("decodeHistoricalCorpus() error = %v, want duplicate normalized change set", err)
	}
}

func TestHistoricalCorpusRejectsInvalidDocuments(t *testing.T) {
	validCase := historicalCaseJSON("history-01", validHistoricalChange)
	tests := []struct {
		name  string
		input string
	}{
		{"unknown corpus field", `{"schema_version":1,"cases":[` + validCase + `],"unknown":true}`},
		{"missing corpus field", `{"schema_version":1}`},
		{"unknown case field", `{"schema_version":1,"cases":[` + strings.TrimSuffix(validCase, `}`) + `,"unknown":true}]}`},
		{"missing case field", `{"schema_version":1,"cases":[` + strings.Replace(validCase, `,"required_e2e_checks":[]`, ``, 1) + `]}`},
		{"unknown change field", `{"schema_version":1,"cases":[` + historicalCaseJSON("history-01", strings.TrimSuffix(validHistoricalChange, `}`)+`,"unknown":true}`) + `]}`},
		{"zero live mode", `{"schema_version":1,"cases":[` + historicalCaseJSON("history-01", strings.Replace(validHistoricalChange, `"new_mode":"100644"`, `"new_mode":"000000"`, 1)) + `]}`},
		{"absolute path", `{"schema_version":1,"cases":[` + historicalCaseJSON("history-01", strings.ReplaceAll(validHistoricalChange, `internal/cli/cli.go`, `/internal/cli/cli.go`)) + `]}`},
		{"traversal path", `{"schema_version":1,"cases":[` + historicalCaseJSON("history-01", strings.ReplaceAll(validHistoricalChange, `internal/cli/cli.go`, `internal/../cli.go`)) + `]}`},
		{"NUL path", `{"schema_version":1,"cases":[` + historicalCaseJSON("history-01", strings.ReplaceAll(validHistoricalChange, `internal/cli/cli.go`, `internal/cli/\u0000.go`)) + `]}`},
		{"invalid status shape", `{"schema_version":1,"cases":[` + historicalCaseJSON("history-01", strings.Replace(validHistoricalChange, `"status":"M"`, `"status":"A"`, 1)) + `]}`},
		{"duplicate ID", `{"schema_version":1,"cases":[` + validCase + `,` + validCase + `]}`},
		{"empty cases", `{"schema_version":1,"cases":[]}`},
		{"too few cases", corpusWithCaseCount(29)},
		{"too many cases", corpusWithCaseCount(51)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodeHistoricalCorpus(strings.NewReader(test.input)); err == nil {
				t.Fatal("decodeHistoricalCorpus() accepted invalid document")
			}
		})
	}
}

func historicalCaseJSON(id, change string) string {
	return fmt.Sprintf(`{"id":%q,"changes":[%s],"category":"leaf","expected_full":false,"required_check_sets":[],"required_e2e_checks":[]}`, id, change)
}

func corpusWithCaseCount(count int) string {
	cases := make([]string, count)
	for index := range cases {
		cases[index] = historicalCaseJSON(fmt.Sprintf("history-%02d", index+1), validHistoricalChange)
	}
	return `{"schema_version":1,"cases":[` + strings.Join(cases, ",") + `]}`
}
