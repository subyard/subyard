package ownerservice

import (
	"fmt"
	"io"
	"slices"
	"strings"
)

// ActivationCandidate is a matching runtime and the result of the ordinary
// service-registration proof. A failed proof never authorizes a metadata write.
type ActivationCandidate struct {
	Registration
	Err error
}

// RegisterActivation is best-effort metadata work after native reconciliation.
// Existing entries, including interrupted publications, are left untouched.
func (registry Registry) RegisterActivation(service, publication string, discover func() ([]ActivationCandidate, bool, error), output io.Writer) {
	diagnostic := func(yard string, err error) {
		fmt.Fprintf(output, "Diagnostic: service %s, yard %s: %v; run yard service register %s --yard %s\n", service, yard, err, service, yard)
	}
	records, err := registry.List(service)
	if err != nil {
		diagnostic("NAME", err)
		return
	}
	if len(records) != 0 {
		return
	}
	candidates, selected, err := discover()
	failed := err != nil || len(candidates) > 1
	if err != nil {
		yard := "NAME"
		proven := slices.DeleteFunc(slices.Clone(candidates), func(candidate ActivationCandidate) bool { return candidate.Err != nil || candidate.UUID == "" })
		if len(proven) == 1 {
			yard = proven[0].Yard
		}
		diagnostic(yard, err)
	}
	slices.SortStableFunc(candidates, func(a, b ActivationCandidate) int { return strings.Compare(a.Yard, b.Yard) })
	for _, candidate := range candidates {
		if len(candidates) > 1 {
			diagnostic(candidate.Yard, fmt.Errorf("multiple candidate runtimes (%d); registration requires an explicit yard", len(candidates)))
		}
		if candidate.Err != nil {
			diagnostic(candidate.Yard, candidate.Err)
			failed = true
		}
	}
	if failed {
		return
	}
	if len(candidates) == 0 {
		if selected {
			fmt.Fprintf(output, "Diagnostic: no owned runtime to register; run yard service register %s --yard NAME\n", service)
		}
		return
	}
	record := candidates[0].Registration
	record.SchemaVersion, record.Service, record.Pool = SchemaVersion, service, "default"
	record.Generation, err = NewGeneration()
	if err == nil {
		err = registry.Save(record)
	}
	if err == nil {
		err = registry.Publish(record, publication)
	}
	if err != nil {
		diagnostic(record.Yard, err)
		return
	}
	fmt.Fprintf(output, "Registered service %s pool default: yard %s, project %s, instance %s, UUID %s\n", service, record.Yard, record.Project, record.Instance, record.UUID)
}
