package testimpact

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"unicode/utf8"
)

type resultOutputWire struct {
	SchemaVersion  int                         `json:"schema_version"`
	Status         string                      `json:"status"`
	Changes        []changeOutputWire          `json:"changes"`
	CheckSets      []string                    `json:"check_sets"`
	RiskDomains    []string                    `json:"risk_domains"`
	HostFreeChecks []checkRecommendationWire   `json:"host_free_checks"`
	E2EChecks      []checkRecommendationWire   `json:"e2e_checks"`
	FullP0         fullP0SelectionWire         `json:"full_p0"`
	Reasons        []selectionReasonOutputWire `json:"reasons"`
	Errors         []resultErrorOutputWire     `json:"errors"`
}

type changeOutputWire struct {
	Status     string  `json:"status"`
	Similarity *int    `json:"similarity"`
	OldPath    *string `json:"old_path"`
	NewPath    *string `json:"new_path"`
	OldMode    string  `json:"old_mode"`
	NewMode    string  `json:"new_mode"`
}

type checkRecommendationWire struct {
	ID            string `json:"id"`
	Tier          string `json:"tier"`
	BudgetSeconds int    `json:"budget_seconds"`
	Rationale     string `json:"rationale"`
}

type fullP0SelectionWire struct {
	Required bool               `json:"required"`
	Reasons  []fullP0ReasonWire `json:"reasons"`
}

type fullP0ReasonWire struct {
	Code        string   `json:"code"`
	RiskDomains []string `json:"risk_domains"`
}

type selectionReasonOutputWire struct {
	Path        string   `json:"path"`
	Side        string   `json:"side"`
	RuleID      string   `json:"rule_id"`
	CheckSets   []string `json:"check_sets"`
	RiskDomains []string `json:"risk_domains"`
}

type resultErrorOutputWire struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type errorResultOutputWire struct {
	SchemaVersion int                     `json:"schema_version"`
	Status        string                  `json:"status"`
	Errors        []resultErrorOutputWire `json:"errors"`
}

// WriteJSON renders a complete selection or fallback result as one versioned
// JSON document. The explicit output mapping keeps the wire contract separate
// from the internal model.
func WriteJSON(writer io.Writer, result Result) error {
	wire, err := mapResultOutput(result)
	if err != nil {
		return err
	}
	return encodeJSONDocument(writer, wire)
}

// WriteErrorJSON renders the minimal CLI-misuse result without a plan.
func WriteErrorJSON(writer io.Writer, resultErrors []ResultError) error {
	errorsWire, err := mapErrors(resultErrors)
	if err != nil {
		return err
	}
	return encodeJSONDocument(writer, errorResultOutputWire{
		SchemaVersion: 1,
		Status:        "error",
		Errors:        errorsWire,
	})
}

// WriteErrorHuman renders the minimal CLI-misuse result without a plan.
func WriteErrorHuman(writer io.Writer, resultErrors []ResultError) error {
	if _, err := mapErrors(resultErrors); err != nil {
		return err
	}
	var output bytes.Buffer
	output.WriteString("schema version: 1\n")
	output.WriteString("status: \"error\"\n")
	output.WriteString("errors:\n")
	for _, resultError := range resultErrors {
		fmt.Fprintf(&output, "  - code=%s message=%s\n",
			quoteHuman(resultError.Code), quoteHuman(resultError.Message))
	}
	_, err := writer.Write(output.Bytes())
	return err
}

