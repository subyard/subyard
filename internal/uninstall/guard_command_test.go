package uninstall

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func runGuard(t *testing.T, arguments ...string) (string, int) {
	t.Helper()
	var stdout bytes.Buffer
	code := RunGuard(arguments, nil, &stdout, io.Discard)
	return stdout.String(), code
}

func resourceBinding(t *testing.T, value any) string {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func TestFullSharedRemoverReportsOnlyVerifiedGoneBridges(t *testing.T) {
	network := map[string]any{"name": "owned", "type": "bridge", "managed": true, "config": map[string]any{}, "description": ""}
	profile := map[string]any{"name": "default", "config": map[string]any{}, "devices": map[string]any{}, "description": ""}
	inventory, err := json.Marshal([]ports.TeardownResource{
		{Kind: "host-profile", Name: "default", Binding: resourceBinding(t, profile)},
		{Kind: "network", Name: "owned", Binding: resourceBinding(t, network)},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUBYARD_TEARDOWN_INVENTORY", string(inventory))
	t.Cleanup(func() { incusQuery, incusCall = queryIncus, callIncus })
	for _, kind := range []string{"removed", "absent-retry", "foreign", "selected-foreign", "changed", "delete-error", "still-present", "null-collection", "null-consumers"} {
		t.Run(kind, func(t *testing.T) {
			present, calls := kind != "absent-retry", 0
			incusQuery = func(path, _ string, target any) error {
				var value any
				switch path {
				case "/1.0/networks":
					value = []string{}
					if kind == "null-collection" {
						value = nil
					} else if present {
						value = []string{"/1.0/networks/owned?project=default"}
					}
				case "/1.0/profiles/default":
					value = map[string]any{"name": "default", "config": map[string]any{}, "devices": map[string]any{}, "description": "", "used_by": []string{}}
				case "/1.0/networks/owned":
					observed := map[string]any{"used_by": []string{}}
					for key, item := range network {
						observed[key] = item
					}
					if kind == "foreign" || kind == "selected-foreign" {
						observed["used_by"] = []string{"/1.0/instances/foreign"}
					}
					if kind == "changed" {
						observed["description"] = "changed"
					}
					if kind == "null-consumers" {
						observed["used_by"] = nil
					}
					value = observed
				default:
					return fmt.Errorf("unexpected query %s", path)
				}
				body, _ := json.Marshal(value)
				return json.Unmarshal(body, target)
			}
			incusCall = func(arguments ...string) error {
				calls++
				if kind == "delete-error" {
					return errors.New("fixture delete failed")
				}
				if kind != "still-present" {
					present = false
				}
				return nil
			}
			arguments := []string{"remove-shared"}
			if kind == "selected-foreign" {
				arguments = append(arguments, "--selected")
			}
			output, code := runGuard(t, arguments...)
			switch kind {
			case "selected-foreign":
				if code != 0 || output != "" || calls != 0 {
					t.Fatalf("selected foreign consumer: %d %q %d", code, output, calls)
				}
			case "removed", "absent-retry":
				if code != 0 || output != "owned\n" || (calls == 1) != (kind == "removed") {
					t.Fatalf("verified removal: %d %q %d", code, output, calls)
				}
			default:
				if code != ports.TeardownPlanStaleExitCode || output != "" || kind != "delete-error" && kind != "still-present" && calls != 0 {
					t.Fatalf("refusal: %d %q %d", code, output, calls)
				}
			}
		})
	}
}

func TestTeardownArtifactHelperGuardsActualDeletion(t *testing.T) {
	t.Setenv("SUBYARD_TEARDOWN_PLAN", "")
	for _, scenario := range []string{"unchanged", "tree", "replaced", "symlink", "absent", "disappeared", "added", "added-child", "during-delete", "owner-during-delete", "symlink-parent"} {
		t.Run(scenario, func(t *testing.T) {
			root := testkit.TempDir(t)
			path := filepath.Join(root, "owned")
			foreign := filepath.Join(root, "foreign")
			testkit.WriteFile(t, foreign, []byte("foreign"), 0600)
			switch scenario {
			case "tree", "added-child":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, filepath.Join(path, "child"), []byte("before"), 0600)
				if err := os.Symlink(foreign, filepath.Join(path, "link")); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink("before", path); err != nil {
					t.Fatal(err)
				}
			case "absent", "added":
			default:
				testkit.WriteFile(t, path, []byte("before"), 0600)
			}
			artifact, err := CaptureArtifact(path)
			if err != nil {
				t.Fatal(err)
			}
			payload, err := json.Marshal([]ports.TeardownArtifactBinding{{Path: path, Binding: artifact.Binding}})
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("SUBYARD_TEARDOWN_ARTIFACTS", string(payload))
			replace := func() {
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				replacement := filepath.Join(root, "replacement")
				testkit.WriteFile(t, replacement, []byte("after!"), 0600)
				if err := os.Chtimes(replacement, info.ModTime(), info.ModTime()); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(replacement, path); err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "replaced":
				replace()
			case "symlink":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("after!", path); err != nil {
					t.Fatal(err)
				}
			case "disappeared":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "added":
				testkit.WriteFile(t, path, []byte("after!"), 0600)
			case "added-child":
				testkit.WriteFile(t, filepath.Join(path, "new"), []byte("after!"), 0600)
			case "symlink-parent":
				moved := root + "-moved"
				if err := os.Rename(root, moved); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Remove(root); _ = os.Rename(moved, root) })
				if err := os.Symlink(moved, root); err != nil {
					t.Fatal(err)
				}
			case "during-delete":
				// Drift after the artifact check, before its unlink.
				beforeArtifactRemoval = replace
			case "owner-during-delete":
				// Synthetic ownership drift needs no root.
				beforeArtifactRemoval = func() {
					statAt = func(fd int, name, relative string) (Entry, error) {
						fact, err := factAt(fd, name, relative)
						fact.UID, fact.GID = fact.UID+1, fact.GID+1
						return fact, err
					}
				}
			}
			t.Cleanup(func() { beforeArtifactRemoval, statAt = nil, factAt })
			_, code := runGuard(t, "remove-artifact", path)
			switch scenario {
			case "unchanged", "tree", "absent", "disappeared":
				if code != 0 {
					t.Fatalf("approved deletion failed: exit %d", code)
				}
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("approved artifact remains: %v", err)
				}
			default:
				if code != ports.TeardownPlanStaleExitCode {
					t.Fatalf("drift did not produce typed stale refusal: exit %d", code)
				}
				if _, err := os.Lstat(path); err != nil {
					t.Fatalf("changed artifact was deleted: %v", err)
				}
			}
			if _, err := os.Stat(foreign); err != nil {
				t.Fatalf("foreign artifact was deleted: %v", err)
			}
		})
	}
}

