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
}

func (view *preparedOwnerChildInputView) check(ctx context.Context, cli *CLI, baseline *ownerInputBaseline) error {
	checked, err := testyardmigration.CheckOwnerChildSource(view.options, view.source.Before, view.source, view.root)
	if err != nil {
		observer, ok := ports.NativeOperationObserverFrom(ctx).(ports.NativeOwnerRestorationObserver)
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
	fresh, err := config.Load(config.LoadOptions{Catalog: &cli.catalog, RepositoryRoot: cli.options.RepositoryRoot, OperatorHome: baseline.initial.Context.Paths.OperatorHome, YardName: baseline.initial.Context.YardName, YardSettingsFile: baseline.preset, Environment: cli.baseEnv, ProspectiveFiles: files})
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(fresh.Context, baseline.initial.Context) || !reflect.DeepEqual(fresh.Settings, baseline.initial.Settings) {
		return domain.ErrPlanStale
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
	if !reflect.DeepEqual(actual, baseline.files) {
		return domain.ErrPlanStale
	}
	peers, err := cli.powerYardContexts(fresh)
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
		return domain.ErrPlanStale
	}
	// Retain exact bytes and security metadata through the complete loader and
	// peer observation, not just the first registration read.
	last, matched, err := checked.ReadOriginal(path)
	if err != nil || !matched || !bytes.Equal(last.Content, view.source.Content) {
		return domain.ErrPlanStale
	}
	return nil
}
