package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestOwnerSourceDiagnosticKeepsLabelsAndOtherRefusals(t *testing.T) {
	source := func(role string) error { return OwnerMetadataError("", role, "differs", "observed=local", nil) }
	inner := domain.CollectRefusals(source("identity"), errors.New("zeta"))
	err := domain.CollectRefusals(fmt.Errorf("yard one: %w", inner), fmt.Errorf("yard two: %w", source("settings")))
	err = fmt.Errorf("verify yard adoption: %w: %w", err, domain.ErrPlanStale)
	want := "verify yard adoption: yard one: identity: differs; observed=local; zeta; yard two: settings: differs; observed=local: " + domain.ErrPlanStale.Error()
	if local, ok := OwnerSourceDiagnostic(err); !ok || local != want {
		t.Fatalf("ok=%t\n got: %s\nwant: %s", ok, local, want)
	}
	if strings.Contains(err.Error(), "observed=local") {
		t.Fatal("default rendering exposed owner-local details", err)
	}
	same := func(details string) error { return OwnerMetadataError("", "identity", "differs", details, nil) }
	err = domain.CollectRefusals(fmt.Errorf("yard one: %w", same("observed=ONE")), fmt.Errorf("yard two: %w", same("observed=TWO")))
	want = "yard one: identity: differs; observed=ONE; yard two: identity: differs; observed=TWO"
	if local, ok := OwnerSourceDiagnostic(err); !ok || local != want {
		t.Fatalf("same text swapped details: ok=%t\n got: %s\nwant: %s", ok, local, want)
	}
	remote := fmt.Errorf("verify: %w", domain.CollectRefusals(errors.New("one"), errors.New("two")))
	if _, ok := OwnerSourceDiagnostic(remote); ok {
		t.Fatal("aggregate without owner-local causes claimed local details")
	}
}

func TestSourceDiagnosticDoesNotRenderMalformedInput(t *testing.T) {
	const secret = "SYNTHETIC_PRIVATE_DIAGNOSTIC"
	for _, fixture := range []struct {
		name, content, key, reason string
	}{
		{"required parameter", "SSH_PORT=${SSH_PORT:?" + secret + "}\n", "SSH_PORT", "only literal values"},
		{"invalid parameter", "SSH_PORT=${" + secret + "!}\n", "SSH_PORT", "only literal values"},
		{"invalid variable", secret + "!=value\n", "", "invalid variable name"},
		{"unknown setting", secret + "=value\n", "", "unknown setting"},
		{"invalid mount", "HOST_MOUNTS=" + secret + "\n", "HOST_MOUNTS", "invalid mount list"},
		{"mixed quotes", "GIT_USER_NAME='start'$" + secret + "'end'\n", "GIT_USER_NAME", "Replace it with one literal value"},
		{"unterminated value", "SSH_PORT='" + secret + "\n", "SSH_PORT", "unterminated quoted assignment"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			root := testkit.TempDir(t)
			path := filepath.Join(root, "config.env")
			testkit.WriteFile(t, path, []byte("# source location\n"+fixture.content), 0o600)
			values := environment{}
			err := applyEnvFileValidated(path, values, ScopeHost, false, nil)
			if err == nil {
				t.Fatal("malformed configuration was accepted")
			}
			diagnostic, ok := SourceDiagnostic(root, fmt.Errorf("outer context: %w", err))
			if !ok || !strings.Contains(diagnostic, "config.env:2") ||
				!strings.Contains(diagnostic, "local scalar settings") || !strings.Contains(diagnostic, fixture.reason) {
				t.Fatalf("source diagnostic = %q, recognized=%v", diagnostic, ok)
			}
			if strings.Contains(diagnostic, secret) || strings.Contains(err.Error(), secret) || strings.Contains(diagnostic, root) {
				t.Fatalf("source diagnostic exposed private input: %q", diagnostic)
			}
			if fixture.key != "" && !strings.Contains(diagnostic, fixture.key) {
				t.Fatalf("canonical key missing from diagnostic: %q", diagnostic)
			}
			if fixture.name == "required parameter" {
				var source *SourceError
				if !errors.As(err, &source) || source.Unwrap() == nil {
					t.Fatal("source error lost its wrapped cause")
				}
			}
		})
	}
}

