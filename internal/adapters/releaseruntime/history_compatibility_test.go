package releaseruntime

import (
	"bytes"
	"context"
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

// An active release whose registry only retires prefixes admits its declared
// predecessor without writing; an undeclared release keeps the refusal.
func TestHistoryCompatibilityVerbAdmitsDeclaredEquivalent(t *testing.T) {
	const active, target = "1.1.0-aaaaaaaaaaaa", "1.0.0-bbbbbbbbbbbb"
	for _, test := range []struct {
		name     string
		declared bool
		code     int
	}{{name: "declared predecessor", declared: true, code: 0}, {name: "undeclared", code: 1}} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newHistoryFixture(t)
			retired, _, predecessor := fixture.retiredRegistry(t)
			if !test.declared {
				predecessor = releasetransition.Fingerprint(strings.Repeat("f", 64))
			}
			fixture.writeRelease(t, fixture.runtimeRoot, active, map[string][]byte{
				"config/release-transition.json": retired,
				"config/release-rollback.json":   rollbackDeclarationPayload(predecessor),
			}, nil)
			fixture.activate(t, active)
			fixture.writeCompleteLedger(t)
			fixture.writeRelease(t, fixture.runtimeRoot, target, map[string][]byte{"config/release-transition.json": fixture.registry}, nil)
			before := treeDigest(t, filepath.Dir(fixture.runtimeRoot))
			code, report, stderr := runHistoryVerb(t, fixture.runtimeRoot, fixture.configHome, target)
			if code != test.code || report.Kind != releasetransition.HistoryEquivalent || report.Registry != releasetransition.RegistryDiffers ||
				report.Admitted != test.declared || report.DeclaredPredecessor != test.declared {
				t.Fatalf("verdict %d %#v, stderr %q", code, report, stderr)
			}
			if test.declared && len(report.Refusals) != 0 || !test.declared && (len(report.Refusals) != 1 ||
				report.Refusals[0].Reason != releasetransition.HistoryRegistryDiffers) {
				t.Fatalf("refusals = %#v", report.Refusals)
			}
			if after := treeDigest(t, filepath.Dir(fixture.runtimeRoot)); after != before {
				t.Fatal("history verdict changed the installation")
			}
		})
	}
}

