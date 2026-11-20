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
