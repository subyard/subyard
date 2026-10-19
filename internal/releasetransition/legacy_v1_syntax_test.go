package releasetransition_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	runtimeadapter "github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/migration"
	r "github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/sys/unix"
)

// Only this owning test executable supplies these fixed pure syntax/version
// endpoints. Production source-query always receives an authenticated image.
func TestMain(m *testing.M) {
	if len(os.Args) == 2 {
		switch os.Args[1] {
		case "_release-owner-codec-v1":
			if r.RunLegacyCodec(os.Stdin, os.Stdout) != nil {
				os.Exit(1)
			}
			os.Exit(0)
		case "--version":
			fmt.Println("yard-engine 1.0.0-source-query")
			os.Exit(0)
		case "_release-transition":
			var request struct {
				SchemaVersion int    `json:"schemaVersion"`
				Mode          string `json:"mode"`
			}
			if json.NewDecoder(os.Stdin).Decode(&request) != nil || request.SchemaVersion != 2 ||
				(request.Mode != "lifecycle-capabilities" && request.Mode != "capabilities") {
				os.Exit(1)
			}
			caps := r.ActivationRecoveryLifecycleCapabilities()
			if os.Getenv("SUBYARD_SOURCE_CAPABILITY_TEST_INVALID") == "1" {
				caps.Contract = "future-unsupported"
			}
			json.NewEncoder(os.Stdout).Encode(r.RecoveryProcessResponse{SchemaVersion: 2, Capabilities: &caps})
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}

func TestLegacyV1Syntax(t *testing.T) {
	for _, row := range []struct {
		name     string
		raw      []byte
		field    string
		accepted bool
	}{
		{"fold-duplicate", []byte(`{"SCHEMAVERSION":0,"schemaVersion":1,"phase":"committed","toRelease":"x"}`), `"schemaVersion":1`, true},
		{"null", []byte(`null`), `"schemaVersion":0`, true},
		{"null-scalars", []byte(`{"schemaVersion":null,"phase":null}`), `"phase":""`, true},
		{"bad-utf8", []byte("{\"toRelease\":\"\xff\xfe\"}"), `"toRelease":"��"`, true},
		{"unknown", []byte(`{"unknown":1}`), "", false},
		{"fraction", []byte(`{"schemaVersion":1.2}`), "", false},
		{"overflow", []byte(`{"entries":[{"mode":4294967296}]}`), "", false},
		{"trailing", []byte(`{} {}`), "", false},
	} {
		t.Run(row.name, func(t *testing.T) {
			projection, flags, err := r.NormalizeFrozenRecord(18, row.raw)
			if (err == nil) != row.accepted || flags != 0 || err == nil && !bytes.Contains(projection, []byte(row.field)) {
				t.Fatalf("syntax result accepted=%v flags=%d", err == nil, flags)
			}
		})
	}
	// Unsupported capability semantics must still project as syntax.
	if _, _, err := r.NormalizeFrozenRecord(19, []byte(`{"schemaVersion":99,"capabilities":{"contract":"future"}}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.NormalizeFrozenRecord(15, []byte(`{}`)); err == nil {
		t.Fatal("native kind admitted by CODEC")
	}
	for _, kind := range []byte{20, 21} {
		for _, raw := range []string{`{"SCHEMAVERSION":0,"schemaVersion":99}`, `null`, `{"schemaVersion":null}`} {
			if _, _, err := r.NormalizeFrozenRecord(kind, []byte(raw)); err != nil {
				t.Fatalf("kind%d original syntax refused: %v", kind, err)
			}
		}
		for _, raw := range []string{`{"foreign":true}`, `{"schemaVersion":1.1}`, `{} {}`} {
			if _, _, err := r.NormalizeFrozenRecord(kind, []byte(raw)); err == nil {
				t.Fatalf("kind%d malformed syntax accepted", kind)
			}
		}
	}
}

func TestSourcePathMapCorpus(t *testing.T) {
	path := os.Getenv("SUBYARD_RUST_SOURCE_BINDING_CORPUS")
	if path == "" {
		return
	}
	type row struct {
		Name   string   `json:"name"`
		Keys   []string `json:"keys"`
		Values []uint64 `json:"values"`
		SHA256 string   `json:"sha256"`
	}
	var rows []row
	for _, name := range []string{"raw-keys", "large-inventory"} {
		keys := []string{"/raw/\xff", "/raw/\xfe", "/raw/\xe2\x82", "/raw/�", "/raw/😀", "/raw/<&\"\\\n\u2028\u2029"}
		if name == "large-inventory" {
			keys = nil
			for i := 0; i < 6000; i++ {
				keys = append(keys, fmt.Sprintf("/fixture/%s/%06d", strings.Repeat("a", 200), i))
			}
		}
		value := map[string]uint64{}
		result := row{Name: name}
		for i, key := range keys {
			value[key] = uint64(i)
			result.Keys = append(result.Keys, base64.StdEncoding.EncodeToString([]byte(key)))
			result.Values = append(result.Values, uint64(i))
		}
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if name == "large-inventory" && len(raw) <= 1<<20 {
			t.Fatal("large corpus below oldframe ceiling")
		}
		result.SHA256 = sourceHash(raw)
		rows = append(rows, result)
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, raw, 0600)
}

func sourceMkdir(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	var missing []string
	for p := path; ; p = filepath.Dir(p) {
		if _, err := os.Stat(p); err == nil {
			break
		}
		missing = append(missing, p)
	}
	for i := len(missing) - 1; i >= 0; i-- {
		if err := os.Mkdir(missing[i], mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(missing[i], mode); err != nil {
			t.Fatal(err)
		}
	}
}
func sourceIdentity(t *testing.T, path string) map[string]any {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	s := info.Sys().(*syscall.Stat_t)
	return map[string]any{"device": uint64(s.Dev), "inode": s.Ino, "mode": s.Mode, "uid": s.Uid, "links": uint64(s.Nlink)}
}
func sourceHash(raw []byte) string { h := sha256.Sum256(raw); return hex.EncodeToString(h[:]) }
func sourceConfigSnapshot(t *testing.T, root string) string {
	t.Helper()
	inputs := map[string]any{}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		stat := info.Sys().(*syscall.Stat_t)
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		value := []any{stat.Dev, stat.Ino, stat.Mode, stat.Uid, stat.Gid, stat.Nlink}
		if info.Mode().IsRegular() {
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			value = append(value, sourceHash(raw))
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			value = append(value, target)
		}
		inputs[relative] = value
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(inputs)
	if err != nil {
		t.Fatal(err)
	}
	return sourceHash(raw)
}
func sourceQuery(t *testing.T, owner, helper string, options any, config, runtime string, malformed bool) ([]byte, error) {
	t.Helper()
	fd, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	parent, child := os.NewFile(uintptr(fd[0]), "source-query"), os.NewFile(uintptr(fd[1]), "source-query-child")
	defer parent.Close()
	defer child.Close()
	cmd := exec.Command(owner, "_release-owner-source-v1")
	cmd.Stdin = child
	cmd.Stdout = child
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	child.Close()
	conn, err := net.FileConn(parent)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	unixConn := conn.(*net.UnixConn)
	var files []*os.File
	for _, path := range []string{helper, config, runtime} {
		f, e := os.Open(path)
		if e != nil {
			t.Fatal(e)
		}
		files = append(files, f)
		defer f.Close()
	}
	rights := []int{int(files[0].Fd()), int(files[1].Fd()), int(files[2].Fd())}
	if malformed {
		rights = rights[:2]
	}
	if _, _, err := unixConn.WriteMsgUnix([]byte("S"), unix.UnixRights(rights...), nil); err != nil {
		t.Fatal(err)
	}
	if !malformed {
		if err := json.NewEncoder(unixConn).Encode(options); err != nil {
			t.Fatal(err)
		}
	}
	unixConn.CloseWrite()
	raw, readErr := io.ReadAll(unixConn)
	waitErr := cmd.Wait()
	if readErr != nil {
		return raw, readErr
	}
	if waitErr != nil {
		return raw, fmt.Errorf("source query refused (%v): %s", waitErr, stderr.String())
	}
	return raw, nil
}

func TestLegacySourceAdmissionRust(t *testing.T) {
	owner := os.Getenv("SUBYARD_RUST_SOURCE_QUERY_EXECUTABLE")
	if owner == "" {
		t.Skip("actual Rust executable supplied by owning gate")
	}
	helper, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	engine, err := os.ReadFile(helper)
	if err != nil {
		t.Fatal(err)
	}
	registry := []byte(`{"schemaVersion":2,"minimumEpochs":{"settings":1},"currentEpochs":{"settings":1},"migrations":[]}`)
	variants := []string{"absent", "closed-v1", "unsupported-v1", "symlink", "fifo", "nonutf8", "nonutf8-assets", "deep-assets", "same-byte-inode", "wrong-tuple", "wrong-root", "wrong-capability", "wrong-fds", "native-unknown", "complete-go", "activation-only-go", "unsupported-capability", "imported-v1"}
	if os.Getenv("SUBYARD_RUST_SOURCE_QUERY_AUTHENTIC_ROOT") != "" {
		variants = append(variants, "authentic-go")
	}
	for _, variant := range variants {
		t.Run(variant, func(t *testing.T) {
			engine, registry := engine, registry
			root := testkit.TempDir(t)
			if variant == "nonutf8" {
				root = filepath.Join(root, "root-\xff\xe2\x82")
				sourceMkdir(t, root, 0700)
			}
			config, runtime := filepath.Join(root, "config"), filepath.Join(root, "runtime")
			sourceMkdir(t, config, 0700)
			release := "1.0.0-source-query-fixture"
			version := "1.0.0-source-query"
			if variant == "authentic-go" {
				version = "0.21.0"
				release = "0.21.0-1fee6251856e"
			}
			assets := filepath.Join(runtime, "releases", release)
			sourceMkdir(t, filepath.Join(assets, "bin"), 0700)
			sourceMkdir(t, filepath.Join(assets, "config"), 0700)
			testkit.WriteFile(t, filepath.Join(assets, "bin", "yard-engine"), engine, 0700)
			testkit.WriteFile(t, filepath.Join(assets, "config", "release-transition.json"), registry, 0644)
			manifest := []byte(fmt.Sprintf("%s  ./bin/yard-engine\n%s  ./config/release-transition.json\n", sourceHash(engine), sourceHash(registry)))
			if variant == "nonutf8-assets" {
				for _, name := range []string{"payload-\xff", "payload-\xfe", "payload-\xe2\x82", "payload-😀"} {
					testkit.WriteFile(t, filepath.Join(assets, name), []byte("source"), 0644)
					manifest = append(manifest, []byte(sourceHash([]byte("source"))+"  ./"+name+"\n")...)
				}
			}
			if variant == "deep-assets" {
				name := strings.Repeat("nested/", 80) + "original"
				sourceMkdir(t, filepath.Dir(filepath.Join(assets, name)), 0700)
				testkit.WriteFile(t, filepath.Join(assets, name), []byte("deep source"), 0644)
				manifest = append(manifest, []byte(sourceHash([]byte("deep source"))+"  ./"+name+"\n")...)
			}
			testkit.WriteFile(t, filepath.Join(assets, "runtime-files.sha256"), manifest, 0644)
			if variant == "authentic-go" {
				from := os.Getenv("SUBYARD_RUST_SOURCE_QUERY_AUTHENTIC_ROOT")
				err := filepath.WalkDir(from, func(path string, entry fs.DirEntry, err error) error {
					if err != nil {
						return err
					}
					rel, err := filepath.Rel(from, path)
					if err != nil {
						return err
					}
					to := filepath.Join(assets, rel)
					if entry.IsDir() {
						sourceMkdir(t, to, 0700)
						return nil
					}
					if !entry.Type().IsRegular() {
						return fmt.Errorf("authentic source contains nonregular member")
					}
					data, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					info, err := entry.Info()
					if err != nil {
						return err
					}
					testkit.WriteFile(t, to, data, info.Mode().Perm())
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				engine, err = os.ReadFile(filepath.Join(assets, "bin", "yard-engine"))
				if err != nil {
					t.Fatal(err)
				}
				registry, err = os.ReadFile(filepath.Join(assets, "config", "release-transition.json"))
				if err != nil {
					t.Fatal(err)
				}
				manifest, err = os.ReadFile(filepath.Join(assets, "runtime-files.sha256"))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Lstat(filepath.Join(assets, "config", "release-owner.json")); !os.IsNotExist(err) {
					t.Fatal("authentic Go source unexpectedly declares Rust")
				}
			}
			if err := os.Symlink("releases/"+release, filepath.Join(runtime, "current")); err != nil {
				t.Fatal(err)
			}
			if variant == "closed-v1" || variant == "unsupported-v1" || variant == "symlink" || variant == "fifo" {
				version := "0.4.1"
				txdir := filepath.Join(config, "migrations", "transactions", sourceHash([]byte(version))[:32])
				sourceMkdir(t, txdir, 0700)
				phase := "committed"
				if variant == "unsupported-v1" {
					phase = "rolling-back"
				}
				raw := []byte(`{"SCHEMAVERSION":0,"schemaVersion":1,"toRelease":"0.4.1","phase":"` + phase + `","entries":[{"recovery":"original.bin"}]}`)
				testkit.WriteFile(t, filepath.Join(txdir, "transaction.json"), raw, 0600)
				testkit.WriteFile(t, filepath.Join(txdir, "original.bin"), []byte("original recovery bytes"), 0600)
				if variant == "symlink" || variant == "fifo" {
					os.Remove(filepath.Join(txdir, "original.bin"))
					if variant == "symlink" {
						err = os.Symlink("transaction.json", filepath.Join(txdir, "original.bin"))
					} else {
						err = unix.Mkfifo(filepath.Join(txdir, "original.bin"), 0600)
					}
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			if variant == "native-unknown" {
				type binding struct {
					Session, Transaction, Plan, Journal, Artifact, Helper string
					Ordinal                                               uint64
				}
				// Tags/order are the original native Binding, not a caller hash.
				braw := []byte(fmt.Sprintf(`{"session":%q,"transaction":"tx-source-query","plan":"plan-v1-%s","journal":%q,"artifact":%q,"helper":%q,"ordinal":1}`, strings.Repeat("a", 64), strings.Repeat("a", 64), strings.Repeat("a", 64), sourceHash(manifest), sourceHash(engine)))
				dir := filepath.Join(config, "migrations", "native-custody", "v1", "tx-source-query")
				sourceMkdir(t, dir, 0700)
				testkit.WriteFile(t, filepath.Join(dir, "001.reservation.json"), append([]byte(`{"version":1,"binding":`), append(braw, []byte(`,"files":2570,"bytes":16777216}`+"\n")...)...), 0600)
				testkit.WriteFile(t, filepath.Join(dir, "001.intent.json"), []byte(fmt.Sprintf(`{"version":1,"binding":%s,"owner":"synthetic","resource":"test","expected":%q,"desired":%q,"source":%q,"recipe":%q}`+"\n", braw, strings.Repeat("a", 64), strings.Repeat("a", 64), strings.Repeat("a", 64), strings.Repeat("a", 64))), 0600)
				testkit.WriteFile(t, filepath.Join(dir, "001.accepted.json"), []byte(fmt.Sprintf(`{"version":1,"binding":%s,"owner":"synthetic","resource":"test","state":"accepted","server":"","project":"","instance":"","instanceUuid":"","operation":"","finalStatus":0,"operations":[],"observed":""}`+"\n", braw)), 0600)
			}
			if variant == "complete-go" || variant == "activation-only-go" || variant == "unsupported-capability" {
				sourceMkdir(t, filepath.Join(runtime, "releases", "prior"), 0700)
				if err := os.Symlink("releases/prior", filepath.Join(runtime, "previous")); err != nil {
					t.Fatal(err)
				}
				links := r.ReleaseLinks{Active: "prior"}
				transition, err := r.NewV2Transition(r.V2Options{ConfigHome: config, RegistryPayload: registry, ArtifactDigest: r.Fingerprint(sourceHash(manifest)), CandidateVersion: "1.0.0-source-query", Releases: r.ReleasePair{From: "prior", Target: r.ReleaseID(release)}, ObserveLinks: func(context.Context) (r.ReleaseLinks, error) { return links, nil }, ActivateLinks: func(_ context.Context, pair r.ReleasePair) (r.ReleaseLinks, error) {
					old := pair.From
					links = r.ReleaseLinks{Active: pair.Target, Previous: &old}
					return links, nil
				}, NewTransactionID: func() r.TransactionID { return "tx-source-completed-go" }, VerifyAuthorization: func(r.PlanToken, r.Authorization) bool { return true }})
				if err != nil {
					t.Fatal(err)
				}
				goal := r.Goal{Target: r.ReleaseID(release), Direction: r.DirectionActivateTarget}
				inspection, err := transition.Inspect(context.Background(), goal)
				if err != nil {
					t.Fatal(err)
				}
				outcome, err := transition.Converge(context.Background(), r.Execution{Plan: inspection.Plan, Authorization: "test-only-consent"})
				if err != nil || outcome.Status != r.StatusReady {
					t.Fatalf("actualGo completed history failed: %v %s", err, outcome.Status)
				}
				if variant != "complete-go" {
					path := filepath.Join(config, "release-transition", "v2", "journal.json")
					raw, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					journal, err := r.ParseJournal(raw)
					if err != nil || len(journal.Steps) != 0 {
						t.Fatalf("activation-only prerequisite: %v steps%d", err, len(journal.Steps))
					}
					journal.Checkpoint = r.JournalReconciling
					raw, err = r.MarshalJournal(journal)
					if err != nil {
						t.Fatal(err)
					}
					testkit.WriteFile(t, path, raw, 0600)
					if variant == "unsupported-capability" {
						t.Setenv("SUBYARD_SOURCE_CAPABILITY_TEST_INVALID", "1")
					}
				}
			}

			if variant == "imported-v1" {
				route := `{"schemaVersion":1,"active":true,"consumers":[{"project":"subyard","instance":"yard","yard":"default","mounted":false}]}`
				operations := []map[string]any{{"migrationId": "migrate-test-yard-owner", "operationId": "test-yard-owner", "kind": migration.OperationKindTestYardOwnerV1, "before": "current", "phase": "rolled-back"}, {"migrationId": "migrate-test-yard-owner", "operationId": "test-yard-route-consumers", "kind": migration.OperationKindTestYardRouteConsumersV1, "before": route, "phase": "rolled-back"}}
				tx := map[string]any{"schemaVersion": 1, "fromLayout": 1, "toLayout": 2, "fromRuntime": "releases/0.4.0-68b9925f6880", "toRelease": "0.4.1", "toRuntime": "releases/0.4.1-fc5b03078508", "phase": "rolled-back", "migrations": []string{"migrate-test-yard-owner"}, "operations": operations, "rollbackOperations": true}
				txraw, _ := json.Marshal(tx)
				dir := filepath.Join(config, "migrations", "transactions", sourceHash([]byte("0.4.1"))[:32])
				sourceMkdir(t, dir, 0700)
				testkit.WriteFile(t, filepath.Join(dir, "transaction.json"), txraw, 0600)
				retained := filepath.Join(runtime, "releases", "0.4.1-fc5b03078508", "config")
				sourceMkdir(t, retained, 0700)
				v1registry := migration.Registry{SchemaVersion: 1, MinimumLayout: 1, CurrentLayout: 2, Migrations: []migration.Definition{{ID: "migrate-test-yard-owner", FromLayout: 1, ToLayout: 2, Resources: []string{"test-yard-owner", "test-yard-route-consumers"}, FinalizePolicy: "ordered-after-active-verify", RollbackPolicy: "reverse-before-runtime-swap", Operations: []migration.Operation{{ID: "test-yard-owner", Kind: migration.OperationKindTestYardOwnerV1}, {ID: "test-yard-route-consumers", Kind: migration.OperationKindTestYardRouteConsumersV1}}}}}
				v1raw, _ := json.Marshal(v1registry)
				testkit.WriteFile(t, filepath.Join(retained, "migrations.json"), v1raw, 0644)
				reader, err := migration.NewV1ImportReader(migration.V1ImportOptions{ConfigHome: config, RuntimeRoot: runtime, Facts: migration.V1ImportFactObserverFunc(func(context.Context, migration.V1ImportFactExpectation) (r.Fingerprint, error) {
					t.Fatal("static import called live facts")
					return "", nil
				})})
				if err != nil {
					t.Fatal(err)
				}
				imported, err := reader.InspectBoundStatic(context.Background(), migration.V1ImportRuntimePair{Current: "releases/0.4.1-fc5b03078508", Previous: "releases/0.4.0-68b9925f6880"})
				if err != nil {
					t.Fatal(err)
				}
				proof, err := r.MarshalCompatibilityEvidence(r.CompatibilityEvidence{SchemaVersion: 1, Kind: r.V2LegacyV1Import, Identity: imported.StaticDigest, From: "0.4.1-fc5b03078508", Previous: "0.4.0-68b9925f6880"})
				if err != nil {
					t.Fatal(err)
				}
				proofdir := filepath.Join(config, "release-transition", "v2", "compatibility")
				sourceMkdir(t, proofdir, 0700)
				testkit.WriteFile(t, filepath.Join(proofdir, string(imported.StaticDigest)+".json"), proof, 0600)
				gate, err := migration.InspectMutationGate(context.Background(), migration.ReleaseOptions{ConfigHome: config, RuntimeRoot: runtime, Version: "1.0.0-source-query"})
				if err != nil || gate != nil {
					t.Fatalf("actualGo import proof prerequisite refused: %v", err)
				}
			}

			adapter := runtimeadapter.New(runtimeadapter.Config{})
			defer adapter.Close()
			proof, goErr := adapter.InspectSourceAdmission(context.Background(), runtime, config)
			if variant == "authentic-go" && goErr != nil {
				t.Fatal("actual published source prerequisite:", goErr)
			}
			options := map[string]any{"configHome": []byte(config), "runtimeRoot": []byte(runtime), "configIdentity": sourceIdentity(t, config), "runtimeIdentity": sourceIdentity(t, runtime), "active": map[string]any{"release": release, "version": version, "artifactDigest": sourceHash(manifest), "engineDigest": sourceHash(engine), "root": sourceIdentity(t, assets)}}
			if variant == "same-byte-inode" {
				testkit.WriteFile(t, filepath.Join(assets, "config", "replacement"), registry, 0644)
				if err := os.Rename(filepath.Join(assets, "config", "replacement"), filepath.Join(assets, "config", "release-transition.json")); err != nil {
					t.Fatal(err)
				}
			}
			if variant == "wrong-tuple" {
				options["active"].(map[string]any)["artifactDigest"] = strings.Repeat("0", 64)
			}
			if variant == "wrong-root" {
				options["active"].(map[string]any)["root"].(map[string]any)["inode"] = uint64(0)
			}
			if variant == "wrong-capability" {
				options["capability"] = map[string]any{"schemaVersion": 2}
			}
			original := sourceConfigSnapshot(t, config)
			raw, rustErr := sourceQuery(t, owner, helper, options, config, runtime, variant == "wrong-fds")
			if original != sourceConfigSnapshot(t, config) {
				t.Fatal("readonly source query changed protected inputs")
			}
			negative := goErr != nil || variant == "wrong-tuple" || variant == "wrong-capability" || variant == "wrong-fds" || variant == "wrong-root" || variant == "native-unknown"
			if negative {
				if rustErr == nil {
					t.Fatal("Rust admitted refused source")
				}
				if variant == "native-unknown" && !strings.Contains(rustErr.Error(), "Unknown") {
					t.Fatal("native unknown refused for unrelated reason:", rustErr)
				}
				return
			}
			if rustErr != nil {
				t.Fatal(rustErr)
			}
			var actual struct{ Binding, MetadataBinding string }
			if err := json.Unmarshal(raw, &actual); err != nil {
				t.Fatal(err)
			}
			if variant == "same-byte-inode" {
				if actual.Binding == proof.Binding {
					t.Fatal("source physical revision omitted")
				}
				return
			}
			if actual.Binding != proof.Binding || actual.MetadataBinding != string(proof.Metadata.Binding) {
				t.Fatalf("Go/Rust exact source binding mismatch: Go %s Rust %s", proof.Binding, actual.Binding)
			}
		})
	}
}
