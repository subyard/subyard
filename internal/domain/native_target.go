package domain

import "fmt"

// NativeTargetOwnership refuses a known foreign expanded owner before guest access.
// Missing markers retain existing behavior; registered migration checks local consistency.
func NativeTargetOwnership(yard Context, expanded map[string]string) error {
	if yard.YardName == "" {
		yard.YardName = "default"
	}
	marker := expanded["user.subyard.name"]
	if marker == "" || marker == yard.YardName {
		return nil
	}
	action := fmt.Sprintf("set INCUS_PROJECT or YARD_INSTANCE_NAME at yard scope for %s", yard.YardName)
	return &NativeTargetRefusal{Message: fmt.Sprintf("yard %s: native target %s/%s belongs to yard %s (user.subyard.name); configuration points at another yard's instance", yard.YardName, yard.IncusProject, yard.YardInstanceName, MigrationDiagnosticValue(marker)), Action: action}
}

// NativeTargetRefusal is an explicit state verdict, including at activation boundaries.
type NativeTargetRefusal struct{ Message, Action, Summary string }

func (r *NativeTargetRefusal) Error() string { return r.Message + "; " + r.Action }
func (r *NativeTargetRefusal) ActivationDiagnostic() (string, string) {
	if r.Summary != "" {
		return r.Summary, r.Action
	}
	return r.Message, r.Action
}
func (*NativeTargetRefusal) MigrationRefusal() bool { return true }
