package domain

import "errors"

const (
	ConfirmationRequiredCode = "confirmation_required"
	OperationDeclinedCode    = "operation_declined"
	PlanStaleCode            = "plan_stale"
)

var (
	ErrConfirmationRequired = errors.New("confirmation required")
	ErrOperationDeclined    = errors.New("operation declined")
	ErrPlanStale            = errors.New("operation plan is stale")
)

func ConfirmationErrorClass(err error) string {
	switch {
	case errors.Is(err, ErrConfirmationRequired):
		return ConfirmationRequiredCode
	case errors.Is(err, ErrOperationDeclined):
		return OperationDeclinedCode
	default:
		return ""
	}
}
