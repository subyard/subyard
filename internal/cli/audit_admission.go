package cli

import (
	"context"
	"errors"
	"sync"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/uninstall"
)

func (cli *CLI) initializeOwnerAdmission() {
	if cli.ownerAdmissionMutex == nil {
		cli.ownerAdmissionMutex = &sync.Mutex{}
	}
}

// Audit and native writers share the command's original installation authority.
// An audit only writes DataHome; other roots remain facts for their own writers.
func (cli *CLI) auditWriteAdmission(home string) func(context.Context) (func(), bool, error) {
	loaded := config.Loaded{Context: domain.Context{Paths: domain.RuntimePaths{DataHome: home}}}
	_, _ = cli.originalOwnerWrite(loaded)
	return func(ctx context.Context) (func(), bool, error) {
		admission, err := cli.originalOwnerWrite(loaded)
		if err != nil {
			return func() {}, false, nil
		}
		generation := admission.generation
		admission.generation = nil
		if cli.ownerMutationLock == nil {
			for _, root := range generation {
				if root.Path == home {
					admission.generation = []uninstall.Tree{root}
				}
			}
		}
		// An already admitted transaction may have published its own roots.
		// acquireOwnerMutation still verifies the held descriptor and epoch.
		release, err := cli.acquireOwnerMutation(ctx, admission, false, true)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, false, err
			}
			return func() {}, false, nil
		}
		return release, true, nil
	}
}

// Called with the admission mutex and stable owner barrier held. Only the
// command's own first home publication can refine its original absent fact.
func (cli *CLI) acceptOwnerHomePublication(home string, epoch uint64, created uninstall.Tree) {
	if original := cli.ownerAdmission; original != nil && original.home == home && original.epoch == epoch {
		for index, root := range original.generation {
			if root.Path == home && len(root.Entries) == 0 {
				original.generation[index] = created
			}
		}
	}
}
