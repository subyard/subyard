package profile

import "testing"

func TestCleanupCacheDeclaration(t *testing.T) {
	paths, err := CleanupCaches("/srv/cache/a /home/dev/.tool/caches", "dev")
	if err != nil || len(paths) != 2 {
		t.Fatal(paths, err)
	}
	for _, value := range []string{"/tmp", "/srv/cache", "/srv/workspaces/id/src", "/srv/cache/../workspaces", "/home/dev", "/home/dev/.ssh/keys", "/home/dev/.subyard-runtime/operations", "/srv/cache/.subyard-runtime/ssh", "/home/dev/.codex/cache", "/home/dev/.local/state/subyard"} {
		if _, err := CleanupCaches(value, "dev"); err == nil {
			t.Errorf("accepted unsafe cache %q", value)
		}
	}
	if paths, err := CleanupCaches("", "dev"); err != nil || len(paths) != 0 {
		t.Fatal(paths, err)
	}
}
