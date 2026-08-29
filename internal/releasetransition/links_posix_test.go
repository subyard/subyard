package releasetransition

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRuntimeLinkStoreResumesForwardActivation(t *testing.T) {
	root := runtimeLinkFixture(t, "release-a", "release-old", "release-b")
	store, err := NewRuntimeLinkStore(root)
	if err != nil {
		t.Fatal(err)
	}
	pair := ReleasePair{
		From: "release-a", Previous: releaseIDPointer("release-old"), Target: "release-b",
	}
	injected := errors.New("crash after staged link")
	store.fault = func(point string) error {
		if point == "after-staged-link" {
			return injected
		}
		return nil
	}
	if _, err := store.Activate(pair); !errors.Is(err, injected) {
		t.Fatalf("fault error = %v", err)
	}
	intermediate, err := store.Observe()
	if err != nil || intermediate.Active != "release-a" ||
		intermediate.Previous == nil || *intermediate.Previous != "release-b" {
		t.Fatalf("intermediate links = %#v, err=%v", intermediate, err)
	}
	store.fault = nil
	activated, err := store.Activate(pair)
	if err != nil || activated.Active != "release-b" ||
		activated.Previous == nil || *activated.Previous != "release-a" {
		t.Fatalf("activated links = %#v, err=%v", activated, err)
	}
}

func TestRuntimeLinkStoreRollsBackWithAtomicExchange(t *testing.T) {
	root := runtimeLinkFixture(t, "release-b", "release-a")
	store, err := NewRuntimeLinkStore(root)
	if err != nil {
		t.Fatal(err)
	}
	pair := ReleasePair{
		From: "release-b", Previous: releaseIDPointer("release-a"), Target: "release-a",
	}
	activated, err := store.Activate(pair)
	if err != nil || activated.Active != "release-a" ||
		activated.Previous == nil || *activated.Previous != "release-b" {
		t.Fatalf("rollback links = %#v, err=%v", activated, err)
	}
}

func TestRuntimeLinkStoreRejectsForeignLinkState(t *testing.T) {
	root := runtimeLinkFixture(t, "release-a", "release-old", "release-b", "foreign")
	store, _ := NewRuntimeLinkStore(root)
	if err := os.Remove(filepath.Join(root, "previous")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/foreign", filepath.Join(root, "previous")); err != nil {
		t.Fatal(err)
	}
	_, err := store.Activate(ReleasePair{
		From: "release-a", Previous: releaseIDPointer("release-old"), Target: "release-b",
	})
	if err == nil {
		t.Fatal("foreign link state was overwritten")
	}
}

func runtimeLinkFixture(t *testing.T, current, previous string, releases ...string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "runtime")
	if err := os.MkdirAll(filepath.Join(root, "releases"), 0o700); err != nil {
		t.Fatal(err)
	}
	all := append([]string{current, previous}, releases...)
	for _, release := range all {
		if release == "" {
			continue
		}
		if err := os.Mkdir(filepath.Join(root, "releases", release), 0o700); err != nil &&
			!errors.Is(err, os.ErrExist) {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("releases/"+current, filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	if previous != "" {
		if err := os.Symlink("releases/"+previous, filepath.Join(root, "previous")); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
