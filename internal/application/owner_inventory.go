package application

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

type OwnerYardSource interface {
	HostID(context.Context) (string, error)
	Yards(context.Context) ([]domain.Context, error)
	Projects(context.Context, domain.Context) ([]domain.ProjectRecord, error)
	Runtime(context.Context, domain.Context) (string, domain.ResolvedYardImage, error)
}

type OwnerInventoryBuilder struct {
	Source OwnerYardSource
	Clock  ports.Clock
}

func (builder OwnerInventoryBuilder) Read(ctx context.Context) (domain.OwnerInventory, error) {
	if builder.Source == nil {
		return domain.OwnerInventory{}, fmt.Errorf("owner inventory source is required")
	}
	hostID, err := builder.Source.HostID(ctx)
	if err != nil {
		return domain.OwnerInventory{}, err
	}
	yards, err := builder.Source.Yards(ctx)
	if err != nil {
		return domain.OwnerInventory{}, err
	}
	now := time.Now()
	if builder.Clock != nil {
		now = builder.Clock.Now()
	}
	result := domain.OwnerInventory{
		Schema: domain.OwnerInventorySchema, HostID: hostID, ObservedAt: now.UTC(),
		Yards: make([]domain.OwnerYard, 0, len(yards)),
	}
	for _, yard := range yards {
		if yard.AccessKind != domain.AccessLocal {
			continue
		}
		records, err := builder.Source.Projects(ctx, yard)
		if err != nil {
			return domain.OwnerInventory{}, fmt.Errorf("read projects for yard %q: %w", yard.YardName, err)
		}
		state, resolvedImage, err := builder.Source.Runtime(ctx, yard)
		if err != nil {
			return domain.OwnerInventory{}, fmt.Errorf("read state for yard %q: %w", yard.YardName, err)
		}
		state = strings.ToUpper(state)
		entry := domain.OwnerYard{
			Name: yard.YardName, Kind: string(yard.YardKind), Instance: yard.YardInstanceName,
			State: state, SSHPort: yard.SSHPort, DevUser: yard.DevUser,
			YardImageRef: yard.YardImageRef, ResolvedYardImage: resolvedImage,
			Projects: make([]domain.OwnerProject, 0, len(records)),
		}
		for _, record := range records {
			entry.Projects = append(entry.Projects, domain.OwnerProject{
				ProjectID: record.ProjectID, IdentityVersion: record.IdentityVersion,
				Name: record.Name, Mode: string(record.Mode), Target: record.Target,
				SourceKey: record.SourceKey,
			})
		}
		sort.Slice(entry.Projects, func(i, j int) bool {
			if entry.Projects[i].Name != entry.Projects[j].Name {
				return entry.Projects[i].Name < entry.Projects[j].Name
			}
			return entry.Projects[i].ProjectID < entry.Projects[j].ProjectID
		})
		result.Yards = append(result.Yards, entry)
	}
	sort.Slice(result.Yards, func(i, j int) bool { return result.Yards[i].Name < result.Yards[j].Name })
	if err := result.Validate(); err != nil {
		return domain.OwnerInventory{}, err
	}
	return result, nil
}
