package domain

import (
	"errors"
	"regexp"
	"slices"
	"strings"
)

// Recovery advice stays typed until it is rendered: one observation yields
// findings, one selector picks one Action, and one renderer prints it. Owner
// retries arrive as frozen V1 strings; ParseRetry reads them once, and a retry
// it cannot read stays verbatim on its finding.

// Verb is the closed set of commands recovery advice may name.
type Verb uint8

const (
	VerbNone Verb = iota
	VerbInit
	VerbConfigApply
	VerbConfigStatus
	VerbConfigValidate
	VerbMigrate
	VerbMigrateCheck
	VerbUpdate
	VerbUpdateCheck
	VerbStart
	VerbStopForce
	VerbIntegrationStatus
)

var verbWords = map[Verb]string{
	VerbInit: "init", VerbConfigApply: "config apply", VerbConfigStatus: "config status",
	VerbConfigValidate: "config validate", VerbMigrate: "migrate", VerbMigrateCheck: "migrate --check",
	VerbUpdate: "update", VerbUpdateCheck: "update --check", VerbStart: "start", VerbStopForce: "stop --force",
	VerbIntegrationStatus: "integration status",
}

// Host verbs act on the whole host and never select a yard.
func (verb Verb) Host() bool {
	return verb == VerbMigrate || verb == VerbMigrateCheck || verb == VerbUpdate || verb == VerbUpdateCheck
}

// Action is one recovery command. Version is used only by VerbUpdate.
type Action struct {
	Verb    Verb
	Yard    string
	Version string
}

var versionPattern = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)

// RenderAction is the exact inverse of ParseAction.
func RenderAction(action Action) string {
	words, ok := verbWords[action.Verb]
	if !ok {
		return ""
	}
	parts := []string{"yard"}
	if !action.Verb.Host() && action.Yard != "" {
		parts = append(parts, "-Y", action.Yard)
	}
	parts = append(parts, words)
	if action.Verb == VerbUpdate && action.Version != "" {
		parts = append(parts, "--version", action.Version)
	}
	return strings.Join(parts, " ")
}

var errUnparsedAction = errors.New("not a recovery action")

// ParseAction reads exactly one rendered action.
func ParseAction(text string) (Action, error) {
	fields := strings.Split(text, " ")
	if len(fields) < 2 || fields[0] != "yard" {
		return Action{}, errUnparsedAction
	}
	action, rest := Action{}, fields[1:]
	if rest[0] == "-Y" {
		if len(rest) < 3 || !SafeName(rest[1]) {
			return Action{}, errUnparsedAction
		}
		action.Yard, rest = rest[1], rest[2:]
	}
	if len(rest) == 3 && rest[0] == "update" && rest[1] == "--version" && action.Yard == "" && versionPattern.MatchString(rest[2]) {
		return Action{Verb: VerbUpdate, Version: rest[2]}, nil
	}
	words := strings.Join(rest, " ")
	for verb, candidate := range verbWords {
		if candidate == words && !(verb.Host() && action.Yard != "") {
			action.Verb = verb
			return action, nil
		}
	}
	return Action{}, errUnparsedAction
}

// ParseRetry reads an owner retry in its accepted forms: "<command>",
// "run <command>" or "<prose>, then run <command>". prose is what precedes
// the command; ok is false for any other text, which callers keep verbatim.
func ParseRetry(retry string) (action Action, prose string, ok bool) {
	command := strings.TrimPrefix(retry, "run ")
	if before, after, found := strings.Cut(retry, ", then run "); found {
		prose, command = before, after
	}
	action, err := ParseAction(command)
	return action, prose, err == nil
}

// FindingKind is the closed set of recovery findings.
type FindingKind string

const (
	// FindingSource is a refused installed-source or tooling prerequisite.
	FindingSource FindingKind = "source"
	// FindingOwner carries an owner's retry.
	FindingOwner         FindingKind = "owner"
	FindingConfigInvalid FindingKind = "config-invalid"
	FindingUnreachable   FindingKind = "unreachable"
	FindingFailed        FindingKind = "failed"
	FindingDrift         FindingKind = "drift"
	FindingPending       FindingKind = "pending"
	// File findings come from a yard's guest config files.
	FindingFileChanged    FindingKind = "file-changed"
	FindingFileUnreadable FindingKind = "file-unreadable"
	FindingFileUnknown    FindingKind = "file-unknown"
	FindingFileLeftover   FindingKind = "file-leftover"
	FindingStopped        FindingKind = "stopped"
)

// Finding is one observed cause. Public is printable; Predicate names the
// failed admission predicate when there is one; Retry is an owner's retry.
type Finding struct {
	Kind      FindingKind
	Yard      string
	Predicate string
	Public    string
	Retry     string
}

