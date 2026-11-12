package releasetransition

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/Subyard/Subyard/internal/domain"
)

type migrationDiagnosticWriterKey struct{}
type migrationDiagnosticCaptureKey struct{}
type migrationDiagnosticCapture struct {
	writer   io.Writer
	messages map[string]bool
	yards    map[string]map[string]bool
}

// WithMigrationDiagnosticWriter selects the engine's diagnostic stream. It is
// separate from the frozen JSON response: verdict details are filtered, while
// dependency errors retain the stderr detail of failed inspections.
func WithMigrationDiagnosticWriter(ctx context.Context, writer io.Writer) context.Context {
	if writer == nil {
		return ctx
	}
	return context.WithValue(ctx, migrationDiagnosticWriterKey{}, writer)
}

func captureMigrationDiagnostics(ctx context.Context) (context.Context, func()) {
	if ctx.Value(migrationDiagnosticCaptureKey{}) != nil {
		return ctx, func() {}
	}
	writer, _ := ctx.Value(migrationDiagnosticWriterKey{}).(io.Writer)
	capture := &migrationDiagnosticCapture{writer: writer, messages: map[string]bool{}, yards: map[string]map[string]bool{}}
	return context.WithValue(ctx, migrationDiagnosticCaptureKey{}, capture), func() {
		if capture.writer == nil {
			return
		}
		rows := []string{}
		for message := range capture.messages {
			rows = append(rows, message)
		}
		slices.Sort(rows)
		for _, message := range rows {
			fmt.Fprintln(capture.writer, message)
		}
		yards := make([]string, 0, len(capture.yards))
		for yard := range capture.yards {
			yards = append(yards, yard)
		}
		slices.Sort(yards)
		for _, yard := range yards {
			fmt.Fprintf(capture.writer, "yard %s:\n", yard)
			rows := make([]string, 0, len(capture.yards[yard]))
			for row := range capture.yards[yard] {
				rows = append(rows, row)
			}
			slices.Sort(rows)
			for _, row := range rows {
				fmt.Fprintf(capture.writer, "  %s\n", row)
			}
		}
	}
}

func recordMigrationDiagnostics(ctx context.Context, err error) {
	capture, _ := ctx.Value(migrationDiagnosticCaptureKey{}).(*migrationDiagnosticCapture)
	if capture == nil || capture.writer == nil {
		return
	}
	for _, cause := range refusalCauses(err) {
		var dependency *nativeDependencyError
		if errors.As(cause, &dependency) {
			capture.messages["release transition dependency: "+dependency.Error()] = true
			continue
		}
		var nativeTarget *domain.NativeTargetRefusal
		if errors.As(cause, &nativeTarget) {
			capture.messages[nativeTarget.Error()] = true
			continue
		}
		message, action := publicActivationDiagnostic(cause, "", "")
		if message == "" {
			continue
		}
		var details interface {
			MigrationRefusalDetails() (string, string, []string)
		}
		if errors.As(cause, &details) {
			yard, reason, names := details.MigrationRefusalDetails()
			if len(yard) <= 63 && domain.SafeName(yard) && utf8.ValidString(reason) && validateText(reason, "migration refusal", maxDiagnosticText, true) == nil {
				var parameters interface {
					MigrationParameters() []domain.MigrationParameter
				}
				if errors.As(cause, &parameters) {
					if capture.yards[yard] == nil {
						capture.yards[yard] = map[string]bool{}
					}
					for _, parameter := range parameters.MigrationParameters() {
						if len(parameter.Name) <= 96 && domain.SafeID(parameter.Name) {
							row := parameter.Name + "  " + domain.MigrationDiagnosticValue(parameter.Installed) + " -> " + domain.MigrationDiagnosticValue(parameter.Desired)
							capture.yards[yard][row] = true
						}
					}
				}
				names = slices.Clone(names)
				for index, name := range names {
					if len(name) > 96 || !domain.SafeID(name) {
						names[index] = "unrecognized name"
					}
				}
				slices.Sort(names)
				names = slices.Compact(names)
				message = "yard " + yard + ": " + reason
				if len(names) > 0 {
					message += ": " + strings.Join(names, ", ")
				}
			}
		}
		capture.messages["release transition refusal: "+message+"; retry: "+action] = true
	}
}

// Attach the protected owner name before flattening a multi-cause error. Raw
// dependency text remains confined to Error()/the engine diagnostic stream.
type nativeDependencyError struct {
	yard  string
	cause error
}

