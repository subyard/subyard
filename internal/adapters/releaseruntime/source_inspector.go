package releaseruntime

import (
	"context"
	"io"
	"os"
)

// SourceInspector exposes metadata admission, never protected release operations.
// Its process transport is captured natively and cannot be replaced by loaded settings.
type SourceInspector struct{ runtime *Runtime }

func NewSourceInspector(repositoryRoot string, stderr io.Writer) *SourceInspector {
	return &SourceInspector{runtime: New(Config{Inputs: CaptureCommandInputs(os.Environ(), nil), RepositoryRoot: repositoryRoot, Stderr: stderr})}
}

func (inspector *SourceInspector) Close() error { return inspector.runtime.Close() }

func (inspector *SourceInspector) InspectSourceAdmission(ctx context.Context, runtimeRoot, configHome string) (SourceAdmission, error) {
	return inspector.runtime.InspectSourceAdmission(ctx, runtimeRoot, configHome)
}

func (inspector *SourceInspector) InspectSourceTooling(ctx context.Context, runtimeRoot, repositoryRoot string) (string, string, error) {
	return inspector.runtime.InspectSourceTooling(ctx, runtimeRoot, repositoryRoot)
}
