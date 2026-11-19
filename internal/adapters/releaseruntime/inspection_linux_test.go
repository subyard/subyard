package releaseruntime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
)

func TestInstallationObservationNeverWaitsForTheOwnerLock(t *testing.T) {
	runtime := New(Config{Inputs: fixtureCommandInputs(map[string]string{"HOME": testkit.TempDir(t)})})
	defer runtime.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, release, err := uninstall.OwnerLock(ctx, runtime.ownerDataHome(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	request := &releasetransition.ProcessRequest{Mode: releasetransition.ProcessInspect, RuntimeRoot: "/runtime", Target: "1.2.3-abc"}
	unlock, err := runtime.lockReleaseTransition(context.WithValue(ctx, observationKey{}, true), "", request)
	if unlock != nil || !errors.Is(err, domain.ErrPlanStale) || ctx.Err() != nil {
		t.Fatalf("observation waited for or bypassed the held owner lock: %v", err)
	}
}
