package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/hostruntime"
	"github.com/Subyard/Subyard/internal/adapters/incusclient"
	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/adapters/shelladapter"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerpublication"
	"github.com/Subyard/Subyard/internal/ports"
)

// The original capsule installs these transient leaves only for its native instance
// start. Ordinary lifecycle execution keeps its existing physical adapter.
type ownerStartPhysical struct {
	cli                *CLI
	yard               domain.Context
	uuid               string
	weight             string
	guard              func(context.Context) error
	original           *ports.InstanceInfo
	consumer           *ownerpublication.Consumer
	publicationRuntime *reconcileruntime.Runtime
}

type ownerStartPowerConfig struct {
	cli   *CLI
	yard  domain.Context
	uuid  string
	guard func(context.Context) error
}

func (w ownerStartPowerConfig) SetInstanceConfig(ctx context.Context, project, name string, values map[string]string) error {
	if project != w.yard.IncusProject || name != w.yard.YardInstanceName || w.uuid == "" {
		return domain.ErrPlanStale
	}
	if w.guard != nil {
		if err := w.guard(ctx); err != nil {
			return err
		}
	}
	incus, _ := w.cli.statusPorts()
	writer, ok := incus.(interface {
		SetOriginalInstanceConfigIf(context.Context, string, string, string, domain.YardKind, map[string]string, map[string]string) error
	})
	if !ok {
		return errors.New("original lifecycle requires an exact instance config writer")
	}
	return writer.SetOriginalInstanceConfigIf(ctx, project, name, w.uuid, w.yard.YardKind, nil, values)
}

func (r ownerStartPhysical) instance(ctx context.Context) (ports.InstanceInfo, error) {
	if r.guard != nil {
		if err := r.guard(ctx); err != nil {
			return ports.InstanceInfo{}, err
		}
	}
	incus, _ := r.cli.statusPorts()
	i, err := incus.Instance(ctx, r.yard.IncusProject, r.yard.YardInstanceName)
	if err != nil {
		return i, err
	}
	if r.uuid == "" || i.Name != r.yard.YardInstanceName || i.Project != r.yard.IncusProject || i.Type != r.yard.YardKind || i.Config["volatile.uuid"] != r.uuid || i.LocalConfig["volatile.uuid"] != r.uuid || i.Config["user.subyard.vm_cpu_weight"] != r.weight || i.LocalConfig["user.subyard.vm_cpu_weight"] != r.weight {
		return i, domain.ErrPlanStale
	}
	return i, nil
}

func (r ownerStartPhysical) hostSafe(ctx context.Context) error {
	if policy := r.cli.options.NetworkPolicy; policy != nil && policy.Guard != nil {
		return policy.Guard(ctx)
	}
	guard := hostruntime.NetworkGuard{Run: func(ctx context.Context, path string, args ...string) ([]byte, error) {
		command := exec.CommandContext(ctx, path, args...)
		if filepath.Base(path) == "NetworkManager" && r.cli.effectiveUID() != 0 {
			if r.cli.env["SUBYARD_SUDO_PREAUTHORIZED"] != "1" {
				return nil, errors.New("original lifecycle NetworkManager reader is not authorized")
			}
			command = exec.CommandContext(ctx, "/usr/bin/sudo", append([]string{"-n", "--", path}, args...)...)
		}
		command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8", "LC_ALL=C.UTF-8"}
		return command.CombinedOutput()
	}}
	return guard.Check(ctx, []string{r.yard.IncusBridge})
}

func (r ownerStartPhysical) publications(ctx context.Context) error {
	contracts, err := ownerpublication.Load(r.cli.options.RepositoryRoot)
	if err != nil {
		return err
	}
	incus, _ := r.cli.statusPorts()
	publisher, ok := incus.(interface {
		ReconcileOriginalPublicationsIf(context.Context, string, string, string, domain.YardKind, []ownerpublication.Contract, []netip.Addr, ...func() error) ([]string, error)
	})
	if !ok {
		return errors.New("original lifecycle publication adapter is unavailable")
	}
	waiting, err := ownerpublication.ReconcileAddresses(hostruntime.LocalAddresses, func(addresses []netip.Addr) ([]string, error) {
		return publisher.ReconcileOriginalPublicationsIf(ctx, r.yard.IncusProject, r.yard.YardInstanceName, r.uuid, r.yard.YardKind, contracts, addresses, func() error {
			if r.guard != nil {
				if err := r.guard(ctx); err != nil {
					return err
				}
			}
			return ownerpublication.RequireReader(r.cli.options.RepositoryRoot, r.yard.Paths.ConfigHome)
		})
	})
	if err == nil && len(waiting) != 0 {
		fmt.Fprintf(r.cli.options.Stderr, "Optional owner publication waiting for address: %s\n", strings.Join(waiting, "\n"))
	}
	return err
}

