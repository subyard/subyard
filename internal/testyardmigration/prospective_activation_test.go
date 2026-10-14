package testyardmigration

import (
	"context"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
	"os"
	"path/filepath"
	"testing"
)

func TestPreparedActivationProjectionMatchesCommittedRegistrationAndOverrides(t *testing.T) {
	for _, overrides := range []bool{false, true} {
		t.Run(map[bool]string{false: "directory", true: "directory-overrides"}[overrides], func(t *testing.T) {
			options, before := ownerFaultFixture(t, false, overrides)
			read := func(path string) (config.PersistentFileSnapshot, error) {
				return config.ReadPersistentFileSnapshot(options.ConfigHome, path)
			}
			projection, err := PrepareActivationProjection(options, before, read)
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(options.ConfigHome, "yards", CurrentYard, "config.env")
			if projection.Sources[target] == "" {
				t.Fatal("ordinary owner commit mapping missing")
			}
			expected := map[string][]byte{}
			for target, source := range projection.Sources {
				snapshot, err := read(source)
				if err != nil || !snapshot.Exists {
					t.Fatal(err)
				}
				expected[target] = snapshot.Content
			}
			if _, err := os.Stat(target); !os.IsNotExist(err) {
				t.Fatal("projection published canonical registration", err)
			}
			for _, nested := range []bool{false, true} {
				broker, route, err := PrepareProjectedActivationApplicability(context.Background(), options, before, config.Loaded{Context: domain.Context{YardName: CurrentYard, NestedE2EVMs: nested}})
				want := BrokerRuntimeInactive
				if nested {
					want = BrokerRuntimeActive
				}
				if err != nil || broker != want || route != StateCurrent {
					t.Fatalf("nested=%t broker=%s route=%s err=%v", nested, broker, route, err)
				}
			}
			if err := Commit(context.Background(), options, before); err != nil {
				t.Fatal(err)
			}
			for target, bytes := range expected {
				snapshot, err := read(target)
				if err != nil || !snapshot.Exists || string(snapshot.Content) != string(bytes) {
					t.Fatal("projected bytes differ from owner commit", target, err)
				}
			}
			if _, err := PrepareActivationProjection(options, before, read); err != nil {
				t.Fatal("restart projection lost original bound facts", err)
			}
			testkit.WriteFile(t, target, []byte("YARD_TEMPLATE=test-vms\nSSH_PORT=2240\n"), 0600)
			if _, err := PrepareActivationProjection(options, before, read); err == nil {
				t.Fatal("changed registration retained prepared projection")
			}
		})
	}
}

func TestPreparedActivationProjectionUsesExactFlatPaths(t *testing.T) {
	options, before := ownerFaultFixture(t, false, false)
	directory := filepath.Join(options.ConfigHome, "yards", LegacyYard)
	flat := filepath.Join(options.ConfigHome, "yards", LegacyYard+".env")
	if err := os.Rename(filepath.Join(directory, "config.env"), flat); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(directory); err != nil {
		t.Fatal(err)
	}
	before.State = StateLegacyFlat
	read := func(path string) (config.PersistentFileSnapshot, error) {
		return config.ReadPersistentFileSnapshot(options.ConfigHome, path)
	}
	projection, err := PrepareActivationProjection(options, before, read)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(options.ConfigHome, "yards", CurrentYard+".env")
	if projection.Sources[target] != flat {
		t.Fatal("flat registration was projected as a directory", projection)
	}
	before.State = StateLegacyFlatAdoptCurrent
	projection, err = PrepareActivationProjection(options, before, read)
	if err != nil || projection.Sources[target] != flat {
		t.Fatal("adoption lost its exact source mapping", err)
	}
}
