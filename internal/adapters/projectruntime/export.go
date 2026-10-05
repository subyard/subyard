package projectruntime

import (
	"context"
	"time"
)

type PatchStore struct {
	Directory string
	Now       func() time.Time
}

func (store PatchStore) Publish(ctx context.Context, projectID string, patch []byte) (string, error) {
	prepared, err := store.Prepare(projectID)
	if err != nil {
		return "", err
	}
	return prepared.Publish(ctx, projectID, patch)
}
