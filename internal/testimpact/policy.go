package testimpact

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"regexp/syntax"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

// Policy is a canonical, validated version 1 impact policy.
type Policy struct {
	schemaVersion               int
	checkSets                   map[string][]string
	riskDomains                 []string
	rules                       []Rule
	riskSuppressions            []riskSuppression
	standaloneFullDomains       []string
	highRiskCombinations        [][]string
	defaultMultiDomainFull      bool
	safeMultiDomainCombinations []SafeCombination
}

// riskSuppression marks non-production paths whose owning rule still selects
// checks but whose risk domains must not affect the full-P0 decision.
type riskSuppression struct {
	ID   string
	Kind string
}

// Rule classifies repository-relative paths. Higher Priority wins overlaps.
type Rule struct {
	ID          string
	Priority    int
	Pattern     string
	CheckSets   []string
	RiskDomains []string

	compiled       *regexp.Regexp
	resolvedChecks []string
}

// SafeCombination exempts one exact multi-domain set from the default full
// matrix rule and records the contract that makes the exception safe.
type SafeCombination struct {
	Domains   []string
	Contract  string
	Rationale string
}

// Classification is the one effective classification for a path.
type Classification struct {
	RuleIDs     []string
	CheckSets   []string
	RiskDomains []string
}

type policyWire struct {
	SchemaVersion               *int                  `json:"schema_version"`
	CheckSets                   map[string][]string   `json:"check_sets"`
	RiskDomains                 []string              `json:"risk_domains"`
	Rules                       []policyRuleWire      `json:"rules"`
	RiskSuppressions            []riskSuppressionWire `json:"risk_suppressions"`
	StandaloneFullDomains       []string              `json:"standalone_full_domains"`
	HighRiskCombinations        [][]string            `json:"high_risk_combinations"`
	DefaultMultiDomainFull      *bool                 `json:"default_multi_domain_full"`
	SafeMultiDomainCombinations []safeCombinationWire `json:"safe_multi_domain_combinations"`
}

type riskSuppressionWire struct {
	ID   *string `json:"id"`
	Kind *string `json:"kind"`
}

type policyRuleWire struct {
	ID          *string  `json:"id"`
	Priority    *int     `json:"priority"`
	Pattern     *string  `json:"pattern"`
	CheckSets   []string `json:"check_sets"`
	RiskDomains []string `json:"risk_domains"`
}

type safeCombinationWire struct {
	Domains   []string `json:"domains"`
	Contract  *string  `json:"contract"`
	Rationale *string  `json:"rationale"`
}

