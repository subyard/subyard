package application

import (
	"context"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/sshrelay"
)

func localSSHConfigured(ctx context.Context, yard domain.Context, instance ports.InstanceInfo) bool {
	if yard.YardKind != domain.YardVM {
		_, configured := instance.Devices["ssh"]
		return configured
	}
	address := instance.Devices["eth0"]["ipv4.address"]
	return sshrelay.Ready(ctx, "/etc/systemd/system", 0,
		yard.SSHPort, address, sshrelay.SystemctlPath())
}