func TestTeardownFullOriginalFactsUseBoundedWireAfterPartialRemoval(t *testing.T) {
	root := testkit.TempDir(t)
	owned := filepath.Join(root, "owned")
	if err := os.Mkdir(owned, 0700); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 500; index++ {
		testkit.WriteFile(t, filepath.Join(owned, fmt.Sprintf("entry-%03d", index)), []byte("approved"), 0600)
	}
	original, err := CaptureArtifact(owned)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(original)
	if len(body) <= 64<<10 {
		t.Fatal("fixture does not exceed environment transport limit")
	}
	wire, _ := json.Marshal(ports.ArtifactBindings([]ports.TeardownArtifact{original}))
	if len(wire) >= 64<<10 {
		t.Fatal("bounded binding transport expanded")
	}
	plan, _ := json.Marshal(map[string]any{"Installation": map[string]any{"operatorUid": os.Getuid()}, "Services": []any{}, "Yards": []any{map[string]any{"Snapshot": map[string]any{"Artifacts": []ports.TeardownArtifact{original}}}}})
	path := filepath.Join(root, "plan.json")
	testkit.WriteFile(t, path, plan, 0600)
	digest := sha256.Sum256(plan)
	t.Setenv("SUBYARD_TEARDOWN_ARTIFACTS", string(wire))
	t.Setenv("SUBYARD_TEARDOWN_PLAN", path)
	t.Setenv("SUBYARD_TEARDOWN_PLAN_SHA256", hex.EncodeToString(digest[:]))
	if err := os.Remove(filepath.Join(owned, "entry-000")); err != nil {
		t.Fatal(err)
	}
	if _, code := runGuard(t, "guard-artifacts"); code != 0 {
		t.Fatal("partial retry rejected original surviving facts", code)
	}
	testkit.WriteFile(t, filepath.Join(owned, "foreign"), []byte("foreign"), 0600)
	if _, code := runGuard(t, "guard-artifacts"); code != ports.TeardownPlanStaleExitCode {
		t.Fatal("fresh artifact expanded original full plan", code)
	}
	if _, err := os.Stat(filepath.Join(owned, "foreign")); err != nil {
		t.Fatal("assessment deleted foreign artifact", err)
	}
}

func TestGuardAllReportsOnlyStructuralDetails(t *testing.T) {
	t.Setenv("SUBYARD_TEARDOWN_INVENTORY", `[{"kind":"instance","name":"yard","binding":"`+strings.Repeat("a", 64)+`"}]`)
	t.Cleanup(func() { incusQuery = queryIncus })
	incusQuery = func(path, _ string, target any) error {
		return json.Unmarshal([]byte(`{"name":"yard","type":"container","config":{"secret":"value"},"devices":{},"profiles":[]}`), target)
	}
	var stderr bytes.Buffer
	if code := RunGuard([]string{"guard-all", "subyard"}, nil, io.Discard, &stderr); code != 1 {
		t.Fatalf("exit %d", code)
	}
	expected := `plan_stale: {"action":"guard-all","stage":"resource","kind":"instance","ordinal":1,"category":"KeyError","field":"created_at"}` + "\n" +
		"plan_stale: teardown inventory, artifact or resource binding is invalid\n"
	if stderr.String() != expected {
		t.Fatalf("diagnostic %q", stderr.String())
	}
}

