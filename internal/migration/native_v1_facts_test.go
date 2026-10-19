package migration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/releaseowner"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestMain(m *testing.M) {
	// The existing physical fixture's fixed readonly facade must be an ELF
	// image when entered through a retained /proc/self/fd repository. A shell
	// interpreter would reopen that path after CLOEXEC and lose the descriptor.
	if len(os.Args) == 5 && os.Args[1] == "-Y" && os.Args[2] == "test-yard" && os.Args[3] == "test-vms" && os.Args[4] == "status" {
		fmt.Println("ttl_remaining_seconds\t0")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestOriginalV1RustNativeCheckpointRoundtrip(t *testing.T) {
	binary := os.Getenv("SUBYARD_RUST_V1_IMPORT_TEST_BINARY")
	helper := os.Getenv("SUBYARD_RUST_NATIVE_HELPER")
	if binary == "" || helper == "" {
		t.Skip("explicit Rust libtest and current native helper required")
	}
	options, journalPath, _ := publishedV1CompatibilityFixture(t, "0.4.2")
	readCalls := filepath.Join(testkit.TempDir(t), "readonly-calls")
	testkit.WriteFile(t, readCalls, nil, 0600)
	originalIncus := options.Incus + "-original"
	if err := os.Rename(options.Incus, originalIncus); err != nil {
		t.Fatal(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	testkit.WriteFile(t, options.Incus, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+quote(readCalls)+"\nexec "+quote(originalIncus)+" \"$@\"\n"), 0700)
	// The real CLI native factory requires current declarations. They are only
	// supporting fixture inputs; the historical registry and runtime pair stay
	// untouched and are selected from their original retained roots.
	err := filepath.WalkDir(filepath.Join("..", "..", "config"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(filepath.Join("..", "..", "config"), path)
		if err != nil {
			return err
		}
		to := filepath.Join(options.RepositoryRoot, "config", rel)
		if _, err := os.Lstat(to); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(to), 0700); err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		testkit.WriteFile(t, to, raw, info.Mode().Perm())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(options.RepositoryRoot, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(options.RepositoryRoot, "bin", "yard-engine"), image, 0700)
	reader, err := NewV1ImportReader(V1ImportOptions{ConfigHome: options.ConfigHome, RuntimeRoot: options.RuntimeRoot, Facts: mustOriginalV1Observer(t, options)})
	if err != nil {
		t.Fatal(err)
	}
	result, err := reader.InspectBound(context.Background(), V1ImportRuntimePair{testPublishedV1042Runtime, testPublishedV1PreviousRuntime})
	if err != nil {
		t.Fatal(err)
	}
	// Match the existing CLI's fresh migration environment before starting
	// the real helper, so an invalid fixture cannot hide behind its bounded
	// public unavailable diagnostic.
	catalog, err := config.LoadCatalog(options.RepositoryRoot)
	if err != nil {
		t.Fatal(err)
	}
	fresh := options
	fresh.Environment = nil
	for _, setting := range options.Environment {
		name, _, _ := strings.Cut(setting, "=")
		if _, known := catalog.LookupSetting(name); known || config.IsLegacySetting(name) {
			continue
		}
		switch name {
		case "E2E_VM_TTL_MINUTES", "SUBYARD_CONFIG_DIR", "SUBYARD_CONFIG_LOADED", "SUBYARD_DISPATCH_ARG0", "SUBYARD_DISPATCH_COMMAND", "SUBYARD_DISPATCH_PATH", "SUBYARD_ENGINE_CONTEXT", "SUBYARD_ENGINE_CONTEXT_SCHEMA", "SUBYARD_ENGINE_CONTEXT_SOURCED", "SUBYARD_SUDO_PREAUTHORIZED", "SUBYARD_YARD":
			continue
		}
		fresh.Environment = append(fresh.Environment, setting)
	}
	fresh.Environment = append(fresh.Environment, "SUBYARD_REPOSITORY_ROOT="+options.RepositoryRoot)
	journalRaw, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	registryRaw, err := os.ReadFile(filepath.Join(options.RuntimeRoot, result.RuntimePair.Current, "config", "migrations.json"))
	if err != nil {
		t.Fatal(err)
	}
	expectation := V1ImportFactExpectation{result.Release, result.Checkpoint, result.Transaction, result.RuntimePair}
	if _, err := ObserveOriginalV1Checkpoint(context.Background(), fresh, expectation, journalRaw, registryRaw); err != nil {
		t.Fatalf("fresh native fixture physical preflight: %v", err)
	}
	root, err := os.Open(options.ConfigHome)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	repository, err := os.Open(options.RepositoryRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	rootIdentity, err := releaseowner.IdentityOf(root)
	if err != nil {
		t.Fatal(err)
	}
	repositoryIdentity, err := releaseowner.IdentityOf(repository)
	if err != nil {
		t.Fatal(err)
	}
	previous := releasetransition.ReleaseID("0.4.0-68b9925f6880")
	ctx := releaseowner.Context{Session: strings.Repeat("a", 64), DataHome: options.DataHome, ConfigHome: options.ConfigHome, RuntimeRoot: options.RuntimeRoot, RepositoryRoot: options.RepositoryRoot, Yard: "default", Target: "0.5.0-test-release", Artifact: strings.Repeat("b", 64), Direction: "activate-target", Releases: releasetransition.ReleasePair{From: "0.4.2-17608894ab09", Previous: &previous, Target: "0.5.0-test-release"}, Root: rootIdentity, Repository: repositoryIdentity}
	descriptor := struct {
		Context releaseowner.Context `json:"context"`
		Result  V1Import             `json:"result"`
	}{ctx, result}
	raw, err := json.Marshal(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(testkit.TempDir(t), "native-v1.json")
	testkit.WriteFile(t, path, raw, 0600)
	before, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "--exact", "legacy_import::tests::actual_go_native_checkpoint_roundtrip", "--nocapture")
	command.Env = append(os.Environ(), options.Environment...)
	command.Env = append(command.Env, "SUBYARD_RUST_V1_NATIVE_FIXTURE="+path, "SUBYARD_RUST_NATIVE_HELPER="+helper, "PATH="+filepath.Dir(options.Incus)+":"+os.Getenv("PATH"))
	output, err := command.CombinedOutput()
	if err != nil {
		calls, _ := os.ReadFile(readCalls)
		t.Fatalf("Rust current native roundtrip: %v\n%s\nfixed readonly fixture calls:\n%s", err, output, calls)
	}
	t.Logf("%s", output)
	after, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("readonly native roundtrip changed original journal")
	}
	if _, err := os.Lstat(filepath.Join(options.ConfigHome, "migrations", "update.lock")); !os.IsNotExist(err) {
		t.Fatal("readonly native roundtrip acquired update lease")
	}
}

func mustOriginalV1Observer(t *testing.T, options ReleaseOptions) V1ImportFactObserver {
	t.Helper()
	observer, err := NewV1ImportFactObserver(options)
	if err != nil {
		t.Fatal(err)
	}
	return observer
}

func TestOriginalV1CheckpointFacts(t *testing.T) {
	for _, selected := range []string{"current-fallback", "original-previous"} {
		t.Run(selected, func(t *testing.T) {
			version := "0.4.2"
			options, journalPath, _ := publishedV1CompatibilityFixture(t, version)
			previousEngine := filepath.Join(options.RuntimeRoot, testPublishedV1PreviousRuntime, "bin", "yard-engine")
			if selected == "original-previous" {
				image, err := os.ReadFile(previousEngine)
				if err != nil {
					t.Fatal(err)
				}
				for _, setting := range options.Environment {
					if path, ok := strings.CutPrefix(setting, "V1_BROKER_DIGEST_STATE="); ok {
						testkit.WriteFile(t, path, []byte(string(fingerprintV1ImportPayload(image))+"\n"), 0600)
					}
				}
			}
			journal, err := os.ReadFile(journalPath)
			if err != nil {
				t.Fatal(err)
			}
			var tx transaction
			if err := json.Unmarshal(journal, &tx); err != nil {
				t.Fatal(err)
			}
			registry, err := os.ReadFile(filepath.Join(options.RuntimeRoot, tx.ToRuntime, "config", "migrations.json"))
			if err != nil {
				t.Fatal(err)
			}
			release, checkpoint, valid := v1ImportCheckpoint(tx)
			if !valid {
				t.Fatal("fixture checkpoint")
			}
			expectation := V1ImportFactExpectation{release, checkpoint, v1CompatibilityTransactionID(tx), V1ImportRuntimePair{tx.ToRuntime, tx.FromRuntime}}
			old, err := NewV1ImportFactObserver(options)
			if err != nil {
				t.Fatal(err)
			}
			want, err := old.ObserveV1ImportFacts(context.Background(), expectation)
			if err != nil {
				t.Fatal(err)
			}
			// No discovery: the original records remain the only selector even
			// when the live journal and previous link are no longer available.
			if err := os.Remove(journalPath); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(options.RuntimeRoot, "previous")); err != nil {
				t.Fatal(err)
			}
			got, err := ObserveOriginalV1Checkpoint(context.Background(), options, expectation, journal, registry)
			if err != nil || got != want {
				t.Fatalf("original facts=%s want=%s err=%v", got, want, err)
			}
			wrong := expectation
			wrong.Transaction = "v1-00000000000000000000000000000000"
			if _, err := ObserveOriginalV1Checkpoint(context.Background(), options, wrong, journal, registry); err == nil {
				t.Fatal("wrong original identity accepted")
			}
			var decoded Registry
			if err := json.Unmarshal(registry, &decoded); err != nil {
				t.Fatal(err)
			}
			decoded.Migrations[0].Operations[0].Kind = "unregistered-reader"
			bad, _ := json.Marshal(decoded)
			if _, err := ObserveOriginalV1Checkpoint(context.Background(), options, expectation, journal, bad); err == nil {
				t.Fatal("different operation accepted")
			}
			if selected == "original-previous" {
				testkit.WriteFile(t, previousEngine, []byte("#!/bin/sh\nexit 1\n"), 0700)
				if _, err := ObserveOriginalV1Checkpoint(context.Background(), options, expectation, journal, registry); err == nil {
					t.Fatal("changed original broker runtime accepted")
				}
			}
		})
	}
}

func TestOriginalV1ImportCorpus(t *testing.T) {
	var rows []struct {
		Name     string   `json:"name"`
		Journal  []byte   `json:"journal"`
		Registry []byte   `json:"registry"`
		Result   V1Import `json:"result"`
	}
	for _, version := range []string{"0.4.1", "0.4.2"} {
		options, journalPath, _ := publishedV1CompatibilityFixture(t, version)
		journal, err := os.ReadFile(journalPath)
		if err != nil {
			t.Fatal(err)
		}
		var tx transaction
		if err := json.Unmarshal(journal, &tx); err != nil {
			t.Fatal(err)
		}
		registry, err := os.ReadFile(filepath.Join(options.RuntimeRoot, tx.ToRuntime, "config", "migrations.json"))
		if err != nil {
			t.Fatal(err)
		}
		vectors := [][]string{{operationRollingBack, operationRolledBack}, {operationRolledBack, operationRolledBack}}
		if version == "0.4.2" {
			vectors = [][]string{{operationCommitted, operationCommitted, operationRollingBack}, {operationCommitted, operationCommitted, operationRolledBack}, {operationCommitted, operationRollingBack, operationRolledBack}, {operationCommitted, operationRolledBack, operationRolledBack}, {operationRollingBack, operationRolledBack, operationRolledBack}, {operationRolledBack, operationRolledBack, operationRolledBack}}
		}
		for n, phases := range append(vectors, vectors[len(vectors)-1]) {
			tx.Phase = "rolling-back"
			if n == len(vectors) {
				tx.Phase = "rolled-back"
			}
			for i, p := range phases {
				tx.Operations[i].Phase = p
			}
			writeV1CompatibilityTransaction(t, journalPath, tx)
			journal, err = os.ReadFile(journalPath)
			if err != nil {
				t.Fatal(err)
			}
			reader, err := NewV1ImportReader(V1ImportOptions{ConfigHome: options.ConfigHome, RuntimeRoot: options.RuntimeRoot, Facts: V1ImportFactObserverFunc(func(_ context.Context, e V1ImportFactExpectation) (releasetransition.Fingerprint, error) {
				return fingerprintV1ImportValue(struct {
					SchemaVersion int                     `json:"schemaVersion"`
					Expectation   V1ImportFactExpectation `json:"expectation"`
					Class         string                  `json:"class"`
				}{1, e, "supported-published-checkpoint"})
			})})
			if err != nil {
				t.Fatal(err)
			}
			result, err := reader.InspectBound(context.Background(), V1ImportRuntimePair{tx.ToRuntime, tx.FromRuntime})
			if err != nil {
				t.Fatal(err)
			}
			rows = append(rows, struct {
				Name     string   `json:"name"`
				Journal  []byte   `json:"journal"`
				Registry []byte   `json:"registry"`
				Result   V1Import `json:"result"`
			}{string(result.Checkpoint), bytes.Clone(journal), bytes.Clone(registry), result})
		}
	}
	if path := os.Getenv("SUBYARD_RUST_V1_IMPORT_CORPUS"); path != "" {
		raw, err := json.Marshal(rows)
		if err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, path, raw, 0o600)
	}
}