func TestSourceAliasRepairRetryIdentifiesOnlySupportedLocalSource(t *testing.T) {
	root := testkit.TempDir(t)
	for _, test := range []struct{ path, key, scope string }{
		{filepath.Join(root, "config.env"), "YARD_IMAGE", "host"},
		{filepath.Join(root, "overrides", "shared", "config.env"), "ENVIRONMENT_PROFILES", "shared"},
		{filepath.Join(root, "yards", "demo", "config.env"), "CODING_TOOL_INTEGRATIONS", "yard"},
	} {
		cause := &SourceError{Path: test.path, Key: test.key, Reason: "conflicting legacy and canonical settings", cause: errors.New("SECRET_MARKER")}
		retry := SourceAliasRepairRetry(root, "demo", cause)
		if !strings.Contains(retry, "config repair-alias "+test.key+" CHOSEN_VALUE --scope "+test.scope+" --local --check") || strings.Contains(retry, "SECRET_MARKER") {
			t.Fatalf("repair instruction is unsafe or inaccessible: %q", retry)
		}
		cause.Key = "SSH_PORT"
		if SourceAliasRepairRetry(root, "demo", cause) != "" {
			t.Fatal("unrelated setting gained repair authority")
		}
	}
}

func TestLoadSourceDiagnosticLocalizesValidation(t *testing.T) {
	home := testkit.TempDir(t)
	configHome := filepath.Join(home, "config")
	if err := os.Mkdir(configHome, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(configHome, "config.env")
	testkit.WriteFile(t, path, []byte("# invalid setting\nSSH_PORT=70000\n"), 0o600)
	_, err := Load(LoadOptions{
		RepositoryRoot: filepath.Join("..", ".."), OperatorHome: home,
		Environment: map[string]string{"SUBYARD_CONFIG_HOME": configHome},
	})
	diagnostic, ok := SourceDiagnostic(configHome, err)
	if !ok || !strings.Contains(diagnostic, "config.env:2: SSH_PORT") ||
		!strings.Contains(diagnostic, "port value") || strings.Contains(diagnostic, "70000") {
		t.Fatalf("loaded source diagnostic = %q, recognized=%v", diagnostic, ok)
	}
	if _, ok := SourceDiagnostic(configHome, errors.New("arbitrary private cause")); ok {
		t.Fatal("arbitrary error was accepted as safe context")
	}
}

func TestPersistentParserSourceDiagnosticCanonicalizesAndRedacts(t *testing.T) {
	root := testkit.TempDir(t)
	path := filepath.Join(root, GitSettingsRelativePath, "yards", "fixture", "config.env")
	_, err := ParsePersistentAssignments(path, []byte("# source\nAGENTS=${MISSING:?synthetic-private-error}\n"))
	diagnostic, ok := SourceDiagnostic(root, err)
	if !ok || !strings.Contains(diagnostic, ".sync/settings/yards/fixture/config.env:2") ||
		!strings.Contains(diagnostic, "CODING_TOOL_INTEGRATIONS") ||
		!strings.Contains(diagnostic, "Git persistent settings") || strings.Contains(diagnostic, "synthetic-private-error") {
		t.Fatalf("persistent parser source diagnostic = %q, recognized=%v", diagnostic, ok)
	}
	outside := filepath.Join(filepath.Dir(root), "synthetic-private-path", "config.env")
	_, err = ParsePersistentAssignments(outside, []byte("SSH_PORT='unterminated\n"))
	diagnostic, ok = SourceDiagnostic(root, err)
	if !ok || strings.Contains(diagnostic, "synthetic-private-path") ||
		!strings.Contains(diagnostic, "configuration source outside config home:1") {
		t.Fatalf("external source diagnostic = %q, recognized=%v", diagnostic, ok)
	}
}

func TestPersistentSourceDiagnosticsDistinguishSafetyChecks(t *testing.T) {
	for _, fixture := range []struct {
		name, reason string
		prepare      func(*testing.T, string)
	}{
		{"mode", "target is group/world writable", func(t *testing.T, path string) {
			// Another hard link keeps the writable file from being repaired.
			testkit.WriteFile(t, path, []byte("SSH_PORT=2222\n"), 0o622)
			if err := os.Link(path, path+".alias"); err != nil {
				t.Fatal(err)
			}
		}},
		{"type", "regular non-symlink file", func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink", "unsafe file type", func(t *testing.T, path string) {
			testkit.WriteFile(t, path+".target", nil, 0o600)
			if err := os.Symlink(path+".target", path); err != nil {
				t.Fatal(err)
			}
		}},
		{"ownership", "target is not operator-owned", func(t *testing.T, path string) {
			if os.Geteuid() != 0 {
				t.Skip("ownership fixture requires root")
			}
			testkit.WriteFile(t, path, []byte("SSH_PORT=2222\n"), 0o600)
			if err := os.Chown(path, 1, -1); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			root := testkit.TempDir(t)
			path := filepath.Join(root, "config.env")
			fixture.prepare(t, path)
			_, err := ReadPersistentFileSnapshot(root, path)
			diagnostic, ok := SourceDiagnostic(root, err)
			if !ok || !strings.Contains(diagnostic, "config.env") || !strings.Contains(diagnostic, fixture.reason) {
				t.Fatalf("persistent source diagnostic = %q, recognized=%v", diagnostic, ok)
			}
			if fixture.name == "mode" && (!strings.Contains(diagnostic, "observed type=regular, mode=0622") || !strings.Contains(diagnostic, "mask=0022")) {
				t.Fatal(diagnostic)
			}
		})
	}
}

