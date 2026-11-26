package domain

import (
	"errors"
	"regexp"
	"strings"
	"testing"
)

func TestConfigRefusalCodesHaveStableUniqueWireNames(t *testing.T) {
	pattern := regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	seen := map[string]ConfigRefusalCode{}
	for code := range configRefusalTexts {
		name := code.Name()
		if !pattern.MatchString(name) || len(name) > 64 {
			t.Fatalf("code %d has no valid wire name: %q", code, name)
		}
		if previous, ok := seen[name]; ok {
			t.Fatalf("codes %d and %d share wire name %q", previous, code, name)
		}
		seen[name] = code
	}
	if len(configRefusalNames) != len(configRefusalTexts) || ConfigRefusalCode(0).Name() != "" {
		t.Fatalf("wire names %d differ from texts %d", len(configRefusalNames), len(configRefusalTexts))
	}
	// Spot-check that names are pinned, not derived at run time.
	if ConfigRoleForbidsSetting.Name() != "role-forbids-setting" || ConfigE2EVMCPU.Name() != "e2e-vm-cpu" {
		t.Fatal("wire names changed")
	}
}

func TestConfigRefusalRendersOnlyValidatedNames(t *testing.T) {
	const sentinel = "secret value/SENTINEL"
	for _, name := range []func(string) (ConfigName, bool){ConfigProfileName, ConfigSettingName, ConfigYardKindName} {
		invalid, ok := name(sentinel)
		if ok {
			t.Fatal("constructor accepted an arbitrary value")
		}
		for code := range configRefusalTexts {
			if text := NewConfigRefusal(code, invalid).Error(); strings.Contains(text, "SENTINEL") {
				t.Fatalf("code %d rendered an unvalidated value: %s", code, text)
			}
		}
	}
	profile, _ := ConfigProfileName("route-client")
	setting, _ := ConfigSettingName("HOST_LINKS")
	kind, _ := ConfigYardKindName("vm")
	for refusal, want := range map[*ConfigRefusal]string{
		NewConfigRefusal(ConfigProfileRequiresHostAccess, profile): `profile "route-client" requires host access; this yard role sets ALLOWS_HOST_ACCESS=false, so deselect the profile or use a yard that allows host access`,
		NewConfigRefusal(ConfigRoleForbidsSetting, setting):        "selected yard role forbids non-empty HOST_LINKS",
		NewConfigRefusal(ConfigRoleRequiresYardKind, kind):         "selected yard role requires YARD_KIND=vm",
		NewConfigRefusal(ConfigRoleForbidsSetting):                 "configuration refusal",
	} {
		if refusal.Error() != want || refusal.Next() == "" {
			t.Fatalf("refusal = %q, next %q; want %q", refusal, refusal.Next(), want)
		}
	}
	var typed *ConfigRefusal
	if fallback := UnclassifiedConfigRefusal(ConfigRolePermitsOnlyProfile, sentinel); errors.As(fallback, &typed) ||
		fallback.Error() != `selected yard role permits only environment profile "`+sentinel+`"` {
		t.Fatalf("unvalidated fallback = %v", fallback)
	}
}

func TestContextValidationReportsEveryTypedRefusal(t *testing.T) {
	ctx := Context{YardName: "default", AccessKind: AccessLocal, YardKind: "SENTINEL", ShiftMode: "SENTINEL", DevUID: -1, NestedE2EVMs: true,
		Paths: RuntimePaths{RepositoryRoot: "/r", ConfigDir: "relative", OperatorHome: "/home/dev", ConfigHome: "/c", DataHome: "/home/dev", StoragePath: "/s", HostBase: "/srv", StateDir: "/t"}}
	_, err := NormalizeContext(ctx)
	var refusals *Refusals
	if !errors.As(err, &refusals) || len(refusals.Unwrap()) != 9 || !strings.Contains(err.Error(), "config dir must be an absolute path") {
		t.Fatalf("context refusals = %v", err)
	}
	ctx.Paths.ConfigDir, ctx.Paths.HostBase = "/d", "relative"
	if _, err := NormalizeContext(ctx); !errors.As(err, &refusals) || len(refusals.Unwrap()) != 8 || strings.Contains(err.Error(), "host base is too broad") {
		t.Fatalf("an unusable path was checked further: %v", err)
	}
	ctx.Paths.HostBase = "/srv"
	_, err = NormalizeContext(ctx)
	if !errors.As(err, &refusals) || len(refusals.Unwrap()) != 8 {
		t.Fatalf("context refusals = %v", err)
	}
	for _, cause := range refusals.Unwrap() {
		var typed *ConfigRefusal
		if !errors.As(cause, &typed) || strings.Contains(typed.Error(), "SENTINEL") || strings.Contains(typed.Error(), "/srv") {
			t.Fatalf("untyped or value-bearing context refusal: %v", cause)
		}
	}
}
