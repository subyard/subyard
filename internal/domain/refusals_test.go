package domain

import (
	"errors"
	"fmt"
	"testing"
)

func TestCollectRefusalsIsStableAndKeepsClassification(t *testing.T) {
	if CollectRefusals(nil, nil) != nil {
		t.Fatal("no causes produced an error")
	}
	lone := errors.New("lone")
	if CollectRefusals(nil, lone) != lone {
		t.Fatal("a lone cause must stay the same error")
	}
	stale := fmt.Errorf("zeta: %w", ErrPlanStale)
	alpha := errors.New("alpha")
	joined := CollectRefusals(stale, alpha, errors.New("alpha"))
	if joined.Error() != "alpha; zeta: "+ErrPlanStale.Error() || !errors.Is(joined, ErrPlanStale) || !errors.Is(joined, alpha) {
		t.Fatal(joined)
	}
}