func TestOwnerMetadataDiagnosticsStayLocalAndPreserveCauses(t *testing.T) {
	if quoted := QuoteMetadataPath("/invalid/https://credential:SECRET@host/path"); strings.Contains(quoted, "SECRET") || !strings.Contains(quoted, "unavailable") {
		t.Fatal(quoted)
	}
	path := "/owner/" + strings.Repeat("x", 300) + "\n\x1b[31m"
	cause := &os.PathError{Op: "stat", Path: "PRIVATE_CAUSE", Err: os.ErrNotExist}
	err := OwnerMetadataError(path, "installation inventory", MetadataAccessReason(cause), "configHome: stored="+QuoteMetadataPath("/stored\n\x1b")+", current="+QuoteMetadataPath("/current"), cause)
	err = OwnerMetadataContext(path, "installation inventory", fmt.Errorf("outer: %w", err))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing cause lost")
	}
	var pathError *os.PathError
	if !errors.As(err, &pathError) {
		t.Fatal("typed path cause lost")
	}
	for _, public := range []string{err.Error(), func() string { text, _ := SourceDiagnostic("/config", err); return text }(), func() string { text, _ := SourceDiagnostic("", err); return text }()} {
		for _, private := range []string{"/owner/", "/stored", "/current", "PRIVATE_CAUSE"} {
			if strings.Contains(public, private) {
				t.Fatalf("public diagnostic exposed local facts: %q", public)
			}
		}
	}
	local, ok := OwnerSourceDiagnostic(err)
	if !ok || !strings.Contains(local, `configHome: stored="/stored\n\x1b"`) || !strings.Contains(local, `current="/current"`) || !strings.Contains(local, "source is missing") || len(local) > 600 || strings.ContainsAny(local, "\n\x1b") || strings.Contains(local, "PRIVATE_CAUSE") {
		t.Fatalf("unsafe or incomplete local diagnostic: %q", local)
	}
	for _, test := range []struct {
		cause error
		want  string
	}{{os.ErrPermission, "permission denied"}, {errors.New("SECRET"), "cannot read source"}} {
		if got := MetadataAccessReason(test.cause); !strings.Contains(got, test.want) || strings.Contains(got, "SECRET") {
			t.Fatal(got)
		}
	}
}

