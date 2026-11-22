package profile

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// ReadinessTimeout bounds read-only profile readiness observation.
const ReadinessTimeout = 30 * time.Second

// ServiceCheckError contains only a validated public owner, native status and
// optional closed readiness reason.
type ServiceCheckError struct {
	Owner      string
	NativeExit int
	Reason     string
	cause      error
}

func (failure *ServiceCheckError) Error() string {
	message := fmt.Sprintf("profile service check owner=%s native-exit=%d", failure.Owner, failure.NativeExit)
	if ValidServiceReadinessReason(failure.Reason) {
		message += " reason=" + failure.Reason
	}
	return message
}
func (failure *ServiceCheckError) Unwrap() error { return failure.cause }

// SourceUnavailable reports that a profile applied everything it can while an
// external source it reads stays unreachable until the operator grants access.
const SourceUnavailable = "source-unavailable"

// serviceReadinessHints is the generic remediation the operation boundary
// renders next to a readiness code; a code without a hint renders alone.
var serviceReadinessHints = map[string]string{
	SourceUnavailable: "authorize access to the source (yard ssh-agent unlock --key PATH --ttl DURATION, or the yard's git credentials) or use a public URL, then run yard init",
}

// ServiceReadinessHint returns the remediation for a readiness code, or "".
func ServiceReadinessHint(reason string) string { return serviceReadinessHints[reason] }

// ServiceReadinessAcceptedAfterApply reports whether a post-apply check may end
// with this reason. Pre-apply checks stay strict, so the next init retries.
func ServiceReadinessAcceptedAfterApply(reason string) bool { return reason == SourceUnavailable }

// ValidServiceReadinessReason admits only public, secret-free readiness facts.
func ValidServiceReadinessReason(reason string) bool {
	switch reason {
	case "boot-link-missing", "boot-link-target-mismatch", "unit-fragment-mismatch", "boot-dependency-unloaded", "unit-inactive", "transport-unavailable", SourceUnavailable:
		return true
	}
	return false
}

// ValidServiceAdvisory admits the closed codes a converged profile may report.
// An advisory never changes readiness; the profile declares its remediation.
func ValidServiceAdvisory(code string) bool { return code == "storage-capacity" }

// ServiceAdvisory reads one `subyard-profile-advisory-v1 CODE` record from a
// converged check's bounded stderr.
func ServiceAdvisory(output []byte) string {
	buffer := serviceReadinessBuffer(output[:min(len(output), serviceReadinessLimit)])
	return buffer.record("subyard-profile-advisory-v1 ", ValidServiceAdvisory)
}

type serviceReadinessBuffer []byte

const serviceReadinessLimit = 128

func (buffer *serviceReadinessBuffer) Write(data []byte) (int, error) {
	*buffer = append(*buffer, data[:min(len(data), serviceReadinessLimit-len(*buffer))]...)
	return len(data), nil
}

func (buffer *serviceReadinessBuffer) reason() string {
	return buffer.record("subyard-profile-readiness-v1 ", ValidServiceReadinessReason)
}

func (buffer *serviceReadinessBuffer) record(prefix string, valid func(string) bool) string {
	data := string(*buffer)
	// Every valid closed record is shorter than a full (possibly truncated) buffer.
	if len(*buffer) == serviceReadinessLimit || !strings.HasPrefix(data, prefix) || !strings.HasSuffix(data, "\n") {
		return ""
	}
	code := strings.TrimSuffix(strings.TrimPrefix(data, prefix), "\n")
	if !valid(code) {
		return ""
	}
	return code
}

// ServiceCheckExit preserves ordinary drift and native failures separately from
// dispatcher failures. Other service verbs retain their existing exit policy.
func ServiceCheckExit(err error) int {
	var failure *ServiceCheckError
	if errors.As(err, &failure) && failure.NativeExit > 0 && failure.NativeExit <= 255 {
		return failure.NativeExit
	}
	return 2
}

