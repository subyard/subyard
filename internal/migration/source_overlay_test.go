package migration

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestDiscoverSourceInstallExactAllowlist(t *testing.T) {
	root := ownedOverlayTempDir(t)
	files := map[string]string{
		"private/config.env":                    "AGENT_codex_RULES=\"$SUBYARD_CONFIG_DIR/../private/agents/codex/rules/repo.rules\"\n",
		"private/yards/demo.env":                "SSH_PORT=2223\n",
		"private/agents/codex/rules/repo.rules": "rule\n",
		"config/profiles/openclaw/profile.env":  "TOKEN=profile\n",
		"config/staging/demo.conf":              "PROFILE=openclaw\n",
		"config/staging/demo.env":               "TOKEN=staging\n",
		"config/prod-fingerprints":              "abc\n",
		"config/qa-pool/broker.conf":            "PORT=1\n",
		"config/qa-pool/secrets.env":            "TOKEN=qa\n",
		"config/qa-pool/pool.jsonl":             "{}\n",
		"config/staging/demo.conf.example":      "not runtime\n",
		"config/qa-pool/secrets.env.example":    "not runtime\n",
		"config/unrelated.local":                "not allowlisted\n",
	}
	for path, contents := range files {
		writeOverlayFixture(t, filepath.Join(root, path), contents)
	}
	manifest, err := DiscoverSourceInstall(root)
	if err != nil {
		t.Fatal(err)
	}
	var sources []string
	for _, entry := range manifest.Entries {
		sources = append(sources, entry.Source)
		if entry.Mode != "0600" || entry.ConflictPolicy != "identical-or-fail" {
			t.Fatalf("unsafe manifest contract: %#v", entry)
		}
	}
	sort.Strings(sources)
	want := []string{
		"config/prod-fingerprints",
		"config/profiles/openclaw/profile.env",
		"config/qa-pool/broker.conf",
		"config/qa-pool/pool.jsonl",
		"config/qa-pool/secrets.env",
		"config/staging/demo.conf",
		"config/staging/demo.env",
		"private/agents/codex/rules/repo.rules",
		"private/config.env",
		"private/yards/demo.env",
	}
	sort.Strings(want)
	if !reflect.DeepEqual(sources, want) {
		t.Fatalf("manifest sources = %#v, want %#v", sources, want)
	}
}

