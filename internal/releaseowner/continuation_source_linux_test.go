//go:build linux

package releaseowner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/ports"
)

type completeSourceFixture struct {
	noEffects
	got   *CompleteSourceReference
	calls int
}

func (f *completeSourceFixture) CaptureOwner(ctx context.Context, source *CompleteSourceReference) (OwnerBefore, error) {
	if ports.NativeOperationObserverFrom(ctx) != nil || ports.NativeSelfExecutableFrom(ctx) != nil {
		panic("source fact minted effect authority")
	}
	f.got = source
	f.calls++
	return OwnerBefore{State: "absent"}, nil
}
func TestCaptureOwnerOriginalCompleteReference(t *testing.T) {
	for _, fault := range []string{"original", "missing", "transaction", "digest", "metadata", "unknown", "wrong-verb"} {
		t.Run(fault, func(t *testing.T) {
			s, b, _ := nativeFactFixture(t)
			id, err := IdentityOf(s.root)
			if err != nil {
				t.Fatal(err)
			}
			s.context.Root = id
			fixture := &completeSourceFixture{}
			s.components = fixture
			reference := &CompleteSourceReference{Transaction: "original-source", JournalDigest: strings.Repeat("a", 64), MetadataBinding: strings.Repeat("b", 64)}
			request := Request{Op: "capture-owner", Session: b.Session, CompleteSource: reference}
			switch fault {
			case "missing":
				request.CompleteSource = nil
			case "transaction":
				reference.Transaction = "../foreign"
			case "digest":
				reference.JournalDigest = "wrong"
			case "metadata":
				reference.MetadataBinding = ""
			case "wrong-verb":
				request.Op = "capture-inventory"
			}
			raw, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			if fault == "unknown" {
				raw = []byte(strings.Replace(string(raw), `"transaction":"original-source"`, `"transaction":"original-source","authority":true`, 1))
			}
			_, err = s.handle(context.Background(), raw, nil)
			if fault == "original" || fault == "missing" {
				if err != nil || fixture.calls != 1 {
					t.Fatal("read-only source conveyance failed", err)
				}
				if fault == "original" && *fixture.got != *reference {
					t.Fatal("original reference changed")
				}
			} else if err == nil || fixture.calls != 0 {
				t.Fatal("invalid source reference crossed boundary", err, fixture.calls)
			}
		})
	}
}