// LoadPolicy decodes and validates a declarative policy. It performs no
// subprocess execution; inventory checks belong only in tests.
func LoadPolicy(reader io.Reader, registry Registry) (Policy, error) {
	if reader == nil {
		return Policy{}, errors.New("policy reader is nil")
	}
	source, err := io.ReadAll(reader)
	if err != nil {
		return Policy{}, fmt.Errorf("read policy: %w", err)
	}
	if !utf8.Valid(source) {
		return Policy{}, errors.New("policy contains invalid UTF-8")
	}
	if bytes.IndexByte(source, 0) >= 0 {
		return Policy{}, errors.New("policy contains NUL")
	}
	if err := rejectUnpairedUnicodeSurrogateEscapes(source); err != nil {
		return Policy{}, fmt.Errorf("policy contains %w", err)
	}
	if err := rejectPolicyDuplicateMembers(source); err != nil {
		return Policy{}, err
	}

	var wire policyWire
	if err := decodeStrict(source, &wire); err != nil {
		return Policy{}, fmt.Errorf("decode policy: %w", err)
	}
	if wire.SchemaVersion == nil || *wire.SchemaVersion != 1 {
		return Policy{}, errors.New("policy schema_version must be 1")
	}
	if wire.CheckSets == nil || len(wire.CheckSets) == 0 {
		return Policy{}, errors.New("policy check_sets must be a non-empty object")
	}
	if wire.RiskDomains == nil || len(wire.RiskDomains) == 0 {
		return Policy{}, errors.New("policy risk_domains must be a non-empty array")
	}
	if wire.Rules == nil || len(wire.Rules) == 0 {
		return Policy{}, errors.New("policy rules must be a non-empty array")
	}
	if wire.RiskSuppressions == nil || wire.StandaloneFullDomains == nil || wire.HighRiskCombinations == nil ||
		wire.SafeMultiDomainCombinations == nil || wire.DefaultMultiDomainFull == nil {
		return Policy{}, errors.New("policy is missing a required field")
	}
	if !*wire.DefaultMultiDomainFull {
		return Policy{}, errors.New("default_multi_domain_full must be true")
	}

	policy := Policy{
		schemaVersion:          1,
		checkSets:              make(map[string][]string, len(wire.CheckSets)),
		defaultMultiDomainFull: true,
	}
	for name, checkIDs := range wire.CheckSets {
		if strings.TrimSpace(name) == "" || strings.TrimSpace(name) != name || strings.ContainsAny(name, "\x00\r\n\t ") {
			return Policy{}, fmt.Errorf("invalid check set name %q", name)
		}
		if len(checkIDs) == 0 || !uniqueStrings(checkIDs) {
			return Policy{}, fmt.Errorf("check set %q must contain unique check IDs", name)
		}
		for _, checkID := range checkIDs {
			if _, exists := registry.Check(checkID); !exists {
				return Policy{}, fmt.Errorf("check set %q contains unknown check ID %q", name, checkID)
			}
		}
		policy.checkSets[name] = sortedClone(checkIDs)
	}

	if !uniqueStrings(wire.RiskDomains) {
		return Policy{}, errors.New("risk_domains must contain unique non-empty names")
	}
	policy.riskDomains = sortedClone(wire.RiskDomains)
	domains := make(map[string]struct{}, len(policy.riskDomains))
	for _, domain := range policy.riskDomains {
		if strings.TrimSpace(domain) != domain || strings.ContainsAny(domain, "\x00\r\n\t ") {
			return Policy{}, fmt.Errorf("invalid risk domain %q", domain)
		}
		domains[domain] = struct{}{}
	}

	ruleIDs := make(map[string]struct{}, len(wire.Rules))
	for index, raw := range wire.Rules {
		if raw.ID == nil || raw.Priority == nil || raw.Pattern == nil || raw.CheckSets == nil || raw.RiskDomains == nil {
			return Policy{}, fmt.Errorf("rules[%d] is missing a required field", index)
		}
		if strings.TrimSpace(*raw.ID) == "" || strings.TrimSpace(*raw.ID) != *raw.ID || strings.ContainsAny(*raw.ID, "\x00\r\n\t ") {
			return Policy{}, fmt.Errorf("rules[%d] has invalid ID", index)
		}
		if _, exists := ruleIDs[*raw.ID]; exists {
			return Policy{}, fmt.Errorf("duplicate rule ID %q", *raw.ID)
		}
		ruleIDs[*raw.ID] = struct{}{}
		if !uniqueStringsAllowEmpty(raw.CheckSets) || !uniqueStringsAllowEmpty(raw.RiskDomains) {
			return Policy{}, fmt.Errorf("rule %q contains duplicate references", *raw.ID)
		}
		for _, checkSet := range raw.CheckSets {
			if _, exists := policy.checkSets[checkSet]; !exists {
				return Policy{}, fmt.Errorf("rule %q refers to unknown check set %q", *raw.ID, checkSet)
			}
		}
		if err := validateDomainReferences(raw.RiskDomains, domains, "rule "+*raw.ID); err != nil {
			return Policy{}, err
		}
		compiled, err := compileAnchoredPattern(*raw.Pattern)
		if err != nil {
			return Policy{}, fmt.Errorf("rule %q: %w", *raw.ID, err)
		}
		checkSetNames := sortedClone(raw.CheckSets)
		resolvedChecks := make([]string, 0)
		for _, name := range checkSetNames {
			resolvedChecks = append(resolvedChecks, policy.checkSets[name]...)
		}
		resolvedChecks = uniqueSorted(resolvedChecks)
		policy.rules = append(policy.rules, Rule{
			ID:             *raw.ID,
			Priority:       *raw.Priority,
			Pattern:        *raw.Pattern,
			CheckSets:      checkSetNames,
			RiskDomains:    sortedClone(raw.RiskDomains),
			compiled:       compiled,
			resolvedChecks: resolvedChecks,
		})
	}
	slices.SortFunc(policy.rules, func(left, right Rule) int {
		if result := cmp.Compare(right.Priority, left.Priority); result != 0 {
			return result
		}
		return strings.Compare(left.ID, right.ID)
	})
	if err := validateRuleAmbiguity(policy.rules); err != nil {
		return Policy{}, err
	}

	suppressionIDs := make(map[string]struct{}, len(wire.RiskSuppressions))
	suppressionKinds := make(map[string]struct{}, len(wire.RiskSuppressions))
	for index, raw := range wire.RiskSuppressions {
		if raw.ID == nil || raw.Kind == nil {
			return Policy{}, fmt.Errorf("risk_suppressions[%d] is missing a required field", index)
		}
		if strings.TrimSpace(*raw.ID) == "" || strings.TrimSpace(*raw.ID) != *raw.ID || strings.ContainsAny(*raw.ID, "\x00\r\n\t ") {
			return Policy{}, fmt.Errorf("risk_suppressions[%d] has invalid ID", index)
		}
		if _, exists := suppressionIDs[*raw.ID]; exists {
			return Policy{}, fmt.Errorf("duplicate risk suppression ID %q", *raw.ID)
		}
		suppressionIDs[*raw.ID] = struct{}{}
		if *raw.Kind != "go-test-file" {
			return Policy{}, fmt.Errorf("risk suppression %q has unknown kind %q", *raw.ID, *raw.Kind)
		}
		if _, exists := suppressionKinds[*raw.Kind]; exists {
			return Policy{}, fmt.Errorf("duplicate risk suppression kind %q", *raw.Kind)
		}
		suppressionKinds[*raw.Kind] = struct{}{}
		policy.riskSuppressions = append(policy.riskSuppressions, riskSuppression{
			ID: *raw.ID, Kind: *raw.Kind,
		})
	}
	slices.SortFunc(policy.riskSuppressions, func(left, right riskSuppression) int {
		return strings.Compare(left.ID, right.ID)
	})

	if !uniqueStringsAllowEmpty(wire.StandaloneFullDomains) {
		return Policy{}, errors.New("standalone_full_domains contains duplicates")
	}
	if err := validateDomainReferences(wire.StandaloneFullDomains, domains, "standalone_full_domains"); err != nil {
		return Policy{}, err
	}
	policy.standaloneFullDomains = sortedClone(wire.StandaloneFullDomains)
	standalone := sliceSet(policy.standaloneFullDomains)

	highKeys := make(map[string]struct{}, len(wire.HighRiskCombinations))
	for index, combination := range wire.HighRiskCombinations {
		canonical, key, err := validateCombination(combination, domains)
		if err != nil {
			return Policy{}, fmt.Errorf("high_risk_combinations[%d]: %w", index, err)
		}
		if _, exists := highKeys[key]; exists {
			return Policy{}, fmt.Errorf("duplicate high-risk exact set %v", canonical)
		}
		highKeys[key] = struct{}{}
		policy.highRiskCombinations = append(policy.highRiskCombinations, canonical)
	}
	sort.Slice(policy.highRiskCombinations, func(i, j int) bool {
		return combinationKey(policy.highRiskCombinations[i]) < combinationKey(policy.highRiskCombinations[j])
	})

	safeKeys := make(map[string]struct{}, len(wire.SafeMultiDomainCombinations))
	for index, raw := range wire.SafeMultiDomainCombinations {
		if raw.Domains == nil || raw.Contract == nil || raw.Rationale == nil {
			return Policy{}, fmt.Errorf("safe_multi_domain_combinations[%d] is missing a required field", index)
		}
		if strings.TrimSpace(*raw.Contract) == "" || strings.TrimSpace(*raw.Rationale) == "" {
			return Policy{}, fmt.Errorf("safe_multi_domain_combinations[%d] needs contract and rationale", index)
		}
		canonical, key, err := validateCombination(raw.Domains, domains)
		if err != nil {
			return Policy{}, fmt.Errorf("safe_multi_domain_combinations[%d]: %w", index, err)
		}
		if _, exists := safeKeys[key]; exists {
			return Policy{}, fmt.Errorf("duplicate safe exact set %v", canonical)
		}
		for _, highRisk := range policy.highRiskCombinations {
			if domainSetContains(canonical, highRisk) {
				return Policy{}, fmt.Errorf("safe exact set %v contains high-risk combination %v", canonical, highRisk)
			}
		}
		for _, domain := range canonical {
			if _, exists := standalone[domain]; exists {
				return Policy{}, fmt.Errorf("safe exact set %v contains standalone-full domain %q", canonical, domain)
			}
		}
		safeKeys[key] = struct{}{}
		policy.safeMultiDomainCombinations = append(policy.safeMultiDomainCombinations, SafeCombination{
			Domains: canonical, Contract: *raw.Contract, Rationale: *raw.Rationale,
		})
	}
	slices.SortFunc(policy.safeMultiDomainCombinations, func(left, right SafeCombination) int {
		return strings.Compare(combinationKey(left.Domains), combinationKey(right.Domains))
	})
	return policy, nil
}

