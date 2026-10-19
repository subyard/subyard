package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releaseowner"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/testyardmigration"
)

func TestOwnerInputViewKeepsOriginalInputsAcrossOwningPublication(t *testing.T) {
	for _, drift := range []string{"none", "new-yard", "ordinary-inode", "member-inode"} {
		t.Run(drift, func(t *testing.T) {
			ctx := context.Background()
			c, home := nativeActivationSourceFixture(t)
			c.cli.baseEnv["SUBYARD_CONFIG_HOME"] = home
			c.cli.env["SUBYARD_CONFIG_HOME"] = home
			profile, err := os.ReadFile(filepath.Join(repositoryRoot(t), "config/yards/profiles/test-vms.env"))
			if err != nil {
				t.Fatal(err)
			}
			profilePath := filepath.Join(c.context.RepositoryRoot, "config/yards/profiles/test-vms.env")
			if err := os.MkdirAll(filepath.Dir(profilePath), 0700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, profilePath, profile, 0600)
			legacy := filepath.Join(home, "yards/e2e-yard/config.env")
			if err := os.MkdirAll(filepath.Dir(legacy), 0700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, legacy, []byte("YARD_TEMPLATE=test-vms\nNESTED_E2E_VMS=0\n"), 0600)
			projects := filepath.Join(c.context.RepositoryRoot, "owner-projects.json")
			testkit.WriteFile(t, projects, []byte("[{\"name\":\"subyard-e2e-yard\"}]"), 0600)
			incus := filepath.Join(c.context.RepositoryRoot, "owner-view-incus")
			testkit.WriteFile(t, incus, []byte("#!/bin/sh\nset -eu\ncase \"$*\" in\n 'project list --format=json') cat \"$OWNER_PROJECTS\";;\n 'project get subyard-e2e-yard features.images'|'project get subyard-test-yard features.images') echo false;;\n 'list yard-e2e-yard --project subyard-e2e-yard --format=json') echo '[{\"name\":\"yard-e2e-yard\",\"status\":\"STOPPED\"}]';;\n 'config get yard-e2e-yard user.subyard.desired_power --project subyard-e2e-yard') echo running;;\n *) exit 2;;\nesac\n"), 0700)
			options := testyardmigration.Options{ConfigHome: home, DataHome: c.context.DataHome, RepositoryRoot: c.context.RepositoryRoot, Incus: incus, Executable: incus, RecoveryToken: strings.Repeat("2", 32), Environment: append(os.Environ(), "OWNER_PROJECTS="+projects), Stderr: io.Discard}
			before, err := testyardmigration.Prepare(ctx, options)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := c.CaptureInventory(ctx); err != nil {
				t.Fatal(err)
			}
			operation := c.ownerChildOperation()
			loaded, err := c.load(testyardmigration.LegacyYard)
			if err != nil {
				t.Fatal(err)
			}
			originalPeers, err := operation.powerYardContexts(loaded)
			if err != nil {
				t.Fatal(err)
			}
			baseline, err := operation.captureOwnerInputs(loaded, "", originalPeers)
			if err != nil {
				t.Fatal(err)
			}
			future, err := c.captureOwnerChildren(ctx, releaseowner.OwnerBefore{State: string(testyardmigration.StateLegacyDirectoryAdoptCurrent), Registration: before.RegistrationDigest, Overrides: before.OverridesDigest, Controller: before.ControllerDigest, SharedImages: before.SharedImages}, false)
			if err != nil {
				t.Fatal(err)
			}
			source, err := testyardmigration.CaptureOwnerChildSource(options, before, c.root)
			if err != nil {
				t.Fatal(err)
			}
			final := future.checks[testyardmigration.CurrentYard].Yards
			middle := slices.Clone(originalPeers)
			for _, peer := range final {
				if !slices.ContainsFunc(middle, func(value domain.Context) bool { return value.YardName == peer.YardName }) {
					middle = append(middle, peer)
				}
			}
			slices.SortFunc(middle, func(a, b domain.Context) int { return strings.Compare(a.YardName, b.YardName) })
			view := &preparedOwnerChildInputView{root: c.root, source: source, options: options, current: future.checks[testyardmigration.CurrentYard], peers: [3][]domain.Context{originalPeers, middle, final}, names: [3][]string{{"default", "e2e-yard"}, {"default", "e2e-yard", "test-yard"}, {"default", "test-yard"}}}
			baseline.ownerSource = view
			if err := baseline.check(ctx, operation); err != nil {
				t.Fatal("original view refused", err)
			}
			options.RestoreLegacyStop = func(context.Context) error { return nil }
			options.EnsureImageProject = func(context.Context, string, bool) error { return nil }
			options.RunYard = func(_ context.Context, yard string, output io.Writer, args ...string) error {
				if slices.Equal(args, []string{"test-vms", "status"}) {
					_, err := io.WriteString(output, `{"pool":{"slots":[]}}`)
					return err
				}
				if yard == testyardmigration.LegacyYard && slices.Equal(args, []string{"teardown", "--yes"}) {
					return os.WriteFile(projects, []byte("[]"), 0600)
				}
				if yard == testyardmigration.CurrentYard && slices.Equal(args, []string{"init", "--yes"}) {
					return os.WriteFile(projects, []byte("[{\"name\":\"subyard-test-yard\"}]"), 0600)
				}
				return nil
			}
			if err := testyardmigration.Commit(ctx, options, before); err != nil {
				t.Fatal(err)
			}
			if err := baseline.check(ctx, operation); err != nil {
				t.Fatal("owning archived source refused", err)
			}
			switch drift {
			case "new-yard":
				path := filepath.Join(home, "yards/new-yard/config.env")
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, path, []byte("YARD_TEMPLATE=test-vms\nNESTED_E2E_VMS=0\n"), 0600)
			case "ordinary-inode":
				testkit.WriteFile(t, profilePath+".new", profile, 0600)
				if err := os.Rename(profilePath+".new", profilePath); err != nil {
					t.Fatal(err)
				}
			case "member-inode":
				archive := filepath.Join(home, "yards/.owner-migration-registration-archive."+options.RecoveryToken, "config.env")
				testkit.WriteFile(t, archive+".new", source.Content, 0600)
				if err := os.Rename(archive+".new", archive); err != nil {
					t.Fatal(err)
				}
			}
			err = baseline.check(ctx, operation)
			if drift == "none" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("changed original owner scope admitted")
			} else if !errors.Is(err, domain.ErrPlanStale) && drift != "member-inode" {
				t.Fatal("unexpected refusal category", err)
			}
		})
	}
}
