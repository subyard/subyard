package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestRemoteDownProfileDashboardPreservesCheckHint(t *testing.T) {
	var output bytes.Buffer
	program := &CLI{options: Options{Stdout: &output}}
	hint := "yard init (profile check reason=deadline native_exit=none elapsed_ms=5001 budget_ms=5000)"
	program.printRemoteStatusFacts(domain.YardStatus{Facts: domain.StatusFacts{Shared: []domain.SharedResourceStatus{{Profile: "sample", Name: "dashboard", State: "down", Hint: hint}}}}, "owner")
	if !strings.Contains(output.String(), hint) {
		t.Fatalf("down status lost original check diagnostic: %s", output.String())
	}
}

func TestProfileDashboardRenderingPreservesClosedStatusSchema(t *testing.T) {
	root := testkit.TempDir(t)
	directory := filepath.Join(root, "config/profiles/sample")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(directory, "owner.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o700)
	testkit.WriteFile(t, filepath.Join(directory, "runtime.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o700)
	definition := profile.Definition{SchemaVersion: 1, OwnerService: "owner.sh", Runtime: &profile.RuntimeHook{ActivationID: "sample-runtime", Handler: "runtime.sh"},
		Settings:  []profile.Setting{{Name: "SAMPLE_PORT", Type: "port", Scopes: []string{"yard"}, Application: "yard-init", Minimum: 1024, Maximum: 65535, HostListener: true}},
		Dashboard: &profile.IntegrationDashboard{Scheme: "http", Path: "/ui", PortSetting: "SAMPLE_PORT", GuestPort: 3000, ContainerRoute: &profile.IntegrationRoute{MarkerKey: "user.subyard.sample_dashboard", DeviceName: "sample-dashboard"}}}
	payload, _ := json.Marshal(definition)
	testkit.WriteFile(t, filepath.Join(directory, "profile.json"), payload, 0o600)
	catalog, err := config.LoadCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	program := &CLI{catalog: catalog}
	shared := domain.SharedResourceStatus{Profile: "sample", Name: "dashboard", State: "up", URL: "http://127.0.0.1:22222/ui"}
	for _, test := range []struct {
		kind  domain.YardKind
		owner string
		want  string
	}{
		{domain.YardContainer, "", "http://127.0.0.1:22222/ui"},
		{domain.YardContainer, "owner@example", "22222:127.0.0.1:22222"},
		{domain.YardVM, "", "22222:127.0.0.1:3000"},
		{domain.YardVM, "owner@example", "22222:127.0.0.1:3000"},
	} {
		got := program.sharedDashboardDetail(domain.Context{YardKind: test.kind, SSHHost: "yard-alias"}, shared, test.owner)
		if !strings.Contains(got, test.want) {
			t.Fatalf("%s/%s: %s", test.kind, test.owner, got)
		}
		if test.kind == domain.YardVM && !strings.Contains(got, "yard-alias") {
			t.Fatal("VM guest SSH endpoint missing")
		}
	}
	shared.URL = "http://100.101.102.103:22222/ui"
	if got := program.sharedDashboardDetail(domain.Context{YardKind: domain.YardContainer}, shared, "owner@example"); got != shared.URL {
		t.Fatal("private owner address was tunneled")
	}
	for _, kind := range []domain.YardKind{domain.YardContainer, domain.YardVM} {
		status := domain.YardStatus{Context: domain.Context{YardKind: kind}, Facts: domain.StatusFacts{Shared: []domain.SharedResourceStatus{shared, {Profile: "other", Name: "dashboard", State: "up", URL: "http://127.0.0.1:9999/"}}}}
		legacy := program.legacyDashboardStatus(status)
		if kind == domain.YardVM {
			if legacy.Facts.Shared[0].URL != "" || !strings.Contains(legacy.Facts.Shared[0].Hint, "update the controller") {
				t.Fatalf("legacy VM route was not safely withheld: %+v", legacy.Facts.Shared[0])
			}
		} else if legacy.Facts.Shared[0] != shared {
			t.Fatalf("legacy container route changed: %+v", legacy.Facts.Shared[0])
		}
		if legacy.Facts.Shared[1].URL != "http://127.0.0.1:9999/" {
			t.Fatal("legacy projection changed an unrelated shared resource")
		}
	}
	encoded, _ := json.Marshal(shared)
	var fields map[string]any
	json.Unmarshal(encoded, &fields)
	if len(fields) != 4 {
		t.Fatalf("closed owner schema changed: %s", encoded)
	}
	// Container private-owner URLs remain usable without the new declaration.
	if got := (&CLI{}).sharedDashboardDetail(domain.Context{}, shared, "owner@example"); got != shared.URL {
		t.Fatal("declaration skew broke URL rendering")
	}
}
