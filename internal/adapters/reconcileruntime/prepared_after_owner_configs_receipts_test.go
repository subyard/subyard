package reconcileruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/configmaterial"
	"github.com/Subyard/Subyard/internal/testkit"
)

func afterOwnerReceiptRuntime(t *testing.T, root, format string) (Runtime, *afterOwnerFileExecutor) {
	t.Helper()
	runtime, e := afterOwnerFilesFixture(t, root)
	runtime.Environment = append(runtime.Environment, "AGENT_sample_CONFIG_DEST=.sample/a."+format)
	return runtime, e
}
func TestAfterOwnerStructuredPublicationCutsResumeOriginalRecordAcrossFreshProcess(t *testing.T) {
	ctx := context.Background()
	if root := os.Getenv("SUBYARD_RECEIPT_CHILD"); root != "" {
		format := os.Getenv("SUBYARD_RECEIPT_FORMAT")
		runtime, executor := afterOwnerReceiptRuntime(t, root, format)
		recipe, err := os.ReadFile(filepath.Join(root, "original.recipe"))
		if err != nil {
			t.Fatal(err)
		}
		physical, err := os.ReadFile(filepath.Join(root, "original.physical"))
		if err != nil {
			t.Fatal(err)
		}
		destination := filepath.Join(root, "home/.sample/a."+format)
		scenario := os.Getenv("SUBYARD_RECEIPT_SCENARIO")
		identity := fmt.Sprintf("%x", sha256.Sum256([]byte("dev\x00"+destination)))
		baseline := filepath.Join(root, "materialization", identity+".json")
		receipt := filepath.Join(root, "materialization", identity+".ownership")
		switch scenario {
		case "owned":
			raw, _ := os.ReadFile(destination)
			testkit.WriteFile(t, destination, bytes.ReplaceAll(raw, []byte("desired"), []byte("foreign")), 0644)
		case "mode":
			if err := os.Chmod(destination, 0600); err != nil {
				t.Fatal(err)
			}
		case "uid":
			if err := os.Chown(destination, 0, -1); err != nil {
				t.Fatal(err)
			}
		case "gid":
			if err := os.Chown(destination, -1, 0); err != nil {
				t.Fatal(err)
			}
		case "path":
			parent := filepath.Dir(destination)
			if err := os.Rename(parent, parent+"-original"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(parent+"-original", parent); err != nil {
				t.Fatal(err)
			}
		case "source":
			testkit.WriteFile(t, filepath.Join(root, "sample.CONFIG"), []byte("foreign source"), 0600)
		case "uuid":
			runtime.Incus.(*testkit.Incus).Reconcile.Instance.Config["volatile.uuid"] = "replacement"
		case "selector":
			runtime.Environment = append(runtime.Environment, "AGENT_sample_CONFIG_DEST=.sample/other."+format)
		case "foreign-receipt":
			raw, err := os.ReadFile(receipt)
			if err != nil {
				t.Fatal(err)
			}
			var value map[string]any
			if json.Unmarshal(raw, &value) != nil {
				t.Fatal("receipt fixture")
			}
			value["desired_digest"] = strings.Repeat("f", 64)
			raw, _ = json.Marshal(value)
			testkit.WriteFile(t, receipt, append(raw, '\n'), 0600)
		case "foreign-prefix":
			raw, err := os.ReadFile(filepath.Join(root, "original.baseline"))
			if err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, baseline, raw, 0600)
		case "missing-witness":
			var value afterOwnerConfigPhysical
			if json.Unmarshal(physical, &value) != nil {
				t.Fatal("physical fixture")
			}
			value.Files[0].Materialization = nil
			physical, _ = json.Marshal(value)
		}
		before := afterOwnerFileStat(t, destination)
		p, err := runtime.BindAfterOwnerMaterializedConfigs(ctx, recipe, physical)
		if err == nil {
			defer p.Close()
			err = runtime.ApplyPreparedMaterializedConfigs(ctx, p)
		}
		if scenario != "resume" {
			if err == nil {
				t.Fatal("unapproved receipt retry accepted", scenario)
			}
			if before != afterOwnerFileStat(t, destination) {
				t.Fatal("rejected retry rewrote destination", scenario)
			}
			if scenario == "missing-witness" && executor.calls != 0 {
				t.Fatal("old missing witness captured late state")
			}
			return
		}
		if err != nil {
			t.Fatal("fresh receipt retry", err)
		}
		if before != afterOwnerFileStat(t, destination) {
			t.Fatal("receipt retry rewrote already desired destination")
		}
		if !bytes.Equal(recipe, p.Recipe()) {
			t.Fatal("receipt repair replaced original recipe")
		}
		originalPhysical, _ := os.ReadFile(filepath.Join(root, "original.physical"))
		if !bytes.Equal(physical, originalPhysical) {
			t.Fatal("receipt repair replaced physical record")
		}
		fresh, err := runtime.observeAfterOwnerConfigFiles(ctx, p.PreparedConfigs())
		if err != nil || len(fresh) != 1 || !fresh[0].Assessment.Converged {
			t.Fatal("receipt repair did not converge", fresh, err)
		}
		return
	}
	for _, format := range []string{"json", "toml"} {
		for _, initial := range []string{"missing", "old"} {
			for _, cut := range []string{"destination", "baseline"} {
				scenarios := []string{"resume"}
				if initial == "old" && cut == "baseline" {
					scenarios = append(scenarios, "owned", "mode", "path", "source", "uuid", "selector", "foreign-receipt", "foreign-prefix", "missing-witness")
					if os.Getuid() == 0 {
						scenarios = append(scenarios, "uid", "gid")
					}
				}
				for _, scenario := range scenarios {
					t.Run(format+"/"+initial+"/"+cut+"/"+scenario, func(t *testing.T) {
						root := testkit.TempDir(t)
						runtime, e := afterOwnerReceiptRuntime(t, root, format)
						desired, original := []byte(`{"managed":"desired"}`), []byte(`{"managed":"original","runtime":"kept"}`)
						if format == "toml" {
							desired = []byte("managed = 'desired'\n")
							original = []byte("managed = 'original'\nruntime = 'kept'\n")
						}
						destination := filepath.Join(root, "home/.sample/a."+format)
						if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
							t.Fatal(err)
						}
						testkit.WriteFile(t, destination, original, 0644)
						testkit.WriteFile(t, filepath.Join(root, "sample.CONFIG"), desired, 0600)
						testkit.WriteFile(t, filepath.Join(root, "publication-events"), nil, 0600)
						identity := fmt.Sprintf("%x", sha256.Sum256([]byte("dev\x00"+destination)))
						if initial == "old" {
							oldTemplate := []byte(`{"managed":"original"}`)
							if format == "toml" {
								oldTemplate = []byte("managed = 'original'\n")
							}
							request, err := configmaterial.Request(format, configmaterial.ModeApply, "dev", "/home/dev/.sample/a."+format, runtime.Yard.DevUID, oldTemplate)
							if err != nil {
								t.Fatal(err)
							}
							if result, err := e.Exec(ctx, "", "", request); err != nil || result.ExitCode != 0 {
								t.Fatal("original owning receipt seed", err, string(result.Stderr))
							}
							baseline, err := os.ReadFile(filepath.Join(root, "materialization", identity+".json"))
							if err != nil {
								t.Fatal(err)
							}
							testkit.WriteFile(t, filepath.Join(root, "original.baseline"), baseline, 0600)
						}
						source, err := runtime.PrepareAfterOwnerMaterializedConfigs(ctx, []byte(`{"version":1,"phase":"template","metadataBytes":8192,"descriptorBytes":8192}`))
						if err != nil {
							t.Fatal(err)
						}
						defer source.Close()
						p, physical, err := runtime.ResolveAfterOwnerMaterializedConfigs(ctx, source.Recipe())
						if err != nil {
							t.Fatal(err)
						}
						defer p.Close()
						if initial == "missing" {
							if _, err := os.Stat(filepath.Join(root, "materialization")); !os.IsNotExist(err) {
								t.Fatal("read-only capture created materialization state", err)
							}
							var captured afterOwnerConfigPhysical
							if json.Unmarshal(physical, &captured) != nil || len(captured.Files) != 1 || captured.Files[0].Materialization == nil || *captured.Files[0].Materialization != (configmaterial.ProtectedState{}) {
								t.Fatal("captured absence lacks explicit witness")
							}
						}
						resolved, err := p.WithAfterOwnerMetadata([]byte(`{"version":1,"phase":"resolved"}`))
						if err != nil {
							t.Fatal(err)
						}
						defer resolved.Close()
						testkit.WriteFile(t, filepath.Join(root, "original.recipe"), resolved.Recipe(), 0600)
						testkit.WriteFile(t, filepath.Join(root, "original.physical"), physical, 0600)
						e.publicationCut = cut
						if scenario == "foreign-prefix" {
							e.publicationCut = "receipt"
						}
						if err := runtime.ApplyPreparedMaterializedConfigs(ctx, resolved); err == nil {
							t.Fatal("publisher cut missing")
						}
						events, _ := os.ReadFile(filepath.Join(root, "publication-events"))
						wanted := "destination\n"
						if cut == "baseline" {
							wanted += "baseline\n"
						}
						if scenario == "foreign-prefix" {
							wanted += "receipt\n"
						}
						if string(events) != wanted {
							t.Fatal("wrong real publication cut", string(events))
						}
						current, _ := os.ReadFile(destination)
						if !bytes.Contains(current, []byte("desired")) || !bytes.Contains(current, []byte("kept")) {
							t.Fatal("cut did not retain desired/unmanaged document")
						}
						request, _ := configmaterial.Request(format, configmaterial.ModeObserve, "dev", "/home/dev/.sample/a."+format, runtime.Yard.DevUID, desired)
						result, err := e.Exec(ctx, "", "", request)
						if err != nil || result.ExitCode != 0 {
							t.Fatal("ordinary observe after cut", err)
						}
						observation, err := configmaterial.ParseObservation(result.Stdout)
						if err != nil || scenario != "foreign-prefix" && observation.Converged {
							t.Fatal("receipt gap claimed ordinary convergence", err)
						}
						var record afterOwnerConfigPhysical
						if json.Unmarshal(physical, &record) != nil || len(record.Files) != 1 {
							t.Fatal("original physical record invalid")
						}
						child := exec.Command(os.Args[0], "-test.run=^TestAfterOwnerStructuredPublicationCutsResumeOriginalRecordAcrossFreshProcess$", "-test.v")
						child.Env = []string{"PATH=" + os.Getenv("PATH"), "SUBYARD_RECEIPT_CHILD=" + root, "SUBYARD_RECEIPT_FORMAT=" + format, "SUBYARD_RECEIPT_SCENARIO=" + scenario}
						if output, err := child.CombinedOutput(); err != nil {
							t.Fatalf("fresh helper: %v\n%s", err, strings.TrimSpace(string(output)))
						}
					})
				}
			}
		}
	}
}
