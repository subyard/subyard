package cli

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/adapters/transport"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerinventory"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

func (cli *CLI) remoteReleaseReadiness(ctx context.Context, endpoint string) *domain.ReleaseReadiness {
	process, err := transport.SSH("ssh", endpoint, 3*time.Second)
	if err != nil {
		return nil
	}
	process.Timeout = remoteYardStatusTimeout
	readiness, err := (ownerinventory.Client{Transport: process}).ReleaseReadiness(ctx)
	if err != nil {
		return nil
	}
	return readiness
}

func (cli *CLI) summaryReleaseReadiness(ctx context.Context, loaded config.Loaded, results []ownerInventoryResult) []*domain.ReleaseReadiness {
	readiness := make([]*domain.ReleaseReadiness, len(results))
	if len(results) == 0 {
		return readiness
	}
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		readiness[0] = cli.releaseReadiness(ctx)
	}()
	connections, err := (ownerinventory.Connections{
		Root: loaded.Context.Paths.DataHome + "/owner-inventory",
	}).ListReadOnly()
	if err != nil {
		wait.Wait()
		return readiness
	}
	for index, result := range results[1:] {
		if result.err != nil {
			continue
		}
		endpoint := ""
		for _, connection := range connections {
			if connection.HostID == result.inventory.HostID {
				endpoint = connection.Destination
				break
			}
		}
		if endpoint == "" {
			endpoint = cli.discoveredOwners[result.inventory.HostID].Destination
		}
		if endpoint != "" {
			wait.Add(1)
			go func(index int, endpoint string) {
				defer wait.Done()
				readiness[index+1] = cli.remoteReleaseReadiness(ctx, endpoint)
			}(index, endpoint)
		}
	}
	wait.Wait()
	return readiness
}

func (cli *CLI) releaseReadiness(ctx context.Context) *domain.ReleaseReadiness {
	unknown := &domain.ReleaseReadiness{
		Status: "unknown", Message: "installed release readiness could not be verified",
		Next: "yard migrate --check",
	}
	options, available, err := cli.mutationGateReleaseOptions()
	if err != nil || !available {
		return unknown
	}
	runtimeConfig := cli.releaseRuntimeConfig(releaseruntime.RuntimeContext{
		DataHome: options.DataHome, ConfigHome: options.ConfigHome, RuntimeRoot: options.RuntimeRoot,
	})
	runtimeConfig.Stdout, runtimeConfig.Stderr, runtimeConfig.Progress = io.Discard, io.Discard, io.Discard
	runtime := releaseruntime.New(runtimeConfig)
	defer runtime.Close()
	probeCtx, cancel := context.WithTimeout(ctx, profile.ReadinessTimeout)
	defer cancel()
	outcome, err := runtime.InspectCurrentReadiness(probeCtx, options.ConfigHome)
	if err != nil || probeCtx.Err() != nil {
		return unknown
	}
	return &domain.ReleaseReadiness{
		Status: string(outcome.Status), Code: string(outcome.Code),
		Message: outcome.Message, Next: outcome.Retry,
	}
}

func (cli *CLI) printReleaseReadiness(readiness *domain.ReleaseReadiness, ownerEndpoint string) {
	if readiness == nil {
		readiness = &domain.ReleaseReadiness{
			Status: "unknown", Message: "owner did not report installed release readiness",
			Next: "yard migrate --check",
		}
	}
	if readiness.Status == string(releasetransition.StatusReady) {
		fmt.Fprintln(cli.options.Stdout, "  release  OK (ready)")
		return
	}
	if readiness.Status == "unknown" {
		fmt.Fprintln(cli.options.Stdout, "  release  unknown")
	} else {
		fmt.Fprintf(cli.options.Stdout, "  release  NOT OK (%s)\n", readiness.Status)
	}
	if readiness.Message != "" {
		fmt.Fprintf(cli.options.Stdout, "    %s\n", readiness.Message)
	}
	if readiness.Code != "" {
		fmt.Fprintf(cli.options.Stdout, "    code: %s\n", readiness.Code)
	}
	if readiness.Next != "" {
		location := ""
		if ownerEndpoint != "" {
			location = " on owner " + ownerEndpoint
		}
		fmt.Fprintf(cli.options.Stdout, "    next%s: %s\n", location, readiness.Next)
	}
}
