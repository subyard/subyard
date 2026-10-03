package credentialruntime

import (
	"path/filepath"
	"testing"
)

func TestShippedImportExclusionsMatchAnyConjunction(t *testing.T) {
	runtime := credentialFixture(t)
	writeCredentialFile(t, filepath.Join(runtime.config.RepositoryRoot, "config", "profiles", "exclusions", "profile.json"), `{"schema_version":1,"credential_import_exclusions":[["/mutable/","/sessions/"],["/single-store/"]]}`, 0o600)
	configured, err := New(runtime.config)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		path    string
		blocked bool
	}{
		{"mutable/demo/sessions/key.bin", true},
		{"sessions/demo/mutable/key.bin", true},
		{"single-store/key.bin", true},
		{"mutable/demo/private/key.bin", false},
		{"other/demo/sessions/key.bin", false},
		{"mutable-other/demo/sessions/key.bin", false},
	} {
		path := filepath.Join(runtime.config.RepositoryRoot, "sources", item.path)
		writeCredentialFile(t, path, "synthetic file", 0o600)
		if _, err := configured.validateImportPath(path); (err != nil) != item.blocked {
			t.Fatalf("path=%s blocked=%t err=%v", item.path, item.blocked, err)
		}
		if _, err := runtime.validateImportPath(path); err != nil {
			t.Fatalf("undeclared exclusion changed core behavior for %s: %v", item.path, err)
		}
	}
}
