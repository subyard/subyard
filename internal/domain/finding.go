package domain

import "errors"

// Finding is one independently observed input failure and its public recovery.
// Owner and Input identify it independently of its wording.
type Finding struct {
	Owner, Input, Message, Recovery string
	// ReleaseRecovery overrides Recovery when applying a release activation.
	ReleaseRecovery string
}

func (finding Finding) Key() [2]string { return [2]string{finding.Owner, finding.Input} }

// DirectFinding recognizes an explicit public diagnostic on this error.
func DirectFinding(err error) (Finding, bool) {
	switch diagnostic := err.(type) {
	case interface{ ActivationFinding() Finding }:
		return diagnostic.ActivationFinding(), true
	case interface{ ActivationDiagnostic() (string, string) }:
		message, recovery := diagnostic.ActivationDiagnostic()
		return Finding{Message: message, Recovery: recovery}, true
	}
	return Finding{}, false
}

// PublicFinding follows single-cause wrappers to an explicit public diagnostic.
func PublicFinding(err error) (Finding, bool) {
	for ; err != nil; err = errors.Unwrap(err) {
		if finding, ok := DirectFinding(err); ok {
			return finding, true
		}
	}
	return Finding{}, false
}

// RefusalCauses walks independent causes in order. An explicit public finding
// owns its cause, so its implementation detail stays behind that boundary.
func RefusalCauses(err error) []error {
	if err == nil {
		return nil
	}
	if causes, ok := IndependentRefusals(err); ok {
		return causes
	}
	return []error{err}
}

// IndependentRefusals reports the leaves of an aggregate before a diagnostic boundary.
func IndependentRefusals(err error) ([]error, bool) {
	for cause := err; cause != nil; cause = errors.Unwrap(cause) {
		if _, public := DirectFinding(cause); public {
			break
		}
		if aggregate, ok := cause.(interface{ Unwrap() []error }); ok {
			var causes []error
			for _, child := range aggregate.Unwrap() {
				causes = append(causes, RefusalCauses(child)...)
			}
			return causes, true
		}
	}
	return nil, false
}