// ClassifyPath returns the unique highest-priority classification for path.
// Selector-owned paths always receive compiled self-checks in addition to map
// policy, so policy JSON cannot suppress bootstrap validation.
func (policy Policy) ClassifyPath(path string) (Classification, error) {
	if err := validatePath(path); err != nil {
		return Classification{}, err
	}
	var matches []Rule
	for _, rule := range policy.rules {
		if rule.compiled.MatchString(path) {
			if len(matches) == 0 || rule.Priority > matches[0].Priority {
				matches = []Rule{rule}
			} else if rule.Priority == matches[0].Priority {
				matches = append(matches, rule)
			}
		}
	}

	classification := Classification{}
	for _, rule := range matches {
		classification.RuleIDs = append(classification.RuleIDs, rule.ID)
		classification.CheckSets = append(classification.CheckSets, rule.resolvedChecks...)
		classification.RiskDomains = append(classification.RiskDomains, rule.RiskDomains...)
	}
	for _, suppression := range policy.riskSuppressions {
		if suppression.matches(path) {
			classification.RiskDomains = nil
			break
		}
	}
	if selectorOwnedPath(path) {
		classification.CheckSets = append(classification.CheckSets,
			"go:test-impact-cli", "go:testimpact", "shell:test-impact")
	}
	classification.RuleIDs = uniqueSorted(classification.RuleIDs)
	classification.CheckSets = uniqueSorted(classification.CheckSets)
	classification.RiskDomains = uniqueSorted(classification.RiskDomains)
	return classification, nil
}