func (r ownerStartPhysical) Run(ctx context.Context, request domain.AdapterRequest, protected io.Reader) (domain.AdapterResult, string, error) {
	result := domain.AdapterResult{Schema: shelladapter.ProtocolSchema, OperationID: request.OperationID, Status: "ok"}
	if request.Schema != shelladapter.ProtocolSchema || !domain.SafeID(request.OperationID) || request.Adapter != "lifecycle" || request.Action != "start" || protected != nil || ports.NativeOperationObserverFrom(ctx) == nil {
		return domain.AdapterResult{}, "", domain.ErrPlanStale
	}
	if _, err := r.instance(ctx); err != nil {
		return domain.AdapterResult{}, "", err
	}
	if slices.Equal(request.Arguments, []string{"start", "--publication-retry-only"}) {
		contracts, err := ownerpublication.Load(r.cli.options.RepositoryRoot)
		if err != nil {
			return domain.AdapterResult{}, "", err
		}
		err = r.handoffPublications(ctx, contracts)
		if err != nil {
			return domain.AdapterResult{}, "", err
		}
		return result, "", nil
	}
	if !slices.Equal(request.Arguments, []string{"start"}) {
		return domain.AdapterResult{}, "", domain.ErrPlanStale
	}
	incus, _ := r.cli.statusPorts()
	err := incusclient.StartOriginalInstance(ctx, incus, incusclient.OriginalLifecycle{Project: r.yard.IncusProject, Name: r.yard.YardInstanceName, UUID: r.uuid, Kind: r.yard.YardKind, Weight: r.weight, Inspect: r.instance, HostSafe: r.hostSafe, Publications: r.publications})
	if err != nil {
		return domain.AdapterResult{}, "", err
	}
	return result, "", nil
}

func (r ownerStartPhysical) handoffPublications(ctx context.Context, contracts []ownerpublication.Contract) error {
	instance, err := r.instance(ctx)
	if err != nil {
		return err
	}
	target := ownerpublication.OriginalTarget{Project: r.yard.IncusProject, Name: r.yard.YardInstanceName, UUID: r.uuid, Kind: r.yard.YardKind, Desired: domain.PowerRunning}
	if err := target.Check(instance); err != nil {
		return err
	}
	if _, _, _, err := ownerpublication.Plan(instance, contracts, nil); err != nil {
		return err
	}
	var pending []ownerpublication.Handoff
	// With no deferred receipt the original approved device must already be
	// present exactly; retirement or a merely empty diagnostic cannot close it.
	for _, c := range contracts {
		if instance.LocalConfig[ownerpublication.Prefix+c.Device] == "" {
			continue
		}
		if r.consumer == nil || r.publicationRuntime == nil {
			return domain.ErrPlanStale
		}
		checked, ok := ports.NativeOperationObserverFrom(ctx).(ports.NativeCheckedInvocation)
		if !ok {
			return domain.ErrPlanStale
		}
		intent, recipe, e := checked.CheckedInvocation()
		if e != nil {
			return e
		}
		h, e := ownerpublication.NewHandoff(target, c.Device, ports.NativeInvocationKey(ctx), intent, recipe, *r.consumer, instance, contracts)
		if e != nil {
			return e
		}
		pending = append(pending, h)
	}
	if len(pending) == 0 {
		return r.checkOriginalPublications(instance, contracts)
	}
	incus, _ := r.cli.statusPorts()
	exchange, ok := incus.(interface {
		ExchangePublicationHandoff(context.Context, ownerpublication.Handoff, []ownerpublication.Contract, string, string, func() error) error
	})
	if !ok {
		return domain.ErrPlanStale
	}
	inspect := func(call context.Context) (ports.InstanceInfo, error) {
		i, e := r.instance(call)
		if e != nil {
			return i, e
		}
		for _, h := range pending {
			if e := h.Check(i, contracts, true); e != nil {
				return i, e
			}
		}
		return i, nil
	}
	checkConsumer := func(call context.Context) error {
		if r.guard != nil {
			if e := r.guard(call); e != nil {
				return e
			}
		}
		if e := ownerpublication.RequireReader(r.cli.options.RepositoryRoot, r.yard.Paths.ConfigHome); e != nil {
			return e
		}
		return r.publicationRuntime.CheckPublicationConsumer(call, *r.consumer)
	}
	return ownerpublication.Deliver(ctx, pending, inspect, checkConsumer, func(call context.Context, h ownerpublication.Handoff, before, after string) error {
		return exchange.ExchangePublicationHandoff(call, h, contracts, before, after, func() error { return checkConsumer(call) })
	}, func(call context.Context) error {
		return r.publicationRuntime.StartPublicationConsumer(call, *r.consumer)
	})
}
func (r ownerStartPhysical) checkOriginalPublications(current ports.InstanceInfo, contracts []ownerpublication.Contract) error {
	return ownerpublication.CheckRestoredOriginal(current, r.original, contracts)
}
