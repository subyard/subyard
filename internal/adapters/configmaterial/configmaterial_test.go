package configmaterial

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/ports"
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
	harness.writeDestination(t, `{"managed":{"old":1,"keep":"stale"},"hooks":["runtime"],"statusLine":"orca"}`)

	harness.apply(t, []byte(`{"managed":{"old":1,"keep":"wanted"}}`))
	harness.apply(t, []byte(`{"managed":{"keep":"next","fresh":true}}`))

	got := harness.readDestination(t)
	want := map[string]any{
		"managed": map[string]any{"keep": "next", "fresh": true},
		"hooks":   []any{"runtime"}, "statusLine": "orca",
	}
	if !jsonEqual(got, want) {
		t.Fatalf("materialized JSON = %#v, want %#v", got, want)
	}
	observation := harness.observe(t, []byte(`{"managed":{"fresh":true,"keep":"next"}}`))
	if !observation.Converged || len(observation.Fingerprint) != 64 {
		t.Fatalf("observation = %#v", observation)
	}
	before := observation.Fingerprint
	got["orcaRuntime"] = map[string]any{"hook": true}
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
		bytes.Contains(baseline, []byte("orcaRuntime")) {
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
		if err == nil || !strings.Contains(stderr, "invalid current JSON") || strings.Contains(stderr, "secret") {
			t.Fatalf("apply error = %v stderr=%q", err, stderr)
		}
		after, readErr := os.ReadFile(harness.destination)
		if readErr != nil || !bytes.Equal(before, after) {
			t.Fatalf("malformed destination changed: err=%v before=%q after=%q", readErr, before, after)
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
	if err != nil || request.Command[len(request.Command)-3] != "1000" {
		t.Fatalf("legacy uid fallback request=%#v err=%v", request, err)
	}
	for _, output := range [][]byte{
		[]byte(`{"converged":true,"fingerprint":"short"}`),
		[]byte(`{"fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`),
		[]byte(`{"converged":true,"fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","extra":1}`),
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

type guestHarness struct {
	t           *testing.T
	root        string
	state       string
	destination string
	format      string
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
	request, err := h.request(payload, ModeObserve)
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
		h.state, os.Getuid(), filepath.Join(h.root, "home", "dev"))
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