func selectorOwnedPath(path string) bool {
	return strings.HasPrefix(path, "internal/testimpact/") ||
		strings.HasPrefix(path, "cmd/test-impact/") ||
		path == "dev/test-impact.sh" || path == "tests/impact-map.json" ||
		path == "tests/test-impact.sh"
}

func validateRuleAmbiguity(rules []Rule) error {
	for left := range rules {
		for right := left + 1; right < len(rules); right++ {
			if rules[left].Priority != rules[right].Priority || sameClassification(rules[left], rules[right]) {
				continue
			}
			overlaps, err := anchoredPatternsOverlap(rules[left].Pattern, rules[right].Pattern)
			if err != nil {
				return fmt.Errorf("compare rules %q and %q: %w", rules[left].ID, rules[right].ID, err)
			}
			if overlaps {
				return fmt.Errorf("equal-priority rules %q and %q overlap with different classifications", rules[left].ID, rules[right].ID)
			}
		}
	}
	return nil
}

func (suppression riskSuppression) matches(path string) bool {
	switch suppression.Kind {
	case "go-test-file":
		return strings.HasPrefix(path, "internal/") && strings.HasSuffix(path, "_test.go")
	default:
		return false
	}
}

func sameClassification(left, right Rule) bool {
	return slices.Equal(left.resolvedChecks, right.resolvedChecks) && slices.Equal(left.RiskDomains, right.RiskDomains)
}

