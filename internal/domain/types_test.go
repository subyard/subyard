package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestYardRefRequiresCompleteCanonicalIdentity(t *testing.T) {
	valid := YardRef{HostID: "owner-a", YardName: "default"}
	if err := valid.Validate(); err != nil || valid.String() != "owner-a/default" {
		t.Fatalf("valid YardRef = %q err=%v", valid.String(), err)
	}
	for _, ref := range []YardRef{
		{YardName: "default"}, {HostID: "owner-a"},
		{HostID: "../owner", YardName: "default"}, {HostID: "owner-a", YardName: "../yard"},
	} {
		if err := ref.Validate(); err == nil {
			t.Fatalf("invalid YardRef accepted: %#v", ref)
		}
	}
}

func TestYardSelectorSeparatesInputFromIdentity(t *testing.T) {
	for _, test := range []struct {
		input string
		want  YardSelector
	}{
		{input: "demo", want: YardSelector{YardName: "demo"}},
		{input: "owner-a/demo", want: YardSelector{HostID: "owner-a", YardName: "demo"}},
	} {
		got, err := ParseYardSelector(test.input)
		if err != nil || got != test.want {
			t.Fatalf("ParseYardSelector(%q) = %#v err=%v", test.input, got, err)
		}
	}
	for _, input := range []string{"", "/demo", "owner-a/", "a/b/c", "../demo"} {
		if _, err := ParseYardSelector(input); err == nil {
			t.Fatalf("invalid selector %q accepted", input)
		}
	}
}

func TestContextJSONUsesCanonicalAccessAndRuntimeKindNames(t *testing.T) {
	payload, err := json.Marshal(Context{
		AccessKind: AccessLocal, YardKind: YardContainer,
		OwnerEndpoint: "dev@owner.example", OwnerYardName: "demo",
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(payload)
	if !strings.Contains(encoded, `"accessKind":"local"`) ||
		!strings.Contains(encoded, `"yardKind":"container"`) ||
		!strings.Contains(encoded, `"yardInstanceName":""`) ||
		!strings.Contains(encoded, `"ownerEndpoint":"dev@owner.example"`) ||
		!strings.Contains(encoded, `"ownerYardName":"demo"`) ||
		strings.Contains(encoded, `"yardType"`) || strings.Contains(encoded, `"instanceType"`) ||
		strings.Contains(encoded, `"remoteDest"`) || strings.Contains(encoded, `"remoteYard"`) {
		t.Fatalf("context JSON uses ambiguous names: %s", encoded)
	}
}

func TestContextJSONReadsLegacyNamesOnlyAtInputBoundary(t *testing.T) {
	var context Context
	if err := json.Unmarshal([]byte(`{"yardType":"remote","instanceType":"vm","instanceName":"yard-demo"}`), &context); err != nil {
		t.Fatal(err)
	}
	if context.AccessKind != AccessRemote || context.YardKind != YardVM ||
		context.YardInstanceName != "yard-demo" {
		t.Fatalf("legacy context did not migrate: %#v", context)
	}
	for _, payload := range []string{
		`{"accessKind":"local","yardType":"remote"}`,
		`{"yardKind":"container","instanceType":"vm"}`,
		`{"yardInstanceName":"yard-a","instanceName":"yard-b"}`,
		`{"ownerEndpoint":"owner-a","remoteDest":"owner-b"}`,
		`{"ownerYardName":"yard-a","remoteYard":"yard-b"}`,
	} {
		if err := json.Unmarshal([]byte(payload), &context); err == nil {
			t.Fatalf("conflicting canonical and legacy context accepted: %s", payload)
		}
	}
}

func TestContextRejectsUnsafeBoundaries(t *testing.T) {
	valid := Context{
		YardName: "default", AccessKind: AccessLocal, YardKind: YardContainer,
		YardInstanceName: "yard", IncusProject: "subyard", SSHHost: "yard", DevUser: "dev",
		SSHPort: 2222, ShiftMode: "shift", DevUID: 1000,
		Paths: RuntimePaths{
			RepositoryRoot: "/repo", ConfigDir: "/repo/config", OperatorHome: "/home/dev",
			ConfigHome: "/home/dev/.config/subyard", DataHome: "/home/dev/.subyard",
			StoragePath: "/home/dev/.subyard/incus", HostBase: "/srv/subyard", StateDir: "/state",
		},
	}
	if _, err := NormalizeContext(valid); err != nil {
		t.Fatal(err)
	}
	tests := map[string]Context{
		"nested VM yard": func() Context {
			value := valid
			value.YardKind, value.NestedE2EVMs = YardVM, true
			return value
		}(),
		"broad host base": func() Context { value := valid; value.Paths.HostBase = "/"; return value }(),
		"invalid port":    func() Context { value := valid; value.SSHPort = 70000; return value }(),
	}
	for name, value := range tests {
		if _, err := NormalizeContext(value); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestProjectNamePolicy(t *testing.T) {
	if !SafeProjectName("Demo_2.0") || SafeProjectName("-Demo") ||
		SafeProjectName("Демо") || SafeProjectName("123456789012345678901234567890123456789012345678901") {
		t.Fatal("project name safety policy changed")
	}
	if !ProjectNamesEqual("Demo", "demo") ||
		ProjectNameKey("Demo") != ProjectNameKey("demo") {
		t.Fatal("project name equivalence policy changed")
	}
}
