package testimpact

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// Result is a version 1 impact-selection result. Renderers consume this value
// without reclassifying paths or consulting executable registry fields.
type Result struct {
	SchemaVersion  int                   `json:"schema_version"`
	Status         string                `json:"status"`
	Changes        []Change              `json:"changes"`
	CheckSets      []string              `json:"check_sets"`
	RiskDomains    []string              `json:"risk_domains"`
	HostFreeChecks []CheckRecommendation `json:"host_free_checks"`
	E2EChecks      []CheckRecommendation `json:"e2e_checks"`
	FullP0         FullP0Selection       `json:"full_p0"`
	Reasons        []SelectionReason     `json:"reasons"`
	Errors         []ResultError         `json:"errors"`
}

// CheckRecommendation is the non-executable metadata exposed for a selected
// registry leaf.
type CheckRecommendation struct {
	ID            string `json:"id"`
	Tier          string `json:"tier"`
	BudgetSeconds int    `json:"budget_seconds"`
	Rationale     string `json:"rationale"`
}

// FullP0Selection records whether the static diff requires a fresh full P0.
type FullP0Selection struct {
	Required bool           `json:"required"`
	Reasons  []FullP0Reason `json:"reasons"`
}

// FullP0Reason identifies the first applicable full-P0 policy class and the
// production domains that triggered it.
type FullP0Reason struct {
	Code        string   `json:"code"`
	RiskDomains []string `json:"risk_domains"`
}

// SelectionReason records one distinct path-side-rule classification.
type SelectionReason struct {
	Path        string   `json:"path"`
	Side        string   `json:"side"`
	RuleID      string   `json:"rule_id"`
	CheckSets   []string `json:"check_sets"`
	RiskDomains []string `json:"risk_domains"`
}

// ResultError is an analysis error for the outer CLI to convert to a
// universal fallback. Select itself never synthesizes fallback policy.
type ResultError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Select deterministically reduces a validated policy, registry, and
// canonical change set to affected checks, production domains, and static
// full-P0 requirements. It performs no I/O and executes no recommendations.
func Select(policy Policy, registry Registry, changeSet ChangeSet) Result {
	result := Result{
		SchemaVersion:  1,
		Status:         "selected",
		Changes:        cloneCanonicalChanges(changeSet.Changes),
		CheckSets:      []string{},
		RiskDomains:    []string{},
		HostFreeChecks: []CheckRecommendation{},
		E2EChecks:      []CheckRecommendation{},
		FullP0:         FullP0Selection{Reasons: []FullP0Reason{}},
		Reasons:        []SelectionReason{},
		Errors:         []ResultError{},
	}

	checkSets := make(map[string]struct{})
	riskDomains := make(map[string]struct{})
	reasonKeys := make(map[string]struct{})
	errorKeys := make(map[string]struct{})
	for _, change := range result.Changes {
		for _, identity := range classificationIdentities(change) {
			classification, err := policy.ClassifyPath(identity.path)
			if err != nil {
				addResultError(&result, errorKeys, ResultError{
					Code: "INVALID_CHANGE_PATH", Message: fmt.Sprintf("invalid %s path: %s", identity.side, identity.path),
				})
				continue
			}
			if len(classification.RuleIDs) == 0 {
				addResultError(&result, errorKeys, ResultError{
					Code: "UNMATCHED_PATH", Message: "repository path is not covered by impact policy: " + identity.path,
				})
				continue
			}

			for _, checkSet := range classification.CheckSets {
				checkSets[checkSet] = struct{}{}
			}
			for _, domain := range classification.RiskDomains {
				riskDomains[domain] = struct{}{}
			}
			for _, ruleID := range classification.RuleIDs {
				key := identity.path + "\x00" + identity.side + "\x00" + ruleID
				if _, exists := reasonKeys[key]; exists {
					continue
				}
				reasonKeys[key] = struct{}{}
				result.Reasons = append(result.Reasons, SelectionReason{
					Path: identity.path, Side: identity.side, RuleID: ruleID,
					CheckSets:   cloneStrings(classification.CheckSets),
					RiskDomains: cloneStrings(classification.RiskDomains),
				})
			}
		}
	}

	result.CheckSets = sortedSet(checkSets)
	result.RiskDomains = sortedSet(riskDomains)
	slices.SortFunc(result.Reasons, compareSelectionReason)
	slices.SortFunc(result.Errors, compareResultError)

	leaves, err := registry.Expand(result.CheckSets)
	if err != nil {
		addResultError(&result, errorKeys, ResultError{
			Code: "REGISTRY_EXPANSION_FAILED", Message: err.Error(),
		})
		slices.SortFunc(result.Errors, compareResultError)
	} else {
		for _, leaf := range leaves {
			recommendation := CheckRecommendation{
				ID: leaf.ID, Tier: leaf.Tier,
				BudgetSeconds: leaf.BudgetSeconds, Rationale: leaf.Rationale,
			}
			switch leaf.Tier {
			case "T1", "T2":
				result.HostFreeChecks = append(result.HostFreeChecks, recommendation)
			case "T3":
				result.E2EChecks = append(result.E2EChecks, recommendation)
			}
		}
	}

	result.FullP0 = selectFullP0(policy, result.RiskDomains)
	return result
}