func compileAnchoredPattern(pattern string) (*regexp.Regexp, error) {
	if _, err := parseAnchoredBody(pattern); err != nil {
		return nil, err
	}
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid RE2 pattern: %w", err)
	}
	return compiled, nil
}

func anchoredPatternsOverlap(left, right string) (bool, error) {
	leftProgram, err := compilePatternBody(left)
	if err != nil {
		return false, err
	}
	rightProgram, err := compilePatternBody(right)
	if err != nil {
		return false, err
	}
	return programsOverlap(leftProgram, rightProgram)
}

func compilePatternBody(pattern string) (*syntax.Prog, error) {
	expression, err := parseAnchoredBody(pattern)
	if err != nil {
		return nil, err
	}
	return syntax.Compile(expression.Simplify())
}

func parseAnchoredBody(pattern string) (*syntax.Regexp, error) {
	expression, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return nil, fmt.Errorf("invalid RE2 pattern: %w", err)
	}
	body, anchored := stripAnchoredExpression(expression)
	if !anchored {
		return nil, errors.New("pattern must anchor every alternative to the whole path")
	}
	return body, nil
}

func stripAnchoredExpression(expression *syntax.Regexp) (*syntax.Regexp, bool) {
	switch expression.Op {
	case syntax.OpCapture:
		return stripAnchoredExpression(expression.Sub[0])
	case syntax.OpAlternate:
		alternatives := make([]*syntax.Regexp, 0, len(expression.Sub))
		for _, alternative := range expression.Sub {
			body, anchored := stripAnchoredExpression(alternative)
			if !anchored {
				return nil, false
			}
			alternatives = append(alternatives, body)
		}
		return &syntax.Regexp{Op: syntax.OpAlternate, Sub: alternatives}, true
	case syntax.OpConcat:
		if len(expression.Sub) < 2 || expression.Sub[0].Op != syntax.OpBeginText ||
			expression.Sub[len(expression.Sub)-1].Op != syntax.OpEndText {
			return nil, false
		}
		body := expression.Sub[1 : len(expression.Sub)-1]
		switch len(body) {
		case 0:
			return &syntax.Regexp{Op: syntax.OpEmptyMatch}, true
		case 1:
			return body[0], true
		default:
			return &syntax.Regexp{Op: syntax.OpConcat, Sub: body}, true
		}
	default:
		return nil, false
	}
}

type programPair struct {
	left  string
	right string
}

func programsOverlap(left, right *syntax.Prog) (bool, error) {
	leftStart, err := epsilonClosure(left, []uint32{uint32(left.Start)})
	if err != nil {
		return false, err
	}
	rightStart, err := epsilonClosure(right, []uint32{uint32(right.Start)})
	if err != nil {
		return false, err
	}
	queue := [][2][]uint32{{leftStart, rightStart}}
	seen := make(map[programPair]struct{})
	for len(queue) > 0 {
		state := queue[0]
		queue = queue[1:]
		key := programPair{left: programStateKey(state[0]), right: programStateKey(state[1])}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		if closureMatches(left, state[0]) && closureMatches(right, state[1]) {
			return true, nil
		}
		for _, leftPC := range state[0] {
			leftInstruction := &left.Inst[leftPC]
			if !runeInstruction(leftInstruction.Op) {
				continue
			}
			for _, rightPC := range state[1] {
				rightInstruction := &right.Inst[rightPC]
				if !runeInstruction(rightInstruction.Op) || !runeInstructionsOverlap(leftInstruction, rightInstruction) {
					continue
				}
				leftNext, err := epsilonClosure(left, []uint32{leftInstruction.Out})
				if err != nil {
					return false, err
				}
				rightNext, err := epsilonClosure(right, []uint32{rightInstruction.Out})
				if err != nil {
					return false, err
				}
				queue = append(queue, [2][]uint32{leftNext, rightNext})
			}
		}
	}
	return false, nil
}

