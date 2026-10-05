package testvmsruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Subyard/Subyard/internal/domain"
)

const doctorFallback = "broker doctor readiness check failed"

type readinessError struct {
	yard, reason string
	cause        error
}

// NewReadinessError accepts a source-owned check description, never guest output
// or configuration values. The underlying failure remains available via Unwrap.
func NewReadinessError(yard, reason string, cause error) error {
	if len(yard) > 63 || !domain.SafeName(yard) {
		yard = ""
	}
	if reason == "" || len(reason) > 256 || !utf8.ValidString(reason) ||
		strings.IndexFunc(reason, unicode.IsControl) >= 0 {
		reason = "backend readiness check failed"
	}
	return &readinessError{yard: yard, reason: reason, cause: cause}
}

func (failure *readinessError) Error() string {
	message, _ := failure.ActivationDiagnostic()
	return message
}

func (failure *readinessError) Unwrap() error { return failure.cause }

func (failure *readinessError) ActivationDiagnostic() (string, string) {
	if failure.yard == "" {
		return "test-vms readiness: " + failure.reason, "run yard migrate"
	}
	return fmt.Sprintf("test-vms readiness for yard %s: %s", failure.yard, failure.reason),
		"run yard -Y " + failure.yard + " init"
}

type doctorCheckError struct {
	reason string
	cause  error
}

func doctorCheck(reason string, cause error) error {
	return &doctorCheckError{reason: reason, cause: cause}
}

func (failure *doctorCheckError) Error() string {
	if failure.cause != nil {
		return failure.cause.Error()
	}
	return failure.reason
}

func (failure *doctorCheckError) Unwrap() error { return failure.cause }

type doctorReport struct {
	Converged bool   `json:"converged"`
	Reason    string `json:"reason"`
}

func (runtime *Runtime) doctor(ctx context.Context, want map[string]string) error {
	err := runtime.doctorChecks(ctx, want)
	if want["WANT_DIAGNOSTIC"] == "1" {
		if reportErr := writeDoctorReport(runtime.Stdout, err); reportErr != nil {
			return errors.Join(err, reportErr)
		}
	}
	return err
}

func writeDoctorReport(output io.Writer, err error) error {
	report := doctorReport{Converged: err == nil}
	if err != nil {
		report.Reason = doctorFallback
		var check *doctorCheckError
		if errors.As(err, &check) && knownDoctorReason(check.reason) {
			report.Reason = check.reason
		}
	}
	return json.NewEncoder(output).Encode(report)
}

func parseDoctorReport(payload string) (bool, string) {
	if len(payload) > 4096 {
		return false, doctorFallback
	}
	decoder := json.NewDecoder(strings.NewReader(payload))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return false, doctorFallback
	}
	var report doctorReport
	seen := make(map[string]bool, 2)
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || seen[key] {
			return false, doctorFallback
		}
		seen[key] = true
		token, err = decoder.Token()
		if err != nil {
			return false, doctorFallback
		}
		switch key {
		case "converged":
			report.Converged, ok = token.(bool)
		case "reason":
			report.Reason, ok = token.(string)
		default:
			return false, doctorFallback
		}
		if !ok {
			return false, doctorFallback
		}
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') || len(seen) != 2 {
		return false, doctorFallback
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return false, doctorFallback
	}
	if report.Converged && report.Reason == "" {
		return true, ""
	}
	if !report.Converged && knownDoctorReason(report.Reason) {
		return false, report.Reason
	}
	return false, doctorFallback
}

// Only named checks owned by the candidate engine cross the guest boundary.
func knownDoctorReason(reason string) bool {
	switch reason {
	case doctorFallback,
		"backend enabled state differs", "backend config is missing",
		"installed test-vms engine cannot be located", "installed test-vms engine cannot be inspected",
		"installed test-vms engine hash differs", "lease reaper remains active",
		"firewall remains active", "disabled backend artifact remains", "agent account remains",
		"physical host memory source is unavailable or unsafe",
		"required inner Incus command is missing", "required qemu-system-x86_64 command is missing",
		"required nft command is missing", "inner Incus version cannot be inspected", "inner Incus is too old", "inner Incus is inactive",
		"Incus drop-in differs", "lease reaper is disabled", "firewall is inactive",
		"firewall table is missing", "cannot render sshd config", "SSH password login is enabled",
		"required virtualization device is missing", "broker state directory permissions differ",
		"yard developer account is missing", "yard developer has inner privileges",
		"agent account is missing", "agent account cannot use key login", "agent has supplementary groups",
		"cannot inspect agent group", "agent SSH directory permissions differ",
		"agent authorized keys permissions differ", "cannot inspect controller authorized_keys",
		"static controller key remains active", "controller authorized key command permissions differ",
		"default-open forced facade differs", "controller AuthorizedKeysCommand differs",
		"bounded facade sudo policy differs":
		return true
	}
	return false
}
