package releaseowner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"golang.org/x/sys/unix"
)

func (s *session) captureRollbackSupport(ctx context.Context, request Request, files []*os.File) (Reply, error) {
	repositories := request.RollbackRepositories
	if repositories == nil || len(files) != 2 || len(request.TargetOwners) > 256 || !slices.IsSorted(request.TargetOwners) {
		return Reply{}, errors.New("native rollback repositories")
	}
	for index, owner := range request.TargetOwners {
		if !domain.SafeID(owner) || (index > 0 && request.TargetOwners[index-1] == owner) {
			return Reply{}, errors.New("native rollback owner syntax")
		}
	}
	if repositories.Source.Release != string(s.context.Releases.From) || repositories.Target.Release != s.context.Target || repositories.Target.Release != string(s.context.Releases.Target) || (repositories.Source.Release == repositories.Target.Release && (s.context.Direction != releasetransition.DirectionActivatePrevious || repositories.Source != repositories.Target)) || !domain.SafeID(repositories.Source.Release) || !fingerprint(repositories.Source.ArtifactDigest) || repositories.Target.ArtifactDigest != s.context.Artifact || (repositories.Target.Root != s.context.Repository && s.context.Direction != releasetransition.DirectionActivatePrevious) {
		return Reply{}, errors.New("native rollback repository binding")
	}
	retained := []*os.File{}
	defer func() { closeFiles(retained) }()
	for index, repository := range []RollbackRepository{repositories.Source, repositories.Target} {
		actual, err := IdentityOf(files[index])
		if err != nil || actual != repository.Root || actual.Mode&unix.S_IFMT != unix.S_IFDIR || actual.Mode&0o022 != 0 || (actual.UID != uint32(os.Getuid()) && actual.UID != 0) {
			return Reply{}, errors.New("native rollback repository descriptor identity")
		}
		file, err := retain(files[index])
		if err != nil {
			return Reply{}, err
		}
		retained = append(retained, file)
	}
	reader, ok := s.components.(interface {
		CaptureRollbackSupport(context.Context, string, string, []string) (ports.RollbackSupport, error)
	})
	if !ok {
		return Reply{}, errors.New("native rollback component reader unavailable")
	}
	facts, err := reader.CaptureRollbackSupport(ctx, fmt.Sprintf("/proc/self/fd/%d", retained[0].Fd()), fmt.Sprintf("/proc/self/fd/%d", retained[1].Fd()), request.TargetOwners)
	if err != nil {
		return Reply{}, err
	}
	if !fingerprint(facts.Binding) {
		return Reply{}, errors.New("native rollback component binding")
	}
	raw, err := json.Marshal(struct {
		Session      string               `json:"session"`
		Repositories RollbackRepositories `json:"repositories"`
		Component    string               `json:"component"`
	}{s.context.Session, *repositories, facts.Binding})
	if err != nil {
		return Reply{}, err
	}
	facts.Binding = digest(raw)
	return Reply{RollbackSupport: &facts}, nil
}
