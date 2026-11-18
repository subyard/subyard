package domain

import (
	"slices"
	"strings"
)

// Refusals is one single-line error naming every independent problem. Unwrap keeps
// each cause visible to errors.Is and errors.As.
type Refusals struct{ causes []error }

func (refusals *Refusals) Error() string {
	messages := make([]string, len(refusals.causes))
	for index, cause := range refusals.causes {
		messages[index] = cause.Error()
	}
	return strings.Join(messages, "; ")
}

func (refusals *Refusals) Unwrap() []error { return refusals.causes }

// CollectRefusals returns nil for no causes and a lone cause unchanged. Others are
// sorted by text with repeats dropped, so map iteration cannot change the result.
// A finding's owner and input take precedence over its rendered wording.
func CollectRefusals(causes ...error) error {
	causes = slices.DeleteFunc(slices.Clone(causes), func(cause error) bool { return cause == nil })
	slices.SortStableFunc(causes, func(left, right error) int {
		if order := strings.Compare(left.Error(), right.Error()); order != 0 {
			return order
		}
		first, _ := PublicFinding(left)
		second, _ := PublicFinding(right)
		if order := strings.Compare(first.Owner, second.Owner); order != 0 {
			return order
		}
		return strings.Compare(first.Input, second.Input)
	})
	return collectDistinctRefusals(causes)
}

// CollectOrderedRefusals keeps the caller's stable order (such as YardNames:
// default first, then by name), dropping nil causes and exact repeats.
func CollectOrderedRefusals(causes ...error) error {
	return collectDistinctRefusals(slices.Clone(causes))
}

func collectDistinctRefusals(causes []error) error {
	keys := map[[2]string]bool{}
	messages := map[string]bool{}
	kept := causes[:0]
	for _, cause := range causes {
		if cause == nil {
			continue
		}
		if finding, ok := PublicFinding(cause); ok && finding.Input != "" {
			key := finding.Key()
			if keys[key] {
				continue
			}
			keys[key] = true
		} else {
			message := cause.Error()
			if messages[message] {
				continue
			}
			messages[message] = true
		}
		kept = append(kept, cause)
	}
	switch len(kept) {
	case 0:
		return nil
	case 1:
		return kept[0]
	}
	return &Refusals{kept}
}

// CompareYardMessages orders per-yard diagnostics by target order: the default
// yard first, then by text.
func CompareYardMessages(left, right string) int {
	leftDefault, rightDefault := strings.HasPrefix(left, "yard default: "), strings.HasPrefix(right, "yard default: ")
	if leftDefault != rightDefault {
		if leftDefault {
			return -1
		}
		return 1
	}
	return strings.Compare(left, right)
}
