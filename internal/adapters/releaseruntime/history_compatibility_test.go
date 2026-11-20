package releaseruntime

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

// verifyRetainedHistory is the identity-registry history check of the
// retained-runtime boundary, kept for the frozen rollback contract test.
func verifyRetainedHistory(target *verifiedPublishedCandidate, registry releasetransition.RegistryV2, ledger, checkpoint releasetransition.ProtectedSnapshot) (string, error) {
	return releasetransition.HistoryCompatibility(releasetransition.HistoryCompatibilityInput{
		Active: &registry, Target: &registry,
		History: retainedHistoryEvidence(target, registry, ledger, checkpoint),
	}).Refusal()
}

type historyFixture struct {
	runtimeRoot, configHome string
	registry                []byte
	parsed                  releasetransition.RegistryV2
}

func newHistoryFixture(t *testing.T) historyFixture {
	t.Helper()
	root := testkit.TempDir(t)
	fixture := historyFixture{runtimeRoot: filepath.Join(root, "runtime"), configHome: filepath.Join(root, "config")}
	for _, directory := range []string{filepath.Join(fixture.runtimeRoot, "releases"), fixture.configHome} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	fixture.registry, err = os.ReadFile(filepath.Join("..", "..", "..", "config", "release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	fixture.parsed, _, err = releasetransition.ParseRegistryV2(fixture.registry, releasetransition.BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

// writeRelease seals files under releases/ID. A nil value is omitted; a
// manifest override replaces that file's sealed digest.
func (fixture historyFixture) writeRelease(t *testing.T, runtimeRoot, id string, files map[string][]byte, manifestOverride map[string][]byte) string {
	t.Helper()
	root := filepath.Join(runtimeRoot, "releases", id)
	files["bin/yard-engine"] = []byte("#!/bin/sh\nexit 64\n")
	paths := []string{}
	for path, payload := range files {
		if payload == nil {
			continue
		}
		if err := os.MkdirAll(filepath.Join(root, filepath.Dir(path)), 0o700); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o600)
		if path == "bin/yard-engine" {
			mode = 0o700
		}
		testkit.WriteFile(t, filepath.Join(root, path), payload, mode)
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var manifest bytes.Buffer
	for _, path := range paths {
		sealed := files[path]
		if override, ok := manifestOverride[path]; ok {
			sealed = override
		}
		fmt.Fprintf(&manifest, "%x  ./%s\n", sha256.Sum256(sealed), path)
	}
	testkit.WriteFile(t, filepath.Join(root, "runtime-files.sha256"), manifest.Bytes(), 0o600)
	return root
}

func (fixture historyFixture) activate(t *testing.T, id string) {
	t.Helper()
	if err := os.Symlink(filepath.Join("releases", id), filepath.Join(fixture.runtimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
}

func (fixture historyFixture) writeCompleteLedger(t *testing.T) {
	t.Helper()
	ledger := releasetransition.BaselineLedgerV2(fixture.parsed)
	var err error
	for _, migration := range fixture.parsed.Migrations {
		if ledger, err = ledger.Advance(fixture.parsed, migration); err != nil {
			t.Fatal(err)
		}
	}
	payload, _, err := releasetransition.MarshalLedgerV2(ledger, fixture.parsed)
	if err != nil {
		t.Fatal(err)
	}
	store, err := releasetransition.NewPOSIXV2Store(fixture.configHome)
	if err != nil {
		t.Fatal(err)
	}
	missing, err := store.ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapLedger(missing, payload); err != nil {
		t.Fatal(err)
	}
}

func rollbackDeclarationPayload(predecessor releasetransition.Fingerprint) []byte {
	return []byte(fmt.Sprintf(`{"schemaVersion":1,"predecessorRegistryDigest":%q,"migrations":[],"profileFormats":[]}`+"\n", predecessor))
}

// olderRegistry drops the newest migration, standing in for a predecessor.
func (fixture historyFixture) olderRegistry(t *testing.T) ([]byte, releasetransition.Fingerprint) {
	t.Helper()
	older := fixture.parsed
	last := older.Migrations[len(older.Migrations)-1]
	older.Migrations = older.Migrations[:len(older.Migrations)-1]
	older.CurrentEpochs = map[string]int{}
	for domain, epoch := range fixture.parsed.CurrentEpochs {
		older.CurrentEpochs[domain] = epoch
	}
	older.CurrentEpochs[last.Domain] = last.FromEpoch
	payload, err := json.Marshal(older)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _, err := releasetransition.ParseRegistryV2(payload, releasetransition.BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatalf("older registry: %v", err)
	}
	canonical, err := parsed.CanonicalDigest()
	if err != nil {
		t.Fatal(err)
	}
	return payload, canonical
}

func runHistoryVerb(t *testing.T, arguments ...string) (int, HistoryCompatibilityReport, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := RunHistoryCompatibility(arguments, &stdout, &stderr)
	var report HistoryCompatibilityReport
	if stdout.Len() != 0 {
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatalf("verdict output %q: %v", stdout.String(), err)
		}
	}
	return code, report, stderr.String()
}

func treeDigest(t *testing.T, root string) string {
	t.Helper()
	var listing strings.Builder
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		payload := []byte{}
		if entry.Type().IsRegular() {
			if payload, err = os.ReadFile(path); err != nil {
				return err
			}
		}
		fmt.Fprintf(&listing, "%s %v %d %x\n", path, info.Mode(), info.ModTime().UnixNano(), sha256.Sum256(payload))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return listing.String()
}

func TestHistoryCompatibilityVerb(t *testing.T) {
	const active, target = "1.0.0-aaaaaaaaaaaa", "0.9.0-bbbbbbbbbbbb"
	differs := "the selected release's migration registry differs from the active release's registry"
	for _, test := range []struct {
		name        string
		target      func(t *testing.T, fixture historyFixture) map[string][]byte
		predecessor bool
		code        int
		kind        releasetransition.HistoryCompatibilityKind
		registry    releasetransition.HistoryRegistryRelation
	}{
		{name: "identity", code: 0, kind: releasetransition.HistoryIdentity, registry: releasetransition.RegistryIdentical, target: func(t *testing.T, fixture historyFixture) map[string][]byte {
			return map[string][]byte{"config/release-transition.json": fixture.registry}
		}},
		{name: "canonical identity", code: 0, kind: releasetransition.HistoryIdentity, registry: releasetransition.RegistryIdentical, target: func(t *testing.T, fixture historyFixture) map[string][]byte {
			compact, err := json.Marshal(fixture.parsed)
			if err != nil {
				t.Fatal(err)
			}
			return map[string][]byte{"config/release-transition.json": compact}
		}},
		{name: "declared predecessor", code: 1, kind: releasetransition.HistoryRefuse, registry: releasetransition.RegistryDiffers, predecessor: true, target: func(t *testing.T, fixture historyFixture) map[string][]byte {
			older, _ := fixture.olderRegistry(t)
			return map[string][]byte{"config/release-transition.json": older}
		}},
		{name: "registryless target", code: 1, kind: releasetransition.HistoryRefuse, registry: releasetransition.RegistryDiffers, target: func(t *testing.T, fixture historyFixture) map[string][]byte {
			return map[string][]byte{}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newHistoryFixture(t)
			_, predecessor := fixture.olderRegistry(t)
			fixture.writeRelease(t, fixture.runtimeRoot, active, map[string][]byte{
				"config/release-transition.json": fixture.registry,
				"config/release-rollback.json":   rollbackDeclarationPayload(predecessor),
			}, nil)
			fixture.activate(t, active)
			fixture.writeCompleteLedger(t)
			fixture.writeRelease(t, fixture.runtimeRoot, target, test.target(t, fixture), nil)
			before := treeDigest(t, filepath.Dir(fixture.runtimeRoot))
			code, report, stderr := runHistoryVerb(t, fixture.runtimeRoot, fixture.configHome, target)
			if code != test.code || report.SchemaVersion != 1 || report.Kind != test.kind || report.Registry != test.registry ||
				report.Admitted != (test.code == 0) || report.DeclaredPredecessor != test.predecessor ||
				report.Active != active || report.Target != target {
				t.Fatalf("verdict %d %#v, stderr %q", code, report, stderr)
			}
			if test.code == 0 && len(report.Refusals) != 0 ||
				test.code == 1 && (len(report.Refusals) != 1 || report.Refusals[0].Reason != releasetransition.HistoryRegistryDiffers || report.Refusals[0].Message != differs) {
				t.Fatalf("refusals = %#v", report.Refusals)
			}
			if after := treeDigest(t, filepath.Dir(fixture.runtimeRoot)); after != before {
				t.Fatal("history verdict changed the installation")
			}
		})
	}
}

// The verb shares the runtime's input assembly: a sole published current
// release without history is a first publication its own engine initializes.
func TestHistoryCompatibilityVerbMatchesRuntimeSelection(t *testing.T) {
	fixture := newHistoryFixture(t)
	const active, other = "1.0.0-aaaaaaaaaaaa", "1.1.0-cccccccccccc"
	fixture.writeRelease(t, fixture.runtimeRoot, active, map[string][]byte{"config/release-transition.json": fixture.registry}, nil)
	fixture.activate(t, active)
	code, report, stderr := runHistoryVerb(t, fixture.runtimeRoot, fixture.configHome, active)
	if code != 0 || !report.Admitted || report.Kind != releasetransition.HistoryIdentity || len(report.Refusals) != 0 {
		t.Fatalf("first publication verdict %d %#v, stderr %q", code, report, stderr)
	}
	fixture.writeRelease(t, fixture.runtimeRoot, other, map[string][]byte{"config/release-transition.json": fixture.registry}, nil)
	for _, target := range []string{active, other} {
		code, report, stderr = runHistoryVerb(t, fixture.runtimeRoot, fixture.configHome, target)
		if code != 1 || report.Kind != releasetransition.HistoryIdentity || report.Admitted ||
			len(report.Refusals) != 1 || report.Refusals[0].Reason != releasetransition.HistoryMissing {
			t.Fatalf("%s without history: %d %#v, stderr %q", target, code, report, stderr)
		}
	}
	store, err := releasetransition.NewPOSIXV2Store(fixture.configHome)
	if err != nil {
		t.Fatal(err)
	}
	missing, err := store.ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapLedger(missing, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	code, report, stderr = runHistoryVerb(t, fixture.runtimeRoot, fixture.configHome, other)
	if code != 1 || len(report.Refusals) != 1 || report.Refusals[0].Reason != releasetransition.HistoryUnverifiable ||
		report.Refusals[0].Cause == "" {
		t.Fatalf("unverifiable history verdict %d %#v, stderr %q", code, report, stderr)
	}
	notDirectory := filepath.Join(fixture.runtimeRoot, "releases", active, "runtime-files.sha256")
	for name, arguments := range map[string][]string{
		"no arguments":        nil,
		"extra":               {fixture.runtimeRoot, fixture.configHome, other, "x"},
		"relative":            {"runtime", fixture.configHome, other},
		"missing config home": {fixture.runtimeRoot, filepath.Join(fixture.configHome, "missing"), other},
		"config home file":    {fixture.runtimeRoot, notDirectory, other},
		"unsafe target":       {fixture.runtimeRoot, fixture.configHome, "../" + other},
		"missing target":      {fixture.runtimeRoot, fixture.configHome, "1.2.0-dddddddddddd"},
	} {
		if code, report, _ := runHistoryVerb(t, arguments...); code != 2 || report.SchemaVersion != 0 {
			t.Errorf("%s: exit %d, %#v", name, code, report)
		}
	}
}

// Callers read registry admission, not whole admission: identical registries
// with a history refusal are neither a registry refusal nor an admission.
func TestHistorySelectionReadsTheRegistryRelation(t *testing.T) {
	fixture := newHistoryFixture(t)
	olderPayload, _ := fixture.olderRegistry(t)
	older, _, err := releasetransition.ParseRegistryV2(olderPayload, releasetransition.BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	publication := errors.New("cannot list published releases")
	historyRefused := releasetransition.HistoryCompatibility(releasetransition.HistoryCompatibilityInput{
		Active: &fixture.parsed, Target: &fixture.parsed,
		History: &releasetransition.HistoryEvidence{Registry: fixture.parsed, Ledger: releasetransition.ProtectedSnapshot{Exists: true, Payload: []byte("{}")}},
	})
	differs := releasetransition.HistoryCompatibility(releasetransition.HistoryCompatibilityInput{Active: &fixture.parsed, Target: &older})
	if historyRefused.Admitted() || historyRefused.Registry != releasetransition.RegistryIdentical || differs.Registry != releasetransition.RegistryDiffers {
		t.Fatalf("fixture verdicts = %#v / %#v", historyRefused, differs)
	}
	refused := historySelection{targetRegistry: "x", verdict: historyRefused, publicationErr: publication}
	if refused.needsSourceFloor() {
		t.Fatal("a history refusal with identical registries reached the source floor")
	}
	if _, cause := refused.refusal(); !errors.Is(cause, publication) {
		t.Fatalf("identical-registry refusal dropped first-publication evidence: %v", cause)
	}
	different := historySelection{targetRegistry: "y", verdict: differs, publicationErr: publication}
	if !different.needsSourceFloor() {
		t.Fatal("differing registries skipped the source floor")
	}
	if message, cause := different.refusal(); cause != nil || message != "the selected release's migration registry differs from the active release's registry" {
		t.Fatalf("registry refusal = %q, %v", message, cause)
	}
	if !(historySelection{targetRegistry: "x", missingHistory: true, verdict: historyRefused}).needsSourceFloor() {
		t.Fatal("an installed registry without history skipped the source floor")
	}
	for name, test := range map[string]struct {
		verdict releasetransition.HistoryCompatibilityVerdict
		want    bool
	}{"history refusal": {historyRefused, false}, "registry differs": {differs, true}} {
		selection := &releaseSelection{request: releasetransition.ProcessRequest{Target: "1.0.0-aaaaaaaaaaaa"}, compatibility: &test.verdict}
		if got := unapprovedBootstrapPublication(selection); got != test.want {
			t.Errorf("%s: unapproved bootstrap publication = %v", name, got)
		}
	}
}

func TestRollbackDeclarationRequiresSealedManifest(t *testing.T) {
	for _, scenario := range []string{"absent", "valid", "unmanifested", "tampered", "wrong-manifest-digest", "changed-after-verification", "invalid-digest", "oversize"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newHistoryFixture(t)
			valid := rollbackDeclarationPayload(releasetransition.Fingerprint(strings.Repeat("a", 64)))
			files := map[string][]byte{"config/release-transition.json": fixture.registry, "config/release-rollback.json": valid}
			override := map[string][]byte{}
			switch scenario {
			case "absent":
				files["config/release-rollback.json"] = nil
			case "invalid-digest":
				files["config/release-rollback.json"] = rollbackDeclarationPayload("AAAA")
			case "oversize":
				files["config/release-rollback.json"] = append(valid, bytes.Repeat([]byte(" "), releasetransition.MaxRollbackDeclarationBytes)...)
			case "wrong-manifest-digest":
				override["config/release-rollback.json"] = rollbackDeclarationPayload(releasetransition.Fingerprint(strings.Repeat("b", 64)))
			}
			root := fixture.writeRelease(t, fixture.runtimeRoot, "1.0.0-aaaaaaaaaaaa", files, override)
			path := filepath.Join(root, "config", "release-rollback.json")
			switch scenario {
			case "unmanifested":
				manifest, err := os.ReadFile(filepath.Join(root, "runtime-files.sha256"))
				if err != nil {
					t.Fatal(err)
				}
				kept := []string{}
				for _, line := range strings.SplitAfter(string(manifest), "\n") {
					if line != "" && !strings.HasSuffix(line, "./config/release-rollback.json\n") {
						kept = append(kept, line)
					}
				}
				testkit.WriteFile(t, filepath.Join(root, "runtime-files.sha256"), []byte(strings.Join(kept, "")), 0o600)
			case "tampered":
				testkit.WriteFile(t, path, rollbackDeclarationPayload(releasetransition.Fingerprint(strings.Repeat("c", 64))), 0o600)
			}
			runtime := New(Config{})
			verified, err := runtime.verifyPublishedCandidateFiles(publishedCandidate{release: "1.0.0-aaaaaaaaaaaa", root: root}, fixture.runtimeRoot, nil, false)
			switch scenario {
			case "unmanifested", "tampered", "wrong-manifest-digest", "oversize":
				if err == nil {
					verified.Close()
					t.Fatalf("%s declaration passed sealed verification", scenario)
				}
				if oversize := strings.Contains(err.Error(), "exceeds its size bound"); oversize != (scenario == "oversize") {
					t.Fatalf("%s declaration error = %v", scenario, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer verified.Close()
			if scenario == "changed-after-verification" {
				testkit.WriteFile(t, path, rollbackDeclarationPayload(releasetransition.Fingerprint(strings.Repeat("c", 64))), 0o600)
			}
			declaration, err := verifiedRollbackDeclaration(verified)
			switch scenario {
			case "absent":
				if declaration != nil || err != nil {
					t.Fatalf("absent declaration = %#v, %v", declaration, err)
				}
			case "valid":
				if err != nil || declaration == nil || declaration.PredecessorRegistryDigest != releasetransition.Fingerprint(strings.Repeat("a", 64)) {
					t.Fatalf("valid declaration = %#v, %v", declaration, err)
				}
			default:
				if err == nil || declaration != nil {
					t.Fatalf("%s declaration = %#v, %v", scenario, declaration, err)
				}
			}
		})
	}
}
