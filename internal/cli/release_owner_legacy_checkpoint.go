package cli

import (
	"context"

	"github.com/Subyard/Subyard/internal/migration"
	"github.com/Subyard/Subyard/internal/releaseowner"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

// The native Rust entry captures only the existing source component. Published
// history selection and legacy import eligibility never enter the Go factory.
func (c *releaseOwnerComponents) sourceOnlyIngress() (releasetransition.V2Ingress, error) {
	request := c.request()
	compatibility := &migration.V2CompatibilityIngress{}
	if request.SourceIngress == nil {
		return compatibility, nil
	}
	options, err := c.cli.releaseTransitionTestYardOptions(request)
	if err != nil {
		return nil, err
	}
	source, err := migration.NewV2SourceIngress(migration.V2SourceIngressOptions{Descriptor: *request.SourceIngress, RepositoryRoot: options.RepositoryRoot, RuntimeRoot: request.RuntimeRoot, ConfigHome: options.ConfigHome, Environment: options.Environment, Stderr: options.Stderr})
	if err != nil {
		return nil, err
	}
	compatibility.Source = source
	return compatibility, nil
}

func (c *releaseOwnerComponents) ObserveOriginalV1Checkpoint(ctx context.Context, value releaseowner.OriginalV1Expectation, journal, registry []byte) (string, error) {
	options, err := c.cli.releaseTransitionTestYardOptions(c.request())
	if err != nil {
		return "", err
	}
	expectation := migration.V1ImportFactExpectation{Release: migration.V1ImportRelease(value.Release), Checkpoint: migration.V1ImportCheckpoint(value.Checkpoint), Transaction: releasetransition.TransactionID(value.Transaction), RuntimePair: migration.V1ImportRuntimePair{Current: value.RuntimePair.Current, Previous: value.RuntimePair.Previous}}
	actual, err := migration.ObserveOriginalV1Checkpoint(ctx, migration.ReleaseOptions{RepositoryRoot: options.RepositoryRoot, RuntimeRoot: c.context.RuntimeRoot, ConfigHome: options.ConfigHome, DataHome: options.DataHome, Executable: options.Executable, Incus: options.Incus, Environment: options.Environment, Diagnostics: options.Stdout, Stderr: options.Stderr}, expectation, journal, registry)
	return string(actual), err
}