func (failure *nativeDependencyError) Error() string {
	prefix := "yard " + failure.yard + ": "
	if strings.HasPrefix(failure.cause.Error(), prefix) {
		return failure.cause.Error()
	}
	return prefix + failure.cause.Error()
}
func (failure *nativeDependencyError) Unwrap() error { return failure.cause }
func scopedNativeRefusals(owner string, err error) error {
	if err == nil {
		return nil
	}
	yard := strings.TrimPrefix(owner, "yard.")
	if len(yard) > 63 || !domain.SafeName(yard) {
		yard = "unrecognized"
	}
	causes := refusalCauses(err)
	for index, cause := range causes {
		if !hasMigrationRefusal(cause) {
			causes[index] = &nativeDependencyError{yard: yard, cause: cause}
		}
	}
	result := domain.CollectRefusals(causes...)
	if strings.HasPrefix(result.Error(), "yard "+yard+": ") {
		return result
	}
	return fmt.Errorf("yard %s: %w", yard, result)
}

// A later failed read cannot erase already established owner refusals. Preserve
// the original failure identity and include the safe blockers in terminal stderr.
type blockedInspectionError struct {
	cause    error
	blockers []Blocker
}

func (failure *blockedInspectionError) Unwrap() error { return failure.cause }
func (failure *blockedInspectionError) Error() string {
	rows := []string{failure.cause.Error()}
	for _, blocker := range failure.blockers {
		rows = append(rows, blocker.Message+"; retry: "+blocker.Retry)
	}
	return strings.Join(rows, "; ")
}

type migrationReasonClass struct {
	yard, reason string
	count        int
	details      int
}

// Names are the first detail to yield space. Keep reason classes and counts,
// sharing a yard prefix. Only the final reduction omits classes, with an actual
// update --check stderr stream retaining the complete filtered list.
func compactMigrationSummary(classes []migrationReasonClass) string {
	hint := "; full filtered details: stderr of yard update --check --json"
	omitted := 0
	for _, class := range classes {
		omitted += class.details
	}
	if omitted > 0 {
		hint = fmt.Sprintf("; %d parameter/name details omitted", omitted) + hint
	}
	rows := map[string][]string{}
	for index := range classes {
		reason := classes[index].reason
		for _, prefix := range []string{"legacy continuation ", "yard adoption ", "registered yard "} {
			reason = strings.TrimPrefix(reason, prefix)
		}
		for _, prefix := range []string{"refuses ", "requires "} {
			reason = strings.TrimPrefix(reason, prefix)
		}
		classes[index].reason = reason
		rows[classes[index].yard] = append(rows[classes[index].yard], fmt.Sprintf("%s (%d)", reason, classes[index].count))
	}
	messages := []string{}
	for yard, reasons := range rows {
		slices.Sort(reasons)
		messages = append(messages, "yard "+yard+": "+strings.Join(reasons, ", "))
	}
	slices.Sort(messages)
	message := strings.Join(messages, "; ")
	if len(message)+len(hint) <= maxDiagnosticText {
		return message + hint
	}
	protects := func(reason string) bool {
		return strings.Contains(reason, "srv") || strings.Contains(reason, "persistent data") || strings.Contains(reason, "host-access") || strings.Contains(reason, "host access") || strings.Contains(reason, "native device boundaries")
	}
	slices.SortStableFunc(classes, func(a, b migrationReasonClass) int {
		if protects(a.reason) != protects(b.reason) {
			if protects(a.reason) {
				return -1
			}
			return 1
		}
		return strings.Compare(a.yard+"\x00"+a.reason, b.yard+"\x00"+b.reason)
	})
	guards := []string{}
	for _, class := range classes {
		if strings.Contains(class.reason, "srv") || strings.Contains(class.reason, "persistent data") {
			guards = append(guards, "srv/persistent-data refusals")
		}
		if strings.Contains(class.reason, "host-access") || strings.Contains(class.reason, "host access") || strings.Contains(class.reason, "native device boundaries") {
			guards = append(guards, "host-access boundary refusals")
		}
	}
	slices.Sort(guards)
	guards = slices.Compact(guards)
	messages = nil
	if len(guards) > 0 {
		messages = append(messages, strings.Join(guards, " and ")+" remain blocked")
	}
	kept := 0
	for _, class := range classes {
		row := fmt.Sprintf("yard %s: %s (%d)", class.yard, class.reason, class.count)
		candidate := strings.Join(append(slices.Clone(messages), row), "; ")
		reserve := len(hint) + len(fmt.Sprintf("; %d more reason classes", len(classes)))
		if len(candidate)+reserve > maxDiagnosticText {
			continue
		}
		messages = append(messages, row)
		kept++
	}
	if kept == 0 && len(classes) > 0 {
		prefix := strings.Join(messages, "; ")
		if prefix != "" {
			prefix += "; "
		}
		suffix := fmt.Sprintf("; %d more reason classes", len(classes)-1) + hint
		first := fmt.Sprintf("yard %s: %s (%d)", classes[0].yard, classes[0].reason, classes[0].count)
		return prefix + boundedFailureText(first, maxDiagnosticText-len(prefix)-len(suffix)) + suffix
	}
	return strings.Join(messages, "; ") + fmt.Sprintf("; %d more reason classes", len(classes)-kept) + hint
}
