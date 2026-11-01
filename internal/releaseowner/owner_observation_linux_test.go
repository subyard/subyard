package releaseowner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/ports"
)

type ownerObservationFixture struct {
	noEffects
	value OwnerBefore
	calls int
}

func (f *ownerObservationFixture) CaptureOwnerObservation(ctx context.Context) (OwnerBefore, error) {
	if ports.NativeOperationObserverFrom(ctx) != nil || ports.NativeSelfExecutableFrom(ctx) != nil {
		panic("observation gained effect authority")
	}
	f.calls++
	return f.value, nil
}
func TestOwnerObservationClosedReadOnlyCapability(t *testing.T) {
	for _, fault := range []string{"absent", "current", "legacy", "unknown", "invalid", "prepared", "session", "extra", "unsupported"} {
		t.Run(fault, func(t *testing.T) {
			s, b, _ := nativeFactFixture(t)
			id, err := IdentityOf(s.root)
			if err != nil {
				t.Fatal(err)
			}
			s.context.Root = id
			f := &ownerObservationFixture{value: OwnerBefore{State: "absent"}}
			s.components = f
			r := Request{Op: "capture-owner-observation", Session: b.Session}
			switch fault {
			case "current", "legacy":
				f.value = OwnerBefore{State: "current", Registration: b.Artifact, Overrides: b.Artifact, Controller: b.Artifact}
				if fault == "legacy" {
					f.value.State = "legacy-directory+overrides"
				}
			case "unknown":
				f.value.State = "unknown"
			case "invalid":
				f.value.Controller = b.Artifact
			case "prepared":
				f.value.PreparedRecipe = b.Artifact
			case "session":
				r.Session = strings.Repeat("f", 64)
			case "unsupported":
				s.components = noEffects{}
			}
			raw, _ := json.Marshal(r)
			if fault == "extra" {
				raw = append(raw[:len(raw)-1], []byte(`,"completeSource":null}`)...)
			}
			reply, err := s.handle(context.Background(), raw, nil)
			valid := fault == "absent" || fault == "current" || fault == "legacy"
			if valid {
				if err != nil || reply.Owner == nil || *reply.Owner != f.value || f.calls != 1 {
					t.Fatal("readonly observation", err)
				}
			} else if err == nil {
				t.Fatal("invalid observation admitted")
			}
			if (fault == "session" || fault == "extra" || fault == "unsupported") && f.calls != 0 {
				t.Fatal("invalid request reached reader")
			}
			if len(s.leases) != 0 {
				t.Fatal("observation acquired effect custody")
			}
		})
	}
}