func TestDiagnoseRendersEveryClassifiedRefusalWithoutValues(t *testing.T) {
	root := testkit.TempDir(t)
	if err := os.MkdirAll(filepath.Join(root, "profiles", "route-client"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "profiles", "route-client", "profile.conf"), []byte("REQUIRES_HOST_ACCESS=1\n"), 0o644)
	prior := []error{
		fmt.Errorf("%w in /secret/SENTINEL", domain.NewConfigRefusal(domain.ConfigRoleForbidsCodingTools)),
		namedRefusal(domain.ConfigRoleForbidsSetting, domain.ConfigSettingName, "AGENT_SAMPLE_SOURCE"),
	}
	err := validateProfileConstraints(root, "default", environment{
		"ENVIRONMENT_PROFILES": "route-client", "EXCLUSIVE_ENVIRONMENT_PROFILE": "route-client",
		"ALLOWS_HOST_ACCESS": "false", "HOST_LINKS": "SENTINEL", "FORWARD_SSH_AGENT": "SENTINEL",
	}, prior...)
	diagnostic := Diagnose("", fmt.Errorf("yard default: %w", err))
	want := []string{
		"exclusive environment profiles require a dedicated named yard",
		`profile "route-client" requires host access; this yard role sets ALLOWS_HOST_ACCESS=false, so deselect the profile or use a yard that allows host access`,
		"selected yard role forbids non-empty AGENT_SAMPLE_SOURCE",
		"selected yard role forbids non-empty HOST_LINKS",
		"selected yard role requires FORWARD_SSH_AGENT=0",
		"yard role forbids non-empty CODING_TOOL_INTEGRATIONS",
	}
	if strings.Join(diagnostic.Refusals, "\n") != strings.Join(want, "\n") || diagnostic.Unclassified || len(diagnostic.Next) < 2 {
		t.Fatalf("diagnostic = %#v", diagnostic)
	}
	mixed := Diagnose("", domain.CollectRefusals(err, errors.New("raw SENTINEL"), sourceError("", 0, "SSH_PORT", "scalar settings", "invalid port value", nil)))
	message := mixed.Message("generic tail")
	if !mixed.Unclassified || strings.Contains(message, "SENTINEL") || !strings.HasSuffix(message, "; generic tail") ||
		!strings.Contains(message, "configuration: SSH_PORT (scalar settings): invalid port value") || len(mixed.Refusals) != len(want)+1 {
		t.Fatalf("mixed diagnostic = %q", message)
	}
	if explained := Diagnose("", domain.WithRefusalCause(domain.SettingRefusal(domain.ConfigSettingNumeric, "SSH_PORT"), errors.New("raw SENTINEL"))); explained.Unclassified || explained.Message("generic") != "SSH_PORT must be numeric" {
		t.Fatalf("raw cause under a typed refusal = %#v", explained)
	}
	for _, joined := range []error{
		errors.Join(domain.NewConfigRefusal(domain.ConfigBlockStorageRequiresSize), errors.New("raw SENTINEL")),
		fmt.Errorf("%w: %w", domain.NewConfigRefusal(domain.ConfigBlockStorageRequiresSize), errors.New("raw SENTINEL")),
	} {
		if independent := Diagnose("", joined); !independent.Unclassified || independent.Message("generic") != "block storage requires an explicit SRV_VOLUME_SIZE; generic" {
			t.Fatalf("an independent raw cause was treated as explained: %#v", independent)
		}
	}
	integrity := errors.New("integrity SENTINEL")
	known := Diagnose("", domain.CollectRefusals(fmt.Errorf("owner: %w", integrity), domain.NewConfigRefusal(domain.ConfigBlockStorageRequiresSize)), KnownCause{Err: integrity, Text: "integrity failed", Next: "repair the owner"})
	if known.Unclassified || known.Message("") != "block storage requires an explicit SRV_VOLUME_SIZE; integrity failed" || !slices.Contains(known.Next, "repair the owner") {
		t.Fatalf("known cause = %#v", known)
	}
	if stale := Diagnose("", fmt.Errorf("%w: %w", err, domain.ErrPlanStale)); stale.Unclassified || len(stale.Refusals) != len(want)+1 || !slices.Contains(stale.Refusals, domain.ErrPlanStale.Error()) {
		t.Fatalf("known sentinel made the diagnostic incomplete: %#v", stale)
	}
	values := Diagnose("", domain.CollectRefusals(
		fmt.Errorf("%w %q in %s", domain.NewConfigRefusal(domain.ConfigUnknownYardTemplate), "SENTINEL", "/SENTINEL"),
		fmt.Errorf("%w %q", ErrUnknownYard, "SENTINEL"),
	))
	if message := values.Message("generic"); message != "unknown YARD_TEMPLATE; unknown yard" || values.Unclassified {
		t.Fatalf("value-suffixed refusals = %q", message)
	}
	if opaque := Diagnose("", errors.New("raw SENTINEL")); !opaque.Unclassified || opaque.Message("generic") != "generic" {
		t.Fatalf("unclassified diagnostic = %#v", opaque)
	}
}