func TestDiscoverSourceInstallRejectsUnsafeAndUnsupportedInputs(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*testing.T, string)
		want string
	}{
		{
			name: "unsupported-reference",
			edit: func(t *testing.T, root string) {
				writeOverlayFixture(t, filepath.Join(root, "private/config.env"),
					"AGENT_codex_RULES=\"$SUBYARD_CONFIG_DIR/unsupported.rules\"\n")
				writeOverlayFixture(t, filepath.Join(root, "config/unsupported.rules"), "rule\n")
			},
			want: "references unsupported source-install input",
		},
		{
			name: "agent-symlink",
			edit: func(t *testing.T, root string) {
				writeOverlayFixture(t, filepath.Join(root, "private/config.env"), "DEV_SUDO=1\n")
				writeOverlayFixture(t, filepath.Join(root, "outside"), "rule\n")
				if err := os.MkdirAll(filepath.Join(root, "private/agents/codex"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(root, "outside"),
					filepath.Join(root, "private/agents/codex/rules")); err != nil {
					t.Fatal(err)
				}
			},
			want: "contains a symlink",
		},
		{
			name: "writable-source",
			edit: func(t *testing.T, root string) {
				path := filepath.Join(root, "config/staging/demo.env")
				writeOverlayFixture(t, path, "TOKEN=x\n")
				if err := os.Chmod(path, 0o622); err != nil {
					t.Fatal(err)
				}
			},
			want: "group/world writable",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := ownedOverlayTempDir(t)
			test.edit(t, root)
			if _, err := DiscoverSourceInstall(root); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestDiscoverSourceInstallIncludesPreviousOverlayAndFlatYards(t *testing.T) {
	source := ownedOverlayTempDir(t)
	dataHome := ownedOverlayTempDir(t)
	configHome := ownedOverlayTempDir(t)
	writeOverlayFixture(t, filepath.Join(dataHome, "config.env"),
		"AGENT_codex_RULES=\"$SUBYARD_CONFIG_DIR/../private/agents/codex/repo.rules\"\n")
	writeOverlayFixture(t,
		filepath.Join(dataHome, "operator-overlay/private/agents/codex/repo.rules"), "rule\n")
	writeOverlayFixture(t,
		filepath.Join(dataHome, "operator-overlay/config/staging/demo.env"), "TOKEN=legacy\n")
	writeOverlayFixture(t,
		filepath.Join(dataHome, "operator-overlay/config/unknown.input"), "unknown\n")
	writeOverlayFixture(t, filepath.Join(configHome, "yards/demo.env"),
		"YARD_TEMPLATE=e2e-vms\nSSH_PORT=3333\n")

	manifest, err := DiscoverSourceInstallWithRoots(source, dataHome, configHome)
	if err != nil {
		t.Fatal(err)
	}
	destinations := map[string]SourceInstallEntry{}
	for _, entry := range manifest.Entries {
		destinations[entry.Destination] = entry
	}
	for _, destination := range []string{
		"config.env",
		"overrides/host/agents/codex/repo.rules",
		"secrets/legacy/staging/demo.env",
		"secrets/legacy/operator-overlay/config/unknown.input",
		"yards/demo/config.env",
	} {
		if _, exists := destinations[destination]; !exists {
			t.Fatalf("previous installed input did not migrate to %s: %#v", destination, manifest.Entries)
		}
	}
	if destinations["secrets/legacy/staging/demo.env"].AuthoritativeDestination !=
		"generated/staging/demo.env" {
		t.Fatalf("legacy staging compatibility contract is missing: %#v",
			destinations["secrets/legacy/staging/demo.env"])
	}
	if destinations["yards/demo/config.env"].ContentTransform !=
		ContentTransformRetiredE2EVMTemplate {
		t.Fatalf("retired yard template has no typed transform: %#v",
			destinations["yards/demo/config.env"])
	}
}

func TestNormalizeLegacyYardConfigContent(t *testing.T) {
	payload, err := NormalizeLegacyYardConfigContent(
		[]byte("# retained\nexport YARD_TEMPLATE = 'e2e-vms'\nSSH_PORT=3333\n"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(payload), "# retained\nYARD_TEMPLATE=test-vms\nSSH_PORT=3333\n"; got != want {
		t.Fatalf("normalized yard config = %q, want %q", got, want)
	}
}

func TestNormalizeLegacyYardConfigContentRejectsAmbiguousInput(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload []byte
	}{
		{name: "oversized", payload: bytes.Repeat([]byte{'x'}, MaxLegacyYardConfigBytes+1)},
		{name: "dynamic", payload: []byte("PROFILE=e2e-vms\nYARD_TEMPLATE=$PROFILE\n")},
		{name: "duplicate", payload: []byte("YARD_TEMPLATE=e2e-vms\nYARD_TEMPLATE=e2e-vms\n")},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload, err := NormalizeLegacyYardConfigContent(test.payload)
			if err == nil || payload != nil {
				t.Fatalf("NormalizeLegacyYardConfigContent() = %q, %v; want nil output and error", payload, err)
			}
		})
	}
}

func TestIgnoredRuntimeInputsHaveExplicitMigrationDisposition(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	payload, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, line := range strings.Split(string(payload), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "/config/") || strings.HasPrefix(line, "!/config/") {
			got = append(got, line)
		}
	}
	want := []string{
		"/config/profiles/*.env",
		"/config/profiles/*/profile.env",
		"/config/staging/*.conf",
		"/config/staging/*.env",
		"!/config/staging/*.example",
		"/config/prod-fingerprints",
		"/config/qa-pool/*",
		"!/config/qa-pool/*.example",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("gitignored config inputs changed without a migration disposition:\ngot  %#v\nwant %#v", got, want)
	}
}

func writeOverlayFixture(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func ownedOverlayTempDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}
