package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/profile"
)

// Explicit start waits only after physical power, desired intent and startup
// effects have completed. Temporary inspection boots and repair restarts do not
// pass here: their callers must still reach account/service reconciliation.
func (cli *CLI) waitForStartedProfiles(ctx context.Context, loaded config.Loaded, started time.Time, diagnostics io.Writer) error {
	declared := false
	for _, definition := range loaded.Catalog.Profiles() {
		declared = declared || definition.StartupReadiness != ""
	}
	if !declared {
		return nil
	}
	readinessDeadline := time.Now().Add(profile.ReadinessTimeout)
	inspection, cancel := context.WithDeadline(ctx, readinessDeadline)
	defer cancel()
	marker, err := config.ReadPersistentFileSnapshot(loaded.Context.Paths.ConfigHome,
		config.DeveloperIdentityMarkerPath(loaded.Context.Paths.ConfigHome, loaded.Context.YardName))
	if err != nil {
		return fmt.Errorf("yard started (desired=running) but identity readiness is unavailable: %w", err)
	}
	if marker.Exists {
		return nil // Rename recovery owns the account and its quiesced services.
	}
	target, _, err := cli.appliedIntegrationTargetBaseline(inspection, configTarget{Name: loaded.Context.YardName, Loaded: loaded})
	if err != nil {
		return fmt.Errorf("yard started (desired=running) but installed profile context is unavailable: %w", err)
	}
	if target.installed == nil {
		return nil
	}
	identity, settings := target.installed.applied.Identity, target.installed.applied.Settings
	if (identity["DEV_USER"] == "" || identity["DEV_UID"] == "") && (settings["DEV_USER"] == "" || settings["DEV_UID"] == "") {
		return nil // Missing applied account evidence is stable init repair work.
	}
	environment := environmentMap(environmentList(cli.env, structuredCommandContext(target.Loaded)))
	if !started.IsZero() {
		environment[profile.StartupRemainingEnvironment] = strconv.FormatInt(max(0, time.Until(readinessDeadline).Milliseconds()), 10)
	}
	fmt.Fprintln(diagnostics, "  waiting for installed profile services")
	if err := profile.RunServices(ctx, cli.options.RepositoryRoot, []string{"--wait-ready"}, environment, io.Discard, io.Discard); err != nil {
		return fmt.Errorf("yard started (desired=running) but profile readiness is pending: %w", err)
	}
	return nil
}
