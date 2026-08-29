package migration

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Subyard/Subyard/internal/releasetransition"
)

var ErrV1ImportStaticDrift = errors.New("legacy v1 import history changed")

type V1ImportSource interface {
	InspectFresh(context.Context, V1ImportRuntimePair) (V1Import, bool, error)
	InspectBound(context.Context, V1ImportRuntimePair) (V1Import, error)
	InspectBoundStatic(context.Context, V1ImportRuntimePair) (V1Import, error)
}

type V2LegacyIngress struct {
	reader V1ImportSource
	fresh  V1ImportRuntimePair
}

func NewV2LegacyIngress(
	reader V1ImportSource,
	fresh V1ImportRuntimePair,
) (*V2LegacyIngress, error) {
	if reader == nil {
		return nil, errors.New("legacy v1 ingress requires an import reader")
	}
	if fresh.Current == "" {
		return nil, errors.New("legacy v1 ingress requires an observed runtime pair")
	}
	return &V2LegacyIngress{reader: reader, fresh: fresh}, nil
}

func (ingress *V2LegacyIngress) Inspect(
	ctx context.Context,
	binding *releasetransition.V2IngressBinding,
) (releasetransition.V2IngressInspection, error) {
	if ingress == nil || ingress.reader == nil {
		return releasetransition.V2IngressInspection{}, errors.New("legacy v1 ingress is unavailable")
	}
	if binding == nil {
		result, found, err := ingress.reader.InspectFresh(ctx, ingress.fresh)
		if err != nil || !found {
			return releasetransition.V2IngressInspection{}, err
		}
		return v1ImportInspection(result, result.FactDigest, result.StaticDigest), nil
	}
	step, err := legacyV1BoundStep(*binding)
	if err != nil {
		return releasetransition.V2IngressInspection{}, err
	}
	pair, err := v1PairFromReleases(binding.Releases)
	if err != nil {
		return releasetransition.V2IngressInspection{}, err
	}
	if step.Checkpoint == releasetransition.StepVerified {
		result, inspectErr := ingress.reader.InspectBoundStatic(ctx, pair)
		if inspectErr != nil {
			return releasetransition.V2IngressInspection{}, inspectErr
		}
		if step.Static == "" || result.StaticDigest != step.Static {
			return releasetransition.V2IngressInspection{}, ErrV1ImportStaticDrift
		}
		return v1ImportInspection(result, step.Expected, step.Static), nil
	}
	result, err := ingress.reader.InspectBound(ctx, pair)
	if err != nil {
		return releasetransition.V2IngressInspection{}, err
	}
	return v1ImportInspection(result, result.FactDigest, result.StaticDigest), nil
}

func (ingress *V2LegacyIngress) Observe(
	ctx context.Context,
	step releasetransition.V2IngressStep,
) (releasetransition.Fingerprint, error) {
	if step.Kind != releasetransition.V2LegacyV1Import {
		return "", errors.New("legacy v1 ingress received an unsupported operation")
	}
	bound, err := legacyV1BoundStep(step.Binding)
	if err != nil {
		return "", err
	}
	pair, err := v1PairFromReleases(step.Binding.Releases)
	if err != nil {
		return "", err
	}
	if bound.Checkpoint == releasetransition.StepVerified {
		result, inspectErr := ingress.reader.InspectBoundStatic(ctx, pair)
		if inspectErr != nil {
			return "", inspectErr
		}
		if step.Static == "" || result.StaticDigest != step.Static || bound.Static != step.Static {
			return "", ErrV1ImportStaticDrift
		}
		return step.Expected, nil
	}
	result, err := ingress.reader.InspectBound(ctx, pair)
	if err != nil {
		return "", err
	}
	if result.FactDigest != step.Expected || result.StaticDigest != step.Static {
		return "", errors.New("legacy v1 import facts changed outside the authorized transition")
	}
	return step.Expected, nil
}

func (ingress *V2LegacyIngress) Apply(
	ctx context.Context,
	step releasetransition.V2IngressStep,
) error {
	actual, err := ingress.Observe(ctx, step)
	if err != nil {
		return err
	}
	if actual != step.Expected || step.Expected != step.Desired {
		return errors.New("legacy v1 import is not an identity operation")
	}
	return nil
}

func (ingress *V2LegacyIngress) Verify(
	ctx context.Context,
	step releasetransition.V2IngressStep,
) error {
	actual, err := ingress.Observe(ctx, step)
	if err != nil {
		return err
	}
	if actual != step.Desired {
		return errors.New("legacy v1 import identity could not be verified")
	}
	return nil
}

func v1ImportInspection(
	result V1Import,
	facts releasetransition.Fingerprint,
	static releasetransition.Fingerprint,
) releasetransition.V2IngressInspection {
	return releasetransition.V2IngressInspection{
		Operations: []releasetransition.V2IngressOperation{{
			Kind: releasetransition.V2LegacyV1Import, Decision: releasetransition.DecisionCanonicalize,
			Expected: facts, Desired: facts, Static: static,
		}},
		Decisions: []releasetransition.RedactedDecision{{
			Resource: "legacy-v1.journal", Scope: "legacy-v1",
			Decision: releasetransition.DecisionCanonicalize,
			Result:   "forward-recovery",
		}},
	}
}

func legacyV1BoundStep(
	binding releasetransition.V2IngressBinding,
) (releasetransition.V2IngressStepBinding, error) {
	var result *releasetransition.V2IngressStepBinding
	for index := range binding.Steps {
		if binding.Steps[index].Kind != releasetransition.V2LegacyV1Import {
			continue
		}
		if result != nil {
			return releasetransition.V2IngressStepBinding{}, errors.New("legacy v1 ingress binding is duplicated")
		}
		copy := binding.Steps[index]
		result = &copy
	}
	if result == nil {
		return releasetransition.V2IngressStepBinding{}, errors.New("legacy v1 ingress binding is missing")
	}
	return *result, nil
}

func v1PairFromReleases(
	releases releasetransition.ReleasePair,
) (V1ImportRuntimePair, error) {
	if releases.Previous == nil {
		return V1ImportRuntimePair{}, errors.New("legacy v1 ingress release pair has no previous runtime")
	}
	current := string(releases.From)
	previous := string(*releases.Previous)
	if strings.HasPrefix(current, "releases/") || strings.HasPrefix(previous, "releases/") {
		return V1ImportRuntimePair{}, fmt.Errorf("legacy v1 ingress release IDs are not canonical")
	}
	return V1ImportRuntimePair{
		Current: "releases/" + current, Previous: "releases/" + previous,
	}, nil
}