// RunServices is a physical leaf behind the core operation's approval. Hooks
// belong to shipped profiles; secret values never travel in the command line.
func RunServices(ctx context.Context, root string, arguments []string, environment map[string]string, stdout, stderr io.Writer) error {
	if environment["SUBYARD_ENGINE_CONTEXT"] != "1" || environment["SUBYARD_ENGINE_CONTEXT_SCHEMA"] != "1" {
		return errors.New("prepared engine context required")
	}
	if len(arguments) < 1 || len(arguments) > 2 {
		return errors.New("invalid profile service arguments")
	}
	action := arguments[0]
	if !slices.Contains([]string{"--check", "--yes", "--pause", "--resume", "--remove", "--wait-ready"}, action) || len(arguments) == 2 && !slices.Contains([]string{"--check", "--yes", "--remove", "--resume"}, action) {
		return errors.New("invalid profile service action")
	}
	definitions, err := Load(root)
	if err != nil {
		return err
	}
	if action == "--wait-ready" {
		return waitStartupReadiness(ctx, root, definitions, environment, time.Second)
	}
	selectedResume := []string{}
	if len(arguments) == 2 {
		selectedResume = strings.Fields(arguments[1])
		if action != "--resume" && len(selectedResume) != 1 {
			return errors.New("scoped profile service requires one profile")
		}
	}
	for _, name := range selectedResume {
		if !slices.ContainsFunc(definitions, func(d Definition) bool { return d.Name == name && d.OwnerService != "" }) {
			return errors.New("unknown profile service")
		}
	}
	paused := []Definition{}
	yard := environment["SUBYARD_YARD"]
	// The prepared shell context represents the default yard with an empty selector.
	if yard == "" {
		yard = "default"
	}
	run := func(definition Definition, action string, output io.Writer) error {
		env := make([]string, 0, len(environment)+1)
		for key, value := range environment {
			if key != "SUBYARD_PROFILE_SELECTED" {
				env = append(env, key+"="+value)
			}
		}
		selected := "0"
		if definition.Selected(yard, environment) {
			selected = "1"
		}
		env = append(env, "SUBYARD_PROFILE_SELECTED="+selected)
		command := exec.CommandContext(ctx, "bash", filepath.Join(definition.Root, definition.OwnerService), action)
		command.Dir = root
		command.Env = env
		command.Stdout = output
		command.Stderr = stderr
		var diagnostic serviceReadinessBuffer
		if action == "--check" {
			command.Stdout, command.Stderr = io.Discard, io.Discard
			if environment["SUBYARD_PROFILE_DIAGNOSTICS"] == "1" {
				command.Stderr = &diagnostic
			}
		}
		if err := command.Run(); err != nil {
			if action == "--check" {
				var exit *exec.ExitError
				if errors.As(err, &exit) && exit.ExitCode() > 0 {
					return &ServiceCheckError{Owner: definition.Name, NativeExit: exit.ExitCode(), Reason: diagnostic.reason(), cause: err}
				}
				return errors.New("profile service check unavailable")
			}
			return fmt.Errorf("profile %s owner service %s failed: %w", definition.Name, action, err)
		}
		if code := diagnostic.record("subyard-profile-advisory-v1 ", ValidServiceAdvisory); code != "" {
			fmt.Fprintf(stderr, "profile service advisory owner=%s code=%s\n", definition.Name, code)
		}
		return nil
	}
	// A check that only waits for source access does not hide later owners: a
	// later failure with another reason wins, otherwise the first such result returns.
	var deferred error
	for _, definition := range definitions {
		if definition.OwnerService == "" {
			continue
		}
		if len(arguments) == 2 && !slices.Contains(selectedResume, definition.Name) {
			continue
		}
		var captured bytes.Buffer
		output := stdout
		if action == "--pause" {
			output = &captured
		}
		err := run(definition, action, output)
		var failure *ServiceCheckError
		if action == "--check" && errors.As(err, &failure) && failure.NativeExit == 1 && ServiceReadinessAcceptedAfterApply(failure.Reason) {
			if deferred == nil {
				deferred = err
			}
			continue
		}
		if err != nil {
			// A failed pause must not strand earlier services; the caller receives no
			// usable pause list on failure and cannot restore those itself.
			for _, previous := range paused {
				_ = run(previous, "--resume", stderr)
			}
			return err
		}
		if action == "--pause" {
			result := strings.TrimSpace(captured.String())
			if result != "" && result != "paused" {
				_ = run(definition, "--resume", stderr)
				for _, previous := range paused {
					_ = run(previous, "--resume", stderr)
				}
				return errors.New("invalid profile service pause response")
			}
			if result == "paused" {
				paused = append(paused, definition)
			}
		}
	}
	if action == "--pause" {
		for _, definition := range paused {
			fmt.Fprintln(stdout, definition.Name)
		}
	}
	return deferred
}
