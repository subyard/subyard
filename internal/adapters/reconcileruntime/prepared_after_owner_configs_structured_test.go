package reconcileruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/Subyard/Subyard/internal/adapters/configmaterial"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
	"os"
	"path/filepath"
	"testing"
)

func TestAfterOwnerStructuredMutationPreservesOriginalAuthority(t *testing.T) {
	for _, format := range []string{"json", "toml"} {
		for _, scenario := range []string{"current", "original", "desired", "unmanaged", "owned", "mode", "path"} {
			t.Run(format+"/"+scenario, func(t *testing.T) {
				root := testkit.TempDir(t)
				runtime, executor := afterOwnerFilesFixture(t, root)
				runtime.Environment = append(runtime.Environment, "AGENT_sample_CONFIG_DEST=.sample/a."+format)
				if scenario == "current" {
					runtime.Environment = append(runtime.Environment, "AGENT_sample_RULES=")
					rootPackage := testkit.TempDir(t)
					runtime.Integrations = []profile.IntegrationDefinition{{SchemaVersion: 2, Name: "sample", Root: rootPackage, Artifacts: []profile.IntegrationArtifact{{ID: "instructions", Kind: "instructions", SourceSetting: "SAMPLE_INSTRUCTIONS", Destination: ".sample/AGENTS.md", Format: "raw"}}}}
					declaration, _ := json.Marshal(runtime.Integrations[0])
					testkit.WriteFile(t, filepath.Join(rootPackage, "integration.json"), declaration, 0600)
					instructions := filepath.Join(root, "sample.INSTRUCTIONS")
					testkit.WriteFile(t, instructions, []byte("Current source-owned instructions.\n"), 0600)
					runtime.Environment = append(runtime.Environment, "SAMPLE_INSTRUCTIONS="+instructions)
				}
				desired, original, foreign := []byte(`{"managed":"desired"}`), []byte(`{"managed":"original","runtime":"kept"}`), []byte(`{"managed":"foreign","runtime":"kept"}`)
				if format == "toml" {
					desired = []byte("managed = 'desired'\n")
					original = []byte("managed = 'original'\nruntime = 'kept'\n")
					foreign = []byte("managed = 'foreign'\nruntime = 'kept'\n")
				}
				destination := filepath.Join(root, "home/.sample/a."+format)
				if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, filepath.Join(root, "sample.CONFIG"), desired, 0600)
				testkit.WriteFile(t, destination, original, 0644)
				source, err := runtime.PrepareAfterOwnerMaterializedConfigs(context.Background(), []byte(`{"version":1,"phase":"template","metadataBytes":8192,"descriptorBytes":8192}`))
				if err != nil {
					t.Fatal(err)
				}
				defer source.Close()
				prepared, _, err := runtime.ResolveAfterOwnerMaterializedConfigs(context.Background(), source.Recipe())
				if err != nil {
					t.Fatal(err)
				}
				defer prepared.Close()
				if scenario == "current" {
					selected, e := config.SelectMaterializationSources(runtimeEnvironment(runtime.Environment), runtime.devUser(), config.MaterializationConfigAssets, runtime.Integrations...)
					if e != nil || len(selected) != 1 || selected[0].Asset.Name != "sample.config" {
						t.Fatal("normal selected asset fixture differs")
					}
					inputs, e := runtime.PrepareConfigs(context.Background())
					if e != nil {
						t.Fatal("current source preparation failed")
					}
					current, e := runtime.PrepareCurrentMaterializedConfigs(context.Background(), inputs)
					if e != nil {
						t.Fatal("current physical preparation failed")
					}
					defer current.Close()
					request, _ := configmaterial.Request(format, configmaterial.ModeObserve, runtime.devUser(), "/home/dev/.sample/a."+format, runtime.Yard.DevUID, desired)
					result, execErr := runtime.Executor.Exec(context.Background(), runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, request)
					normal, parseErr := configmaterial.ParseObservation(result.Stdout)
					if execErr != nil || result.ExitCode != 0 || parseErr != nil {
						t.Fatalf("normal observation unavailable exec-error=%t exit=%d parse-error=%t", execErr != nil, result.ExitCode, parseErr != nil)
					}
					digest, _ := configmaterial.DesiredDigestFor(format, desired)
					fact, e := config.AssessMaterializedArtifact(config.DesiredMaterializedArtifact{Name: "sample.config", Destination: "/home/dev/.sample/a." + format, OwnedFormat: format, DesiredHash: digest}, config.MaterializationObservation{ExitCode: result.ExitCode, Output: result.Stdout, Fingerprint: normal.Fingerprint, OwnedConverged: normal.Converged})
					if e != nil || len(current.configs.afterOwnerFiles) != 2 || current.configs.afterOwnerFiles[0].Assessment.Snapshot.Name != "sample.instructions" {
						t.Fatalf("normal assessment or physical row count invalid: assessment_error=%t rows=%d instruction_first=%t", e != nil, len(current.configs.afterOwnerFiles), len(current.configs.afterOwnerFiles) > 0 && current.configs.afterOwnerFiles[0].Assessment.Snapshot.Name == "sample.instructions")
					}
					check := runtime.CheckCurrentConfigAssessments(context.Background(), current, []config.MaterializedArtifactAssessment{fact})
					if check != nil {
						t.Fatal("current normal/physical assessment refused before apply")
					}
					for _, fresh := range [][]config.MaterializedArtifactAssessment{nil, {fact, fact}, {fact, {}}, {current.configs.afterOwnerFiles[0].Assessment}} {
						if runtime.CheckCurrentConfigAssessments(context.Background(), current, fresh) == nil {
							t.Fatal("nonexact config assessment set accepted")
						}
					}
					injected := filepath.Join(root, "home/.sample/AGENTS.md")
					testkit.WriteFile(t, injected, []byte("unapproved instruction state\n"), 0644)
					if runtime.CheckCurrentConfigAssessments(context.Background(), current, []config.MaterializedArtifactAssessment{fact}) == nil {
						t.Fatal("instruction physical drift ignored")
					}
					if err := os.Remove(injected); err != nil {
						t.Fatal(err)
					}
					prepared = current
				}
				var alreadyDesired afterOwnerFileIdentity
				executor.beforeStructured = func() {
					switch scenario {
					case "owned":
						testkit.WriteFile(t, destination, foreign, 0644)
					case "mode":
						if err := os.Chmod(destination, 0600); err != nil {
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
					case "desired":
						value := []byte(`{"managed":"desired","runtime":"kept"}`)
						if format == "toml" {
							value = []byte("managed = 'desired'\nruntime = 'kept'\n")
						}
						testkit.WriteFile(t, destination, value, 0644)
						uid := os.Getuid()
						if uid <= 0 {
							uid = 1000
						}
						if err := os.Chown(destination, uid, uid); err != nil {
							t.Fatal(err)
						}
						alreadyDesired = afterOwnerFileStat(t, destination)
					case "unmanaged":
						value := bytes.ReplaceAll(original, []byte("kept"), []byte("runtime changed"))
						testkit.WriteFile(t, destination, value, 0644)
					}
				}
				applyErr := runtime.ApplyPreparedMaterializedConfigs(context.Background(), prepared)
				negative := scenario == "owned" || scenario == "mode" || scenario == "path"
				if negative {
					if applyErr == nil {
						t.Fatal("structured mutation overwrote unapproved state")
					}
					current, err := os.ReadFile(destination)
					wanted := original
					if scenario == "owned" {
						wanted = foreign
					}
					if err != nil || !bytes.Equal(current, wanted) {
						t.Fatal("structured refusal changed document", err)
					}
					if _, err := os.Stat(filepath.Join(root, "writes")); !os.IsNotExist(err) {
						t.Fatal("rejected mutation recorded destination write")
					}
					return
				}
				if applyErr != nil {
					t.Fatal(applyErr)
				}
				current, err := os.ReadFile(destination)
				if err != nil || !bytes.Contains(current, []byte("desired")) {
					t.Fatal("approved projection did not converge", err)
				}
				runtimeValue := []byte("kept")
				if scenario == "unmanaged" {
					runtimeValue = []byte("runtime changed")
				}
				if !bytes.Contains(current, runtimeValue) {
					t.Fatal("unmanaged field lost")
				}
				if scenario == "desired" && alreadyDesired != afterOwnerFileStat(t, destination) {
					t.Fatal("already desired structured destination rewritten")
				}
				before := afterOwnerFileStat(t, destination)
				if err := runtime.ApplyPreparedMaterializedConfigs(context.Background(), prepared); err != nil {
					t.Fatal("original guard did not resume completed structured file", err)
				}
				if before != afterOwnerFileStat(t, destination) {
					t.Fatal("completed structured file rewritten")
				}

			})
		}
	}
}
