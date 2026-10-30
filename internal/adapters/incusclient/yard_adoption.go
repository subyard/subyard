package incusclient

import (
	"context"
	"errors"
	"maps"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
)

// AdoptProject uses the ETag returned by the same read that checked the exact
// approved configuration. A concurrent project edit cannot be overwritten.
func (client *Client) AdoptProject(ctx context.Context, project string, before, metadata map[string]string) error {
	if !domain.SafeName(project) || project == "default" || metadata["user.subyard.installation"] != domain.AdoptedInstallation || metadata["user.subyard.adopted_uuid"] == "" {
		return domain.ErrPlanStale
	}
	server, err := client.reconcileServer(ctx)
	if err != nil {
		return err
	}
	actual, etag, err := server.GetProject(project)
	if err != nil {
		return normalizeError("read adoption project", err)
	}
	if actual == nil || actual.Name != project || etag == "" || !maps.Equal(actual.Config, before) {
		return domain.ErrPlanStale
	}
	desired := maps.Clone(before)
	for key, value := range metadata {
		if !strings.HasPrefix(key, "user.subyard.") || key == ownerCreateTokenKey {
			return domain.ErrPlanStale
		}
		desired[key] = value
	}
	if maps.Equal(before, desired) {
		return nil
	}
	request := actual.Writable()
	request.Config = desired
	if err := server.UpdateProject(project, request, etag); err != nil {
		return errors.Join(domain.ErrPlanStale, normalizeError("publish adoption project", err))
	}
	verified, _, err := server.GetProject(project)
	if err != nil {
		return normalizeError("verify adoption project", err)
	}
	if verified == nil || !maps.Equal(verified.Config, desired) {
		return domain.ErrPlanStale
	}
	return nil
}
