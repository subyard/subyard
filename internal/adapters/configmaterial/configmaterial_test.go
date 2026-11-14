package configmaterial

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestTOMLMaterializationPreservesRuntimeFields(t *testing.T) {
	h := newGuestHarness(t)
	h.format = "toml"
	desired := []byte("approval_policy = 'on-request'\nsandbox_mode = 'danger-full-access'\n")
	original := string(desired) + "# Runtime-owned additions\n[hooks]\nenabled = true\n[projects.sample]\ntrust_level = 'trusted'\n[tui]\nnotifications = true\n"
	h.writeDestination(t, original)
	h.apply(t, desired)
	first := h.observe(t, desired)
	if !first.Converged {
		t.Fatal("runtime-added hooks/projects/tui caused materialized drift")
	}
	payload, err := os.ReadFile(h.destination)
	if err != nil || string(payload) != original {
		t.Fatalf("adoption changed otherwise converged TOML: err=%v", err)
	}
	info, err := os.Stat(h.destination)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("TOML adoption did not enforce materialized file mode: err=%v", err)
	}
	h.writeDestination(t, strings.Replace(original, "notifications = true", "notifications = false", 1))
	second := h.observe(t, desired)
	if !second.Converged || first.Fingerprint != second.Fingerprint {
		t.Fatal("unmanaged TOML change invalidated readiness")
	}
	h.writeDestination(t, strings.Replace(original, "'on-request'", "'never'", 1))
	if h.observe(t, desired).Converged {
		t.Fatal("managed approval policy drift was ignored")
	}
	h.apply(t, desired)
	if !h.observe(t, desired).Converged {
		t.Fatal("managed TOML repair did not converge")
	}
	payload, _ = os.ReadFile(h.destination)
	for _, runtimeKey := range []string{"[hooks]", "[projects.sample]", "[tui]"} {
		if !bytes.Contains(payload, []byte(runtimeKey)) {
			t.Fatalf("repair lost runtime table %s", runtimeKey)
		}
	}
	baseline, _ := os.ReadFile(h.baselinePath(t))
	if bytes.Contains(baseline, []byte("on-request")) || bytes.Contains(baseline, []byte("projects")) {
		t.Fatal("baseline captured runtime fields or configuration values")
	}
}

func TestManagedInvalidDestinationIsDriftAndRepairable(t *testing.T) {
	for _, format := range []string{"json", "toml"} {
		for _, broken := range []string{"", "[broken"} {
			t.Run(format+fmt.Sprint(len(broken)), func(t *testing.T) {
				h := newGuestHarness(t)
				h.format = format
				desired := []byte(`{"managed":true}`)
				if format == "toml" {
					desired = []byte("managed = true\n")
				}
				h.apply(t, desired)
				h.writeDestination(t, broken)
				if h.observe(t, desired).Converged {
					t.Fatal("invalid managed destination reported current")
				}
				h.apply(t, desired)
				if !h.observe(t, desired).Converged {
					t.Fatal("repair did not converge")
				}
			})
		}
	}
}

func TestMaterializationIgnoresInterruptedTempsWithoutFollowingLinks(t *testing.T) {
	for _, format := range []string{"json", "toml"} {
		t.Run(format, func(t *testing.T) {
			h := newGuestHarness(t)
			h.format = format
			desired := []byte(`{"managed":true}`)
			if format == "toml" {
				desired = []byte("managed = true\n")
			}
			h.apply(t, desired)
			orphan := filepath.Join(filepath.Dir(h.destination), ".subyard-config.interrupted")
			testkit.WriteFile(t, orphan, []byte("unpublished partial document"), 0600)
			link := filepath.Join(filepath.Dir(h.destination), ".subyard-config.foreign-link")
			if err := os.Symlink(orphan, link); err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(orphan)
			if err != nil {
				t.Fatal(err)
			}
			if !h.observe(t, desired).Converged {
				t.Fatal("orphan counted as managed artifact")
			}
			h.writeDestination(t, "")
			if h.observe(t, desired).Converged {
				t.Fatal("empty managed destination not reported as drift")
			}
			h.apply(t, desired)
			if !h.observe(t, desired).Converged {
				t.Fatal("orphan prevented repair")
			}
			after, err := os.Lstat(orphan)
			payload, readErr := os.ReadFile(orphan)
			if err != nil || readErr != nil || !os.SameFile(before, after) || string(payload) != "unpublished partial document" {
				t.Fatal("orphan followed or changed", err, readErr)
			}
			if target, err := os.Readlink(link); err != nil || target != orphan {
				t.Fatal("unrelated symlink changed", err)
			}
		})
	}
}

func TestStructuredWriterRefusesChangedTransportBeforeMutation(t *testing.T) {
	for _, format := range []string{"json", "toml"} {
		t.Run(format, func(t *testing.T) {
			h := newGuestHarness(t)
			h.format = format
			payload, substituted := []byte(`{"managed":true}`), []byte(`{}`)
			if format == "toml" {
				payload, substituted = []byte("sandbox_mode = 'danger-full-access'\napproval_policy = 'on-request'\n"), nil
				h.writeDestination(t, string(payload)+"[projects.sample]\ntrust_level = 'trusted'\n")
			}
			h.apply(t, payload)
			before := snapshotMaterialization(t, h.root)
			request, err := h.request(payload, ModeApply)
			if err != nil {
				t.Fatal(err)
			}
			command := exec.Command(request.Command[0], request.Command[1:]...)
			command.Stdin = bytes.NewReader(substituted)
			if output, err := command.CombinedOutput(); err == nil {
				t.Fatalf("changed transport accepted: %s", output)
			}
			if !jsonEqual(before, snapshotMaterialization(t, h.root)) {
				t.Fatal("changed transport modified managed state")
			}
		})
	}
}

func TestTOMLMaterializationRetiresOwnedFieldsAndPreservesScalarTypes(t *testing.T) {
	h := newGuestHarness(t)
	h.format = "toml"
	h.writeDestination(t, "[managed]\nold = true\nruntime = 'kept'\n[runtime]\ndate = 2026-09-13\ntime = 12:34:56\nstamp = 2026-09-13T12:34:56Z\nfloat = 1.5\ninteger = 1\nnan = nan\ninf = inf\n[[hooks.Stop]]\ncommand = 'synthetic-hook'\n")
	h.apply(t, []byte("[managed]\nold = true\n"))
	desired := []byte("[managed]\nfresh = 2\n")
	h.apply(t, desired)
	if !h.observe(t, desired).Converged {
		t.Fatal("TOML retirement did not converge")
	}
	// Observe the preserved runtime data as a new desired template to verify a
	// real TOML round trip, including hooks represented as arrays of tables.
	preserved := []byte("[managed]\nfresh = 2\nruntime = 'kept'\n[runtime]\ndate = 2026-09-13\ntime = 12:34:56\nstamp = 2026-09-13T12:34:56Z\nfloat = 1.5\ninteger = 1\nnan = nan\ninf = inf\n[[hooks.Stop]]\ncommand = 'synthetic-hook'\n")
	before, _ := os.ReadFile(h.destination)
	if bytes.Contains(before, []byte("old =")) {
		t.Fatal("retired owned field remained")
	}
	h.apply(t, preserved)
	after, _ := os.ReadFile(h.destination)
	if !bytes.Equal(before, after) || !h.observe(t, preserved).Converged {
		t.Fatal("TOML values changed type or value during serialization")
	}
	// TOML integers and floats have different types even when numerically equal.
	h.writeDestination(t, strings.Replace(string(after), "integer = 1", "integer = 1.0", 1))
	if h.observe(t, preserved).Converged {
		t.Fatal("TOML numeric type drift was ignored")
	}
}

func TestTOMLMaterializationRejectsInvalidInputsWithoutOverwrite(t *testing.T) {
	for _, malformed := range []string{"policy = 'secret'\npolicy = 'duplicate'", "[broken\nsecret"} {
		t.Run(malformed[:6], func(t *testing.T) {
			h := newGuestHarness(t)
			h.format = "toml"
			original := "policy = 'preserve'\n"
			h.writeDestination(t, original)
			stderr, err := h.run([]byte(malformed), ModeApply)
			if err == nil || strings.Contains(stderr, "secret") {
				t.Fatalf("unsafe desired TOML handling: error=%v stderr=%q", err, stderr)
			}
			got, _ := os.ReadFile(h.destination)
			if string(got) != original {
				t.Fatal("invalid desired TOML changed the destination")
			}
			h.writeDestination(t, malformed)
			stderr, err = h.run([]byte(original), ModeApply)
			if err == nil || strings.Contains(stderr, "secret") {
				t.Fatalf("unsafe current TOML handling: error=%v stderr=%q", err, stderr)
			}
			got, _ = os.ReadFile(h.destination)
			if string(got) != malformed {
				t.Fatal("invalid current TOML was overwritten")
			}
		})
	}
}

