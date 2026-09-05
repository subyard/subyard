// Package v2 defines the frozen durable JSON contract for release transition
// journals written with schema version 2.
package v2

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

const (
	SchemaVersion        = 2
	ArchiveSchemaVersion = 1
	MaxSteps             = 256
	MaxBytes             = 1 << 20
)

var (
	ErrInvalid = errors.New("invalid release transition journal v2")
	semverRE   = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$`)
)

type Goal struct {
	Target    string `json:"target"`
	Direction string `json:"direction"`
}

type ReleasePair struct {
	From     string  `json:"from"`
	Previous *string `json:"previous,omitempty"`
	Target   string  `json:"target"`
}

type SourceIngress struct {
	SchemaVersion int    `json:"schemaVersion"`
	Kind          string `json:"kind"`
	SourceRoot    string `json:"sourceRoot"`
	DataHome      string `json:"dataHome"`
	BinDir        string `json:"binDir"`
	RC            string `json:"rc"`
	LoginRC       string `json:"loginRC"`
}

type Record struct {
	SchemaVersion       int            `json:"schemaVersion"`
	Transaction         string         `json:"transaction"`
	Goal                Goal           `json:"goal"`
	Releases            ReleasePair    `json:"releases"`
	AuthorizationPlan   string         `json:"authorizationPlan"`
	ResumePlan          string         `json:"resumePlan"`
	ArtifactDigest      string         `json:"artifactDigest"`
	RegistryDigest      string         `json:"registryDigest"`
	CatalogDigest       string         `json:"catalogDigest"`
	ObservationScope    string         `json:"observationScope"`
	AuthorizationDigest string         `json:"authorizationDigest"`
	IntentDigest        string         `json:"intentDigest"`
	SourceIngress       *SourceIngress `json:"sourceIngress,omitempty"`
	Checkpoint          string         `json:"checkpoint"`
	Steps               []Step         `json:"steps"`
}

type Step struct {
	ID         string    `json:"id"`
	Migration  string    `json:"migration"`
	Resource   string    `json:"resource"`
	Decision   string    `json:"decision"`
	Expected   string    `json:"expectedFingerprint"`
	Desired    string    `json:"desiredFingerprint"`
	Checkpoint string    `json:"checkpoint"`
	Evidence   *Evidence `json:"evidence,omitempty"`
}

type Evidence struct {
	SchemaVersion int         `json:"schemaVersion"`
	Transaction   string      `json:"transaction"`
	Releases      ReleasePair `json:"releases"`
	Step          string      `json:"step"`
	Expected      string      `json:"expectedFingerprint"`
	Desired       string      `json:"desiredFingerprint"`
	Observed      string      `json:"observedFingerprint"`
	Recovery      string      `json:"recoveryFingerprint,omitempty"`
	Checkpoint    string      `json:"checkpoint"`
}

// Archive is the frozen schema-v1 envelope for immutable superseded journal
// evidence. Journal remains a schema-v2 record on the wire.
type Archive struct {
	SchemaVersion     int         `json:"schemaVersion"`
	AuthorizationPlan string      `json:"authorizationPlan"`
	Replacement       Replacement `json:"replacement"`
	Journal           Record      `json:"journal"`
}

type Replacement struct {
	Transaction   string `json:"transaction"`
	Fingerprint   string `json:"fingerprint"`
	Reason        string `json:"reason"`
	SourceVersion string `json:"sourceVersion,omitempty"`
}

func (record Record) Validate() error {
	if record.SchemaVersion != SchemaVersion {
		return invalid("unsupported schema %d", record.SchemaVersion)
	}
	if err := validateSafeID(record.Transaction, "transaction ID"); err != nil {
		return err
	}
	if err := record.Goal.validate(); err != nil {
		return err
	}
	if err := record.Releases.validate(); err != nil {
		return err
	}
	if record.Goal.Target != record.Releases.Target {
		return invalid("goal does not match release pair")
	}
	if !validToken(record.AuthorizationPlan, "plan-v1-") {
		return invalid("authorization plan is invalid")
	}
	if !validToken(record.ResumePlan, "resume-v1-") {
		return invalid("resume plan is invalid")
	}
	if record.AuthorizationPlan == record.ResumePlan {
		return invalid("authorization and resume plans are not independent")
	}
	for field, value := range map[string]string{
		"artifact digest":      record.ArtifactDigest,
		"registry digest":      record.RegistryDigest,
		"catalog digest":       record.CatalogDigest,
		"observation scope":    record.ObservationScope,
		"authorization digest": record.AuthorizationDigest,
	} {
		if !validFingerprint(value) {
			return invalid("%s is invalid", field)
		}
	}
	if record.SourceIngress != nil {
		if err := record.SourceIngress.validate(); err != nil {
			return err
		}
	}
	if !validJournalCheckpoint(record.Checkpoint) {
		return invalid("unknown journal checkpoint %q", record.Checkpoint)
	}
	if len(record.Steps) > MaxSteps {
		return invalid("too many steps")
	}
	seen := make(map[string]struct{}, len(record.Steps))
	for _, step := range record.Steps {
		if err := step.validate(record, seen); err != nil {
			return err
		}
	}
	if !validFingerprint(record.IntentDigest) {
		return invalid("intent digest is invalid")
	}
	if record.IntentDigest != bindIntent(record) {
		return invalid("intent digest does not match journal intent")
	}
	return nil
}

func (archive Archive) Validate() error {
	if archive.SchemaVersion != ArchiveSchemaVersion {
		return invalid("unsupported archive schema %d", archive.SchemaVersion)
	}
	if !validToken(archive.AuthorizationPlan, "plan-v1-") {
		return invalid("archive authorization plan is invalid")
	}
	if err := archive.Replacement.validate(); err != nil {
		return err
	}
	if err := archive.Journal.Validate(); err != nil {
		return err
	}
	if archive.Replacement.Transaction != archive.Journal.Transaction {
		return invalid("archive transaction does not match replacement")
	}
	journal, err := Encode(archive.Journal)
	if err != nil {
		return err
	}
	journal = append(journal, '\n')
	if archive.Replacement.Fingerprint != fingerprint(journal) {
		return invalid("archive fingerprint does not match canonical journal")
	}
	return nil
}

func (replacement Replacement) validate() error {
	if err := validateSafeID(replacement.Transaction, "replacement transaction ID"); err != nil {
		return err
	}
	if !validFingerprint(replacement.Fingerprint) {
		return invalid("replacement fingerprint is invalid")
	}
	if replacement.Reason != "post-activation-scope-v0.11.1" {
		return invalid("unknown archive replacement reason %q", replacement.Reason)
	}
	if !validCanonicalSemver(replacement.SourceVersion) {
		return invalid("replacement source version is invalid")
	}
	return nil
}

func (goal Goal) validate() error {
	if err := validateSafeID(goal.Target, "target release"); err != nil {
		return err
	}
	if goal.Direction != "activate-target" && goal.Direction != "activate-previous" {
		return invalid("unknown direction %q", goal.Direction)
	}
	return nil
}

func (pair ReleasePair) validate() error {
	if err := validateSafeID(pair.From, "source release"); err != nil {
		return err
	}
	if err := validateSafeID(pair.Target, "target release"); err != nil {
		return err
	}
	if pair.Previous != nil {
		if err := validateSafeID(*pair.Previous, "previous release"); err != nil {
			return err
		}
		if *pair.Previous == pair.From {
			return invalid("previous release equals source release")
		}
	}
	return nil
}

func (source SourceIngress) validate() error {
	if source.SchemaVersion != 1 || source.Kind != "pre-go-source-v1" {
		return invalid("unknown source ingress schema or kind")
	}
	for field, value := range map[string]string{
		"source root": source.SourceRoot,
		"data home":   source.DataHome,
		"bin dir":     source.BinDir,
		"rc":          source.RC,
		"login rc":    source.LoginRC,
	} {
		if len(value) == 0 || len(value) > 4096 || !filepath.IsAbs(value) ||
			value == string(filepath.Separator) || filepath.Clean(value) != value ||
			strings.ContainsFunc(value, unicode.IsControl) {
			return invalid("%s is invalid", field)
		}
	}
	return nil
}

func (step Step) validate(journal Record, seen map[string]struct{}) error {
	if err := validateSafeID(step.ID, "step ID"); err != nil {
		return err
	}
	if _, exists := seen[step.ID]; exists {
		return invalid("duplicate step %q", step.ID)
	}
	seen[step.ID] = struct{}{}
	if err := validateSafeID(step.Migration, "migration ID"); err != nil {
		return err
	}
	if err := validateSafeID(step.Resource, "resource ID"); err != nil {
		return err
	}
	switch step.Decision {
	case "preserve", "transform", "canonicalize", "reset", "quarantine":
	default:
		return invalid("unknown step decision %q", step.Decision)
	}
	if !validFingerprint(step.Expected) || !validFingerprint(step.Desired) {
		return invalid("step fingerprints are invalid")
	}
	if !validStepCheckpoint(step.Checkpoint) {
		return invalid("unknown step checkpoint %q", step.Checkpoint)
	}
	if step.Checkpoint == "intent" {
		if step.Evidence != nil {
			return invalid("intent step has premature evidence")
		}
	} else {
		if step.Evidence == nil {
			return invalid("step is missing evidence")
		}
		if err := step.Evidence.validate(); err != nil {
			return err
		}
		if step.Evidence.Transaction != journal.Transaction ||
			!releasePairsEqual(step.Evidence.Releases, journal.Releases) ||
			step.Evidence.Step != step.ID || step.Evidence.Expected != step.Expected ||
			step.Evidence.Desired != step.Desired {
			return invalid("step evidence does not match journal intent")
		}
		want := map[string]string{
			"evidence": "captured",
			"applied":  "applied",
			"verified": "verified",
		}[step.Checkpoint]
		if step.Evidence.Checkpoint != want {
			return invalid("evidence checkpoint does not match step checkpoint")
		}
	}
	if journalAfterMigrations(journal.Checkpoint) && step.Checkpoint != "verified" {
		return invalid("activation starts before step is verified")
	}
	return nil
}

func (evidence Evidence) validate() error {
	if evidence.SchemaVersion != SchemaVersion {
		return invalid("unsupported evidence schema %d", evidence.SchemaVersion)
	}
	if err := validateSafeID(evidence.Transaction, "evidence transaction ID"); err != nil {
		return err
	}
	if err := evidence.Releases.validate(); err != nil {
		return err
	}
	if err := validateSafeID(evidence.Step, "evidence step ID"); err != nil {
		return err
	}
	if !validFingerprint(evidence.Expected) || !validFingerprint(evidence.Desired) ||
		!validFingerprint(evidence.Observed) {
		return invalid("evidence fingerprints are invalid")
	}
	if evidence.Recovery != "" && !validFingerprint(evidence.Recovery) {
		return invalid("recovery fingerprint is invalid")
	}
	switch evidence.Checkpoint {
	case "captured":
		if evidence.Observed != evidence.Expected {
			return invalid("captured evidence does not match expected state")
		}
	case "applied", "verified":
		if evidence.Observed != evidence.Desired {
			return invalid("post-apply evidence does not match desired state")
		}
	default:
		return invalid("unknown evidence checkpoint %q", evidence.Checkpoint)
	}
	return nil
}

func bindIntent(record Record) string {
	type intentStep struct {
		ID        string `json:"id"`
		Migration string `json:"migration"`
		Resource  string `json:"resource"`
		Decision  string `json:"decision"`
		Expected  string `json:"expected"`
		Desired   string `json:"desired"`
	}
	bound := struct {
		AuthorizationPlan string       `json:"authorizationPlan"`
		ResumePlan        string       `json:"resumePlan"`
		ObservationScope  string       `json:"observationScope"`
		Steps             []intentStep `json:"steps"`
	}{
		AuthorizationPlan: record.AuthorizationPlan,
		ResumePlan:        record.ResumePlan,
		ObservationScope:  record.ObservationScope,
		Steps:             make([]intentStep, len(record.Steps)),
	}
	for index, step := range record.Steps {
		bound.Steps[index] = intentStep{
			ID: step.ID, Migration: step.Migration, Resource: step.Resource,
			Decision: step.Decision, Expected: step.Expected, Desired: step.Desired,
		}
	}
	payload, _ := json.Marshal(bound)
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func validJournalCheckpoint(value string) bool {
	switch value {
	case "authorized", "migrating", "activation-intent", "target-active", "reconciling", "complete":
		return true
	default:
		return false
	}
}

func validStepCheckpoint(value string) bool {
	return value == "intent" || value == "evidence" || value == "applied" || value == "verified"
}

func journalAfterMigrations(value string) bool {
	return value == "activation-intent" || value == "target-active" || value == "reconciling" || value == "complete"
}

func releasePairsEqual(left, right ReleasePair) bool {
	if left.From != right.From || left.Target != right.Target {
		return false
	}
	if left.Previous == nil || right.Previous == nil {
		return left.Previous == nil && right.Previous == nil
	}
	return *left.Previous == *right.Previous
}

func validToken(value, prefix string) bool {
	return strings.HasPrefix(value, prefix) && validFingerprint(strings.TrimPrefix(value, prefix))
}

func validCanonicalSemver(value string) bool {
	matches := semverRE.FindStringSubmatch(value)
	if matches == nil {
		return false
	}
	for _, component := range matches[1:4] {
		if _, err := strconv.ParseUint(component, 10, 64); err != nil {
			return false
		}
	}
	if matches[4] != "" {
		for _, identifier := range strings.Split(matches[4], ".") {
			numeric := strings.IndexFunc(identifier, func(char rune) bool {
				return char < '0' || char > '9'
			}) == -1
			if numeric && len(identifier) > 1 && identifier[0] == '0' {
				return false
			}
			if numeric {
				if _, err := strconv.ParseUint(identifier, 10, 64); err != nil {
					return false
				}
			}
		}
	}
	return true
}

func fingerprint(payload []byte) string {
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func validFingerprint(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}

func validateSafeID(value, field string) error {
	if len(value) == 0 || len(value) > 128 || value == "." || value == ".." || strings.HasPrefix(value, "-") {
		return invalid("%s is invalid", field)
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z') && !(char >= 'A' && char <= 'Z') &&
			!(char >= '0' && char <= '9') && char != '.' && char != '_' && char != '-' {
			return invalid("%s is invalid", field)
		}
	}
	return nil
}

func invalid(format string, arguments ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, arguments...))
}