func TestDiagnoseNamesShippedPackageSymlinkWithoutPath(t *testing.T) {
	root := testkit.TempDir(t)
	target := testkit.TempDir(t)
	if err := os.Mkdir(filepath.Join(target, "sample"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(target, "sample", "profile.conf"), []byte("PROVISION_SCOPE=dedicated\n"), 0o644)
	if err := os.Symlink(target, filepath.Join(root, "profiles")); err != nil {
		t.Fatal(err)
	}
	_, err := ProfileProvisionScope(root, "sample")
	diagnostic := Diagnose("", err)
	if message := diagnostic.Message("generic"); message != "shipped package directory must not traverse symlinks" || len(diagnostic.Next) != 1 {
		t.Fatalf("shipped package diagnostic = %q (%v)", message, err)
	}
}

func TestIntegrationSelectionReportsEveryRefusalNamingOnlyShippedIntegrations(t *testing.T) {
	catalog, err := LoadCatalog(integrationCatalogFixture(t, `{"schema_version":1,"command":"sample"}`))
	if err != nil {
		t.Fatal(err)
	}
	// Metadata settings alone, such as an operator-authored AGENT_canary_COMMAND, do not ship a name.
	values := environment{"AGENT_sample_COMMAND": "sample", "AGENT_canary_COMMAND": "true"}
	_, err = catalog.ResolveIntegrationSelection(values, []string{"missing-a", "missing-b", "missing-a", "sample", "sample", "canary", "canary"})
	diagnostic := Diagnose("", err)
	// Refusals that render alike appear once.
	want := []string{"duplicate agent", `duplicate agent "sample"`, "unknown integration"}
	if strings.Join(diagnostic.Refusals, "\n") != strings.Join(want, "\n") || strings.Contains(diagnostic.Message(""), "missing-") ||
		strings.Contains(diagnostic.Message(""), "canary") || diagnostic.Unclassified {
		t.Fatalf("integration refusals = %#v (%v)", diagnostic, err)
	}
	if _, err := ResolveIntegrationSelection(values, []string{"sample", "sample"}); Diagnose("", err).Message("") != "duplicate agent" {
		t.Fatalf("selection without a catalog named an integration: %v", err)
	}
}

func TestLoadReportsIndependentRefusalsAcrossStages(t *testing.T) {
	root := integrationCatalogFixture(t, `{"schema_version":1,"command":"sample","settings":[{"name":"SAMPLE_PORT","type":"port","scopes":["shipped","command"],"application":"yard-init","minimum":1024,"maximum":65535,"optional":true,"host_listener":true},{"name":"SAMPLE_ALT_PORT","type":"port","scopes":["shipped","command"],"application":"yard-init","minimum":1024,"maximum":65535,"optional":true,"host_listener":true}]}`)
	writeCatalogProfile(t, root, "fixture", `{"schema_version":1,"settings":[{"name":"FIXTURE_PORT","type":"port","scopes":["shipped","command"],"application":"next-command","minimum":1,"maximum":65535,"optional":true,"host_listener":true},{"name":"FIXTURE_ALT_PORT","type":"port","scopes":["shipped","command"],"application":"next-command","minimum":1,"maximum":65535,"optional":true,"host_listener":true},{"name":"FIXTURE_THIRD_PORT","type":"port","scopes":["shipped","command"],"application":"next-command","minimum":1,"maximum":65535,"optional":true,"host_listener":true}]}`)
	load := func(environment map[string]string) ConfigDiagnostic {
		t.Helper()
		_, err := Load(LoadOptions{RepositoryRoot: root, OperatorHome: testkit.TempDir(t), Environment: environment})
		if err == nil {
			t.Fatal("invalid configuration loaded")
		}
		return Diagnose("", err)
	}
	ports := load(map[string]string{"CODING_TOOL_INTEGRATIONS": "sample", "ENVIRONMENT_PROFILES": "fixture",
		"SAMPLE_PORT": "3000", "SAMPLE_ALT_PORT": "3000", "FIXTURE_PORT": "4000", "FIXTURE_ALT_PORT": "4000", "FIXTURE_THIRD_PORT": "4000"})
	if got := strings.Join(ports.Refusals, "\n"); got != "FIXTURE_PORT collides with FIXTURE_ALT_PORT; choose another port\nFIXTURE_THIRD_PORT collides with FIXTURE_ALT_PORT; choose another port\nFIXTURE_THIRD_PORT collides with FIXTURE_PORT; choose another port\nSAMPLE_ALT_PORT collides with SAMPLE_PORT; choose another port" {
		t.Fatalf("collisions = %q", got)
	}
	stages := load(map[string]string{"DEV_SUDO": "2", "FORWARD_SSH_AGENT": "2", "HOST_BASE": "/srv/a", "RESTRICTED_DISK_PATHS": "/srv/b", "SSH_PORT": "SENTINEL"})
	message := stages.Message("generic")
	for _, want := range []string{"DEV_SUDO", "FORWARD_SSH_AGENT"} {
		if !strings.Contains(message, want) {
			t.Errorf("command refusals lack %s: %s", want, message)
		}
	}
	if strings.Contains(message, "SENTINEL") || stages.Unclassified {
		t.Fatalf("command refusals = %q", message)
	}
	home := testkit.TempDir(t)
	if err := os.MkdirAll(filepath.Join(home, ".config", "subyard", "yards", "default"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(home, ".config", "subyard", "config.env"), []byte("HOST_BASE=/srv/a\nRESTRICTED_DISK_PATHS=/srv/b\nE2E_VM_DISK=9GiB\n"), 0o600)
	testkit.WriteFile(t, filepath.Join(home, ".config", "subyard", "yards", "default", "config.env"), []byte("VM_CPU_WEIGHT=1000\n"), 0o600)
	_, err := Load(LoadOptions{RepositoryRoot: root, OperatorHome: home, Environment: map[string]string{"CODING_TOOL_INTEGRATIONS": "missing-sentinel", "DEV_SUDO": "2"}})
	context := Diagnose("", err)
	message = context.Message("generic")
	for _, want := range []string{"require YARD_KIND=vm", "HOST_BASE must equal RESTRICTED_DISK_PATHS", "E2E_VM_DISK must be at least 10GiB", "unknown integration", "DEV_SUDO (command override)"} {
		if !strings.Contains(message, want) {
			t.Errorf("stage refusals lack %q: %s", want, message)
		}
	}
	if context.Unclassified || strings.Contains(message, "sentinel") {
		t.Fatalf("stage refusals = %q", message)
	}
	aliases := Diagnose("", normalizeLegacyEnvironment(environment{"AGENTS": "codex", "CODING_TOOL_INTEGRATIONS": "claude", "YARD_PROFILES": "one", "ENVIRONMENT_PROFILES": "two"}))
	if got := strings.Join(aliases.Refusals, "\n"); got != "conflicting command environment settings CODING_TOOL_INTEGRATIONS and AGENTS\nconflicting command environment settings ENVIRONMENT_PROFILES and YARD_PROFILES" {
		t.Fatalf("alias conflicts = %q", got)
	}
}

func TestDiagnoseRendersRetiredTemplateRelativeToConfigHome(t *testing.T) {
	home := testkit.TempDir(t)
	err := retiredE2EVMTemplateError("named", filepath.Join(home, "yards", "named", "config.env"))
	if message := Diagnose(home, err).Message(""); strings.Contains(message, home) || !strings.Contains(message, `retired in "yards/named/config.env"`) {
		t.Fatalf("retired diagnostic = %q", message)
	}
	if message := Diagnose("", err).Message(""); strings.Contains(message, home) {
		t.Fatalf("retired diagnostic without home = %q", message)
	}
}

func TestLoadCollectsAssetPathAndCodingToolRoleRefusals(t *testing.T) {
	root := integrationCatalogFixture(t, `{"schema_version":1,"command":"sample"}`)
	if err := os.MkdirAll(filepath.Join(root, "config", "yards", "profiles"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "config", "yards", "profiles", "nocode.env"), []byte("ALLOWS_CODING_TOOLS=false\n"), 0o600)
	home := testkit.TempDir(t)
	yard := filepath.Join(home, ".config", "subyard", "yards", "work")
	if err := os.MkdirAll(yard, 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(yard, "config.env"), []byte("YARD_TEMPLATE=nocode\nCODING_TOOL_INTEGRATIONS=missing-sentinel\nVM_CPU_WEIGHT=1000\n"), 0o600)
	_, err := Load(LoadOptions{RepositoryRoot: root, OperatorHome: home, YardName: "work", Environment: map[string]string{
		"AGENT_alpha_RULES": filepath.Join(home, "missing-alpha"), "AGENT_beta_CONFIG": filepath.Join(home, "missing-beta"),
	}})
	diagnostic := Diagnose(filepath.Join(home, ".config", "subyard"), err)
	message := diagnostic.Message("generic")
	for _, want := range []string{"AGENT_alpha_RULES", "AGENT_beta_CONFIG", "require YARD_KIND=vm", "yard role forbids non-empty CODING_TOOL_INTEGRATIONS", "unknown integration"} {
		if !strings.Contains(message, want) {
			t.Errorf("load refusals lack %q: %s", want, message)
		}
	}
	if strings.Contains(message, "sentinel") || strings.Contains(message, home) || diagnostic.Unclassified || len(diagnostic.Next) == 0 {
		t.Fatalf("load refusals = %q (%#v)", message, diagnostic)
	}
}

func TestDiagnoseGivesSourceErrorsANextStep(t *testing.T) {
	for _, source := range []error{
		sourceError("", 0, "SSH_PORT", "scalar settings", "invalid port value", nil),
		sourceError("/home/config.env", 3, "SSH_PORT", "scalar settings", "invalid port value", nil),
	} {
		if diagnostic := Diagnose("/home", source); len(diagnostic.Next) != 1 {
			t.Fatalf("source diagnostic lacks a next step: %#v", diagnostic)
		}
	}
}

func TestEnvironmentProfileResolutionReportsEveryBrokenPackage(t *testing.T) {
	root := testkit.TempDir(t)
	for _, name := range []string{"one", "two"} {
		if err := os.MkdirAll(filepath.Join(root, "profiles", name), 0o700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(root, "profiles", name, "profile.conf"), []byte("PROVISION_SCOPE=unknown\n"), 0o644)
	}
	_, err := resolveEnvironmentProfiles(root, "named", environment{"ENVIRONMENT_PROFILES": "one two"}, Catalog{})
	if got := strings.Join(Diagnose("", err).Refusals, "; "); got != `profile "one" has an unsupported PROVISION_SCOPE; profile "two" has an unsupported PROVISION_SCOPE` {
		t.Fatalf("profile resolution refusals = %q", got)
	}
}
