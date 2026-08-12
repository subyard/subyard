package application

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

type StatusService struct {
	Incus        ports.Incus
	Executor     ports.InstanceExecutor
	Store        ports.ProjectStore
	Facts        ports.StatusFactsReader
	ProbeTimeout time.Duration
}

func (service StatusService) Read(ctx context.Context, yard domain.Context) (domain.YardStatus, error) {
	if service.Incus == nil || service.Store == nil || service.Facts == nil {
		return domain.YardStatus{}, errors.New("status service requires Incus, state and facts ports")
	}
	if _, err := service.Incus.Server(ctx); err != nil {
		return domain.YardStatus{}, err
	}
	instance, err := service.Incus.Instance(ctx, yard.IncusProject, yard.YardInstanceName)
	if err != nil {
		return domain.YardStatus{}, err
	}
	running := strings.EqualFold(instance.Status, "running")
	records, err := service.Store.List(ctx)
	if err != nil {
		return domain.YardStatus{}, err
	}
	facts, err := service.Facts.ReadStatusFacts(ctx, yard, running)
	if err != nil {
		return domain.YardStatus{}, err
	}
	status := domain.YardStatus{
		Context: yard, State: strings.ToUpper(instance.Status),
		ResolvedYardImage: domain.ResolvedYardImage(instance.Config["volatile.base_image"]),
		Desired:           valueOr(instance.Config["user.subyard.desired_power"], "unmanaged"),
		Initialized:       valueOr(instance.Config["user.subyard.initialized"], "no"),
		IncusAutostart:    instance.Config["boot.autostart"],
		ProjectCount:      len(records), Facts: facts,
	}
	status.SSHConfigured = localSSHConfigured(ctx, yard, instance)
	for name := range instance.Devices {
		if strings.HasPrefix(name, "host-") {
			status.Mounts = append(status.Mounts, name)
		}
	}
	sort.Strings(status.Mounts)
	if running && service.Executor != nil {
		status.IP = service.execLine(ctx, yard, []string{"sh", "-c", `ip -4 -o addr show eth0 2>/dev/null | awk '{print $4}' | cut -d/ -f1`}, nil)
		status.Services, status.VSCode = "?", "?"
		details := service.execLine(ctx, yard, []string{"sh", "-c", `
d="/home/$DU"
printf 'services=%s\n' "$(systemctl is-active ssh docker 2>/dev/null | tr '\n' '/' | sed 's:/$::')"
printf 'vscode=key=%s server=%s git-id=%s\n' \
  "$([ -s "$d/.ssh/authorized_keys" ] && echo yes || echo no)" \
  "$([ -d "$d/.vscode-server" ] && echo yes || echo not-yet)" \
  "$([ -s "$d/.gitconfig" ] && echo yes || echo no)"
`}, map[string]string{"DU": yard.DevUser})
		for _, line := range strings.Split(details, "\n") {
			if value, ok := strings.CutPrefix(line, "services="); ok {
				status.Services = value
			}
			if value, ok := strings.CutPrefix(line, "vscode="); ok {
				status.VSCode = value
			}
		}
	}
	return status, nil
}

func (service StatusService) execLine(
	ctx context.Context,
	yard domain.Context,
	command []string,
	environment map[string]string,
) string {
	timeout := service.ProbeTimeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result, err := service.Executor.Exec(probeCtx, yard.IncusProject, yard.YardInstanceName,
		ports.InstanceExecRequest{Command: command, Environment: environment})
	if err != nil || result.ExitCode != 0 || probeCtx.Err() != nil {
		return "?"
	}
	return strings.TrimSpace(string(result.Stdout))
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
