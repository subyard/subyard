package resource

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Subyard/Subyard/internal/domain"
)

const (
	ResourcePlanInvalid   = "resource_plan_invalid"
	ResourceActionUnknown = "resource_action_unknown"

	PrepareAssessmentSchema = "yard.resource-action-assessment.v1"
	MaxPrepareOutputBytes   = 64 << 10
	maxConsequences         = 64
	maxConsequenceBytes     = 512
)

var (
	ErrResourcePlanInvalid   = errors.New(ResourcePlanInvalid)
	ErrResourceActionUnknown = errors.New(ResourceActionUnknown)

	credentialAssignment = regexp.MustCompile(`(?i)(?:^|[^a-z0-9_-])(?:password|passphrase|secret|token|api[_-]?key|access[_-]?token|private[_-]?key)\s*[:=]\s*[^[:space:]]+`)
	jsonWebToken         = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{6,}\b`)
)

type prepareAssessmentDocument struct {
	Schema       string    `json:"schema"`
	Action       string    `json:"action"`
	Changed      *bool     `json:"changed"`
	Consequences *[]string `json:"consequences"`
}

func ResourceErrorClass(err error) string {
	switch {
	case errors.Is(err, ErrResourcePlanInvalid):
		return ResourcePlanInvalid
	case errors.Is(err, ErrResourceActionUnknown):
		return ResourceActionUnknown
	default:
		return ""
	}
}

// AssessPrepareResult validates a resource handler's bounded dynamic result and
// combines it with the immutable action metadata held by the owner registry.
func (registry Registry) AssessPrepareResult(
	actions *domain.ActionRegistry,
	resource string,
	verb string,
	output []byte,
) (domain.ActionAssessment, error) {
	if len(output) == 0 || len(output) > MaxPrepareOutputBytes || !utf8.Valid(output) {
		return domain.ActionAssessment{}, resourcePlanInvalid("prepare output has an invalid size or encoding")
	}
	document, err := decodePrepareAssessment(output)
	if err != nil {
		return domain.ActionAssessment{}, err
	}
	if document.Schema != PrepareAssessmentSchema {
		return domain.ActionAssessment{}, resourcePlanInvalid("unsupported prepare assessment schema")
	}
	if document.Action == "" || document.Changed == nil || document.Consequences == nil {
		return domain.ActionAssessment{}, resourcePlanInvalid("prepare assessment is incomplete")
	}
	if len(*document.Consequences) > maxConsequences {
		return domain.ActionAssessment{}, resourcePlanInvalid("prepare assessment has too many consequences")
	}
	for _, consequence := range *document.Consequences {
		if err := validateConsequence(consequence); err != nil {
			return domain.ActionAssessment{}, err
		}
	}
	qualified, ok := registry.LookupAction(resource, verb, document.Action)
	if !ok {
		return domain.ActionAssessment{}, resourceActionUnknown(
			"resource %q does not declare action %q for verb %q", resource, document.Action, verb,
		)
	}
	if actions == nil {
		return domain.ActionAssessment{}, resourcePlanInvalid("owner action registry is required")
	}
	assessment, err := actions.Assess(qualified, domain.ActionDelta{
		Changed: *document.Changed, Consequences: *document.Consequences,
	})
	if err != nil {
		return domain.ActionAssessment{}, resourcePlanInvalid("invalid prepare assessment delta: %v", err)
	}
	return assessment, nil
}

func decodePrepareAssessment(output []byte) (prepareAssessmentDocument, error) {
	if err := rejectDuplicateFields(output); err != nil {
		return prepareAssessmentDocument{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.DisallowUnknownFields()
	var document prepareAssessmentDocument
	if err := decoder.Decode(&document); err != nil {
		return prepareAssessmentDocument{}, resourcePlanInvalid("decode prepare assessment: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return prepareAssessmentDocument{}, resourcePlanInvalid("prepare output contains multiple documents")
		}
		return prepareAssessmentDocument{}, resourcePlanInvalid("prepare output has trailing data: %v", err)
	}
	return document, nil
}

func rejectDuplicateFields(output []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(output))
	token, err := decoder.Token()
	if err != nil {
		return resourcePlanInvalid("decode prepare assessment: %v", err)
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return resourcePlanInvalid("prepare assessment must be a JSON object")
	}
	fields := make(map[string]struct{})
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return resourcePlanInvalid("decode prepare assessment field: %v", err)
		}
		name, ok := token.(string)
		if !ok {
			return resourcePlanInvalid("prepare assessment has an invalid field name")
		}
		foldedName := strings.ToLower(name)
		if _, duplicate := fields[foldedName]; duplicate {
			return resourcePlanInvalid("prepare assessment has duplicate field %q", name)
		}
		fields[foldedName] = struct{}{}
		switch name {
		case "schema", "action", "changed", "consequences":
		default:
			return resourcePlanInvalid("prepare assessment has unknown field %q", name)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return resourcePlanInvalid("decode prepare assessment field %q: %v", name, err)
		}
	}
	if _, err := decoder.Token(); err != nil {
		return resourcePlanInvalid("decode prepare assessment: %v", err)
	}
	return nil
}

func validateConsequence(consequence string) error {
	if consequence == "" || strings.TrimSpace(consequence) != consequence ||
		len(consequence) > maxConsequenceBytes || strings.ContainsFunc(consequence, unicode.IsControl) {
		return resourcePlanInvalid("prepare assessment contains invalid consequence text")
	}
	if containsProtectedMaterial(consequence) {
		return resourcePlanInvalid("prepare assessment contains protected material")
	}
	return nil
}

func containsProtectedMaterial(value string) bool {
	lower := strings.ToLower(value)
	if strings.Contains(lower, "-----begin ") && strings.Contains(lower, "private key-----") {
		return true
	}
	for _, prefix := range []string{"ghp_", "github_pat_", "sk-proj-", "sk-live-", "xoxb-", "xoxp-", "xoxa-", "xoxr-"} {
		if strings.Contains(lower, prefix) {
			return true
		}
	}
	if credentialAssignment.MatchString(value) || jsonWebToken.MatchString(value) {
		return true
	}
	if containsOpaqueToken(value) {
		return true
	}
	for _, protectedPath := range []string{
		"/.ssh/", "/run/secrets/", "/var/run/secrets/", "/srv/env-secrets/", "/private/", "/generated/",
	} {
		if strings.Contains(lower, protectedPath) {
			return true
		}
	}
	return strings.Contains(lower, "/id_rsa") || strings.Contains(lower, "/id_ed25519")
}

func containsOpaqueToken(value string) bool {
	fields := strings.Fields(value)
	for index, field := range fields {
		candidate := strings.Trim(field, `"'()[]{}<>,.;:`)
		if payload, labeled := digestPayload(fields, index, candidate); labeled {
			if validHexDigest(payload) {
				continue
			}
			candidate = payload
		}
		if len(candidate) < 32 {
			continue
		}
		letters, digits := false, false
		valid := true
		for _, character := range candidate {
			switch {
			case character >= 'a' && character <= 'z', character >= 'A' && character <= 'Z':
				letters = true
			case character >= '0' && character <= '9':
				digits = true
			case strings.ContainsRune("_+/=-", character):
			default:
				valid = false
			}
		}
		if valid && letters && digits {
			return true
		}
	}
	return false
}