type classificationIdentity struct {
	path string
	side string
}

func classificationIdentities(change Change) []classificationIdentity {
	switch change.Status {
	case "A", "M", "T":
		if change.NewPath != nil {
			return []classificationIdentity{{path: *change.NewPath, side: "new"}}
		}
	case "D":
		if change.OldPath != nil {
			return []classificationIdentity{{path: *change.OldPath, side: "old"}}
		}
	case "R", "C":
		identities := make([]classificationIdentity, 0, 2)
		if change.OldPath != nil {
			identities = append(identities, classificationIdentity{path: *change.OldPath, side: "old"})
		}
		if change.NewPath != nil {
			identities = append(identities, classificationIdentity{path: *change.NewPath, side: "new"})
		}
		return identities
	}
	return nil
}

func selectFullP0(policy Policy, domains []string) FullP0Selection {
	selection := FullP0Selection{Reasons: []FullP0Reason{}}
	domainSet := sliceSet(domains)
	for _, domain := range policy.standaloneFullDomains {
		if _, exists := domainSet[domain]; exists {
			selection.Reasons = append(selection.Reasons, FullP0Reason{
				Code: "standalone_full_domain", RiskDomains: []string{domain},
			})
		}
	}
	if len(selection.Reasons) > 0 {
		selection.Required = true
		return selection
	}

	for _, combination := range policy.highRiskCombinations {
		if domainSetContains(domains, combination) {
			selection.Reasons = append(selection.Reasons, FullP0Reason{
				Code: "high_risk_combination", RiskDomains: slices.Clone(combination),
			})
		}
	}
	if len(selection.Reasons) > 0 {
		selection.Required = true
		return selection
	}

	for _, safe := range policy.safeMultiDomainCombinations {
		if slices.Equal(domains, safe.Domains) {
			return selection
		}
	}
	if policy.defaultMultiDomainFull && len(domains) >= 2 {
		selection.Required = true
		selection.Reasons = append(selection.Reasons, FullP0Reason{
			Code: "default_multi_domain", RiskDomains: slices.Clone(domains),
		})
	}
	return selection
}

func cloneCanonicalChanges(changes []Change) []Change {
	result := make([]Change, len(changes))
	for index, change := range changes {
		result[index] = change
		if change.Similarity != nil {
			value := *change.Similarity
			result[index].Similarity = &value
		}
		if change.OldPath != nil {
			value := *change.OldPath
			result[index].OldPath = &value
		}
		if change.NewPath != nil {
			value := *change.NewPath
			result[index].NewPath = &value
		}
	}
	slices.SortFunc(result, compareChange)
	return result
}

func sortedSet(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	slices.Sort(result)
	return result
}

func cloneStrings(values []string) []string {
	result := make([]string, len(values))
	copy(result, values)
	return result
}

func addResultError(result *Result, keys map[string]struct{}, resultError ResultError) {
	key := resultError.Code + "\x00" + resultError.Message
	if _, exists := keys[key]; exists {
		return
	}
	keys[key] = struct{}{}
	result.Errors = append(result.Errors, resultError)
}

func compareSelectionReason(left, right SelectionReason) int {
	if result := strings.Compare(left.Path, right.Path); result != 0 {
		return result
	}
	if result := strings.Compare(left.Side, right.Side); result != 0 {
		return result
	}
	return strings.Compare(left.RuleID, right.RuleID)
}

func compareResultError(left, right ResultError) int {
	if result := strings.Compare(left.Code, right.Code); result != 0 {
		return result
	}
	return cmp.Compare(left.Message, right.Message)
}