func TestFullFileRemoverRequiresAnActualPoolCollection(t *testing.T) {
	for _, pools := range []string{"null", "{}", `["/1.0/storage-pools/backing"]`, "[]"} {
		t.Run(pools, func(t *testing.T) {
			store, plan := recoveryFixture(t)
			backing := filepath.Join(store.Directory, "backing")
			if err := os.Mkdir(backing, 0700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(backing, "data"), []byte("pool data"), 0600)
			tree, err := Capture(backing)
			if err != nil {
				t.Fatal(err)
			}
			tree.Resource = &ports.TeardownResource{Kind: "pool", Name: "backing"}
			plan.Trees, plan.Stage = []Tree{tree}, RecoveryYardsRemoved
			lock, release, err := OwnerLock(context.Background(), plan.Installation.DataHome, nil)
			if err != nil {
				t.Fatal(err)
			}
			plan.OwnerLock = &lock
			release()
			if _, err = store.Save(plan, nil); err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(filepath.Join(store.Directory, "plan.json"))
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(body)
			t.Setenv("SUBYARD_TEARDOWN_PLAN", filepath.Join(store.Directory, "plan.json"))
			t.Setenv("SUBYARD_TEARDOWN_PLAN_SHA256", hex.EncodeToString(sum[:]))
			t.Setenv("SUBYARD_TEARDOWN_PARENT_ID", "0:0")
			t.Cleanup(func() { incusQuery = queryIncus })
			incusQuery = func(path, _ string, target any) error {
				if path != "/1.0/storage-pools" {
					return fmt.Errorf("unexpected query %s", path)
				}
				return decodeFixture(pools, target)
			}
			code := RunGuard([]string{"remove-files"}, nil, io.Discard, io.Discard)
			_, statErr := os.Stat(filepath.Join(backing, "data"))
			if pools == "[]" {
				if code != 0 || !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("verified absent pool kept its backing data: exit %d, %v", code, statErr)
				}
				return
			}
			if code != ports.TeardownPlanStaleExitCode || statErr != nil {
				t.Fatalf("unproven pool absence removed backing data: exit %d, %v", code, statErr)
			}
		})
	}
}

// decodeFixture decodes like queryIncus, so shape errors keep their category.
func decodeFixture(body string, target any) error {
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			return typeMismatch("unexpected Incus response shape")
		}
		return err
	}
	return nil
}

func TestArtifactRemovalStopsAtCancellationBeforeNextUnlink(t *testing.T) {
	t.Setenv("SUBYARD_TEARDOWN_PLAN", "")
	path := filepath.Join(testkit.TempDir(t), "overrides")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b", "c"} {
		testkit.WriteFile(t, filepath.Join(path, name), []byte(name), 0600)
	}
	artifact, err := CaptureArtifact(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	t.Cleanup(func() { beforeArtifactRemoval, statAt = nil, factAt })
	beforeArtifactRemoval = func() {
		stats := 0
		statAt = func(fd int, name, relative string) (Entry, error) {
			// Cancel while the directory removal is already under way.
			if stats++; stats == 3 {
				cancel()
			}
			return factAt(fd, name, relative)
		}
	}
	err = RemoveArtifact(ctx, path, ports.ArtifactBindings([]ports.TeardownArtifact{artifact}))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled removal: %v", err)
	}
	names, _ := os.ReadDir(path)
	if len(names) != 2 {
		t.Fatalf("removal continued after cancellation: %d entries remain", len(names))
	}
	// An already-cancelled removal reports cancellation even with nothing to unlink.
	absent := ports.ArtifactBindings([]ports.TeardownArtifact{{Path: path + "-absent"}})
	if err := RemoveArtifact(ctx, path+"-absent", absent); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled removal of an absent artifact: %v", err)
	}
}

func TestOperatorArtifactRemovalUsesOnlyThatAccountsCredentials(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the credential drop itself is covered by the root teardown lanes")
	}
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUBYARD_TEARDOWN_PLAN", "")
	for _, account := range []string{"root", current.Username} {
		t.Run(account, func(t *testing.T) {
			path := filepath.Join(testkit.TempDir(t), "subyard-demo.config")
			testkit.WriteFile(t, path, []byte("Host yard-demo\n"), 0600)
			artifact, err := CaptureArtifact(path)
			if err != nil {
				t.Fatal(err)
			}
			wire, _ := json.Marshal(ports.ArtifactBindings([]ports.TeardownArtifact{artifact}))
			t.Setenv("SUBYARD_TEARDOWN_ARTIFACTS", string(wire))
			_, code := runGuard(t, "remove-artifact-as", account, path)
			_, statErr := os.Lstat(path)
			if account == "root" {
				// Without root no other account's credentials can be assumed.
				if code != ports.TeardownPlanStaleExitCode || statErr != nil {
					t.Fatalf("foreign credentials assumed: exit %d, %v", code, statErr)
				}
				return
			}
			if code != 0 || !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("operator removal: exit %d, %v", code, statErr)
			}
		})
	}
}
