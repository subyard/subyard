package domain

import "errors"

// ErrIncusAccessDenied marks an installed Incus server whose socket this
// account cannot open: no incus-admin membership, or membership that is not
// active in the current login session.
var ErrIncusAccessDenied = errors.New("this account cannot access the Incus socket")

// IncusAccessNextStep is the only repair: native state stays uninspected until
// the operator's session can reach the socket.
const IncusAccessNextStep = "open a new login session or run newgrp incus-admin " +
	"(if this account is not a member, first run sudo usermod -aG incus-admin \"$USER\"), then retry"

// IncusAccessRefusal reports one local yard whose native state cannot be read
// because socket access is denied. It is a public verdict, not a missing
// release dependency; raw transport text never crosses the boundary.
func IncusAccessRefusal(yard string, cause error) error {
	if cause == nil {
		return nil
	}
	return &incusAccessRefusal{yard: yard, cause: cause}
}

type incusAccessRefusal struct {
	yard  string
	cause error
}

func (failure *incusAccessRefusal) message() string {
	prefix := "registered yard: "
	if len(failure.yard) <= 63 && SafeName(failure.yard) {
		prefix = "yard " + failure.yard + ": "
	}
	return prefix + "native state cannot be inspected: " + ErrIncusAccessDenied.Error()
}

func (failure *incusAccessRefusal) Error() string {
	return failure.message() + "; next: " + IncusAccessNextStep
}
func (failure *incusAccessRefusal) Unwrap() error  { return failure.cause }
func (*incusAccessRefusal) MigrationRefusal() bool { return true }
func (failure *incusAccessRefusal) ActivationDiagnostic() (string, string) {
	return IncusAccessDiagnostic(failure.yard)
}

// IncusAccessDiagnostic is the public message and next step for one yard.
func IncusAccessDiagnostic(yard string) (string, string) {
	return (&incusAccessRefusal{yard: yard}).message(), IncusAccessNextStep
}