func TestDesiredDigestIsSemanticAndRequiresObject(t *testing.T) {
	first, err := DesiredDigest([]byte("{\n  \"enabled\": true, \"nested\": {\"count\": 2}\n}\n"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := DesiredDigest([]byte(`{"nested":{"count":2},"enabled":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if first != second || len(first) != 64 {
		t.Fatalf("semantic digests = %q and %q", first, second)
	}
	for _, payload := range [][]byte{[]byte(`[]`), []byte(`{"broken":`)} {
		if _, err := DesiredDigest(payload); err == nil || strings.Contains(err.Error(), "broken") {
			t.Fatalf("unsafe desired JSON error = %v", err)
		}
	}
	if _, err := DesiredDigest([]byte(`{"duplicate":1,"duplicate":2}`)); err == nil {
		t.Fatal("duplicate desired key was accepted")
	}
	integer, err := DesiredDigest([]byte(`{"number":1}`))
	if err != nil {
		t.Fatal(err)
	}
	decimal, err := DesiredDigest([]byte(`{"number":1.00e0}`))
	if err != nil || integer != decimal {
		t.Fatalf("equivalent number digests = %q and %q, err=%v", integer, decimal, err)
	}
}

func TestJSONMaterializationPreservesRuntimeFieldsAndRetiresOwnedFields(t *testing.T) {
	harness := newGuestHarness(t)
	harness.writeDestination(t, `{"managed":{"old":1,"keep":"stale"},"hooks":["runtime"],"statusLine":"profile-state"}`)

	harness.apply(t, []byte(`{"managed":{"old":1,"keep":"wanted"}}`))
	harness.apply(t, []byte(`{"managed":{"keep":"next","fresh":true}}`))

	got := harness.readDestination(t)
	want := map[string]any{
		"managed": map[string]any{"keep": "next", "fresh": true},
		"hooks":   []any{"runtime"}, "statusLine": "profile-state",
	}
	if !jsonEqual(got, want) {
		t.Fatalf("materialized JSON = %#v, want %#v", got, want)
	}
	observation := harness.observe(t, []byte(`{"managed":{"fresh":true,"keep":"next"}}`))
	if !observation.Converged || len(observation.Fingerprint) != 64 {
		t.Fatalf("observation = %#v", observation)
	}
	before := observation.Fingerprint
	got["profileRuntime"] = map[string]any{"hook": true}
	payload, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(harness.destination, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	after := harness.observe(t, []byte(`{"managed":{"fresh":true,"keep":"next"}}`))
	if !after.Converged || after.Fingerprint != before {
		t.Fatalf("runtime-only field changed managed observation: before=%q after=%#v", before, after)
	}
	baseline, err := os.ReadFile(harness.baselinePath(t))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(baseline, []byte("wanted")) || bytes.Contains(baseline, []byte("next")) ||
		bytes.Contains(baseline, []byte("profileRuntime")) {
		t.Fatalf("baseline contains configuration values: %s", baseline)
	}
}

func TestJSONMaterializationOwnsArraysAndHandlesTypeTransitions(t *testing.T) {
	harness := newGuestHarness(t)
	harness.writeDestination(t, `{"managed":{"runtime":true},"sibling":"kept"}`)
	harness.apply(t, []byte(`{"managed":["one"]}`))
	harness.apply(t, []byte(`{"managed":{"leaf":2}}`))

	got := harness.readDestination(t)
	want := map[string]any{"managed": map[string]any{"leaf": float64(2)}, "sibling": "kept"}
	if !jsonEqual(got, want) {
		t.Fatalf("materialized JSON = %#v, want %#v", got, want)
	}
	if observation := harness.observe(t, []byte(`{"managed":{"leaf":3}}`)); observation.Converged {
		t.Fatalf("managed drift converged: %#v", observation)
	}
}

func TestJSONObservationDistinguishesBooleanFromNumber(t *testing.T) {
	harness := newGuestHarness(t)
	harness.apply(t, []byte(`{"managed":true}`))
	harness.writeDestination(t, `{"managed":1}`)
	if observation := harness.observe(t, []byte(`{"managed":true}`)); observation.Converged {
		t.Fatalf("numeric managed value matched boolean: %#v", observation)
	}
}

func TestJSONScalarToObjectTransitionPreservesRuntimeChildren(t *testing.T) {
	harness := newGuestHarness(t)
	harness.apply(t, []byte(`{"managed":1}`))
	harness.writeDestination(t, `{"managed":{"runtime":true}}`)
	harness.apply(t, []byte(`{"managed":{"desired":2}}`))
	got := harness.readDestination(t)
	want := map[string]any{
		"managed": map[string]any{"runtime": true, "desired": float64(2)},
	}
	if !jsonEqual(got, want) {
		t.Fatalf("scalar-to-object transition = %#v, want %#v", got, want)
	}
}

func TestJSONMaterializationEmptyObjectPreservesRuntimeChildren(t *testing.T) {
	harness := newGuestHarness(t)
	harness.writeDestination(t, `{"managed":{"runtime":true}}`)
	harness.apply(t, []byte(`{"managed":{}}`))
	harness.apply(t, []byte(`{}`))

	got := harness.readDestination(t)
	want := map[string]any{"managed": map[string]any{"runtime": true}}
	if !jsonEqual(got, want) {
		t.Fatalf("retired empty object removed runtime fields: %#v", got)
	}
}

func TestJSONMaterializationRetryRepairsDestinationBeforeBaselineInterruption(t *testing.T) {
	harness := newGuestHarness(t)
	harness.apply(t, []byte(`{"managed":{"old":1}}`))
	baselinePath := harness.baselinePath(t)
	oldBaseline, err := os.ReadFile(baselinePath)
	if err != nil {
		t.Fatal(err)
	}
	harness.apply(t, []byte(`{"managed":{"new":2}}`))
	if err := os.WriteFile(baselinePath, oldBaseline, 0o600); err != nil {
		t.Fatal(err)
	}
	if observation := harness.observe(t, []byte(`{"managed":{"new":2}}`)); observation.Converged {
		t.Fatal("stale baseline converged after simulated interrupted apply")
	}
	harness.apply(t, []byte(`{"managed":{"new":2}}`))
	if observation := harness.observe(t, []byte(`{"managed":{"new":2}}`)); !observation.Converged {
		t.Fatalf("retry did not converge: %#v", observation)
	}
}

func TestJSONMaterializationRejectsDestinationSymlink(t *testing.T) {
	harness := newGuestHarness(t)
	target := filepath.Join(harness.root, "outside.json")
	if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, harness.destination); err != nil {
		t.Fatal(err)
	}
	stderr, err := harness.run([]byte(`{"managed":1}`), ModeApply)
	if err == nil || !strings.Contains(stderr, "invalid destination path") {
		t.Fatalf("symlink apply error=%v stderr=%q", err, stderr)
	}
	got, readErr := os.ReadFile(target)
	if readErr != nil || string(got) != `{}` {
		t.Fatalf("symlink target changed: %q err=%v", got, readErr)
	}
}

func TestJSONObservationIsReadOnlyWhenBaselineIsMissing(t *testing.T) {
	harness := newGuestHarness(t)
	harness.writeDestination(t, `{"managed":1}`)
	before := treeNames(t, harness.root)
	observation := harness.observe(t, []byte(`{"managed":1}`))
	after := treeNames(t, harness.root)
	if observation.Converged || strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Fatalf("missing-baseline observation mutated state: before=%v after=%v result=%#v",
			before, after, observation)
	}
}

func TestJSONObservationRequiresDestinationForEmptyTemplate(t *testing.T) {
	harness := newGuestHarness(t)
	harness.apply(t, []byte(`{}`))
	if err := os.Remove(harness.destination); err != nil {
		t.Fatal(err)
	}
	if observation := harness.observe(t, []byte(`{}`)); observation.Converged {
		t.Fatalf("missing empty destination converged: %#v", observation)
	}
}

func TestJSONMaterializationRejectsMalformedCurrentAndBaselineWithoutOverwrite(t *testing.T) {
	t.Run("current", func(t *testing.T) {
		harness := newGuestHarness(t)
		harness.writeDestination(t, `{"secret":"unterminated`)
		before, err := os.ReadFile(harness.destination)
		if err != nil {
			t.Fatal(err)
		}
		stderr, err := harness.run([]byte(`{"managed":1}`), ModeApply)
		if err == nil || !UnownedInvalidDestination([]byte(stderr), "json") || strings.Contains(stderr, "secret") {
			t.Fatalf("apply error = %v stderr=%q", err, stderr)
		}
		if !strings.Contains(stderr, "in the yard run mv --backup=numbered --") || !strings.Contains(stderr, "on the owner run yard config apply") {
			t.Fatal("invalid unowned destination has no executable preservation step", stderr)
		}
		after, readErr := os.ReadFile(harness.destination)
		if readErr != nil || !bytes.Equal(before, after) {
			t.Fatalf("malformed destination changed: err=%v before=%q after=%q", readErr, before, after)
		}
		backup := harness.destination + ".subyard-invalid"
		if output, err := exec.Command("mv", "--backup=numbered", "--", harness.destination, backup).CombinedOutput(); err != nil {
			t.Fatal(err, string(output))
		}
		harness.apply(t, []byte(`{"managed":1}`))
		if !harness.observe(t, []byte(`{"managed":1}`)).Converged {
			t.Fatal("following preservation step did not allow config repair")
		}
		preserved, err := os.ReadFile(backup)
		if err != nil || !bytes.Equal(before, preserved) {
			t.Fatal("preservation step lost unowned bytes", err)
		}
	})

	t.Run("baseline", func(t *testing.T) {
		harness := newGuestHarness(t)
		harness.apply(t, []byte(`{"managed":1}`))
		baseline := harness.baselinePath(t)
		if err := os.WriteFile(baseline, []byte(`{"schema":99}`), 0o600); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(harness.destination)
		if err != nil {
			t.Fatal(err)
		}
		stderr, err := harness.run([]byte(`{"managed":2}`), ModeApply)
		if err == nil || !strings.Contains(stderr, "invalid materialization baseline") {
			t.Fatalf("apply error = %v stderr=%q", err, stderr)
		}
		after, readErr := os.ReadFile(harness.destination)
		if readErr != nil || !bytes.Equal(before, after) {
			t.Fatalf("destination changed after bad baseline: err=%v", readErr)
		}
	})
}

func TestJSONRequestAndObservationParserRejectUnsafeInputs(t *testing.T) {
	for _, test := range []struct{ developer, destination string }{
		{"bad/user", "/home/dev/settings.json"},
		{"dev", "/tmp/settings.json"},
		{"dev", "/home/dev/../root/settings.json"},
	} {
		if _, err := JSONRequest(ModeObserve, test.developer, test.destination, 1000, []byte(`{}`)); err == nil {
			t.Fatalf("accepted unsafe request: %#v", test)
		}
	}
	request, err := JSONRequest(ModeApply, "1dev", "/home/1dev/settings.json", 0, []byte(`{}`))
	if err != nil || request.Command[7] != "1000" {
		t.Fatalf("legacy uid fallback request=%#v err=%v", request, err)
	}
	for _, output := range [][]byte{
		[]byte(`{"converged":true,"fingerprint":"short"}`),
		[]byte(`{"fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`),
		[]byte(`{"converged":true,"fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","extra":1}`),
		[]byte(`{"converged":false,"adoptable":"yes","fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`),
		[]byte(`{"converged":true,"fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","released":["relative"],"released_count":1}`),
		[]byte(`{"converged":true,"fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","released":["/b","/a"],"released_count":2}`),
		[]byte(`{"converged":true,"fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","released":["/a","/a"],"released_count":2}`),
		[]byte(`{"converged":true,"fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","released":["/a\u001b[2J"],"released_count":1}`),
		[]byte(`{"converged":true,"fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","release":"other"}`),
		[]byte(`{"converged":true,"fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","released":["/a","/b"],"released_count":1}`),
		[]byte(`{"converged":true,"fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","released_count":-1}`),
	} {
		if _, err := ParseJSONObservation(output); err == nil {
			t.Fatalf("accepted invalid observation %q", output)
		}
	}
}

func TestJSONObservationRejectsUnsafeStateRootWithoutMutation(t *testing.T) {
	harness := newGuestHarness(t)
	if err := os.Mkdir(harness.state, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(harness.state, 0o777); err != nil {
		t.Fatal(err)
	}
	stderr, err := harness.run([]byte(`{}`), ModeObserve)
	if err == nil || !strings.Contains(stderr, "invalid materialization state") {
		t.Fatalf("unsafe state observation error=%v stderr=%q", err, stderr)
	}
	info, statErr := os.Stat(harness.state)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Mode().Perm() != 0o777 {
		t.Fatalf("observation changed state root: mode=%v", info.Mode().Perm())
	}
}

func TestPreBaselineJSONAdoptionRequiresExactPublishedFile(t *testing.T) {
	previous := []byte("{\"managed\":\"old\"}\n")
	desired := []byte(`{"managed":"new","added":true}`)
	for _, problem := range []string{"none", "unknown", "edited", "reformatted", "added"} {
		t.Run(problem, func(t *testing.T) {
			h := newGuestHarness(t)
			h.legacyTemplates = [][]byte{previous}
			payload := previous
			switch problem {
			case "unknown":
				h.legacyTemplates = nil
			case "edited":
				payload = []byte(`{"managed":"private-value"}`)
			case "reformatted":
				payload = []byte(`{ "managed": "old" }`)
			case "added":
				payload = []byte(`{"managed":"old","custom":"private-value"}`)
			}
			testkit.WriteFile(t, h.destination, payload, 0644)
			before := snapshotMaterialization(t, h.root)
			if problem == "none" {
				observed := h.observeMode(t, desired, ModeAssessAdopt)
				if !observed.Adoptable || observed.Converged {
					t.Fatal("exact published JSON was not pending adoption")
				}
				if h.observeMode(t, []byte(`{"managed":"next","added":true}`), ModeAssessAdopt).Fingerprint == observed.Fingerprint {
					t.Fatal("adoption did not bind the desired JSON")
				}
			} else {
				stderr, err := h.run(desired, ModeAssessAdopt)
				if err == nil || strings.Contains(stderr, "private-value") {
					t.Fatal("unrecognized JSON was accepted or leaked content")
				}
			}
			if !jsonEqual(before, snapshotMaterialization(t, h.root)) {
				t.Fatal("read-only JSON assessment changed state")
			}
		})
	}
}

func TestJSONAdoptsDisjointForeignFieldsWithoutClaimingThem(t *testing.T) {
	h := newGuestHarness(t)
	desired := []byte(`{"memory":"managed","permissions":{"ask":["managed command"]}}`)
	testkit.WriteFile(t, h.destination, []byte(`{"hooks":{"start":"foreign"},"model":"foreign","permissions":{"deny":["foreign command"]}}`), 0644)
	before := snapshotMaterialization(t, h.root)
	observation := h.observeMode(t, desired, ModeAssessAdopt)
	if !observation.Adoptable || observation.Converged {
		t.Fatal("foreign-only fields not adoptable")
	}
	if !jsonEqual(before, snapshotMaterialization(t, h.root)) {
		t.Fatal("assessment changed foreign data")
	}
	if output, err := h.run(desired, ModeApply); err != nil {
		t.Fatalf("apply: %v %s", err, output)
	}
	payload, _ := os.ReadFile(h.destination)
	if !strings.Contains(string(payload), `"start":"foreign"`) || !strings.Contains(string(payload), `"deny":["foreign command"]`) || !strings.Contains(string(payload), `"memory":"managed"`) {
		t.Fatalf("merge lost fields: %s", payload)
	}
	if !h.observeMode(t, desired, ModeObserve).Converged {
		t.Fatal("merged document not converged")
	}
	if output, err := h.run(nil, ModeRetire); err != nil {
		t.Fatalf("retire: %v %s", err, output)
	}
	payload, _ = os.ReadFile(h.destination)
	if strings.Contains(string(payload), `"memory"`) || !strings.Contains(string(payload), `"deny":["foreign command"]`) || !strings.Contains(string(payload), `"start":"foreign"`) {
		t.Fatalf("retirement claimed foreign fields: %s", payload)
	}
}

func TestPreBaselineTOMLAdoptionRequiresExactPublishedFile(t *testing.T) {
	previous := []byte("# Published template\nmanaged = 'old'\n")
	desired := []byte("managed = 'new'\nadded = true\n")
	for _, problem := range []string{"none", "unknown", "edited", "added", "mode", "owner", "symlink", "hardlink", "receipt", "lock", "removed-field"} {
		t.Run(problem, func(t *testing.T) {
			h := newGuestHarness(t)
			h.format, h.legacyTemplates = "toml", [][]byte{previous}
			testkit.WriteFile(t, h.destination, previous, 0644)
			payload := desired
			switch problem {
			case "unknown":
				h.legacyTemplates = nil
			case "edited":
				testkit.WriteFile(t, h.destination, []byte("managed = 'private-value'\n"), 0644)
			case "added":
				testkit.WriteFile(t, h.destination, append(bytes.Clone(previous), []byte("custom = 'private-value'\n")...), 0644)
			case "mode":
				if err := os.Chmod(h.destination, 0666); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(h.destination, h.destination+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(h.destination+".original", h.destination); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(h.destination, h.destination+".linked"); err != nil {
					t.Fatal(err)
				}
			case "receipt", "lock":
				if err := os.Mkdir(h.state, 0700); err != nil {
					t.Fatal(err)
				}
				identity := fmt.Sprintf("%x", sha256.Sum256([]byte("dev\x00"+h.destination)))
				suffix := ".ownership"
				if problem == "lock" {
					suffix = ".lock"
				}
				testkit.WriteFile(t, filepath.Join(h.state, identity+suffix), []byte("{}\n"), 0600)
			case "removed-field":
				payload = []byte("added = true\n")
			}
			request, err := h.request(payload, ModeAssessAdopt)
			if err != nil {
				t.Fatal(err)
			}
			if problem == "owner" {
				request.Command[7] = fmt.Sprint(os.Getuid() + 1)
			}
			before := snapshotMaterialization(t, h.root)
			info, err := os.Lstat(h.destination)
			if err != nil {
				t.Fatal(err)
			}
			command := exec.Command(request.Command[0], request.Command[1:]...)
			command.Stdin = bytes.NewReader(request.Stdin)
			var stdout, stderr bytes.Buffer
			command.Stdout, command.Stderr = &stdout, &stderr
			err = command.Run()
			if !jsonEqual(before, snapshotMaterialization(t, h.root)) {
				t.Fatal("assessment changed protected state")
			}
			after, statErr := os.Lstat(h.destination)
			if statErr != nil || !os.SameFile(info, after) || info.Mode() != after.Mode() || !info.ModTime().Equal(after.ModTime()) {
				t.Fatal("assessment changed destination metadata")
			}
			if problem == "none" {
				observation, parseErr := ParseObservation(stdout.Bytes())
				if err != nil || parseErr != nil || !observation.Adoptable || observation.Converged {
					t.Fatalf("exact published file was not adoptable: %v %v %s", err, parseErr, stderr.String())
				}
				evolved := h.observeMode(t, []byte("managed = 'next'\nadded = true\n"), ModeAssessAdopt)
				if evolved.Fingerprint == observation.Fingerprint {
					t.Fatal("desired template was not bound")
				}
				// A different recognized template with identical parsed values also
				// changes the binding: the exact observed bytes are the evidence.
				alternate := append([]byte("# Another published comment\n"), previous...)
				h.legacyTemplates = append(h.legacyTemplates, alternate)
				testkit.WriteFile(t, h.destination, alternate, 0644)
				if h.observeMode(t, payload, ModeAssessAdopt).Fingerprint == observation.Fingerprint {
					t.Fatal("historical template bytes were not bound")
				}
				testkit.WriteFile(t, h.destination, previous, 0644)
				return
			}
			if err == nil || strings.Contains(stderr.String(), "private-value") {
				t.Fatalf("unsafe unrecorded adoption accepted or leaked content: %v %s", err, stderr.String())
			}
		})
	}
}

type guestHarness struct {
	t               *testing.T
	root            string
	state           string
	destination     string
	format          string
	legacyTemplates [][]byte
}

func newGuestHarness(t *testing.T) guestHarness {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home", "dev")
	if err := os.MkdirAll(filepath.Join(home, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	return guestHarness{t: t, root: root, state: filepath.Join(root, "state"),
		destination: filepath.Join(home, ".agent", "settings.json")}
}

func (h guestHarness) writeDestination(t *testing.T, payload string) {
	t.Helper()
	if err := os.WriteFile(h.destination, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (h guestHarness) apply(t *testing.T, payload []byte) {
	t.Helper()
	stderr, err := h.run(payload, ModeApply)
	if err != nil {
		t.Fatalf("apply: %v stderr=%q", err, stderr)
	}
}

func (h guestHarness) observe(t *testing.T, payload []byte) JSONObservation {
	t.Helper()
	return h.observeMode(t, payload, ModeObserve)
}

func (h guestHarness) observeMode(t *testing.T, payload []byte, mode string) JSONObservation {
	t.Helper()
	request, err := h.request(payload, mode)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(request.Command[0], request.Command[1:]...)
	command.Stdin = bytes.NewReader(request.Stdin)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("observe: %v stderr=%q", err, stderr.String())
	}
	observation, err := ParseJSONObservation(stdout.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return observation
}

func (h guestHarness) request(payload []byte, mode string) (ports.InstanceExecRequest, error) {
	format := h.format
	if format == "" {
		format = "json"
	}
	return materializationRequest(format, mode, "dev", h.destination, os.Getuid(), payload,
		h.state, os.Getuid(), filepath.Join(h.root, "home", "dev"), h.legacyTemplates...)
}

func (h guestHarness) run(payload []byte, mode string) (string, error) {
	h.t.Helper()
	request, err := h.request(payload, mode)
	if err != nil {
		return "", err
	}
	command := exec.Command(request.Command[0], request.Command[1:]...)
	command.Stdin = bytes.NewReader(request.Stdin)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	err = command.Run()
	return stderr.String(), err
}

func (h guestHarness) readDestination(t *testing.T) map[string]any {
	t.Helper()
	payload, err := os.ReadFile(h.destination)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(payload, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func (h guestHarness) baselinePath(t *testing.T) string {
	t.Helper()
	entries, err := filepath.Glob(filepath.Join(h.state, "*.json"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("baseline entries=%v err=%v", entries, err)
	}
	return entries[0]
}

func (h guestHarness) receiptPath(t *testing.T) string {
	t.Helper()
	return strings.TrimSuffix(h.baselinePath(t), ".json") + ".ownership"
}

func jsonEqual(left, right any) bool {
	leftPayload, _ := json.Marshal(left)
	rightPayload, _ := json.Marshal(right)
	return bytes.Equal(leftPayload, rightPayload)
}

func treeNames(t *testing.T, root string) []string {
	t.Helper()
	var names []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		names = append(names, relative)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func TestAdoptionAssessmentPreservesLegacyAndCurrentDocuments(t *testing.T) {
	for _, format := range []string{"json", "toml"} {
		for _, schema := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s/schema-%d", format, schema), func(t *testing.T) {
				h := newGuestHarness(t)
				h.format = format
				desired := `{"managed":{"leaf":"synthetic-secret"},"empty":{}}`
				current := `{"managed":{"leaf":"synthetic-secret","runtime":true},"empty":{"runtime":true},"history":[1,2]}`
				if format == "toml" {
					desired = "[managed]\nleaf = 'synthetic-secret'\n[empty]\n"
					current = "history = [1,2]\n[managed]\nleaf = 'synthetic-secret'\nruntime = true\n[empty]\nruntime = true\n"
				}
				before := snapshotMaterialization(t, h.root)
				if observed := h.observeMode(t, []byte(desired), ModeAssessAdopt); observed.Converged || observed.Adoptable {
					t.Fatal("absent destination reported converged or adopted")
				}
				if !jsonEqual(before, snapshotMaterialization(t, h.root)) {
					t.Fatal("creation assessment wrote state")
				}
				h.apply(t, []byte(desired))
				baselinePath := h.baselinePath(t)
				receiptPath := h.receiptPath(t)
				if schema == 2 {
					baseline, err := os.ReadFile(receiptPath)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(baselinePath, baseline, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Remove(receiptPath); err != nil {
					t.Fatal(err)
				}
				h.writeDestination(t, current)
				before = snapshotMaterialization(t, h.root)
				if h.observe(t, []byte(desired)).Converged {
					t.Fatal("legacy state did not request compatible ownership publication")
				}
				if observed := h.observeMode(t, []byte(desired), ModeAssessAdopt); !observed.Converged || !observed.Adoptable {
					t.Fatal("matching protected evidence was not adoptable")
				}
				if format == "toml" && schema == 1 {
					if _, err := h.run([]byte(desired+"# changed template\n"), ModeAssessAdopt); err == nil {
						t.Fatal("legacy TOML accepted a different raw template digest")
					}
				}
				if !jsonEqual(before, snapshotMaterialization(t, h.root)) {
					t.Fatal("adoption assessment modified files")
				}
				h.apply(t, []byte(desired))
				baseline, err := os.ReadFile(baselinePath)
				if err != nil {
					t.Fatal(err)
				}
				var value map[string]any
				if err := json.Unmarshal(baseline, &value); err != nil || value["schema"] != float64(1) || len(value) != 5 {
					t.Fatalf("normal apply did not preserve the retained-runtime baseline schema: %v", err)
				}
				if _, err := os.Stat(receiptPath); err != nil {
					t.Fatal("normal apply did not establish ownership proof", err)
				}
				if !h.observe(t, []byte(desired)).Converged {
					t.Fatal("compatible ownership publication did not converge")
				}
				h.observeMode(t, nil, ModeRetire)
				payload, err := os.ReadFile(h.destination)
				if err != nil || bytes.Contains(payload, []byte("synthetic-secret")) || !bytes.Contains(payload, []byte("runtime")) || !bytes.Contains(payload, []byte("history")) {
					t.Fatalf("retirement after adoption lost runtime data: %v", err)
				}
			})
		}
	}
}

func TestAdoptionAssessmentRejectsUnknownOrChangedEvidence(t *testing.T) {
	for _, kind := range []string{"missing", "mode", "invalid", "baseline-digest", "owned-paths", "managed", "legacy-managed", "projection", "retiring", "retired"} {
		t.Run(kind, func(t *testing.T) {
			h := newGuestHarness(t)
			desired := []byte(`{"managed":"synthetic-secret"}`)
			h.apply(t, desired)
			baselinePath := h.baselinePath(t)
			if kind == "projection" || kind == "retiring" || kind == "retired" {
				baselinePath = h.receiptPath(t)
			}
			baseline, err := os.ReadFile(baselinePath)
			if err != nil {
				t.Fatal(err)
			}
			var value map[string]any
			if err := json.Unmarshal(baseline, &value); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "invalid":
				value["schema"] = 99
			case "owned-paths":
				value["owned"] = []any{}
			case "baseline-digest":
				value["desired_digest"] = strings.Repeat("0", 64)
			case "projection":
				value["projection_digest"] = strings.Repeat("0", 64)
			case "retiring", "retired":
				value["phase"] = kind
				value["retired_digest"] = strings.Repeat("0", 64)
				if kind == "retired" {
					shared, err := os.ReadFile(h.baselinePath(t))
					if err != nil {
						t.Fatal(err)
					}
					var released map[string]any
					if err := json.Unmarshal(shared, &released); err != nil {
						t.Fatal(err)
					}
					released["owned"] = []any{}
					shared, _ = json.Marshal(released)
					if err := os.WriteFile(h.baselinePath(t), shared, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			case "legacy-managed":
				if err := os.Remove(h.receiptPath(t)); err != nil {
					t.Fatal(err)
				}
			}
			baseline, _ = json.Marshal(value)
			if err := os.WriteFile(baselinePath, baseline, 0o600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "missing":
				if err := os.Remove(baselinePath); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.Chmod(baselinePath, 0o644); err != nil {
					t.Fatal(err)
				}
			case "managed", "legacy-managed":
				h.writeDestination(t, `{"managed":"changed-secret","history":[1]}`)
			}
			before := snapshotMaterialization(t, h.root)
			stderr, err := h.run(desired, ModeAssessAdopt)
			if err == nil || strings.Contains(stderr, "secret") {
				t.Fatalf("unsafe adoption: error=%v stderr=%q", err, stderr)
			}
			if !jsonEqual(before, snapshotMaterialization(t, h.root)) {
				t.Fatal("rejected adoption modified files")
			}
		})
	}
}

func TestJSONAdoptionAllowsTemplateEvolutionOnlyWithUnchangedHistoricalFields(t *testing.T) {
	for _, receipt := range []bool{false, true} {
		t.Run(fmt.Sprintf("receipt=%t", receipt), func(t *testing.T) {
			h := newGuestHarness(t)
			previous := []byte(`{"a/b":{"~key":"line\u2028para\u2029雪","number":1e2,"removed":true},"empty":{},"list":[1,true,null]}`)
			h.apply(t, previous)
			if !receipt {
				if err := os.Remove(h.receiptPath(t)); err != nil {
					t.Fatal(err)
				}
			}
			current := `{"a/b":{"~key":"line\u2028para\u2029雪","number":100.0,"removed":true,"runtime":42},"empty":{"foreign":true},"list":[1,true,null],"history":["kept"]}`
			h.writeDestination(t, current)
			desired := []byte(`{"a/b":{"~key":"new","number":200},"fresh":true}`)
			before := snapshotMaterialization(t, h.root)
			first := h.observeMode(t, desired, ModeAssessAdopt)
			if first.Converged || !first.Adoptable {
				t.Fatalf("unchanged old template was not adoptable for update: %#v", first)
			}
			second := h.observeMode(t, bytes.ReplaceAll(desired, []byte("200"), []byte("300")), ModeAssessAdopt)
			if !second.Adoptable || second.Fingerprint == first.Fingerprint {
				t.Fatal("assessment did not bind candidate template values")
			}
			if !jsonEqual(before, snapshotMaterialization(t, h.root)) {
				t.Fatal("template evolution assessment wrote state")
			}
			for _, changed := range []string{
				strings.Replace(current, "100.0", "101", 1),
				strings.Replace(current, `"removed":true,`, "", 1),
				strings.Replace(current, `"empty":{"foreign":true}`, `"empty":false`, 1),
			} {
				h.writeDestination(t, changed)
				if _, err := h.run(desired, ModeAssessAdopt); err == nil {
					t.Fatal("candidate template change hid historical managed-field drift")
				}
			}
			h.writeDestination(t, current)
			h.apply(t, desired)
			if !h.observe(t, desired).Converged {
				t.Fatal("adopted template update did not converge")
			}
			want := map[string]any{
				"a/b":   map[string]any{"~key": "new", "number": float64(200), "runtime": float64(42)},
				"empty": map[string]any{"foreign": true}, "history": []any{"kept"}, "fresh": true,
			}
			if got := h.readDestination(t); !jsonEqual(got, want) {
				t.Fatalf("template update lost foreign fields or retained obsolete managed fields: %#v", got)
			}
		})
	}
}

func TestTOMLAdoptionUsesExactHistoricalTemplateEvidence(t *testing.T) {
	previous := []byte("# Original template\nmanaged = 'old'\n[empty]\n")
	desired := []byte("# Updated template comments\nmanaged = 'old'\n[empty]\n")
	current := "managed = 'old'\n[empty]\nadded = true\n[runtime]\nkeep = 'synthetic-private-value'\n"
	for _, problem := range []string{"none", "template", "managed", "owned", "digest"} {
		t.Run(problem, func(t *testing.T) {
			h := newGuestHarness(t)
			h.format = "toml"
			h.apply(t, previous)
			if err := os.Remove(h.receiptPath(t)); err != nil {
				t.Fatal(err)
			}
			h.legacyTemplates = [][]byte{previous}
			h.writeDestination(t, current)
			switch problem {
			case "template":
				h.legacyTemplates = [][]byte{desired}
			case "managed":
				h.writeDestination(t, strings.Replace(current, "'old'", "'changed-private-value'", 1))
			case "owned", "digest":
				path := h.baselinePath(t)
				payload, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var baseline map[string]any
				if err := json.Unmarshal(payload, &baseline); err != nil {
					t.Fatal(err)
				}
				if problem == "owned" {
					baseline["owned"] = []any{}
				} else {
					baseline["desired_digest"] = strings.Repeat("0", 64)
				}
				payload, _ = json.Marshal(baseline)
				testkit.WriteFile(t, path, payload, 0600)
			}
			before := snapshotMaterialization(t, h.root)
			if problem != "none" {
				stderr, err := h.run(desired, ModeAssessAdopt)
				if err == nil || strings.Contains(stderr, "private-value") {
					t.Fatalf("unsafe historical adoption: error=%v stderr=%q", err, stderr)
				}
			} else {
				observed := h.observeMode(t, desired, ModeAssessAdopt)
				if !observed.Adoptable || observed.Converged {
					t.Fatal("comment-only template update rejected unchanged historical fields")
				}
				evolved := h.observeMode(t, []byte("managed = 'new'\n"), ModeAssessAdopt)
				if !evolved.Adoptable || evolved.Fingerprint == observed.Fingerprint {
					t.Fatal("historical evidence did not bind the new desired template")
				}
			}
			if !jsonEqual(before, snapshotMaterialization(t, h.root)) {
				t.Fatal("historical adoption assessment modified files")
			}
			if problem == "none" {
				h.apply(t, desired)
				payload, err := os.ReadFile(h.destination)
				if err != nil || string(payload) != current || !h.observe(t, desired).Converged {
					t.Fatal("ownership publication changed runtime additions or TOML formatting")
				}
			}
		})
	}
}

func TestRetirementPreservesDocumentAndRuntimeFields(t *testing.T) {
	for _, format := range []string{"json", "toml"} {
		t.Run(format, func(t *testing.T) {
			h := newGuestHarness(t)
			h.format = format
			desired, current := `{"managed":{"leaf":"synthetic-secret"},"empty":{}}`, `{"managed":{"leaf":"synthetic-secret","runtime":true},"empty":{"runtime":true},"history":[1,2]}`
			if format == "toml" {
				desired = "[managed]\nleaf = 'synthetic-secret'\n[empty]\n"
				current = "history = [1,2]\n[managed]\nleaf = 'synthetic-secret'\nruntime = true\n[empty]\nruntime = true\n"
			}
			h.apply(t, []byte(desired))
			h.writeDestination(t, current)
			if err := os.Chmod(h.destination, 0o600); err != nil {
				t.Fatal(err)
			}
			before := snapshotMaterialization(t, h.root)
			if result := h.observeMode(t, nil, ModeAssessRetire); result.Converged {
				t.Fatal("active ownership reported retired")
			}
			if !jsonEqual(before, snapshotMaterialization(t, h.root)) {
				t.Fatal("retirement assessment modified files")
			}
			if result := h.observeMode(t, nil, ModeRetire); !result.Converged {
				t.Fatal("retirement did not converge")
			}
			payload, err := os.ReadFile(h.destination)
			if err != nil || bytes.Contains(payload, []byte("synthetic-secret")) || !bytes.Contains(payload, []byte("runtime")) || !bytes.Contains(payload, []byte("history")) {
				t.Fatalf("retirement lost document/runtime fields or retained owned value: %v", err)
			}
			if info, err := os.Stat(h.destination); err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("retirement changed document permissions: %v", err)
			}
			baseline, _ := os.ReadFile(h.baselinePath(t))
			if bytes.Contains(baseline, []byte("synthetic-secret")) || bytes.Contains(baseline, []byte("history")) {
				t.Fatal("ownership evidence contains values or runtime fields")
			}
			before = snapshotMaterialization(t, h.root)
			if !h.observeMode(t, nil, ModeAssessRetire).Converged || !h.observeMode(t, nil, ModeRetire).Converged {
				t.Fatal("retirement retry did not converge")
			}
			if !jsonEqual(before, snapshotMaterialization(t, h.root)) {
				t.Fatal("retirement retry changed files")
			}
			// Re-enabling establishes fresh ownership after retirement.
			h.apply(t, []byte(desired))
			if !h.observe(t, []byte(desired)).Converged {
				t.Fatal("re-enable did not converge")
			}
		})
	}
}

func TestRetirementRejectsUnprovenOwnershipWithoutMutation(t *testing.T) {
	for _, kind := range []string{"missing-baseline", "legacy", "corrupt", "symlink", "renamed"} {
		t.Run(kind, func(t *testing.T) {
			h := newGuestHarness(t)
			h.apply(t, []byte(`{"managed":["synthetic-secret"]}`))
			baselinePath := h.baselinePath(t)
			switch kind {
			case "missing-baseline":
				if err := os.Remove(baselinePath); err != nil {
					t.Fatal(err)
				}
			case "legacy":
				if err := os.Remove(h.receiptPath(t)); err != nil {
					t.Fatal(err)
				}
				if h.observe(t, []byte(`{"managed":["synthetic-secret"]}`)).Converged {
					t.Fatal("missing ownership receipt did not request repair")
				}
			case "corrupt":
				if err := os.WriteFile(baselinePath, []byte(`{"secret":"invalid"}`), 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Remove(h.destination); err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(h.root, "foreign.json")
				if err := os.WriteFile(target, []byte(`{"managed":["synthetic-secret"]}`), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, h.destination); err != nil {
					t.Fatal(err)
				}
			case "renamed":
				h.destination += ".renamed"
				h.writeDestination(t, `{"managed":["synthetic-secret"]}`)
			}
			before := snapshotMaterialization(t, h.root)
			for _, mode := range []string{ModeAssessRetire, ModeRetire} {
				stderr, err := h.run(nil, mode)
				if err == nil || strings.Contains(stderr, "secret") {
					t.Fatalf("unsafe retirement: mode=%s error=%v stderr=%q", mode, err, stderr)
				}
				if !jsonEqual(before, snapshotMaterialization(t, h.root)) {
					t.Fatal("rejected retirement modified files")
				}
			}
			if kind == "legacy" {
				h.apply(t, []byte(`{"managed":["synthetic-secret"]}`))
				if !h.observeMode(t, nil, ModeRetire).Converged {
					t.Fatal("explicit apply did not upgrade legacy evidence")
				}
			}
		})
	}
}

// Changed applied owned fields no longer prove which values Subyard wrote.
// Retirement releases their ownership instead of refusing, leaving the document
// byte-for-byte unchanged and naming the fields it leaves in place.
func TestRetirementReleasesChangedOwnershipWithoutChangingDocument(t *testing.T) {
	for _, format := range []string{"json", "toml"} {
		for _, interrupted := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/interrupted=%v", format, interrupted), func(t *testing.T) {
				h := newGuestHarness(t)
				h.format = format
				desired := `{"kept":"synthetic-secret","removed":true,"edited":{"mode":"ask"},"empty":{}}`
				drifted := `{"kept":"synthetic-secret","edited":{"mode":"changed-secret"},"empty":{"runtime":1},"history":[1]}`
				if format == "toml" {
					desired = "kept = 'synthetic-secret'\nremoved = true\n[edited]\nmode = 'ask'\n[empty]\n"
					drifted = "kept = 'synthetic-secret'\nhistory = [1]\n[edited]\nmode = 'changed-secret'\n[empty]\nruntime = 1\n"
				}
				h.apply(t, []byte(desired))
				h.writeDestination(t, drifted)
				if err := os.Chmod(h.destination, 0o640); err != nil {
					t.Fatal(err)
				}
				document := snapshotMaterialization(t, filepath.Dir(h.destination))
				released := []string{"/edited/mode", "/kept"}
				before := snapshotMaterialization(t, h.root)
				assessed := h.observeMode(t, nil, ModeAssessRetire)
				if assessed.Converged || assessed.Release != ReleaseChanged || !slices.Equal(assessed.Released, released) {
					t.Fatalf("assessment did not report the release: %#v", assessed)
				}
				if !jsonEqual(before, snapshotMaterialization(t, h.root)) {
					t.Fatal("release assessment modified files")
				}
				if interrupted {
					request, err := h.request(nil, ModeRetire)
					if err != nil {
						t.Fatal(err)
					}
					needle := "        atomic_write(baseline_path, semantic_payload(compatible_baseline(baseline, True)) + b\"\\n\", STATE_UID, 0o600)"
					if !strings.Contains(request.Command[3], needle) {
						t.Fatal("interruption point not found")
					}
					request.Command[3] = strings.Replace(request.Command[3], needle, "        raise MaterializationError(\"synthetic interruption\")\n"+needle, 1)
					command := exec.Command(request.Command[0], request.Command[1:]...)
					command.Stdin = bytes.NewReader(request.Stdin)
					if err := command.Run(); err == nil {
						t.Fatal("interruption did not occur")
					}
					if resumed := h.observeMode(t, nil, ModeAssessRetire); resumed.Converged || resumed.Release != ReleaseChanged || !slices.Equal(resumed.Released, released) {
						t.Fatalf("interrupted release was not resumable: %#v", resumed)
					}
				}
				result := h.observeMode(t, nil, ModeRetire)
				if !result.Converged || !slices.Equal(result.Released, released) || result.Release != ReleaseChanged {
					t.Fatalf("release did not converge or name its fields: %#v", result)
				}
				if !jsonEqual(document, snapshotMaterialization(t, filepath.Dir(h.destination))) {
					t.Fatal("release changed the destination document")
				}
				if info, err := os.Stat(h.destination); err != nil || info.Mode().Perm() != 0o640 {
					t.Fatalf("release changed document permissions: %v", err)
				}
				// The shared schema1 baseline is the released form older runtimes read.
				var baseline, receipt map[string]any
				for path, value := range map[string]*map[string]any{h.baselinePath(t): &baseline, h.receiptPath(t): &receipt} {
					payload, err := os.ReadFile(path)
					if err != nil || json.Unmarshal(payload, value) != nil || bytes.Contains(payload, []byte("secret")) {
						t.Fatalf("ownership evidence unreadable or carries values: %v", err)
					}
				}
				if baseline["schema"] != float64(1) || len(baseline["owned"].([]any)) != 0 || receipt["phase"] != "retired" {
					t.Fatalf("ownership was not released: baseline=%v receipt=%v", baseline, receipt)
				}
				before = snapshotMaterialization(t, h.root)
				for _, mode := range []string{ModeAssessRetire, ModeRetire} {
					if repeated := h.observeMode(t, nil, mode); !repeated.Converged || repeated.Release != ReleaseChanged || !slices.Equal(repeated.Released, released) {
						t.Fatalf("released retirement is not idempotent: %s %#v", mode, repeated)
					}
				}
				if !jsonEqual(before, snapshotMaterialization(t, h.root)) {
					t.Fatal("repeated retirement changed files")
				}
				// Re-enabling establishes fresh ownership over the retained document.
				h.apply(t, []byte(desired))
				if !h.observe(t, []byte(desired)).Converged {
					t.Fatal("re-enable did not converge after a release")
				}
			})
		}
	}
}

// A release is reported even when no owned path remains to name: the change
// replaced every owned path, or the document or its directory is gone.
func TestRetirementReleasesWithoutRetainedPaths(t *testing.T) {
	for _, kind := range []string{"all-paths-replaced", "missing-document", "missing-parent"} {
		t.Run(kind, func(t *testing.T) {
			h := newGuestHarness(t)
			h.apply(t, []byte(`{"managed":{"mode":"ask"}}`))
			want := ReleaseMissingDocument
			switch kind {
			case "all-paths-replaced":
				h.writeDestination(t, `{"managed":"operator-data"}`)
				want = ReleaseChanged
			case "missing-document":
				if err := os.Remove(h.destination); err != nil {
					t.Fatal(err)
				}
			case "missing-parent":
				if err := os.RemoveAll(filepath.Dir(h.destination)); err != nil {
					t.Fatal(err)
				}
			}
			home := snapshotMaterialization(t, filepath.Join(h.root, "home"))
			if assessed := h.observeMode(t, nil, ModeAssessRetire); assessed.Converged || assessed.Release != want || len(assessed.Released) != 0 {
				t.Fatalf("pending release not detectable: %#v", assessed)
			}
			if result := h.observeMode(t, nil, ModeRetire); !result.Converged || result.Release != want || len(result.Released) != 0 {
				t.Fatalf("release did not complete: %#v", result)
			}
			if !jsonEqual(home, snapshotMaterialization(t, filepath.Join(h.root, "home"))) {
				t.Fatal("release changed or created the destination")
			}
			if repeated := h.observeMode(t, nil, ModeAssessRetire); !repeated.Converged || repeated.Release != want {
				t.Fatalf("completed release lost its reason: %#v", repeated)
			}
		})
	}
}

// The guest lists retained fields only when the whole list is short and safe to
// print; otherwise it reports just their count.
func TestReleaseListsOrCountsRetainedFields(t *testing.T) {
	many := map[string]any{}
	for index := range 300 {
		many[fmt.Sprintf("k%03d", index)] = index
	}
	for _, scenario := range []struct {
		name             string
		desired, drifted map[string]any
		listed, count    int
	}{
		{"short", map[string]any{"kept": 1, "edited": "ask"}, map[string]any{"kept": 1, "edited": "operator"}, 2, 2},
		{"many", map[string]any{"fields": many, "edited": "ask"}, map[string]any{"fields": many, "edited": "operator"}, 0, 301},
		{"long-key", map[string]any{strings.Repeat("k", 600): 1, "edited": "ask"}, map[string]any{strings.Repeat("k", 600): 1}, 0, 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			h := newGuestHarness(t)
			desired, _ := json.Marshal(scenario.desired)
			drifted, _ := json.Marshal(scenario.drifted)
			h.apply(t, desired)
			h.writeDestination(t, string(drifted))
			for _, mode := range []string{ModeAssessRetire, ModeRetire} {
				result := h.observeMode(t, nil, mode)
				if result.Release != ReleaseChanged || len(result.Released) != scenario.listed || result.ReleasedCount != scenario.count {
					t.Fatalf("%s: listed=%d count=%d release=%q", mode, len(result.Released), result.ReleasedCount, result.Release)
				}
			}
		})
	}
}

// An interrupted release keeps its reason at every ownership write, so the
// retry still previews the release, signals it and reports it exactly once.
func TestInterruptedReleaseKeepsItsReason(t *testing.T) {
	points := []string{
		"\n            atomic_write(release_path, ",
		"\n            atomic_write(receipt_path, ",
		"\n            atomic_write(baseline_path, ",
		"\n        atomic_write(baseline_path, ",
		"\n        atomic_write(receipt_path, ",
	}
	for _, kind := range []string{"missing-document", "replaced-object"} {
		for index, point := range points {
			t.Run(fmt.Sprintf("%s/write-%d", kind, index+1), func(t *testing.T) {
				h := newGuestHarness(t)
				h.apply(t, []byte(`{"managed":{"mode":"ask"}}`))
				want := ReleaseMissingDocument
				if kind == "replaced-object" {
					h.writeDestination(t, `{"managed":"operator-data"}`)
					want = ReleaseChanged
				} else if err := os.Remove(h.destination); err != nil {
					t.Fatal(err)
				}
				home := snapshotMaterialization(t, filepath.Join(h.root, "home"))
				request, err := h.request(nil, ModeRetire)
				if err != nil {
					t.Fatal(err)
				}
				program := request.Command[3]
				start := strings.Index(program, "def retire(")
				offset := strings.Index(program[start:], point)
				if start < 0 || offset < 0 {
					t.Fatalf("interruption point %q not found", point)
				}
				end := start + offset + 1 + strings.Index(program[start+offset+1:], "\n")
				indent := point[1 : len(point)-len(strings.TrimLeft(point[1:], " "))]
				request.Command[3] = program[:end] + "\n" + indent + "raise MaterializationError(\"synthetic interruption\")" + program[end:]
				command := exec.Command(request.Command[0], request.Command[1:]...)
				command.Stdin = bytes.NewReader(request.Stdin)
				var stderr bytes.Buffer
				command.Stderr = &stderr
				// The guest's own refusal proves the injected program ran to this point.
				if err := command.Run(); err == nil || stderr.String() != "config materialization: synthetic interruption\n" {
					t.Fatalf("interruption did not occur: %v %q", err, stderr.String())
				}
				// Only the final receipt write completes the ownership release.
				final := index == len(points)-1
				if resumed := h.observeMode(t, nil, ModeAssessRetire); resumed.Converged != final || resumed.Release != want || len(resumed.Released) != 0 {
					t.Fatalf("interrupted release lost its reason: %#v", resumed)
				}
				if result := h.observeMode(t, nil, ModeRetire); !result.Converged || result.Release != want {
					t.Fatalf("resumed release was not reported: %#v", result)
				}
				// The completed release stays visible until fresh ownership, so the
				// installed-view guard holds until the inventory forgets the record.
				for _, mode := range []string{ModeAssessRetire, ModeRetire} {
					if repeated := h.observeMode(t, nil, mode); !repeated.Converged || repeated.Release != want {
						t.Fatalf("completed release lost its reason: %s %#v", mode, repeated)
					}
				}
				if !jsonEqual(home, snapshotMaterialization(t, filepath.Join(h.root, "home"))) {
					t.Fatal("interrupted release changed or created the destination")
				}
			})
		}
	}
}

// The release reason is protected evidence: a corrupt record refuses retirement
// and apply without mutation, and fresh ownership supersedes a valid stale one.
func TestReleaseReasonIsProtectedAndSupersededByApply(t *testing.T) {
	h := newGuestHarness(t)
	h.apply(t, []byte(`{"managed":1}`))
	marker := strings.TrimSuffix(h.baselinePath(t), ".json") + ".release"
	testkit.WriteFile(t, marker, []byte(`{"schema":1,"release":"other"}`), 0o600)
	before := snapshotMaterialization(t, h.root)
	for _, mode := range []string{ModeAssessRetire, ModeRetire} {
		if stderr, err := h.run(nil, mode); err == nil || !strings.Contains(stderr, "invalid release record") {
			t.Fatalf("corrupt release reason accepted: %s %v %q", mode, err, stderr)
		}
	}
	if !jsonEqual(before, snapshotMaterialization(t, h.root)) {
		t.Fatal("refused release changed files")
	}
	if stderr, err := h.run([]byte(`{"managed":1}`), ModeApply); err == nil || !strings.Contains(stderr, "invalid release record") {
		t.Fatalf("apply deleted a corrupt release reason: %v %q", err, stderr)
	}
	if !jsonEqual(before, snapshotMaterialization(t, h.root)) {
		t.Fatal("refused apply changed files")
	}
	testkit.WriteFile(t, marker, []byte(`{"schema":1,"release":"changed","retired_digest":"0000000000000000000000000000000000000000000000000000000000000000"}`), 0o600)
	h.apply(t, []byte(`{"managed":1}`))
	if _, err := os.Lstat(marker); !os.IsNotExist(err) {
		t.Fatalf("apply kept a stale release reason: %v", err)
	}
	if result := h.observeMode(t, nil, ModeRetire); !result.Converged || result.Release != "" {
		t.Fatalf("ordinary retirement reported a stale release: %#v", result)
	}
}

// A committed release finishes as a release even when the operator restores
// Subyard's values before the retry: the document is never edited afterwards.
func TestResumedReleaseNeverRemovesRestoredFields(t *testing.T) {
	h := newGuestHarness(t)
	h.apply(t, []byte(`{"managed":{"mode":"ask"}}`))
	h.writeDestination(t, `{"managed":{"mode":"operator"}}`)
	request, err := h.request(nil, ModeRetire)
	if err != nil {
		t.Fatal(err)
	}
	program := request.Command[3]
	start := strings.Index(program, "def retire(")
	point := "\n            atomic_write(baseline_path, "
	offset := strings.Index(program[start:], point)
	if start < 0 || offset < 0 {
		t.Fatal("interruption point not found")
	}
	end := start + offset + 1 + strings.Index(program[start+offset+1:], "\n")
	request.Command[3] = program[:end] + "\n            raise MaterializationError(\"synthetic interruption\")" + program[end:]
	command := exec.Command(request.Command[0], request.Command[1:]...)
	command.Stdin = bytes.NewReader(request.Stdin)
	if err := command.Run(); err == nil {
		t.Fatal("interruption did not occur")
	}
	h.writeDestination(t, `{"managed":{"mode":"ask"}}`)
	document := snapshotMaterialization(t, filepath.Dir(h.destination))
	result := h.observeMode(t, nil, ModeRetire)
	if !result.Converged || result.Release != ReleaseChanged || !slices.Equal(result.Released, []string{"/managed/mode"}) || result.ReleasedCount != 1 {
		t.Fatalf("resumed release misreported: %#v", result)
	}
	if !jsonEqual(document, snapshotMaterialization(t, filepath.Dir(h.destination))) {
		t.Fatal("resumed release removed restored fields")
	}
}

// After the final retired receipt, before the inventory forgets the record, the
// completed release still signals: an installed-view repair is refused, and the
// operator's document stays unchanged.
func TestCompletedReleaseKeepsItsSignalUntilFreshOwnership(t *testing.T) {
	h := newGuestHarness(t)
	desired := []byte(`{"managed":{"mode":"subyard"}}`)
	h.apply(t, desired)
	h.writeDestination(t, `{"managed":{"mode":"operator"}}`)
	if result := h.observeMode(t, nil, ModeRetire); !result.Converged || result.Release != ReleaseChanged {
		t.Fatalf("release did not complete: %#v", result)
	}
	document := snapshotMaterialization(t, filepath.Dir(h.destination))
	if assessed := h.observeMode(t, nil, ModeAssessRetire); !assessed.Converged || assessed.Release != ReleaseChanged ||
		!slices.Equal(assessed.Released, []string{"/managed/mode"}) {
		t.Fatalf("completed release lost the guard signal: %#v", assessed)
	}
	// The installed view still selecting this document sees the release too.
	if observed := h.observe(t, desired); observed.Converged || observed.Release != ReleaseChanged {
		t.Fatalf("installed-view observation lost the release: %#v", observed)
	}
	if !jsonEqual(document, snapshotMaterialization(t, filepath.Dir(h.destination))) {
		t.Fatal("assessment changed the released document")
	}
	// Only fresh ownership, a deliberate enable, clears the signal.
	h.apply(t, desired)
	if observed := h.observe(t, desired); !observed.Converged || observed.Release != "" {
		t.Fatalf("fresh ownership kept the release: %#v", observed)
	}
	if result := h.observeMode(t, nil, ModeRetire); !result.Converged || result.Release != "" {
		t.Fatalf("fresh ownership kept a stale release: %#v", result)
	}
}

// After a release, the installed view never writes the document, whatever it
// now holds: observation reports the release before reading it, and an
// installed-view apply refuses.
func TestInstalledViewNeverWritesReleasedDocument(t *testing.T) {
	for _, kind := range []string{"invalid", "missing", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			h := newGuestHarness(t)
			desired := []byte(`{"managed":{"mode":"subyard"}}`)
			h.apply(t, desired)
			h.writeDestination(t, `{"managed":{"mode":"operator"}}`)
			if result := h.observeMode(t, nil, ModeRetire); !result.Converged || result.Release != ReleaseChanged {
				t.Fatalf("release did not complete: %#v", result)
			}
			switch kind {
			case "invalid":
				h.writeDestination(t, `{"managed":`)
			case "missing":
				if err := os.Remove(h.destination); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := filepath.Join(h.root, "foreign.json")
				testkit.WriteFile(t, target, []byte(`{"managed":"foreign"}`), 0o600)
				if err := os.Remove(h.destination); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, h.destination); err != nil {
					t.Fatal(err)
				}
			}
			before := snapshotMaterialization(t, h.root)
			if observed := h.observe(t, desired); observed.Converged || observed.Release != ReleaseChanged {
				t.Fatalf("installed-view observation missed the release: %#v", observed)
			}
			if stderr, err := h.run(desired, ModeApplyInstalled); err == nil || !strings.Contains(stderr, "released ownership refused") {
				t.Fatalf("installed-view apply wrote a released document: %v %q", err, stderr)
			}
			if !jsonEqual(before, snapshotMaterialization(t, h.root)) {
				t.Fatal("installed view changed the released document or its evidence")
			}
		})
	}
}

// A release record bound to another projection, such as one an older runtime
// left behind, does not turn an interrupted ordinary retirement into a release.
func TestStaleReleaseRecordDoesNotChangeOrdinaryRetirement(t *testing.T) {
	h := newGuestHarness(t)
	h.apply(t, []byte(`{"managed":1}`))
	h.writeDestination(t, `{"managed":1,"history":["kept"]}`)
	request, err := h.request(nil, ModeRetire)
	if err != nil {
		t.Fatal(err)
	}
	needle := "    atomic_write(destination, payload, uid, file_mode)"
	request.Command[3] = strings.Replace(request.Command[3], needle, "    raise MaterializationError(\"synthetic interruption\")\n"+needle, 1)
	command := exec.Command(request.Command[0], request.Command[1:]...)
	command.Stdin = bytes.NewReader(request.Stdin)
	if err := command.Run(); err == nil {
		t.Fatal("interruption did not occur")
	}
	marker := strings.TrimSuffix(h.baselinePath(t), ".json") + ".release"
	testkit.WriteFile(t, marker, []byte(`{"schema":1,"release":"changed","retired_digest":"`+strings.Repeat("0", 64)+`"}`), 0o600)
	if result := h.observeMode(t, nil, ModeRetire); !result.Converged || result.Release != "" {
		t.Fatalf("stale release record changed an ordinary retirement: %#v", result)
	}
	if got := h.readDestination(t); !jsonEqual(got, map[string]any{"history": []any{"kept"}}) {
		t.Fatalf("ordinary retirement did not finish: %#v", got)
	}
}

// An interrupted ordinary retirement keeps its strict retry proof; a release
// applies only to an applied document whose owned fields changed.
func TestInterruptedRetirementStillRefusesChangedOwnedFields(t *testing.T) {
	h := newGuestHarness(t)
	h.apply(t, []byte(`{"managed":1}`))
	request, err := h.request(nil, ModeRetire)
	if err != nil {
		t.Fatal(err)
	}
	needle := "    atomic_write(destination, payload, uid, file_mode)"
	request.Command[3] = strings.Replace(request.Command[3], needle, "    raise MaterializationError(\"synthetic interruption\")\n"+needle, 1)
	command := exec.Command(request.Command[0], request.Command[1:]...)
	command.Stdin = bytes.NewReader(request.Stdin)
	if err := command.Run(); err == nil {
		t.Fatal("interruption did not occur")
	}
	h.writeDestination(t, `{"managed":"changed-secret"}`)
	before := snapshotMaterialization(t, h.root)
	for _, mode := range []string{ModeAssessRetire, ModeRetire} {
		if stderr, err := h.run(nil, mode); err == nil || !strings.Contains(stderr, "retirement owned fields changed") || strings.Contains(stderr, "secret") {
			t.Fatalf("interrupted retirement accepted drift: %s %v %q", mode, err, stderr)
		}
	}
	if !jsonEqual(before, snapshotMaterialization(t, h.root)) {
		t.Fatal("refused retirement changed files")
	}
}

func TestRetirementResumesInterruptedReplacement(t *testing.T) {
	for _, stage := range []string{"before", "after", "released-baseline", "legacy-schema2"} {
		t.Run(stage, func(t *testing.T) {
			h := newGuestHarness(t)
			h.apply(t, []byte(`{"managed":1}`))
			h.writeDestination(t, `{"managed":1,"history":["kept"]}`)
			if stage == "legacy-schema2" {
				receipt, err := os.ReadFile(h.receiptPath(t))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(h.baselinePath(t), receipt, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(h.receiptPath(t)); err != nil {
					t.Fatal(err)
				}
			}
			request, err := h.request(nil, ModeRetire)
			if err != nil {
				t.Fatal(err)
			}
			needle := "    atomic_write(destination, payload, uid, file_mode)"
			injected := "    raise MaterializationError(\"synthetic interruption\")\n" + needle
			if stage == "after" {
				injected = needle + "\n    raise MaterializationError(\"synthetic interruption\")"
			}
			if stage == "released-baseline" {
				needle = "        baseline[\"phase\"] = \"retired\""
				injected = "        raise MaterializationError(\"synthetic interruption\")\n" + needle
			}
			if stage == "legacy-schema2" {
				needle = "            # Convert an earlier candidate's schema2 before any document edit."
				injected = "            raise MaterializationError(\"synthetic interruption\")\n" + needle
			}
			request.Command[3] = strings.Replace(request.Command[3], needle, injected, 1)
			command := exec.Command(request.Command[0], request.Command[1:]...)
			command.Stdin = bytes.NewReader(request.Stdin)
			if err := command.Run(); err == nil {
				t.Fatal("interruption did not occur")
			}
			if h.observeMode(t, nil, ModeAssessRetire).Converged {
				t.Fatal("unfinished retirement reported complete")
			}
			interrupted, err := os.ReadFile(h.destination)
			if err != nil {
				t.Fatal(err)
			}
			h.writeDestination(t, `{"managed":"changed-secret","history":["kept"]}`)
			before := snapshotMaterialization(t, h.root)
			if stderr, err := h.run(nil, ModeRetire); err == nil || strings.Contains(stderr, "secret") {
				t.Fatalf("retry accepted drift: %v %q", err, stderr)
			}
			if !jsonEqual(before, snapshotMaterialization(t, h.root)) {
				t.Fatal("retry overwrote drift")
			}
			h.writeDestination(t, string(interrupted))
			if !h.observeMode(t, nil, ModeRetire).Converged {
				t.Fatal("retirement retry failed")
			}
			if got := h.readDestination(t); !jsonEqual(got, map[string]any{"history": []any{"kept"}}) {
				t.Fatalf("retry changed runtime fields: %#v", got)
			}
		})
	}
}

func TestMaterializationResumesInterruptedReceiptPublication(t *testing.T) {
	for _, stage := range []string{"baseline", "receipt"} {
		t.Run(stage, func(t *testing.T) {
			h := newGuestHarness(t)
			h.apply(t, []byte(`{"managed":1}`))
			h.writeDestination(t, `{"managed":1,"history":["kept"]}`)
			desired := []byte(`{"managed":2}`)
			request, err := h.request(desired, ModeApply)
			if err != nil {
				t.Fatal(err)
			}
			needle := "        atomic_write(baseline_path, semantic_payload(compatible_baseline(evidence))"
			if stage == "receipt" {
				needle = "        atomic_write(receipt_path, semantic_payload(evidence)"
			}
			request.Command[3] = strings.Replace(request.Command[3], needle, "        raise MaterializationError(\"synthetic interruption\")\n"+needle, 1)
			command := exec.Command(request.Command[0], request.Command[1:]...)
			command.Stdin = bytes.NewReader(request.Stdin)
			if err := command.Run(); err == nil {
				t.Fatal("interruption did not occur")
			}
			if h.observe(t, desired).Converged {
				t.Fatal("incomplete ownership publication reported ready")
			}
			// Incomplete proof never authorizes removal. Unmatched evidence refuses;
			// intact old evidence only releases ownership of the changed document.
			document := snapshotMaterialization(t, filepath.Dir(h.destination))
			if stage == "receipt" {
				if _, err := h.run(nil, ModeRetire); err == nil {
					t.Fatal("incomplete ownership publication authorized retirement")
				}
			} else if result := h.observeMode(t, nil, ModeRetire); !result.Converged || !slices.Equal(result.Released, []string{"/managed"}) {
				t.Fatalf("incomplete ownership publication was not released: %#v", result)
			}
			if !jsonEqual(document, snapshotMaterialization(t, filepath.Dir(h.destination))) {
				t.Fatal("incomplete ownership publication authorized removal")
			}
			h.apply(t, desired)
			if !h.observe(t, desired).Converged {
				t.Fatal("apply did not repair ownership proof")
			}
			h.observeMode(t, nil, ModeRetire)
			if got := h.readDestination(t); !jsonEqual(got, map[string]any{"history": []any{"kept"}}) {
				t.Fatalf("retry lost unrelated data: %#v", got)
			}
		})
	}
}

func TestRetainedWriterInvalidatesRetirementReceipt(t *testing.T) {
	h := newGuestHarness(t)
	h.apply(t, []byte(`{"managed":1}`))
	baselinePath := h.baselinePath(t)
	baseline, err := os.ReadFile(baselinePath)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(baseline, &value); err != nil {
		t.Fatal(err)
	}
	// v0.14 accepts exactly these five fields, and writes the same schema when
	// it changes desired values. It neither reads nor updates the companion.
	if len(value) != 5 || value["schema"] != float64(1) {
		t.Fatal("shared baseline is incompatible with the retained reader")
	}
	for _, key := range []string{"schema", "developer", "destination", "desired_digest", "owned"} {
		if _, exists := value[key]; !exists {
			t.Fatalf("missing retained baseline field %s", key)
		}
	}
	desired := []byte(`{"managed":2}`)
	value["desired_digest"], err = DesiredDigest(desired)
	if err != nil {
		t.Fatal(err)
	}
	baseline, _ = json.Marshal(value)
	if err := os.WriteFile(baselinePath, baseline, 0o600); err != nil {
		t.Fatal(err)
	}
	h.writeDestination(t, `{"managed":2,"history":["kept"]}`)
	before := snapshotMaterialization(t, h.root)
	for _, mode := range []string{ModeAssessRetire, ModeRetire} {
		if stderr, err := h.run(nil, mode); err == nil {
			t.Fatalf("stale receipt authorized %s: %s", mode, stderr)
		}
	}
	if h.observe(t, desired).Converged || !h.observeMode(t, desired, ModeAssessAdopt).Converged {
		t.Fatal("stale proof must request repair while preserving exact legacy adoption")
	}
	if !jsonEqual(before, snapshotMaterialization(t, h.root)) {
		t.Fatal("read-only stale evidence handling changed state")
	}
	h.apply(t, desired)
	h.observeMode(t, nil, ModeRetire)
	if got := h.readDestination(t); !jsonEqual(got, map[string]any{"history": []any{"kept"}}) {
		t.Fatalf("proof repair/retirement lost runtime additions: %#v", got)
	}
	baseline, _ = os.ReadFile(baselinePath)
	if err := json.Unmarshal(baseline, &value); err != nil || len(value["owned"].([]any)) != 0 {
		t.Fatal("retirement did not release ownership for retained writers")
	}
}

func TestOwnershipReceiptRejectsUnprotectedOrInvalidState(t *testing.T) {
	for _, kind := range []string{"mode", "symlink", "hardlink", "corrupt"} {
		t.Run(kind, func(t *testing.T) {
			h := newGuestHarness(t)
			h.apply(t, []byte(`{"managed":1}`))
			path := h.receiptPath(t)
			switch kind {
			case "mode":
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(path, path+".foreign"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".foreign", path); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(path, path+".foreign"); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				if err := os.WriteFile(path, []byte(`{"schema":1}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := snapshotMaterialization(t, h.root)
			for _, mode := range []string{ModeAssessRetire, ModeRetire, ModeApply} {
				if _, err := h.run([]byte(`{"managed":1}`), mode); err == nil {
					t.Fatalf("untrusted receipt accepted by %s", mode)
				}
				if !jsonEqual(before, snapshotMaterialization(t, h.root)) {
					t.Fatal("untrusted receipt handling changed state")
				}
			}
		})
	}
}

func snapshotMaterialization(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	for _, relative := range treeNames(t, root) {
		path := filepath.Join(root, relative)
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			result[relative] = string(data)
		} else {
			result[relative] = info.Mode().String()
		}
	}
	return result
}
