package releaseowner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type originalOwnerProgress struct {
	noEffects
	actual string
	before OwnerBefore
	token  string
	calls  int
}

func (f *originalOwnerProgress) CaptureOwner(context.Context) (OwnerBefore, error) {
	panic("owner progress must not prepare a replacement")
}
func (f *originalOwnerProgress) ObserveOwner(_ context.Context, before OwnerBefore, token string) (string, error) {
	f.calls++
	f.before = before
	f.token = token
	return f.actual, nil
}

func TestOwnerProgressUsesOriginalPreparationWithoutClaimingEffects(t *testing.T) {
	for _, actual := range []string{"expected", "in-progress", "desired", "unknown"} {
		t.Run(actual, func(t *testing.T) {
			s, b, _ := nativeFactFixture(t)
			identity, err := IdentityOf(s.root)
			if err != nil {
				t.Fatal(err)
			}
			s.context.Root = identity
			reader := &originalOwnerProgress{actual: actual}
			s.components = reader
			before := OwnerBefore{State: "legacy-flat", Registration: b.Artifact, Overrides: b.Artifact, Controller: b.Artifact, PreparedRecipe: b.Artifact}
			request := Request{Op: "observe-owner", Session: b.Session, Before: &before, Token: strings.Repeat("a", 32)}
			raw, _ := json.Marshal(request)
			reply, err := s.handle(context.Background(), raw, nil)
			if actual == "unknown" {
				if err == nil {
					t.Fatal("unrecognized progress admitted")
				}
				return
			}
			if err != nil || reply.Actual != actual || reader.calls != 1 || reader.before != before || reader.token != request.Token {
				t.Fatal("original owner observation changed", err)
			}
			if len(s.leases) != 0 {
				t.Fatal("readonly observation claimed effect custody")
			}
		})
	}
}

func TestOwnerProgressRejectsMissingCapabilityAndMalformedOriginal(t *testing.T) {
	for _, kind := range []string{"missing", "token", "recipe", "before", "session", "extra"} {
		t.Run(kind, func(t *testing.T) {
			s, b, _ := nativeFactFixture(t)
			identity, err := IdentityOf(s.root)
			if err != nil {
				t.Fatal(err)
			}
			s.context.Root = identity
			reader := &originalOwnerProgress{actual: "desired"}
			s.components = reader
			request := Request{Op: "observe-owner", Session: b.Session, Before: &OwnerBefore{State: "legacy-flat", PreparedRecipe: b.Artifact}, Token: strings.Repeat("a", 32)}
			switch kind {
			case "missing":
				s.components = noEffects{}
			case "token":
				request.Token = "not-original"
			case "recipe":
				request.Before.PreparedRecipe = "not-sha"
			case "before":
				request.Before = nil
			case "session":
				request.Session = strings.Repeat("f", 64)
			}
			raw, _ := json.Marshal(request)
			if kind == "extra" {
				var fields map[string]any
				json.Unmarshal(raw, &fields)
				fields["target"] = map[string]string{"owner": "arbitrary"}
				raw, _ = json.Marshal(fields)
			}
			if _, err := s.handle(context.Background(), raw, nil); err == nil {
				t.Fatal("malformed observation admitted")
			}
			if reader.calls != 0 {
				t.Fatal("malformed observation reached owning component")
			}
		})
	}
}

func TestOwnerProgressAfterEffectUsesOriginalPreparation(t *testing.T) {
	for _, actual := range []string{"expected", "in-progress", "desired", "unknown", "missing"} {
		t.Run(actual, func(t *testing.T) {
			s, b, _ := nativeFactFixture(t)
			reader := &originalOwnerProgress{actual: actual}
			s.components = reader
			if actual == "missing" {
				s.components = noEffects{}
			}
			before := OwnerBefore{State: "legacy-flat", Registration: b.Artifact, PreparedRecipe: b.Artifact}
			r := Request{Op: "apply-owner", Binding: &b, Before: &before, Token: strings.Repeat("a", 32)}
			recipe, err := recipeFingerprint(r)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.writeFact(b, "intent", Intent{Version: 1, Binding: b, Owner: "owner-registration", Resource: "test-yard", Expected: b.Artifact, Desired: b.Artifact, Source: b.Artifact, Recipe: recipe}); err != nil {
				t.Fatal(err)
			}
			reply, err := s.apply(context.Background(), r, "owner-registration", "test-yard", func() error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			if actual == "desired" {
				if reply.Terminal == nil || reply.Terminal.State != "complete" {
					t.Fatal("original desired progress not completed")
				}
			} else if reply.Code != "unknown" || reply.Terminal != nil {
				t.Fatal("unproved owner progress completed")
			}
			if actual != "missing" && (reader.calls != 1 || reader.before != before || reader.token != r.Token) {
				t.Fatal("postcondition replaced original preparation")
			}
		})
	}
}
