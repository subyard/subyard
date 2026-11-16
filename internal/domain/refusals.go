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
func CollectRefusals(causes ...error) error {
	causes = slices.DeleteFunc(slices.Clone(causes), func(cause error) bool { return cause == nil })
	slices.SortStableFunc(causes, func(left, right error) int { return strings.Compare(left.Error(), right.Error()) })
	causes = slices.CompactFunc(causes, func(left, right error) bool { return left.Error() == right.Error() })
	switch len(causes) {
	case 0:
		return nil
	case 1:
		return causes[0]
	}
	return &Refusals{causes}
}

// CollectOrderedRefusals keeps the caller's stable order (such as YardNames:
// default first, then by name), dropping nil causes and exact repeats.
func CollectOrderedRefusals(causes ...error) error {
	kept := []error{}
	for _, cause := range causes {
		if cause != nil && !slices.ContainsFunc(kept, func(seen error) bool { return seen.Error() == cause.Error() }) {
			kept = append(kept, cause)
		}
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
