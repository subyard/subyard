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