func digestPayload(fields []string, index int, candidate string) (string, bool) {
	lower := strings.ToLower(candidate)
	for _, prefix := range []string{
		"sha256:", "sha256=", "fingerprint:", "fingerprint=",
		"checksum:", "checksum=", "digest:", "digest=",
	} {
		if strings.HasPrefix(lower, prefix) {
			return candidate[len(prefix):], true
		}
	}
	for _, neighbor := range []int{index - 1, index + 1} {
		if neighbor < 0 || neighbor >= len(fields) {
			continue
		}
		label := strings.Trim(strings.ToLower(fields[neighbor]), `"'()[]{}<>,.;:=`)
		switch label {
		case "sha256", "fingerprint", "checksum", "digest":
			return candidate, true
		}
	}
	return candidate, false
}

func validHexDigest(value string) bool {
	if len(value) < 32 || len(value) > 128 || len(value)%2 != 0 {
		return false
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'f') &&
			!(character >= 'A' && character <= 'F') &&
			!(character >= '0' && character <= '9') {
			return false
		}
	}
	return true
}

func resourcePlanInvalid(format string, arguments ...any) error {
	return fmt.Errorf("%w: %s", ErrResourcePlanInvalid, fmt.Sprintf(format, arguments...))
}

func resourceActionUnknown(format string, arguments ...any) error {
	return fmt.Errorf("%w: %s", ErrResourceActionUnknown, fmt.Sprintf(format, arguments...))
}
