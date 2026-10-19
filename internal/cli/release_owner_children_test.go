package cli

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releaseowner"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/testyardmigration"
)

func TestPreparedOwnerProjectionAllowsOnlyApprovedPublication(t *testing.T) {
	for _, change := range []string{"approved", "wrong-bytes", "wrong-path", "root", "ordinary-inode"} {
		t.Run(change, func(t *testing.T) {
			parent := testkit.TempDir(t)
			root := filepath.Join(parent, "scope")
			if err := os.Mkdir(root, 0700); err != nil {
				t.Fatal(err)
			}
			projected, ordinary := filepath.Join(root, "canonical.env"), filepath.Join(root, "ordinary.env")
			testkit.WriteFile(t, ordinary, []byte("ordinary\n"), 0600)
			info, err := os.Lstat(ordinary)
			if err != nil {
				t.Fatal(err)
			}
			fact, err := ownerInputIdentity(ordinary, info)
			if err != nil {
				t.Fatal(err)
			}
			before := map[string]ownerInputFile{projected: {}, ordinary: fact}
			fd, err := os.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer fd.Close()
			approved := []byte("YARD_TEMPLATE=test-vms\n")
			files := &config.ProspectiveFiles{Root: root, ReadPreparedInput: func(path string) (config.PersistentFileSnapshot, bool, error) {
				if path == projected {
					return config.PersistentFileSnapshot{Exists: true, Content: approved}, true, nil
				}
				return config.PersistentFileSnapshot{}, false, nil
			}}
			baseline := &ownerInputBaseline{initial: config.Loaded{Context: domain.Context{Paths: domain.RuntimePaths{ConfigHome: root}}}, files: before}
			p, err := prepareOwnerInputProjection(baseline, files, fd)
			if err != nil {
				t.Fatal(err)
			}
			if len(p.approved) != 1 {
				t.Fatal("ordinary source was promoted")
			}
			if err := p.validate(maps.Clone(before), before); err != nil {
				t.Fatal("readonly preimage refused", err)
			}
			if err := p.requirePublished(); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatal("future child admitted before publication", err)
			}
			if change == "root" {
				if err := os.Rename(root, root+"-old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(root, 0700); err != nil {
					t.Fatal(err)
				}
			}
			path, payload := projected, approved
			if change == "wrong-path" {
				path = filepath.Join(root, "foreign.env")
			}
			if change == "wrong-bytes" {
				payload = []byte("foreign\n")
			}
			testkit.WriteFile(t, path, payload, 0600)
			if change == "ordinary-inode" {
				testkit.WriteFile(t, ordinary+".new", []byte("ordinary\n"), 0600)
				if err := os.Rename(ordinary+".new", ordinary); err != nil {
					t.Fatal(err)
				}
			}
			current := map[string]ownerInputFile{}
			for path := range before {
				info, err := os.Lstat(path)
				if os.IsNotExist(err) {
					current[path] = ownerInputFile{}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				fact, err := ownerInputIdentity(path, info)
				if err != nil {
					t.Fatal(err)
				}
				current[path] = fact
			}
			err = p.validate(current, before)
			if change == "approved" {
				if err != nil || !reflect.DeepEqual(current, before) || p.requirePublished() != nil {
					t.Fatal("approved original publication refused", err)
				}
			} else if change == "ordinary-inode" {
				if err != nil || reflect.DeepEqual(current, before) {
					t.Fatal("ordinary replacement was normalized", err)
				}
			} else if err == nil && p.requirePublished() == nil {
				t.Fatal("unapproved publication admitted")
			}
		})
	}
}

func TestOriginalOwnerCheckRecipeSurvivesApprovedPublication(t *testing.T) {
	c, home := nativeActivationSourceFixture(t)
	c.cli.baseEnv["SUBYARD_CONFIG_HOME"] = home
	c.cli.env["SUBYARD_CONFIG_HOME"] = home
	root := c.context.RepositoryRoot
	profile, err := os.ReadFile(filepath.Join(repositoryRoot(t), "config/yards/profiles/test-vms.env"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "config/yards/profiles"), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "config/yards/profiles/test-vms.env"), profile, 0600)
	legacy := filepath.Join(home, "yards/e2e-yard/config.env")
	if err := os.MkdirAll(filepath.Dir(legacy), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, legacy, []byte("YARD_TEMPLATE=test-vms\nNESTED_E2E_VMS=0\n"), 0600)
	if _, _, err := c.CaptureInventory(context.Background()); err != nil {
		t.Fatal(err)
	}
	owner := newInterruptedRenameOwner(t, root, home)
	observed, err := owner.Prepare(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	before := releaseowner.OwnerBefore{State: string(testyardmigration.StateLegacyDirectoryAdoptCurrent), Registration: string(observed.Registration), Overrides: string(observed.Overrides), Controller: string(observed.Controller), SharedImages: observed.SharedImages}
	original, err := c.prepareOwnerChildren(context.Background(), before)
	if err != nil {
		t.Fatal(err)
	}
	c.ownerChildren = original
	raw := bytes.Clone(original.raw)
	hash := original.binding()
	if err := c.BindInstalledRuntimeRecipe(context.Background(), hash, raw); err != nil {
		t.Fatal(err)
	}
	if err := original.baselines[testyardmigration.CurrentYard].projected.requirePublished(); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("unpublished future child admitted", err)
	}
	// This publication uses the original owning migration, not a new plan.
	if err := owner.Commit(context.Background(), observed); err != nil {
		t.Fatal(err)
	}
	if err := c.BindInstalledRuntimeRecipe(context.Background(), hash, raw); err != nil {
		t.Fatal("original child recipe did not rebind after authorized rename", err)
	}
	if err := c.ownerChildren.baselines[testyardmigration.CurrentYard].check(context.Background(), c.ownerChildren.operation); err != nil {

		t.Fatal("original check inputs did not survive publication", err)
	}
	if err := c.ownerChildren.baselines[testyardmigration.CurrentYard].projected.requirePublished(); err != nil {
		t.Fatal(err)
	}
	// A fresh retained helper has no in-memory original owner handle. It
	// reconstructs from the original approved bytes, not a new owner Prepare.
	c.ownerChildren = nil
	if err := c.BindInstalledRuntimeRecipe(context.Background(), hash, raw); err != nil {
		t.Fatal("fresh original owner reconstruction", err)
	}
	// An ordinary identical-byte source replacement retains no projected privilege.
	source := filepath.Join(root, "config/yards/profiles/test-vms.env")
	testkit.WriteFile(t, source+".new", profile, 0600)
	if err := os.Rename(source+".new", source); err != nil {
		t.Fatal(err)
	}
	if err := c.BindInstalledRuntimeRecipe(context.Background(), hash, raw); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("ordinary source inode replacement admitted", err)
	}
}
