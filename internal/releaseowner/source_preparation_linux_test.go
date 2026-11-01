package releaseowner

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/sys/unix"
)

func TestNativeServePreparationImageAndCurrentInteractionArePrivate(t *testing.T) {
	root := testkit.TempDir(t)
	directory, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	id, err := IdentityOf(directory)
	if err != nil {
		t.Fatal(err)
	}
	interaction, err := os.OpenFile(root+"/interaction", os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer interaction.Close()
	if err := interaction.Chmod(0o600); err != nil {
		t.Fatal(err)
	}
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	parent := os.NewFile(uintptr(pair[0]), "preparation parent")
	child := os.NewFile(uintptr(pair[1]), "preparation helper")
	defer child.Close()
	captured := make(chan Context, 1)
	done := make(chan error, 1)
	go func() {
		done <- Serve(context.Background(), child, child, func(ctx context.Context, c Context, _ *os.File) (Components, error) {
			if ports.NativeOperationObserverFrom(ctx) != nil || ports.NativeOwnerHome(ctx) != "" || ports.NativeInvocationKey(ctx) != "" {
				panic("preparation received effect authority")
			}
			captured <- c
			return noEffects{}, nil
		})
	}()
	peer := nativeTestPeer{t: t, socket: parent}
	a := strings.Repeat("a", 64)
	c := Context{Session: a, Artifact: a, DataHome: root, ConfigHome: root, RuntimeRoot: root, RepositoryRoot: root, Yard: "default", Target: "next", Root: id, Repository: id}
	peer.request(Request{Op: "initialize", Context: &c}, directory, directory, interaction)
	got := <-captured
	if got.PreparationExecutable == nil || got.Interaction == nil {
		t.Fatal("private preparation roles missing")
	}
	image, digest, err := got.PreparationExecutable.Borrow()
	if err != nil {
		t.Fatal(err)
	}
	defer image.Close()
	flags, err := unix.FcntlInt(image.Fd(), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 || len(digest) != 64 {
		t.Fatal("Serve image proof not retained CLOEXEC", err)
	}
	if _, err := got.Interaction.WriteString("synthetic device UI"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(interaction.Name())
	if err != nil || string(raw) != "synthetic device UI" {
		t.Fatal("current caller output was not retained", err)
	}
	wire, _ := json.Marshal(got)
	var public map[string]json.RawMessage
	_ = json.Unmarshal(wire, &public)
	_, hasImage := public["PreparationExecutable"]
	_, hasInteraction := public["Interaction"]
	if hasImage || hasInteraction {
		t.Fatal("private factory role escaped JSON")
	}
	parent.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve failed to drain")
	}
	if _, _, err := got.PreparationExecutable.Borrow(); err == nil {
		t.Fatal("session image survived close")
	}
	if _, err := got.Interaction.Stat(); err == nil {
		t.Fatal("interaction survived session close")
	}
}