func epsilonClosure(program *syntax.Prog, starts []uint32) ([]uint32, error) {
	seen := make(map[uint32]struct{}, len(starts))
	queue := slices.Clone(starts)
	result := make([]uint32, 0, len(starts))
	for len(queue) > 0 {
		pc := queue[0]
		queue = queue[1:]
		if _, exists := seen[pc]; exists {
			continue
		}
		seen[pc] = struct{}{}
		instruction := &program.Inst[pc]
		switch instruction.Op {
		case syntax.InstAlt, syntax.InstAltMatch:
			queue = append(queue, instruction.Out, instruction.Arg)
		case syntax.InstCapture, syntax.InstNop:
			queue = append(queue, instruction.Out)
		case syntax.InstEmptyWidth:
			return nil, errors.New("zero-width assertions are not supported inside anchored policy patterns")
		default:
			result = append(result, pc)
		}
	}
	slices.Sort(result)
	return result, nil
}

func closureMatches(program *syntax.Prog, closure []uint32) bool {
	return slices.ContainsFunc(closure, func(pc uint32) bool { return program.Inst[pc].Op == syntax.InstMatch })
}

func runeInstruction(operation syntax.InstOp) bool {
	switch operation {
	case syntax.InstRune, syntax.InstRune1, syntax.InstRuneAny, syntax.InstRuneAnyNotNL:
		return true
	default:
		return false
	}
}

func runeInstructionsOverlap(left, right *syntax.Inst) bool {
	candidates := []rune{0, '\n', '/', '-', '.', '0', '9', 'A', 'Z', '_', 'a', 'z', utf8.MaxRune}
	for _, instruction := range []*syntax.Inst{left, right} {
		for _, value := range instruction.Rune {
			candidates = append(candidates, value)
			if value > 0 {
				candidates = append(candidates, value-1)
			}
			if value < utf8.MaxRune {
				candidates = append(candidates, value+1)
			}
		}
	}
	for _, candidate := range candidates {
		if left.MatchRune(candidate) && right.MatchRune(candidate) {
			return true
		}
	}
	return false
}

func programStateKey(state []uint32) string {
	var builder strings.Builder
	for _, pc := range state {
		fmt.Fprintf(&builder, "%d,", pc)
	}
	return builder.String()
}

func validateDomainReferences(references []string, domains map[string]struct{}, owner string) error {
	for _, domain := range references {
		if _, exists := domains[domain]; !exists {
			return fmt.Errorf("%s refers to unknown risk domain %q", owner, domain)
		}
	}
	return nil
}

func validateCombination(combination []string, domains map[string]struct{}) ([]string, string, error) {
	if len(combination) < 2 || !uniqueStrings(combination) {
		return nil, "", errors.New("exact domain set must contain at least two unique domains")
	}
	if err := validateDomainReferences(combination, domains, "exact domain set"); err != nil {
		return nil, "", err
	}
	canonical := sortedClone(combination)
	return canonical, combinationKey(canonical), nil
}

func domainSetContains(container, subset []string) bool {
	for _, domain := range subset {
		if !slices.Contains(container, domain) {
			return false
		}
	}
	return true
}

func combinationKey(domains []string) string {
	return strings.Join(domains, "\x00")
}

func sortedClone(values []string) []string {
	result := slices.Clone(values)
	sort.Strings(result)
	return result
}

func uniqueSorted(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	result := sortedClone(values)
	return slices.Compact(result)
}

func sliceSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func uniqueStringsAllowEmpty(values []string) bool {
	if len(values) == 0 {
		return true
	}
	return uniqueStrings(values)
}

func rejectPolicyDuplicateMembers(source []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.UseNumber()
	if err := scanPolicyJSONValue(decoder); err != nil {
		return fmt.Errorf("scan policy JSON: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("policy has trailing JSON value")
		}
		return fmt.Errorf("scan policy JSON: %w", err)
	}
	return nil
}

func scanPolicyJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		members := make(map[string]struct{})
		for decoder.More() {
			token, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := token.(string)
			if !ok {
				return errors.New("object member name is not a string")
			}
			if _, exists := members[name]; exists {
				return fmt.Errorf("duplicate object member %q", name)
			}
			members[name] = struct{}{}
			if err := scanPolicyJSONValue(decoder); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := scanPolicyJSONValue(decoder); err != nil {
				return err
			}
		}
	}
	_, err = decoder.Token()
	return err
}
