package migration

import (
	"context"
	"errors"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
)

func TestV2LegacyIngressUsesFactsUntilVerifiedThenOnlyStaticHistory(t *testing.T) {
	pair := V1ImportRuntimePair{
		Current: testPublishedV1042Runtime, Previous: testPublishedV1PreviousRuntime,
	}
	result := V1Import{
		Release: V1ImportRelease042, Checkpoint: V1ImportCheckpoint042BrokerRollingBack,
		RuntimePair: pair, FactDigest: importDigestA, StaticDigest: importDigestB,
	}
	reader := &v1ImportReaderPortFixture{full: result, static: result}
	ingress, err := NewV2LegacyIngress(reader, pair)
	if err != nil {
		t.Fatal(err)
	}

	fresh, err := ingress.Inspect(context.Background(), nil)
	if err != nil || len(fresh.Operations) != 1 {
		t.Fatalf("fresh legacy ingress = %#v, err=%v", fresh, err)
	}
	operation := fresh.Operations[0]
	if operation.Kind != releasetransition.V2LegacyV1Import ||
		operation.Expected != importDigestA || operation.Desired != importDigestA ||
		operation.Static != importDigestB || reader.freshCalls != 1 {
		t.Fatalf("fresh legacy operation = %#v reader=%#v", operation, reader)
	}

	releases := releasetransition.ReleasePair{
		From: "0.4.2-17608894ab09", Target: "candidate-a",
		Previous: releaseIDPointer("0.4.0-68b9925f6880"),
	}
	bound := releasetransition.V2IngressBinding{
		Transaction: "tx-legacy-001", Plan: releasetransition.PlanToken(importDigestC),
		Releases: releases,
		Steps: []releasetransition.V2IngressStepBinding{{
			Kind: releasetransition.V2LegacyV1Import, Checkpoint: releasetransition.StepEvidence,
			Expected: importDigestA, Desired: importDigestA, Static: importDigestB,
		}},
	}
	if _, err := ingress.Inspect(context.Background(), &bound); err != nil {
		t.Fatal(err)
	}
	if reader.boundCalls != 1 || reader.staticCalls != 0 {
		t.Fatalf("pre-verified legacy inspection calls = %#v", reader)
	}

	bound.Steps[0].Checkpoint = releasetransition.StepVerified
	verified, err := ingress.Inspect(context.Background(), &bound)
	if err != nil || len(verified.Operations) != 1 ||
		verified.Operations[0].Expected != importDigestA {
		t.Fatalf("verified legacy ingress = %#v, err=%v", verified, err)
	}
	if reader.boundCalls != 1 || reader.staticCalls != 1 {
		t.Fatalf("post-verified legacy inspection re-read mutable facts: %#v", reader)
	}
}

func TestV2LegacyIngressIsIdentityOnlyAndRejectsStaticDrift(t *testing.T) {
	pair := V1ImportRuntimePair{
		Current: testPublishedV1041Runtime, Previous: testPublishedV1PreviousRuntime,
	}
	reader := &v1ImportReaderPortFixture{static: V1Import{
		RuntimePair: pair, StaticDigest: importDigestC,
	}}
	ingress, err := NewV2LegacyIngress(reader, pair)
	if err != nil {
		t.Fatal(err)
	}
	step := releasetransition.V2IngressStep{
		Kind:     releasetransition.V2LegacyV1Import,
		Expected: importDigestA, Desired: importDigestA, Static: importDigestB,
		Binding: releasetransition.V2IngressBinding{
			Transaction: "tx-legacy-001", Plan: releasetransition.PlanToken(importDigestC),
			Releases: releasetransition.ReleasePair{
				From: "0.4.1-fc5b03078508", Target: "candidate-a",
				Previous: releaseIDPointer("0.4.0-68b9925f6880"),
			},
			Steps: []releasetransition.V2IngressStepBinding{{
				Kind: releasetransition.V2LegacyV1Import, Checkpoint: releasetransition.StepVerified,
				Expected: importDigestA, Desired: importDigestA, Static: importDigestB,
			}},
		},
	}
	if err := ingress.Apply(context.Background(), step); !errors.Is(err, ErrV1ImportStaticDrift) {
		t.Fatalf("legacy identity apply drift error = %v", err)
	}
}

func TestV2CompatibilityIngressSkipsLegacyLeafWithoutJournaledLegacyStep(t *testing.T) {
	reader := &v1ImportReaderPortFixture{}
	legacy, err := NewV2LegacyIngress(reader, V1ImportRuntimePair{Current: "releases/source-a"})
	if err != nil {
		t.Fatal(err)
	}
	ingress := &V2CompatibilityIngress{Legacy: legacy}
	binding := releasetransition.V2IngressBinding{
		Transaction: "tx-source-only-001", Plan: releasetransition.PlanToken(importDigestC),
		Releases: releasetransition.ReleasePair{From: "source-a", Target: "candidate-a"},
		Steps: []releasetransition.V2IngressStepBinding{{
			Kind: releasetransition.V2SourceImport, Checkpoint: releasetransition.StepIntent,
			Expected: importDigestA, Desired: importDigestB,
		}},
	}
	result, err := ingress.Inspect(context.Background(), &binding)
	if err != nil || len(result.Operations) != 0 ||
		reader.freshCalls != 0 || reader.boundCalls != 0 || reader.staticCalls != 0 {
		t.Fatalf("source-only bound inspection = %#v, reader=%#v, err=%v", result, reader, err)
	}
}

var (
	importDigestA = releasetransition.Fingerprint("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	importDigestB = releasetransition.Fingerprint("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	importDigestC = releasetransition.Fingerprint("cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")
)

type v1ImportReaderPortFixture struct {
	full, static                        V1Import
	freshCalls, boundCalls, staticCalls int
}

func (fixture *v1ImportReaderPortFixture) InspectFresh(
	context.Context,
	V1ImportRuntimePair,
) (V1Import, bool, error) {
	fixture.freshCalls++
	return fixture.full, true, nil
}

func (fixture *v1ImportReaderPortFixture) InspectBound(
	context.Context,
	V1ImportRuntimePair,
) (V1Import, error) {
	fixture.boundCalls++
	return fixture.full, nil
}

func (fixture *v1ImportReaderPortFixture) InspectBoundStatic(
	context.Context,
	V1ImportRuntimePair,
) (V1Import, error) {
	fixture.staticCalls++
	return fixture.static, nil
}

func releaseIDPointer(value releasetransition.ReleaseID) *releasetransition.ReleaseID {
	return &value
}
