package releasetransition

import (
	"errors"
	"github.com/Subyard/Subyard/internal/config"
	"reflect"
	"testing"
)

type preparedInputPathViewFixture struct {
	paths   []string
	failure error
	content map[string]config.PersistentFileSnapshot
}

func (view preparedInputPathViewFixture) ReadPreparedInput(path string) (config.PersistentFileSnapshot, bool, error) {
	snapshot, exists := view.content[path]
	return snapshot, exists, view.failure
}

func TestPreparedInputReaderForwardsOnlyCapturedFinalSnapshots(t *testing.T) {
	base := preparedInputPathViewFixture{content: map[string]config.PersistentFileSnapshot{"/config/asset": {Exists: true, Content: []byte("captured")}, "/config/config.env": {Exists: true, Content: []byte("before")}}}
	view := settingsViewAfterPlan("/config", base, []settingsV2FilePlan{{Path: "/config/config.env", Desired: []byte("final")}}).(V2PreparedInputReader)
	for path, want := range map[string]string{"/config/config.env": "final", "/config/asset": "captured"} {
		snapshot, matched, err := view.ReadPreparedInput(path)
		if err != nil || !matched || string(snapshot.Content) != want {
			t.Fatalf("captured final snapshot %s: matched=%t content=%q err=%v", path, matched, snapshot.Content, err)
		}
	}
	if _, matched, err := view.ReadPreparedInput("/config/native-instructions"); err != nil || matched {
		t.Fatal("uncaptured native input acquired prepared authority", matched, err)
	}
}

func (view preparedInputPathViewFixture) ListYards() ([]string, error) { return nil, nil }
func (view preparedInputPathViewFixture) ReadSnapshot(string) (config.PersistentFileSnapshot, error) {
	return config.PersistentFileSnapshot{}, nil
}
func (view preparedInputPathViewFixture) PreparedInputPaths() ([]string, error) {
	return append([]string(nil), view.paths...), view.failure
}
func TestPreparedInputPathsForwardFinalSettingsOverlay(t *testing.T) {
	base := preparedInputPathViewFixture{paths: []string{"/config/asset", "/config/config.env"}}
	view := settingsViewAfterPlan("/config", base, []settingsV2FilePlan{{Path: "/config/yards/named/config.env", Desired: []byte("canonical")}, {Path: "/config/config.env", Desired: []byte("changed")}})
	paths, err := view.(V2PreparedInputPaths).PreparedInputPaths()
	if err != nil || !reflect.DeepEqual(paths, []string{"/config/asset", "/config/config.env", "/config/yards/named/config.env"}) {
		t.Fatalf("inventory %v err %v", paths, err)
	}
	snapshot, err := view.ReadSnapshot("/config/config.env")
	if err != nil || string(snapshot.Content) != "changed" {
		t.Fatal("inventory did not retain final view", err)
	}
	failure := errors.New("prepared inventory unavailable")
	view = settingsViewAfterPlan("/config", preparedInputPathViewFixture{failure: failure}, []settingsV2FilePlan{{Path: "/config/config.env"}})
	if _, err := view.(V2PreparedInputPaths).PreparedInputPaths(); !errors.Is(err, failure) {
		t.Fatal("lost inventory failure", err)
	}
}
