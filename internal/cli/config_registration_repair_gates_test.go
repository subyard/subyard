package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/ownerinventory"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestConfigRegistrationRepairDoesNotRecoverUnrelatedOwnerState(t *testing.T) {
	for _, recovery := range []string{"HostID rename", "owner inventory"} {
		for _, mode := range []string{"check", "decline"} {
			t.Run(recovery+"/"+mode, func(t *testing.T) {
				root, home, configHome, environment := configCommandFixture(t)
				writeConfigCommandFile(t, filepath.Join(configHome, "yards/named/config.env"), "# nested\n")
				writeConfigCommandFile(t, filepath.Join(configHome, "yards/named.env"), "# flat\n")
				if recovery == "HostID rename" {
					// This valid, unfinished rename would be consumed by unrelated recovery.
					writeConfigCommandFile(t, configsync.HostIDPath(configHome), "owner-a\n")
					payload := fmt.Sprintf(`{"schemaVersion":1,"oldHostId":"owner-a","newHostId":"owner-b","hostIdDigest":"%x"}`, sha256.Sum256([]byte("owner-a\n")))
					writeConfigCommandFile(t, configsync.HostIDRenameTransactionPath(configHome), payload)
				} else {
					writeConfigCommandFile(t, filepath.Join(home, ".subyard/owner-inventory/registration.json"), "{invalid-owner-recovery\n")
				}
				before := nativeTreeSnapshot(t, home)
				arguments := []string{"config", "repair-registration", "named"}
				if mode == "check" {
					arguments = append(arguments, "--check")
				}
				prompt := &testkit.Prompt{Answers: []bool{false}}
				var stdout, stderr bytes.Buffer
				program, err := New(Options{
					RepositoryRoot: root, Program: "yard", Arguments: arguments,
					Environment: environment, WorkingDir: root, Prompt: prompt,
					Stdout: &stdout, Stderr: &stderr,
				})
				if err != nil {
					t.Fatal(err)
				}
				wantCode, wantPrompts := 0, 0
				if mode == "decline" {
					wantCode, wantPrompts = 1, 1
				}
				if code := program.Run(context.Background()); code != wantCode || len(prompt.Requests) != wantPrompts {
					t.Fatalf("repair failed before its own confirmation: code=%d prompts=%d stdout=%s stderr=%s", code, len(prompt.Requests), stdout.String(), stderr.String())
				}
				if after := nativeTreeSnapshot(t, home); !reflect.DeepEqual(before, after) {
					t.Fatalf("unapproved repair changed owner state:\nbefore=%v\nafter=%v", before, after)
				}
			})
		}
	}
}

func TestConfigRegistrationRepairBlocksUnfinishedReleaseBeforePrompt(t *testing.T) {
	for _, version := range []string{"v1", "v2"} {
		t.Run(version, func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			addRegistrationRepairFixtureCommand(t, root)
			configHome := environmentValue(environment, "SUBYARD_CONFIG_HOME")
			runtimeRoot := filepath.Join(root, "runtime")
			environment = append(environment, "YARD_RUNTIME_ROOT="+runtimeRoot)
			if version == "v1" {
				installUnfinishedMutationGateFixture(t, root, environment, runtimeRoot)
			} else {
				installUnfinishedV2MutationGateFixture(t, root, environment, runtimeRoot)
			}
			writeConfigCommandFile(t, filepath.Join(configHome, "yards/named/config.env"), "# nested\n")
			writeConfigCommandFile(t, filepath.Join(configHome, "yards/named.env"), "# flat\n")
			before := nativeTreeSnapshot(t, root)
			prompt := &testkit.Prompt{Answers: []bool{true}}
			var stdout, stderr bytes.Buffer
			program, err := New(Options{
				RepositoryRoot: root, Program: "yard", Arguments: []string{"config", "repair-registration", "named"},
				Environment: environment, WorkingDir: root, Prompt: prompt,
				Stdout: &stdout, Stderr: &stderr,
			})
			if err != nil {
				t.Fatal(err)
			}
			if code := program.Run(context.Background()); code != 1 || len(prompt.Requests) != 0 || !strings.Contains(stderr.String(), "release transition") {
				t.Fatalf("unfinished transition was not blocked before confirmation: code=%d prompts=%d stderr=%s", code, len(prompt.Requests), stderr.String())
			}
			if after := nativeTreeSnapshot(t, root); !reflect.DeepEqual(before, after) {
				t.Fatalf("blocked repair changed protected state:\nbefore=%v\nafter=%v", before, after)
			}
		})
	}
}

func TestConfigRegistrationRepairCanonicalRemoteRefusalPreservesOwnerRoutes(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	addRegistrationRepairFixtureCommand(t, root)
	environment = append(environment, "SUBYARD_HOST_ID=owner-a")
	configHome := environmentValue(environment, "SUBYARD_CONFIG_HOME")
	writeConfigCommandFile(t, filepath.Join(configHome, "yards/named/config.env"), "# nested\n")
	writeConfigCommandFile(t, filepath.Join(configHome, "yards/named.env"), "# flat\n")
	ownerRoot := filepath.Join(environmentValue(environment, "SUBYARD_HOME"), "owner-inventory")
	if err := (ownerinventory.Connections{Root: ownerRoot}).Write(ownerinventory.Connection{
		HostID: "remote-owner", Destination: "dev@remote.example",
		Yards: map[string]ownerinventory.YardRoute{"default": {SSHHost: "yard-remote"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := (ownerinventory.Cache{Root: ownerRoot}).Write(ownerinventory.Snapshot{
		FetchedAt: time.Now().UTC(), Inventory: inventoryResult("remote-owner", "default", "").inventory,
	}); err != nil {
		t.Fatal(err)
	}
	writeConfigCommandFile(t, filepath.Join(ownerRoot, "registration.json"), "{invalid-owner-recovery\n")
	before := nativeTreeSnapshot(t, root)
	prompt := &testkit.Prompt{Answers: []bool{true}}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"-Y", "remote-owner/default", "config", "repair-registration", "named", "--check"},
		Environment: environment, WorkingDir: root, Prompt: prompt,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 2 || len(prompt.Requests) != 0 || !strings.Contains(stderr.String(), "registration owner") {
		t.Fatalf("remote repair did not reach safe local-only refusal: code=%d prompts=%d stderr=%s", code, len(prompt.Requests), stderr.String())
	}
	if after := nativeTreeSnapshot(t, root); !reflect.DeepEqual(before, after) {
		t.Fatalf("remote repair changed owner state:\nbefore=%v\nafter=%v", before, after)
	}
}

func addRegistrationRepairFixtureCommand(t *testing.T, root string) {
	t.Helper()
	path := filepath.Join(root, "config", "commands.registry")
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	payload = append(payload, []byte("config||@config||local|mutate|dynamic|public|lifecycle|config|config <command>|config|--check --yes --help|repair-registration\n")...)
	writeCLIFile(t, path, string(payload), 0o600)
}
