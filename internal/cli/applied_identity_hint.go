package cli

import (
	"context"
	"errors"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

func (cli *CLI) publishAppliedIdentityHint(ctx context.Context, loaded config.Loaded) error {
	if cli.options.InitPlatform != nil || loaded.Context.AccessKind != domain.AccessLocal {
		return nil
	}
	runtime := cli.initPlatform(loaded, nil).(reconcileruntime.Runtime)
	instance, err := runtime.Incus.Instance(ctx, loaded.Context.IncusProject, loaded.Context.YardInstanceName)
	if errors.Is(err, ports.ErrInstanceNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !strings.EqualFold(instance.Status, "running") {
		return nil
	}
	user, uid, err := runtime.VerifiedAppliedDeveloperIdentity(ctx)
	if err != nil {
		return err
	}
	source, uuid, err := runtime.DeveloperIdentityObservationBinding(ctx)
	if err != nil {
		return err
	}
	release, err := identityReleaseBinding(loaded.Context.Paths.ConfigHome)
	if err != nil {
		return err
	}
	_, before, err := config.ReadAppliedIdentityHint(loaded.Context.Paths.ConfigHome, loaded.Context.YardName)
	if err != nil {
		return err
	}
	hint := config.AppliedIdentityHint{Schema: 1, Yard: loaded.Context.YardName, UUID: uuid, Source: source, Release: release, User: user, UID: uid}
	return config.PublishAppliedIdentityHint(loaded.Context.Paths.ConfigHome, hint, before)
}
