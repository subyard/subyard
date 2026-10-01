package releaseruntime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

// Inspect the installed layout before handing rollback to a retained engine:
// older v2 transition owners do not know that settings may live in the Git cache.
func (runtime *Runtime) inspectConfigLayoutRollback(ctx context.Context, target *verifiedPublishedCandidate, request releasetransition.ProcessRequest) (*releasetransition.Outcome, error) {
	if request.Direction != releasetransition.DirectionActivatePrevious {
		return nil, nil
	}
	manifestPath := filepath.Join(request.ConfigHome, ".sync", "manifest.json")
	if _, err := os.Lstat(manifestPath); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, errors.New("cannot safely inspect configuration layout for rollback")
	}
	snapshot, err := config.ReadPersistentFileSnapshot(request.ConfigHome, manifestPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("cannot safely inspect configuration layout for rollback")
	}
	if !snapshot.Exists {
		return nil, nil
	}
	if len(snapshot.Content) > maximumCandidateResponseBytes {
		return nil, errors.New("configuration layout manifest exceeds the rollback inspection bound")
	}
	var manifest struct {
		SchemaVersion int `json:"schemaVersion"`
		Files         []struct {
			Path string `json:"path"`
		} `json:"files"`
	}
	if err := json.Unmarshal(snapshot.Content, &manifest); err != nil {
		return nil, errors.New("configuration layout manifest cannot be inspected safely")
	}
	if manifest.SchemaVersion < 2 || len(manifest.Files) == 0 {
		return nil, nil
	}
	requiresCache := false
	for _, file := range manifest.Files {
		if strings.HasPrefix(file.Path, config.GitSettingsRelativePath+"/") {
			requiresCache = true
		}
	}
	if !requiresCache {
		return nil, nil
	}
	var output boundedResponseBuffer
	metadataErr := runtime.runVerifiedCandidate(ctx, target, request.RuntimeRoot, []string{"--command-options", "config"}, nil, &output, "")
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	local, git := false, false
	if target.commandManifestBound && metadataErr == nil && output.Len() <= maximumCandidateResponseBytes {
		for _, option := range strings.Fields(output.String()) {
			local = local || option == "--local"
			git = git || option == "--git"
		}
	}
	if local && git {
		return nil, nil
	}
	links, err := runtime.inspectRuntimeLinks(request.RuntimeRoot)
	if err != nil {
		return nil, err
	}
	outcome := publicReleaseOutcome(releaseLinksFromRuntimeSnapshot(links), request.Target, nil, releasetransition.CodeRollbackIncompatible,
		"the retained release cannot read the installed local-first Git configuration cache",
		"retain a release supporting local and Git configuration settings, then run yard update --rollback")
	return &outcome, nil
}
