package profile

import (
	"github.com/Subyard/Subyard/internal/testkit"
	"os"
	"path/filepath"
	"testing"
)

func TestRetiredDeclarationHasNoExecutableAuthority(t *testing.T) {
	root := testkit.TempDir(t)
	if err := os.Mkdir(filepath.Join(root, "config"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config/retired-components.json")
	for _, content := range []string{
		`{"schema_version":2,"components":[]}`,
		`{"schema_version":1,"components":[{"kind":"profile","id":"../foreign"}]}`,
		`{"schema_version":1,"components":[{"kind":"profile","id":"old","handler":"cleanup.sh"}]}`,
		`{"schema_version":1,"components":[{"kind":"profile","id":"old"},{"kind":"profile","id":"old"}]}`,
		`{"schema_version":1,"components":[{"kind":"integration","id":"old","adoption_keys":["security.nesting"]}]}`,
		`{"schema_version":1,"components":[{"kind":"integration","id":"old","settings":["AGENT_foreign_COMMAND"]}]}`,
		`{"schema_version":1,"components":[{"kind":"profile","id":"old","route":{"marker_key":"foreign","device_name":"old-dashboard","guest_port":3000}}]}`,
	} {
		testkit.WriteFile(t, path, []byte(content), 0644)
		if _, err := LoadRetired(root); err == nil {
			t.Fatalf("invalid declaration accepted: %s", content)
		}
	}
}
