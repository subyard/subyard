package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
)

func TestFullTeardownKeepsApprovedProducerSources(t *testing.T) {
	t.Helper()
	program, loaded, incus, _ := teardownResetFixture(t)
	root := program.options.RepositoryRoot
	installation := uninstall.Installation{Schema: 1, OperatorUID: uint32(os.Getuid()), ConfigHome: loaded.Context.Paths.ConfigHome, DataHome: loaded.Context.Paths.DataHome, RuntimeRoot: filepath.Join(root, "runtime"), CacheRoot: filepath.Join(root, "cache")}
	if err := os.MkdirAll(installation.DataHome, 0o700); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(installation)
	testkit.WriteFile(t, filepath.Join(installation.DataHome, uninstall.ManifestName), body, 0o600)
	capture := filepath.Join(root, "captured-source-context.json")
	directory := filepath.Join(root, "config/profiles/continuity")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(directory, "profile.json"), []byte(`{"schema_version":1,"lifecycle_only":true,"full_cleanup":"cleanup.py","full_cleanup_environment":["SUBYARD_FULL_CLEANUP_SOURCES","SUBYARD_FULL_CLEANUP_OPERATOR_UID"]}`), 0o600)
	hook := fmt.Sprintf("#!/usr/bin/env python3\nimport json,os,pathlib\npathlib.Path(%q).write_text(json.dumps(dict(os.environ)))\nprint(json.dumps({'blockers':['continuity-captured']}))\n", capture)
	testkit.WriteFile(t, filepath.Join(directory, "cleanup.py"), []byte(hook), 0o700)
	catalog, err := config.LoadCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	program.catalog = catalog
	loaded.Catalog = catalog
	program.env["SUBYARD_FULL_CLEANUP_SOURCES"] = `["forged-source"]`
	program.env["SUBYARD_FULL_CLEANUP_OPERATOR_UID"] = "99999"
	program.options.ObserveCoreProducers = func(config.Loaded, uninstall.RecoveryPlan, []string, int) ([]uninstall.RecoveryService, error) {
		return nil, nil
	}
	incus.Reconcile.ProjectFound = true
	program.options.Incus = &fullOwnershipFixture{Incus: incus, owner: ports.InstallationYard{Project: loaded.Context.IncusProject, OperatorUID: strconv.Itoa(os.Getuid()), DataHome: installation.DataHome, ConfigHome: installation.ConfigHome, HostBase: loaded.Context.Paths.HostBase}}
	err = program.observeFullTeardown(context.Background(), loaded, &fullTeardown{})
	if err == nil || !strings.Contains(err.Error(), "continuity-captured") {
		t.Fatalf("owning hook was not reached: %v", err)
	}
	body, err = os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	var environment map[string]string
	if err := json.Unmarshal(body, &environment); err != nil {
		t.Fatal(err)
	}
	var sources []string
	if err := json.Unmarshal([]byte(environment["SUBYARD_FULL_CLEANUP_SOURCES"]), &sources); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(sources, "previous-owner") || !slices.Contains(sources, loaded.Context.YardName) || slices.Contains(sources, "forged-source") {
		t.Fatal("raw override changed approved producer sources")
	}
	if environment["SUBYARD_FULL_CLEANUP_OPERATOR_UID"] != strconv.Itoa(os.Getuid()) {
		t.Fatal("raw override changed the approved operator")
	}
}