// WriteHuman renders a complete result with every dynamic string quoted, so
// repository paths and diagnostics cannot inject terminal control sequences.
func WriteHuman(writer io.Writer, result Result) error {
	if _, err := mapResultOutput(result); err != nil {
		return err
	}

	var output bytes.Buffer
	fmt.Fprintf(&output, "schema version: %d\n", result.SchemaVersion)
	fmt.Fprintf(&output, "status: %s\n", quoteHuman(result.Status))
	writeHumanChanges(&output, result.Changes)
	writeHumanStrings(&output, "check sets", result.CheckSets)
	writeHumanStrings(&output, "risk domains", result.RiskDomains)
	writeHumanChecks(&output, "host-free checks", result.HostFreeChecks)
	writeHumanChecks(&output, "e2e checks", result.E2EChecks)
	fmt.Fprintf(&output, "full P0 required: %t\n", result.FullP0.Required)
	output.WriteString("full P0 reasons:\n")
	for _, reason := range result.FullP0.Reasons {
		fmt.Fprintf(&output, "  - code=%s risk_domains=%s\n",
			quoteHuman(reason.Code), quoteHumanStrings(reason.RiskDomains))
	}
	output.WriteString("selection reasons:\n")
	for _, reason := range result.Reasons {
		fmt.Fprintf(&output, "  - path=%s side=%s rule_id=%s check_sets=%s risk_domains=%s\n",
			quoteHuman(reason.Path), quoteHuman(reason.Side), quoteHuman(reason.RuleID),
			quoteHumanStrings(reason.CheckSets), quoteHumanStrings(reason.RiskDomains))
	}
	output.WriteString("errors:\n")
	for _, resultError := range result.Errors {
		fmt.Fprintf(&output, "  - code=%s message=%s\n",
			quoteHuman(resultError.Code), quoteHuman(resultError.Message))
	}
	_, err := writer.Write(output.Bytes())
	return err
}

func mapResultOutput(result Result) (resultOutputWire, error) {
	if err := validateResultUTF8(result); err != nil {
		return resultOutputWire{}, err
	}
	wire := resultOutputWire{
		SchemaVersion:  result.SchemaVersion,
		Status:         result.Status,
		Changes:        make([]changeOutputWire, 0, len(result.Changes)),
		CheckSets:      nonNilStrings(result.CheckSets),
		RiskDomains:    nonNilStrings(result.RiskDomains),
		HostFreeChecks: make([]checkRecommendationWire, 0, len(result.HostFreeChecks)),
		E2EChecks:      make([]checkRecommendationWire, 0, len(result.E2EChecks)),
		FullP0: fullP0SelectionWire{
			Required: result.FullP0.Required,
			Reasons:  make([]fullP0ReasonWire, 0, len(result.FullP0.Reasons)),
		},
		Reasons: make([]selectionReasonOutputWire, 0, len(result.Reasons)),
		Errors:  make([]resultErrorOutputWire, 0, len(result.Errors)),
	}
	for _, change := range result.Changes {
		wire.Changes = append(wire.Changes, changeOutputWire{
			Status: change.Status, Similarity: change.Similarity,
			OldPath: change.OldPath, NewPath: change.NewPath,
			OldMode: change.OldMode, NewMode: change.NewMode,
		})
	}
	for _, check := range result.HostFreeChecks {
		wire.HostFreeChecks = append(wire.HostFreeChecks, mapCheck(check))
	}
	for _, check := range result.E2EChecks {
		wire.E2EChecks = append(wire.E2EChecks, mapCheck(check))
	}
	for _, reason := range result.FullP0.Reasons {
		wire.FullP0.Reasons = append(wire.FullP0.Reasons, fullP0ReasonWire{
			Code: reason.Code, RiskDomains: nonNilStrings(reason.RiskDomains),
		})
	}
	for _, reason := range result.Reasons {
		wire.Reasons = append(wire.Reasons, selectionReasonOutputWire{
			Path: reason.Path, Side: reason.Side, RuleID: reason.RuleID,
			CheckSets: nonNilStrings(reason.CheckSets), RiskDomains: nonNilStrings(reason.RiskDomains),
		})
	}
	errorsWire, err := mapErrors(result.Errors)
	if err != nil {
		return resultOutputWire{}, err
	}
	wire.Errors = errorsWire
	return wire, nil
}

func mapCheck(check CheckRecommendation) checkRecommendationWire {
	return checkRecommendationWire{
		ID: check.ID, Tier: check.Tier, BudgetSeconds: check.BudgetSeconds, Rationale: check.Rationale,
	}
}

func mapErrors(resultErrors []ResultError) ([]resultErrorOutputWire, error) {
	wire := make([]resultErrorOutputWire, 0, len(resultErrors))
	for _, resultError := range resultErrors {
		if !utf8.ValidString(resultError.Code) || !utf8.ValidString(resultError.Message) {
			return nil, errors.New("result error contains invalid UTF-8")
		}
		wire = append(wire, resultErrorOutputWire{Code: resultError.Code, Message: resultError.Message})
	}
	return wire, nil
}