// A declaration that cannot be verified refuses only an Equivalent target, for
// the verb as for the runtime selection and the engine. The verb reports that
// refusal as a verification error.
func TestHistoryCompatibilityVerbDeclarationErrorsRefuseOnlyEquivalent(t *testing.T) {
	const active, target = "1.1.0-aaaaaaaaaaaa", "1.0.0-bbbbbbbbbbbb"
	for _, test := range []struct {
		name                       string
		retireActive, retireTarget bool
		// targetDeclaration formats the target's declaration around the
		// quoted predecessor digest; empty is the schema 1 declaration.
		targetDeclaration string
		code              int
		kind              releasetransition.HistoryCompatibilityKind
	}{
		{name: "identical", code: 0, kind: releasetransition.HistoryIdentity},
		{name: "equivalent predecessor", retireActive: true, code: 2},
		{name: "equivalent successor with a malformed digest", retireTarget: true, code: 2,
			targetDeclaration: `{"schemaVersion":1,"predecessorRegistryDigest":"AAAA","migrations":[],"profileFormats":[]}`},
		{name: "equivalent successor without a schema version", retireTarget: true, code: 2,
			targetDeclaration: `{"predecessorRegistryDigest":%q}`},
		{name: "equivalent successor with readable declaration", retireTarget: true, code: 0, kind: releasetransition.HistoryEquivalent},
		// A newer release's declaration is read only for its predecessor digest.
		{name: "equivalent successor with unknown fields", retireTarget: true, code: 0, kind: releasetransition.HistoryEquivalent,
			targetDeclaration: `{"schemaVersion":1,"predecessorRegistryDigest":%q,"migrations":[],"profileFormats":[],"registryFeatures":["retired-dependencies"]}`},
		{name: "equivalent successor with a higher schema", retireTarget: true, code: 0, kind: releasetransition.HistoryEquivalent,
			targetDeclaration: `{"schemaVersion":2,"predecessorRegistryDigest":%q}`},
		{name: "equivalent successor with entries", retireTarget: true, code: 0, kind: releasetransition.HistoryEquivalent,
			targetDeclaration: `{"schemaVersion":2,"predecessorRegistryDigest":%q,"migrations":[{"id":"future"}],"profileFormats":[{"profile":"future","format":2}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newHistoryFixture(t)
			retired, _, canonical := fixture.retiredRegistry(t)
			activeRegistry, targetRegistry := fixture.registry, fixture.registry
			if test.retireActive {
				activeRegistry = retired
			}
			targetFiles := map[string][]byte{}
			if test.retireTarget {
				targetRegistry = retired
				targetFiles["config/release-rollback.json"] = rollbackDeclarationPayload(canonical)
				if test.targetDeclaration != "" {
					declaration := test.targetDeclaration
					if strings.Contains(declaration, "%q") {
						declaration = fmt.Sprintf(declaration, canonical)
					}
					targetFiles["config/release-rollback.json"] = []byte(declaration)
				}
			}
			targetFiles["config/release-transition.json"] = targetRegistry
			fixture.writeRelease(t, fixture.runtimeRoot, active, map[string][]byte{
				"config/release-transition.json": activeRegistry,
				"config/release-rollback.json":   rollbackDeclarationPayload("AAAA"),
			}, nil)
			fixture.activate(t, active)
			fixture.writeCompleteLedger(t)
			fixture.writeRelease(t, fixture.runtimeRoot, target, targetFiles, nil)
			code, report, stderr := runHistoryVerb(t, fixture.runtimeRoot, fixture.configHome, target)
			if code != test.code {
				t.Fatalf("verdict %d %#v, stderr %q", code, report, stderr)
			}
			if code == 2 {
				if !strings.Contains(stderr, "rollback declaration of the release pair cannot be verified") {
					t.Fatalf("stderr = %q", stderr)
				}
				return
			}
			if !report.Admitted || report.Kind != test.kind || report.Successor != test.retireTarget || report.DeclaredSuccessor != test.retireTarget {
				t.Fatalf("verdict %#v", report)
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

// retiredRegistry retires the shipped project-state prefix, which has no
// definitions: the smallest registry that differs only in retired prefixes.
func (fixture historyFixture) retiredRegistry(t *testing.T) ([]byte, releasetransition.RegistryV2, releasetransition.Fingerprint) {
	t.Helper()
	retired, err := fixture.parsed.RetirePrefixes(map[string]int{"project-state": 1})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(retired)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _, err := releasetransition.ParseRegistryV2(payload, releasetransition.BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatalf("retired registry: %v", err)
	}
	predecessor, err := fixture.parsed.CanonicalDigest()
	if err != nil {
		t.Fatal(err)
	}
	return payload, parsed, predecessor
}

// Callers read registry admission, not whole admission. A declared equivalent
// predecessor behaves like identical registries; an undeclared one like
// differing registries. A history refusal is neither a registry refusal nor
// an admission.
func TestHistorySelectionReadsRegistryAdmission(t *testing.T) {
	fixture := newHistoryFixture(t)
	olderPayload, _ := fixture.olderRegistry(t)
	older, _, err := releasetransition.ParseRegistryV2(olderPayload, releasetransition.BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	_, retired, predecessor := fixture.retiredRegistry(t)
	corrupt := &releasetransition.HistoryEvidence{Registry: fixture.parsed, Ledger: releasetransition.ProtectedSnapshot{Exists: true, Payload: []byte("{}")}}
	publication := errors.New("cannot list published releases")
	differs := "the selected release's migration registry differs from the active release's registry"
	for _, test := range []struct {
		name     string
		input    releasetransition.HistoryCompatibilityInput
		kind     releasetransition.HistoryCompatibilityKind
		relation releasetransition.HistoryRegistryRelation
		admitted bool
	}{
		{name: "identical with history refusal", kind: releasetransition.HistoryIdentity, relation: releasetransition.RegistryIdentical, admitted: true,
			input: releasetransition.HistoryCompatibilityInput{Active: &fixture.parsed, Target: &fixture.parsed, History: corrupt}},
		{name: "declared equivalent with history refusal", kind: releasetransition.HistoryEquivalent, relation: releasetransition.RegistryDiffers, admitted: true,
			input: releasetransition.HistoryCompatibilityInput{Active: &retired, Target: &fixture.parsed, Predecessor: predecessor, History: corrupt}},
		{name: "undeclared equivalent", kind: releasetransition.HistoryEquivalent, relation: releasetransition.RegistryDiffers,
			input: releasetransition.HistoryCompatibilityInput{Active: &retired, Target: &fixture.parsed, History: corrupt}},
		{name: "registry differs", kind: releasetransition.HistoryRefuse, relation: releasetransition.RegistryDiffers,
			input: releasetransition.HistoryCompatibilityInput{Active: &fixture.parsed, Target: &older}},
	} {
		t.Run(test.name, func(t *testing.T) {
			verdict := releasetransition.HistoryCompatibility(test.input)
			if verdict.Admitted() || verdict.Kind != test.kind || verdict.Registry != test.relation || verdict.RegistryAdmitted() != test.admitted {
				t.Fatalf("fixture verdict = %#v", verdict)
			}
			history := historySelection{targetRegistry: "x", verdict: verdict, publicationErr: publication}
			if history.needsSourceFloor() == test.admitted {
				t.Fatalf("source floor = %v", history.needsSourceFloor())
			}
			message, cause := history.refusal()
			if errors.Is(cause, publication) != test.admitted || !test.admitted && message != differs {
				t.Fatalf("refusal = %q, %v", message, cause)
			}
			selection := &releaseSelection{request: releasetransition.ProcessRequest{Target: "1.0.0-aaaaaaaaaaaa"}, compatibility: &verdict}
			if unapprovedBootstrapPublication(selection) == test.admitted {
				t.Fatal("bootstrap publication refusal does not follow registry admission")
			}
			if !test.admitted {
				return
			}
			if !(historySelection{targetRegistry: "x", missingHistory: true, verdict: verdict}).needsSourceFloor() {
				t.Fatal("an installed registry without history skipped the source floor")
			}
		})
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
			predecessor, err := declaredPredecessor(verified, strictRollbackPredecessor)
			switch scenario {
			case "absent":
				if predecessor != "" || err != nil {
					t.Fatalf("absent declaration = %q, %v", predecessor, err)
				}
			case "valid":
				if err != nil || predecessor != releasetransition.Fingerprint(strings.Repeat("a", 64)) {
					t.Fatalf("valid declaration = %q, %v", predecessor, err)
				}
			default:
				if err == nil || predecessor != "" {
					t.Fatalf("%s declaration = %q, %v", scenario, predecessor, err)
				}
			}
		})
	}
}

func TestEngineRollbackPredecessor(t *testing.T) {
	root := testkit.TempDir(t)
	if digest, err := EngineRollbackPredecessor(root); err != nil || digest != "" {
		t.Fatalf("absent declaration = %q, %v", digest, err)
	}
	path := filepath.Join(root, "config", "release-rollback.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	predecessor := strings.Repeat("a", 64)
	testkit.WriteFile(t, path, []byte(`{"schemaVersion":1,"predecessorRegistryDigest":"`+predecessor+`","migrations":[],"profileFormats":[]}`+"\n"), 0o600)
	if digest, err := EngineRollbackPredecessor(root); err != nil || digest != releasetransition.Fingerprint(predecessor) {
		t.Fatalf("declared predecessor = %q, %v", digest, err)
	}
	testkit.WriteFile(t, path, []byte(`{"schemaVersion":2}`), 0o600)
	if digest, err := EngineRollbackPredecessor(root); err == nil || digest != "" {
		t.Fatalf("invalid declaration = %q, %v", digest, err)
	}
	shipped, err := EngineRollbackPredecessor(filepath.Join("..", "..", ".."))
	if err != nil || shipped == "" {
		t.Fatalf("shipped declaration = %q, %v", shipped, err)
	}
}

func TestHistorySelectionCollectsDeclarationAndRegistryRefusals(t *testing.T) {
	for _, test := range []struct {
		name                                         string
		declared, tamperActive, tamperTarget, reader bool
	}{
		{name: "undeclared target failure", tamperTarget: true, reader: true},
		{name: "declared target failure", declared: true, tamperTarget: true, reader: true},
		{name: "active failure with an unaware target", declared: true, tamperActive: true},
		{name: "both declarations fail with an unaware target", declared: true, tamperActive: true, tamperTarget: true},
		{name: "active failure with a capable target", declared: true, tamperActive: true, reader: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newHistoryFixture(t)
			canonical, err := fixture.parsed.CanonicalDigest()
			if err != nil {
				t.Fatal(err)
			}
			retired, err := fixture.parsed.RetirePrefixes(map[string]int{"owner-registration": 3, "power-metadata": 3, "settings": 3})
			if err != nil {
				t.Fatal(err)
			}
			payload, err := json.Marshal(retired)
			if err != nil {
				t.Fatal(err)
			}
			activeFiles := map[string][]byte{"config/release-transition.json": payload}
			if test.declared {
				activeFiles["config/release-rollback.json"] = rollbackDeclarationPayload(canonical)
			}
			activeRoot := fixture.writeRelease(t, fixture.runtimeRoot, "1.1.0-aaaaaaaaaaaa", activeFiles, nil)
			var features []string
			if test.reader {
				features = releasetransition.RegistryReaderFeatures()
			}
			declaration, err := json.Marshal(releasetransition.RollbackDeclaration{
				SchemaVersion: 1, PredecessorRegistryDigest: canonical,
				Migrations: []json.RawMessage{}, ProfileFormats: []json.RawMessage{},
				RegistryFeatures: features,
			})
			if err != nil {
				t.Fatal(err)
			}
			targetID := releasetransition.ReleaseID("1.0.0-bbbbbbbbbbbb")
			targetRoot := fixture.writeRelease(t, fixture.runtimeRoot, string(targetID), map[string][]byte{
				"config/release-transition.json": fixture.registry, "config/release-rollback.json": declaration,
			}, nil)
			runtime := New(Config{})
			active, err := runtime.verifyPublishedCandidateFiles(publishedCandidate{release: "1.1.0-aaaaaaaaaaaa", root: activeRoot}, fixture.runtimeRoot, nil, false)
			if err != nil {
				t.Fatal(err)
			}
			defer active.Close()
			target, err := runtime.verifyPublishedCandidateFiles(publishedCandidate{release: targetID, root: targetRoot}, fixture.runtimeRoot, nil, false)
			if err != nil {
				t.Fatal(err)
			}
			defer target.Close()
			// Valid JSON with different bytes still violates the captured seal.
			if test.tamperActive {
				testkit.WriteFile(t, filepath.Join(activeRoot, "config", "release-rollback.json"), append(rollbackDeclarationPayload(canonical), '\n'), 0o600)
			}
			if test.tamperTarget {
				testkit.WriteFile(t, filepath.Join(targetRoot, "config", "release-rollback.json"), append(declaration, '\n'), 0o600)
			}
			before := treeDigest(t, filepath.Dir(fixture.runtimeRoot))
			history, message, err := assembleHistorySelection(releasetransition.ProcessRequest{
				RuntimeRoot: fixture.runtimeRoot, ConfigHome: fixture.configHome, Target: targetID,
				Direction: releasetransition.DirectionActivatePrevious,
			}, runtimeLinkSnapshot{}, currentSnapshot{}, active, target)
			registryMessage, _ := history.verdict.Refusal()
			if err == nil || !strings.Contains(message, rollbackDeclarationRefusal) || !strings.Contains(message, registryMessage) ||
				!strings.Contains(err.Error(), "rollback declaration does not match its verified manifest") ||
				registryMessage == "" {
				t.Fatalf("declaration and registry findings = %q, %v; verdict %#v", message, err, history.verdict)
			}
			wantDeclarationErrors := 1
			if test.tamperActive && test.tamperTarget {
				wantDeclarationErrors = 2
			}
			if strings.Count(err.Error(), "rollback declaration does not match its verified manifest") != wantDeclarationErrors {
				t.Fatalf("lost an independent declaration error: %v", err)
			}
			if history.verdict.Relation != releasetransition.HistoryEquivalent {
				t.Fatalf("lost equivalent underlying relation: %#v", history.verdict)
			}
			if test.tamperActive && test.tamperTarget && strings.Index(err.Error(), "active release's rollback declaration") >= strings.Index(err.Error(), "selected release's rollback declaration") {
				t.Fatalf("unstable declaration refusal order: %v", err)
			}
			if test.tamperTarget || !test.reader {
				if !errors.Is(err, releasetransition.ErrRegistryInvalid) || history.verdict.Refusals[0].Reason != releasetransition.HistoryRegistryUnreadable {
					t.Fatalf("lost registry unreadability: %#v, %v", history.verdict, err)
				}
			}
			if strings.Index(message, registryMessage) >= strings.Index(message, rollbackDeclarationRefusal) {
				t.Fatalf("unstable refusal order: %q", message)
			}
			if after := treeDigest(t, filepath.Dir(fixture.runtimeRoot)); after != before {
				t.Fatal("failed selection changed the installation")
			}
		})
	}
}

func TestRollbackFactsRejectUnverifiableRegistryFeatures(t *testing.T) {
	fixture := newProtectedRuntimeTransitionFixture(t, releasetransition.JournalComplete)
	root := filepath.Dir(filepath.Dir(fixture.engine))
	declaration := []byte(fmt.Sprintf(`{"schemaVersion":1,"predecessorRegistryDigest":%q,"migrations":[],"profileFormats":[],"registryFeatures":{}}`, strings.Repeat("a", 64)))
	testkit.WriteFile(t, filepath.Join(root, "config", "release-rollback.json"), declaration, 0o600)
	manifest := mustReadFile(t, filepath.Join(root, "runtime-files.sha256"))
	manifest = append(manifest, []byte(fmt.Sprintf("%x  ./config/release-rollback.json\n", sha256.Sum256(declaration)))...)
	testkit.WriteFile(t, filepath.Join(root, "runtime-files.sha256"), manifest, 0o600)
	facts, registry, err := VerifyRollbackTarget(context.Background(), fixture.runtimeRoot, releasetransition.ReleaseID(fixture.target),
		releasetransition.Fingerprint(fmt.Sprintf("%x", sha256.Sum256(manifest))))
	if err == nil || facts != nil || registry != nil || !strings.Contains(err.Error(), rollbackDeclarationRefusal) ||
		!strings.Contains(err.Error(), "decode registry features") {
		t.Fatalf("unverifiable target reader facts = %#v, %#v, %v", facts, registry, err)
	}
}

func TestHistorySelectionKeepsPublicationFailureWhenRegistryUnreadable(t *testing.T) {
	fixture := newHistoryFixture(t)
	retired, err := fixture.parsed.RetirePrefixes(map[string]int{"owner-registration": 3, "power-metadata": 3, "settings": 3})
	if err != nil {
		t.Fatal(err)
	}
	verdict := releasetransition.HistoryCompatibility(releasetransition.HistoryCompatibilityInput{Active: &retired, Target: &retired})
	if verdict.Kind != releasetransition.HistoryRefuse || verdict.RegistryAdmitted() || verdict.Admitted() || !verdict.RelationAdmitted() {
		t.Fatalf("unaware reader admitted: %#v", verdict)
	}
	publication := errors.New("cannot inspect first publication")
	history := historySelection{verdict: verdict, publicationErr: publication}
	_, cause := history.refusal()
	if !errors.Is(cause, publication) || !errors.Is(cause, releasetransition.ErrRegistryInvalid) {
		t.Fatalf("lost independent publication or parser error: %v", cause)
	}
}
