package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"slices"

	"github.com/Subyard/Subyard/internal/adapters/hostruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testyardmigration"
)

type ownerInputDrift string

func (err ownerInputDrift) Error() string {
	return domain.ErrPlanStale.Error() + ": original owner " + string(err) + " changed"
}
func (ownerInputDrift) Unwrap() error { return domain.ErrPlanStale }
func (err ownerInputDrift) ActivationDiagnostic() (string, string) {
	return "original owner " + string(err) + " changed", "run yard update"
}

// Only the original owning registration may be replayed. Every other file is
// read normally; the peer scopes are all captured before consent.
type preparedOwnerChildInputView struct {
	root         *os.File
	source       testyardmigration.PreparedOwnerSource
	sourceDigest string
	options      testyardmigration.Options
	peers        [3][]domain.Context
	names        [3][]string
	current      ownerChildCheck
	restoration  ports.NativeOwnerRestorationObserver
}

func (view *preparedOwnerChildInputView) check(ctx context.Context, cli *CLI, baseline *ownerInputBaseline, locked bool) error {
	checked, err := testyardmigration.CheckOwnerChildSource(view.options, view.source.Before, view.source, view.root)
	if err != nil {
		observer, ok := ports.NativeOperationObserverFrom(ctx).(ports.NativeOwnerRestorationObserver)
		if view.restoration != nil {
			observer, ok = view.restoration, true
		}
		if !ok {
			return err
		}
		member, readErr := observer.ReadOwnerRestoration(view.options.RecoveryToken, view.sourceDigest)
		if readErr != nil {
			return errors.Join(err, readErr)
		}
		checked, err = testyardmigration.CheckRestoredOwnerChildSource(view.options, view.source.Before, view.source, view.root, member)
		if err != nil {
			return err
		}
	}
	defer checked.Close()
	options := view.options
	incus, _ := cli.statusPorts()
	options.RunYard = func(readCtx context.Context, yard string, output io.Writer, args ...string) error {
		if yard != testyardmigration.CurrentYard || !slices.Equal(args, []string{"check"}) {
			return domain.ErrPlanStale
		}
		check := view.current
		return (hostruntime.HostCheck{Yard: check.Yard, Yards: check.Yards, Environment: check.Environment, Incus: incus, Output: output}).Run(readCtx, hostruntime.CheckOptions{})
	}
	progress, err := testyardmigration.ObserveProgress(ports.WithoutNativeOperationObserver(ctx), options, view.source.Before)
	if err != nil {
		return err
	}
	files := &config.ProspectiveFiles{Root: options.ConfigHome, Yards: slices.Clone(view.names[0]), ReadSnapshot: func(path string) (config.PersistentFileSnapshot, error) {
		value, matched, err := checked.ReadOriginal(path)
		if !matched {
			return config.PersistentFileSnapshot{}, domain.ErrPlanStale
		}
		// This virtual value has explicit owning provenance: the exact original
		// inode is checked at its original name or token-bound archive on each
		// read. The loader must not try the vanished original pathname itself.
		value.Identity = config.PersistentFileIdentity{}
		return value, err
	}, ReadPreparedInput: func(path string) (config.PersistentFileSnapshot, bool, error) {
		value, matched, err := checked.ReadOriginal(path)
		if matched {
			value.Identity = config.PersistentFileIdentity{}
		}
		return value, matched, err
	}}
	fresh, err := config.Load(config.LoadOptions{Catalog: &cli.catalog, RepositoryRoot: cli.options.RepositoryRoot, OperatorHome: baseline.initial.Context.Paths.OperatorHome, YardName: baseline.initial.Context.YardName, YardSettingsFile: baseline.preset, Environment: cli.baseEnv, ProspectiveFiles: files, ConfigLocked: locked})
	if err != nil {
		return err
	}
	// Retained catalog paths use the current-PID spelling of the same owned
	// descriptor. Compare typed source labels without changing execution paths.
	label := func(value string) string {
		return ownerSourcePathLabel(value, cli.options.RepositoryRoot, cli.options.RepositoryRoot)
	}
	if !reflect.DeepEqual(fresh.Context, baseline.initial.Context) {
		return ownerInputDrift("context")
	}
	if !reflect.DeepEqual(ownerSettingsLabels(fresh.Settings, label), ownerSettingsLabels(baseline.initial.Settings, label)) {
		return ownerInputDrift("settings")
	}
	actual, err := ownerInputFiles(fresh)
	if err != nil {
		return err
	}
	if err := captureRuntimeSources(baseline.runtimeRoot, actual); err != nil {
		return err
	}
	path := checked.OriginalPath()
	if original, tracked := baseline.files[path]; tracked {
		if _, matched, err := checked.ReadOriginal(path); err != nil || !matched {
			return domain.ErrPlanStale
		}
		actual[path] = original
	}
	if baseline.projected != nil {
		if err := baseline.projected.validate(actual, baseline.files); err != nil {
			return err
		}
	}
	currentFiles, err := ownerInputFileLabels(actual, cli.options.RepositoryRoot)
	if err != nil {
		return err
	}
	originalFiles, err := ownerInputFileLabels(baseline.files, cli.options.RepositoryRoot)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(currentFiles, originalFiles) {
		return ownerInputDrift("input identities")
	}
	peers, err := cli.powerYardContextsLocked(fresh, locked)
	if err != nil {
		return err
	}
	names, err := config.YardNames(fresh.Context.Paths.ConfigDir, fresh.Context.Paths.ConfigHome)
	if err != nil {
		return err
	}
	allowed := false
	for i, scope := range view.peers {
		if progress == testyardmigration.ProgressExpected && i != 0 || progress == testyardmigration.ProgressDesired && i != 2 {
			continue
		}
		if reflect.DeepEqual(peers, scope) && slices.Equal(names, view.names[i]) {
			allowed = true
			break
		}
	}
	if !allowed {
		return ownerInputDrift("peer scope")
	}
	// Retain exact bytes and security metadata through the complete loader and
	// peer observation, not just the first registration read.
	last, matched, err := checked.ReadOriginal(path)
	if err != nil || !matched || !bytes.Equal(last.Content, view.source.Content) {
		return domain.ErrPlanStale
	}
	return nil
}

// Equivalent spellings coalesce only when every observed security fact agrees.
func ownerInputFileLabels(files map[string]ownerInputFile, root string) (map[string]ownerInputFile, error) {
	if files == nil {
		return nil, nil
	}
	result := make(map[string]ownerInputFile, len(files))
	for path, fact := range files {
		path = ownerSourcePathLabel(path, root, root)
		if previous, present := result[path]; present && previous != fact {
			return nil, domain.ErrPlanStale
		}
		result[path] = fact
	}
	return result, nil
}
