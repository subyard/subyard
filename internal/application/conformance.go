package application

import (
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
)

type FindingClass string

const (
	FindingDrift        FindingClass = "drift"
	FindingPending      FindingClass = "pending"
	FindingBlocker      FindingClass = "blocker"
	FindingNotInspected FindingClass = "not-inspected"
	FindingNotice       FindingClass = "notice"
)

type ConformanceFinding struct {
	ID        string       `json:"id"`
	Class     FindingClass `json:"class"`
	Resource  string       `json:"resource"`
	Installed string       `json:"installed"`
	Desired   string       `json:"desired"`
	Message   string       `json:"message"`
	Next      string       `json:"next"`
}

type ConformanceSection struct {
	ID       string               `json:"id"`
	Status   string               `json:"status"`
	Findings []ConformanceFinding `json:"findings"`
}

type ConformanceSummary struct {
	Drift        int `json:"drift"`
	Pending      int `json:"pending"`
	Blocker      int `json:"blocker"`
	NotInspected int `json:"not-inspected"`
	Notice       int `json:"notice"`
}

type ConformanceReport struct {
	SchemaVersion int                  `json:"schemaVersion"`
	Status        string               `json:"status"`
	Summary       ConformanceSummary   `json:"summary"`
	Sections      []ConformanceSection `json:"sections"`
}

// InspectionSection contains completed owner reads, not executable operations.
type InspectionSection struct {
	ID    string
	Items []domain.Inspected
}

// ConformanceClosing is the fixed last line of the text report: the check
// never inspects a yard's substrate, whose owners report it themselves.
const ConformanceClosing = "Yard substrate is not inspected; run yard -Y NAME status and yard -Y NAME security for each yard."

var inspectionKey = regexp.MustCompile(`^[a-z0-9][A-Za-z0-9._%-]*$`)

// InspectConformance mechanically classifies owner observations. Finding IDs
// are section.Key; an invalid or duplicate key is a programming error.
func InspectConformance(sections []InspectionSection) (ConformanceReport, error) {
	report := ConformanceReport{SchemaVersion: 1, Status: "ok", Sections: []ConformanceSection{}}
	seen := map[string]bool{}
	for _, source := range sections {
		if !inspectionKey.MatchString(source.ID) || seen[source.ID] {
			return report, fmt.Errorf("invalid inspection section %q", source.ID)
		}
		seen[source.ID] = true
		section := ConformanceSection{ID: source.ID, Status: "ok", Findings: []ConformanceFinding{}}
		for _, item := range source.Items {
			id := source.ID + "." + item.Key
			if !inspectionKey.MatchString(item.Key) || seen[id] || item.Message == "" || item.State == domain.InspectedDrift && item.Next == "" {
				return report, fmt.Errorf("invalid inspection contract: %s", id)
			}
			seen[id] = true
			finding := ConformanceFinding{ID: id, Resource: item.Resource, Installed: item.Installed, Desired: item.Desired, Message: item.Message, Next: item.Next}
			switch item.State {
			case domain.InspectedDrift:
				finding.Class = FindingDrift
				report.Summary.Drift++
				if section.Status != "blocked" {
					section.Status = "drift"
				}
			case domain.InspectedPending:
				finding.Class = FindingPending
				report.Summary.Pending++
			case domain.InspectedUnknown, domain.InspectedBlocked:
				finding.Class = FindingBlocker
				report.Summary.Blocker++
				section.Status = "blocked"
			case domain.InspectedDeferred:
				finding.Class = FindingNotInspected
				report.Summary.NotInspected++
				if section.Status == "ok" {
					section.Status = "skipped"
				}
			case domain.InspectedNotice:
				finding.Class = FindingNotice
				report.Summary.Notice++
			default:
				return report, fmt.Errorf("invalid inspection state: %s", id)
			}
			section.Findings = append(section.Findings, finding)
		}
		report.Sections = append(report.Sections, section)
	}
	if report.Summary.Drift > 0 {
		report.Status = "drift"
	}
	if report.Summary.Blocker > 0 {
		report.Status = "blocked"
	}
	return report, nil
}

// ExitCode is 0 for ok, including pending, not-inspected and notices, and 1 otherwise.
func (report ConformanceReport) ExitCode() int {
	if report.Status == "ok" {
		return 0
	}
	return 1
}

func (report ConformanceReport) WriteText(output io.Writer) error {
	var text strings.Builder
	for _, section := range report.Sections {
		fmt.Fprintf(&text, "%s: %s\n", section.ID, section.Status)
		for _, finding := range section.Findings {
			fmt.Fprintf(&text, "  [%s] %s: %s\n", finding.Class, finding.ID, finding.Message)
			if finding.Next != "" {
				fmt.Fprintf(&text, "    next: %s\n", finding.Next)
			}
		}
	}
	summary := report.Summary
	fmt.Fprintf(&text, "%s; %d drift, %d blockers, %d pending, %d not inspected, %d notices\n%s\n", strings.ToUpper(report.Status),
		summary.Drift, summary.Blocker, summary.Pending, summary.NotInspected, summary.Notice, ConformanceClosing)
	_, err := io.WriteString(output, text.String())
	return err
}
