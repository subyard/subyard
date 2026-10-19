//go:build linux

package releaseowner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/ports"
	"golang.org/x/sys/unix"
)

func TestNativeOwnerRestorationIsBoundAndNeverTerminal(t *testing.T) {
	s, b, path := nativeFactFixture(t)
	token := strings.Repeat("1", 32)
	intent := Intent{Version: 1, Binding: b, Owner: "owner-registration", Resource: "test-yard", Expected: b.Artifact, Desired: b.Artifact, Source: b.Artifact, Recipe: b.Artifact}
	if err := s.writeFact(b, "intent", intent); err != nil {
		t.Fatal(err)
	}
	accepted := Terminal{Version: 1, Binding: b, Owner: intent.Owner, Resource: intent.Resource, State: "accepted", Operations: []ports.NativeOperation{}}
	if err := s.writeFact(b, "accepted", accepted); err != nil {
		t.Fatal(err)
	}
	o := &invocationObserver{session: s, binding: b, ownerToken: token}
	member := ports.NativeOwnerMember{Device: 1, Inode: 2, UID: uint32(os.Getuid()), Mode: unix.S_IFREG | 0600, Links: 1, Size: 25, ModifiedSeconds: 10, ChangedSeconds: 11, Digest: b.Artifact}
	foreign := member
	foreign.UID++
	if validOwnerRestorationMember(foreign) {
		t.Fatal("foreign owner restoration member admitted")
	}
	if err := o.RecordOwnerRestoration(token, b.Artifact, member); err != nil {
		t.Fatal(err)
	}
	if err := o.RecordOwnerRestoration(token, b.Artifact, member); err != nil {
		t.Fatal("idempotent original result refused", err)
	}
	got, err := o.ReadOwnerRestoration(token, b.Artifact)
	if err != nil || got != member {
		t.Fatal("original result changed", err)
	}
	if _, err := os.Stat(filepath.Join(path, factName(b.Ordinal, "terminal"))); !os.IsNotExist(err) {
		t.Fatal("restoration invented invocation closure", err)
	}
	if _, err := o.ReadOwnerRestoration(strings.Repeat("2", 32), b.Artifact); err == nil {
		t.Fatal("different token admitted")
	}
	if _, err := o.ReadOwnerRestoration(token, strings.Repeat("2", 64)); err == nil {
		t.Fatal("different original member admitted")
	}
	member.ChangedNanos++
	if err := o.RecordOwnerRestoration(token, b.Artifact, member); err == nil {
		t.Fatal("conflicting result overwritten")
	}
	if o.closed() == nil {
		t.Fatal("conflicting result did not poison admission")
	}
}

func TestNativeFrameKindIsClosed(t *testing.T) {
	for kind := 0; kind < 256; kind++ {
		if got, want := nativeFrameKind(byte(kind)), kind >= 1 && kind <= 21; got != want {
			t.Fatalf("kind %d accepted=%v", kind, got)
		}
	}
}