func encodeJSONDocument(writer io.Writer, value any) error {
	var output bytes.Buffer
	if err := json.NewEncoder(&output).Encode(value); err != nil {
		return fmt.Errorf("encode result JSON: %w", err)
	}
	if _, err := writer.Write(output.Bytes()); err != nil {
		return fmt.Errorf("write result JSON: %w", err)
	}
	return nil
}

func validateResultUTF8(result Result) error {
	stringsToCheck := []string{result.Status}
	for _, change := range result.Changes {
		stringsToCheck = append(stringsToCheck, change.Status, change.OldMode, change.NewMode)
		if change.OldPath != nil {
			stringsToCheck = append(stringsToCheck, *change.OldPath)
		}
		if change.NewPath != nil {
			stringsToCheck = append(stringsToCheck, *change.NewPath)
		}
	}
	stringsToCheck = append(stringsToCheck, result.CheckSets...)
	stringsToCheck = append(stringsToCheck, result.RiskDomains...)
	for _, check := range appendChecks(result.HostFreeChecks, result.E2EChecks) {
		stringsToCheck = append(stringsToCheck, check.ID, check.Tier, check.Rationale)
	}
	for _, reason := range result.FullP0.Reasons {
		stringsToCheck = append(stringsToCheck, reason.Code)
		stringsToCheck = append(stringsToCheck, reason.RiskDomains...)
	}
	for _, reason := range result.Reasons {
		stringsToCheck = append(stringsToCheck, reason.Path, reason.Side, reason.RuleID)
		stringsToCheck = append(stringsToCheck, reason.CheckSets...)
		stringsToCheck = append(stringsToCheck, reason.RiskDomains...)
	}
	for _, resultError := range result.Errors {
		stringsToCheck = append(stringsToCheck, resultError.Code, resultError.Message)
	}
	for _, value := range stringsToCheck {
		if !utf8.ValidString(value) {
			return errors.New("result contains invalid UTF-8")
		}
	}
	return nil
}

func appendChecks(left, right []CheckRecommendation) []CheckRecommendation {
	checks := make([]CheckRecommendation, 0, len(left)+len(right))
	checks = append(checks, left...)
	checks = append(checks, right...)
	return checks
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func writeHumanChanges(output *bytes.Buffer, changes []Change) {
	output.WriteString("changes:\n")
	for _, change := range changes {
		fmt.Fprintf(output, "  - status=%s similarity=%s old_path=%s new_path=%s old_mode=%s new_mode=%s\n",
			quoteHuman(change.Status), humanOptionalInt(change.Similarity), humanOptionalString(change.OldPath),
			humanOptionalString(change.NewPath), quoteHuman(change.OldMode), quoteHuman(change.NewMode))
	}
}

func writeHumanStrings(output *bytes.Buffer, label string, values []string) {
	fmt.Fprintf(output, "%s:\n", label)
	for _, value := range values {
		fmt.Fprintf(output, "  - %s\n", quoteHuman(value))
	}
}

func writeHumanChecks(output *bytes.Buffer, label string, checks []CheckRecommendation) {
	fmt.Fprintf(output, "%s:\n", label)
	for _, check := range checks {
		fmt.Fprintf(output, "  - id=%s tier=%s budget_seconds=%d rationale=%s\n",
			quoteHuman(check.ID), quoteHuman(check.Tier), check.BudgetSeconds, quoteHuman(check.Rationale))
	}
}

func quoteHuman(value string) string {
	return strconv.Quote(value)
}

func quoteHumanStrings(values []string) string {
	var output bytes.Buffer
	output.WriteByte('[')
	for index, value := range values {
		if index > 0 {
			output.WriteString(", ")
		}
		output.WriteString(quoteHuman(value))
	}
	output.WriteByte(']')
	return output.String()
}

func humanOptionalString(value *string) string {
	if value == nil {
		return "null"
	}
	return quoteHuman(*value)
}

func humanOptionalInt(value *int) string {
	if value == nil {
		return "null"
	}
	return strconv.Itoa(*value)
}