// Remedies is a finding's candidate actions in order, given the attempted one.
// A finding with none is terminal: it names what the operator must do.
func (finding Finding) Remedies(attempted Action) []Action {
	yard := func(verb Verb) []Action { return []Action{{Verb: verb, Yard: finding.Yard}} }
	switch finding.Kind {
	case FindingSource:
		// Each refused installed-source predicate names the command that can
		// make it hold; only the running tooling itself has none.
		switch finding.Predicate {
		// After that command, the read-only check names why it still fails.
		case "journal-not-complete", "pending-publication", "pending-archive":
			return []Action{{Verb: VerbMigrate}, {Verb: VerbMigrateCheck}}
		case "release-options", "journal-absent", "recovery-history", "source-proof", "proof-transaction", "proof-artifact":
			return []Action{{Verb: VerbUpdate}, {Verb: VerbMigrateCheck}}
		case "store":
			return []Action{{Verb: VerbMigrateCheck}}
		}
	case FindingOwner, FindingPending:
		if action, _, ok := ParseRetry(finding.Retry); ok {
			return []Action{action}
		}
		if finding.Kind == FindingPending && finding.Retry == "" {
			return yard(VerbInit)
		}
	case FindingConfigInvalid:
		return yard(VerbConfigValidate)
	case FindingUnreachable:
		// One step at a time: migrate, then a forced stop, then start.
		switch attempted {
		case Action{Verb: VerbMigrate}:
			return yard(VerbStopForce)
		case Action{Verb: VerbStopForce, Yard: finding.Yard}:
			return yard(VerbStart)
		}
		return []Action{{Verb: VerbMigrate}}
	case FindingFailed, FindingDrift:
		return yard(VerbInit)
	case FindingFileChanged:
		return yard(VerbConfigApply)
	case FindingFileUnreadable, FindingFileUnknown:
		return yard(VerbConfigStatus)
	case FindingStopped:
		return yard(VerbStart)
	}
	return nil
}

// Retryable findings were observed after the attempt, so repeating the
// attempted command is progress: a changed file needs a fresh plan.
func (finding Finding) Retryable() bool { return finding.Kind == FindingFileChanged }

// recoveryRank orders remedies: prerequisites before repairs, repairs before reads.
var recoveryRank = []Verb{VerbMigrate, VerbUpdate, VerbStopForce, VerbStart, VerbInit, VerbConfigApply, VerbIntegrationStatus, VerbConfigStatus, VerbMigrateCheck, VerbUpdateCheck}

func rank(finding Finding, action Action) int {
	switch {
	case finding.Kind == FindingSource:
		return 0
	case finding.Kind == FindingOwner:
		return 1
	case action.Verb == VerbConfigValidate, finding.Kind == FindingFileLeftover:
		// Both need the operator first: no command proceeds until they do.
		return 2
	case action.Verb == VerbNone:
		return 3 + len(recoveryRank) // a terminal finding never hides a remedy
	}
	return 3 + slices.Index(recoveryRank, action.Verb)
}

// NextStep selects one action from all findings: each finding's first remedy
// that is not the attempted command (unless the finding is retryable) and is
// admitted, then the lowest rank, first finding on a tie. Without one it
// returns VerbNone and the finding that explains why; a source or owner
// finding without a readable action is that explanation.
func NextStep(findings []Finding, attempted Action, admitted func(Action) bool) (Action, Finding) {
	best, chosen, bestRank := Action{}, Finding{}, -1
	for _, finding := range findings {
		candidate := Action{}
		for _, remedy := range finding.Remedies(attempted) {
			if (remedy != attempted || finding.Retryable()) && (admitted == nil || admitted(remedy)) {
				candidate = remedy
				break
			}
		}
		if order := rank(finding, candidate); bestRank < 0 || order < bestRank {
			best, chosen, bestRank = candidate, finding, order
		}
	}
	return best, chosen
}

// RenderNext is the one renderer of a selected next step. A source finding
// keeps its named predicate beside the command that repairs it; an owner's
// retry is printed in the owner's words, unless it repeats the attempted
// command; a terminal finding prints what the operator must do.
func RenderNext(action Action, chosen Finding, attempted Action) string {
	rendered := RenderAction(action)
	switch {
	case chosen.Kind == FindingSource && action.Verb != VerbNone:
		return rendered + " (" + chosen.Public + ")"
	case chosen.Kind == FindingSource || chosen.Kind == FindingFileLeftover:
		return chosen.Public
	case chosen.Kind == FindingOwner || chosen.Kind == FindingPending:
		if parsed, _, ok := ParseRetry(chosen.Retry); chosen.Retry != "" && (!ok || parsed == action || action.Verb == VerbNone && parsed != attempted) {
			return chosen.Retry
		}
	case chosen.Kind == FindingConfigInvalid && action.Verb == VerbConfigValidate:
		return "correct the yard's configuration, then " + rendered
	}
	if action.Verb == VerbNone {
		return RenderAction(Action{Verb: VerbMigrateCheck})
	}
	return rendered
}
