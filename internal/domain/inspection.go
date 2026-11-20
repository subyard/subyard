package domain

import (
	"fmt"
	"strings"
)

type InspectionState string

const (
	InspectedDrift    InspectionState = "drift"
	InspectedPending  InspectionState = "pending"
	InspectedBlocked  InspectionState = "blocked"
	InspectedUnknown  InspectionState = "unknown"
	InspectedDeferred InspectionState = "deferred"
	InspectedNotice   InspectionState = "notice"
)

// Inspected is an owner's observation of one resource predicate. No items
// means convergence. Unknown names the failed read; deferred names its cause.
type Inspected struct {
	Key                                         string
	State                                       InspectionState
	Resource, Installed, Desired, Message, Next string
}

// InspectionRead reports a failed read under key, or nothing.
func InspectionRead(key, resource string, err error) []Inspected {
	if err == nil {
		return nil
	}
	return []Inspected{{Key: key, State: InspectedUnknown, Resource: resource, Message: err.Error()}}
}

// InspectionName escapes resource bytes reversibly as %XX, without hashing
// mutable diagnostics or assigning ordinals.
func InspectionName(resource string) string {
	var result strings.Builder
	for _, b := range []byte(resource) {
		if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '-' || b == '.' || b == '_' {
			result.WriteByte(b)
		} else {
			fmt.Fprintf(&result, "%%%02X", b)
		}
	}
	return result.String()
}
