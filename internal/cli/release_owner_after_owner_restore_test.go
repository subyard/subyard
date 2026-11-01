package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releaseowner"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/testyardmigration"
	"github.com/Subyard/Subyard/internal/uninstall"
	"golang.org/x/sys/unix"
)

// Socket framing orders the real requests; this mutex additionally makes the
// subsequent direct component phase visible to Go's race detector.
type framedCLIComponents struct {
	*releaseOwnerComponents
	mu sync.Mutex
}

func (c *framedCLIComponents) CaptureInventory(ctx context.Context) ([]string, []string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.releaseOwnerComponents.CaptureInventory(ctx)
}
func (c *framedCLIComponents) BindInstalledRuntimeRecipeForTarget(ctx context.Context, hash string, raw []byte, target releaseowner.Target) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.releaseOwnerComponents.BindInstalledRuntimeRecipeForTarget(ctx, hash, raw, target)
}
func (c *framedCLIComponents) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.releaseOwnerComponents.Close()
}

func (c *framedCLIComponents) ProjectActivation(ctx context.Context, before releaseowner.OwnerBefore) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.releaseOwnerComponents.ProjectActivation(ctx, before)
}

type originalCLIRecipe struct {
	Target releaseowner.Target
	Raw    []byte
}

func TestAfterOwnerOriginalMaterializedSourcesRebindAcrossYards(t *testing.T) {
	t.Run("projected-profile-runtime", func(t *testing.T) {
		if os.Getenv("SUBYARD_NATIVE_OWNER_GRAPH") != "1" {
			t.Skip("original owner graph capture requires matching ordinary engine")
		}
		ctx := t.Context()
		c, _, _ := ownerOriginalGraphFixture(t)
		t.Setenv("HOME", c.cli.env["HOME"])
		t.Setenv("SUBYARD_OPERATOR_HOME", c.cli.env["HOME"])
		for _, name := range []string{"OWNER_FIXTURE_CONTRACTS", "OWNER_MOCK_CALLS"} {
			t.Setenv(name, c.cli.env[name])
		}
		c.cli.options.Environment = append(os.Environ(), "SUBYARD_HOME="+c.context.DataHome, "SUBYARD_CONFIG_HOME="+c.context.ConfigHome)
		program, err := New(c.cli.options)
		if err != nil {
			t.Fatal(err)
		}
		c.cli, c.catalog = program.releaseActivationOperation(), &program.catalog
		if _, _, err := c.CaptureInventory(ctx); err != nil {
			t.Fatal(err)
		}
		before, err := c.CaptureOwner(ctx, nil)
		if err != nil {
			t.Fatalf("owner capture type=%T", err)
		}
		if _, err = c.ProjectActivation(ctx, before); err != nil {
			t.Fatalf("owner projection type=%T", err)
		}
		values, err := c.CaptureActivation(ctx, c.cli.profileRuntimeIDs()[0], false)
		if err != nil {
			t.Fatalf("profile capture type=%T", err)
		}
		originals := []originalCLIRecipe{{Target: releaseowner.Target{Owner: "owner-registration", PreparedRecipe: before.PreparedRecipe}, Raw: bytes.Clone(c.ownerChildren.raw)}}
		for _, target := range values {
			if target.AfterOwner == "" {
				continue
			}
			raw, err := c.InstalledRuntimeRecipe(target.PreparedRecipe)
			if err != nil {
				t.Fatal(err)
			}
			originals = append(originals, originalCLIRecipe{target, raw})
		}
		if len(originals) != 2 {
			t.Fatalf("conditional count=%d", len(originals)-1)
		}
		fresh, _ := nativeActivationSourceFixture(t)
		fresh.cli, fresh.context, fresh.root = c.cli, c.context, c.root
		if _, _, err := fresh.CaptureInventory(ctx); err != nil {
			t.Fatal(err)
		}
		if err := fresh.BindInstalledRuntimeRecipe(ctx, before.PreparedRecipe, originals[0].Raw); err != nil {
			t.Fatalf("owner source bind type=%T", err)
		}
		if _, err := fresh.ProjectActivation(ctx, before); err != nil {
			t.Fatalf("fresh projection type=%T", err)
		}
		original := originals[1]
		if err := fresh.BindInstalledRuntimeRecipeForTarget(ctx, original.Target.PreparedRecipe, original.Raw, original.Target); err != nil {
			t.Fatalf("direct original profile bind stale=%t type=%T", errors.Is(err, domain.ErrPlanStale), err)
		}
		before.PreparedRecipe, before.Capture = "", nil
		framedOriginalCLIRecipes(t, c.context, c.cli.options.Incus.(*ownerGraphReader).Incus, c.cli.options.Executor, originals, before)
	})
	for _, scenario := range []struct {
		name             string
		projected, hooks bool
	}{
		{"ordinary-config", false, false}, {"ordinary-hooks", false, true},
		{"projected-config", true, false}, {"projected-hooks", true, true},
	} {
		t.Run(scenario.name, func(t *testing.T) { afterOwnerOriginalMaterializedYards(t, scenario.projected, scenario.hooks) })
	}
}

func afterOwnerOriginalMaterializedYards(t *testing.T, projected, hooks bool) {
	ctx := context.Background()
	c, home := nativeActivationSourceFixture(t)
	nativeMaterializedSourceFiles(t, c.context.RepositoryRoot)
	settings := []byte("CODING_TOOL_INTEGRATIONS=sample\nAGENT_sample_COMMAND=sample\nAGENT_sample_RULES=''\nAGENT_sample_CONFIG_DEST=.sample/config.conf\n")
	testkit.WriteFile(t, filepath.Join(c.context.RepositoryRoot, "config/agents.env"), settings, 0600)
	for _, yard := range []string{"default", "zeta"} {
		if err := os.MkdirAll(filepath.Join(home, "yards", yard), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(home, "custom"), 0700); err != nil {
			t.Fatal(err)
		}
		settings := "AGENT_sample_CONFIG=\"$SUBYARD_CONFIG_HOME/custom/" + yard + ".conf\"\n"
		if hooks {
			for _, kind := range []string{"PROVISION", "CLEANUP"} {
				settings += "AGENT_sample_" + kind + "=\"$SUBYARD_CONFIG_HOME/custom/" + yard + "-" + kind + ".sh\"\n"
				testkit.WriteFile(t, filepath.Join(home, "custom", yard+"-"+kind+".sh"), []byte("#!/bin/sh\nexit 0\n"), 0600)
			}
		}
		testkit.WriteFile(t, filepath.Join(home, "yards", yard, "config.env"), []byte(settings), 0600)
		testkit.WriteFile(t, filepath.Join(home, "custom", yard+".conf"), []byte("approved "+yard+"\n"), 0600)
	}
	incus := c.cli.options.Incus.(*testkit.Incus)
	delete(incus.Instances, "subyard/yard")
	incus.Reconcile = ports.ReconcileState{}
	// The actual framed factory inherits this same explicit caller environment.
	t.Setenv("HOME", filepath.Join(c.context.RepositoryRoot, "home"))
	t.Setenv("SUBYARD_OPERATOR_HOME", filepath.Join(c.context.RepositoryRoot, "home"))
	c.cli.env = environmentMap(os.Environ())
	c.cli.env["SUBYARD_HOME"] = c.context.DataHome
	c.cli.env["SUBYARD_CONFIG_HOME"] = c.context.ConfigHome
	c.cli.baseEnv = environmentMap(environmentList(c.cli.env, nil))
	c.cli.options.Environment = environmentList(c.cli.env, nil)
	if projected {
		profile, err := os.ReadFile(filepath.Join(repositoryRoot(t), "config/yards/profiles/test-vms.env"))
		if err != nil {
			t.Fatal(err)
		}
		for _, dir := range []string{filepath.Join(c.context.RepositoryRoot, "config/yards/profiles"), filepath.Join(home, "yards", testyardmigration.LegacyYard)} {
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
		}
		testkit.WriteFile(t, filepath.Join(c.context.RepositoryRoot, "config/yards/profiles/test-vms.env"), profile, 0600)
		testkit.WriteFile(t, filepath.Join(home, "yards", testyardmigration.LegacyYard, "config.env"), []byte("YARD_TEMPLATE=test-vms\nNESTED_E2E_VMS=0\n"), 0600)
	}
	// Match the fresh native factory after all fixture sources are staged.
	program, err := New(c.cli.options)
	if err != nil {
		t.Fatal(err)
	}
	c.cli = program.releaseActivationOperation()
	c.catalog = &c.cli.catalog
	if _, _, err := c.CaptureInventory(ctx); err != nil {
		t.Fatal(err)
	}
	before := releaseowner.OwnerBefore{}
	if projected {
		owner := newInterruptedRenameOwner(t, c.context.RepositoryRoot, home)
		observed, err := owner.Prepare(ctx, c)
		if err != nil {
			t.Fatal("original owner preparation", err)
		}
		before = releaseowner.OwnerBefore{State: string(observed.State), Registration: string(observed.Registration), Overrides: string(observed.Overrides), Controller: string(observed.Controller), SharedImages: observed.SharedImages}
		if _, err := c.ProjectActivation(ctx, before); err != nil {
			t.Fatal("original projection", err)
		}
	}
	values, err := c.CaptureActivation(ctx, "materialized-config", false)
	if err != nil || len(values) < 2 {
		t.Fatal("original two-yard capture", len(values), err)
	}
	for _, yard := range []string{"default", "zeta"} {
		input := c.inputs["custom/"+yard+".conf"]
		if !input.Exists || input.Identity.Mode&0777 != 0600 {
			t.Fatal("persistent source metadata was not captured", yard)
		}
	}
	absence := false
	for _, input := range c.inputs {
		absence = absence || !input.Exists
	}
	if !absence {
		t.Fatal("source absence candidates were not captured")
	}
	c.ownerChildren = &preparedOwnerChildren{inits: map[string]preparedOwnerInitChild{}}
	originals := []originalCLIRecipe{}
	for _, value := range values {
		if value.Yard != "default" && value.Yard != "zeta" {
			continue
		}
		prepared := c.activation[activationKey(value)]
		files, err := c.prepareAfterOwnerFiles(ctx, prepared)
		if err != nil {
			t.Fatal(err)
		}
		integrations, err := c.prepareAfterOwnerIntegration(ctx, prepared.configTarget, prepared.platform.(reconcileruntime.Runtime), value.Source)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range []releaseActivationPrepared{files, integrations} {
			c.activation[activationKey(p.value)] = p
			originals = append(originals, originalCLIRecipe{p.value, p.nativeInstalled.Recipe()})
		}
	}
	for _, original := range originals {
		t.Run(original.Target.Yard+"/"+original.Target.Class, func(t *testing.T) {
			if _, err := c.read("unrelated-migration.env"); err != nil {
				t.Fatal("unrelated migration read", err)
			}
			if err := c.BindInstalledRuntimeRecipeForTarget(ctx, original.Target.PreparedRecipe, original.Raw, original.Target); err != nil {
				t.Fatalf("cached original source after unrelated read stale=%t", errors.Is(err, domain.ErrPlanStale))
			}
			fresh, _ := nativeActivationSourceFixture(t)
			fresh.cli, fresh.context = c.cli, c.context
			fresh.root = c.root
			if projected {
				if _, _, err := fresh.CaptureInventory(ctx); err != nil {
					t.Fatal(err)
				}
				if _, err := fresh.ProjectActivation(ctx, before); err != nil {
					t.Fatal(err)
				}
			}
			if err := fresh.BindInstalledRuntimeRecipeForTarget(ctx, original.Target.PreparedRecipe, original.Raw, original.Target); err != nil {
				t.Fatal("unchanged original source rebind", err)
			}
			if err := fresh.BindInstalledRuntimeRecipeForTarget(ctx, original.Target.PreparedRecipe, original.Raw, original.Target); err != nil {
				t.Fatal("already-bound unchanged original source", err)
			}
			_, earlier := fresh.inputs["custom/default.conf"]
			_, later := fresh.inputs["custom/zeta.conf"]
			if !earlier || !later {
				t.Fatalf("eager source validation inputs lost: earlier=%v later=%v", earlier, later)
			}
			if !strings.Contains(original.Target.AfterOwner, original.Target.Class) {
				t.Fatal("missing original template")
			}
		})
	}
	if !t.Failed() {
		// The production empty factory receives actual ORIGINAL bind messages first.
		// No physical capture, producer completion or effect grant is involved.
		if projected {
			framedOriginalCLIRecipes(t, c.context, incus, c.cli.options.Executor, originals, before)
		} else {
			framedOriginalCLIRecipes(t, c.context, incus, c.cli.options.Executor, originals)
		}
	}
}

// Use production framing and the actual empty factory. Only the fixture's
// physical Incus/executor boundary is replaced. No producer facts are invented:
// these messages bind immutable ORIGINAL templates, which carry no effect grant.
func framedOriginalCLIRecipes(t *testing.T, value releaseowner.Context, incus *testkit.Incus, executor ports.InstanceExecutor, originals []originalCLIRecipe, projection ...releaseowner.OwnerBefore) *releaseOwnerComponents {
	t.Helper()
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	peer, native := os.NewFile(uintptr(pair[0]), "original peer"), os.NewFile(uintptr(pair[1]), "original native")
	root, err := os.Open(value.ConfigHome)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	repository, err := os.Open(value.RepositoryRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	defer uninstall.UseArtifactGuard(repositoryRoot(t))
	value.Root, err = releaseowner.IdentityOf(root)
	if err != nil {
		t.Fatal(err)
	}
	value.Repository, err = releaseowner.IdentityOf(repository)
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan *framedCLIComponents, 1)
	done := make(chan error, 1)
	go func() {
		defer native.Close()
		done <- releaseowner.Serve(context.Background(), native, native, func(ctx context.Context, c releaseowner.Context, fd *os.File) (releaseowner.Components, error) {
			component, err := newReleaseOwnerComponents(ctx, c, fd)
			if err != nil {
				return nil, err
			}
			actual := component.(*releaseOwnerComponents)
			actual.cli.options.Incus = incus
			actual.cli.options.Executor = executor
			wrapped := &framedCLIComponents{releaseOwnerComponents: actual}
			ready <- wrapped
			return wrapped, nil
		})
	}()
	t.Cleanup(func() {
		peer.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Error("framed original drain", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("framed original helper did not drain")
		}
	})
	var sequence uint64
	exchange := func(request releaseowner.Request, files ...*os.File) releaseowner.Reply {
		t.Helper()
		raw, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		sequence++
		header := make([]byte, 52)
		copy(header, "SYC1")
		header[4] = 1
		header[5] = 15
		binary.BigEndian.PutUint64(header[8:16], sequence)
		sum := sha256.Sum256(raw)
		copy(header[16:48], sum[:])
		binary.BigEndian.PutUint32(header[48:], uint32(len(raw)))
		if len(files) > 0 {
			fds := make([]int, len(files))
			for i, file := range files {
				fds[i] = int(file.Fd())
			}
			if err := unix.Sendmsg(int(peer.Fd()), header[:1], unix.UnixRights(fds...), nil, unix.MSG_NOSIGNAL); err != nil {
				t.Fatal(err)
			}
			header = header[1:]
		}
		if _, err := peer.Write(append(header, raw...)); err != nil {
			t.Fatal(err)
		}
		reply := make([]byte, 52)
		if _, err := io.ReadFull(peer, reply); err != nil {
			t.Fatal("original bind frame", err)
		}
		if !bytes.Equal(reply[:8], []byte{'S', 'Y', 'C', '1', 1, 15, 0, 0}) || binary.BigEndian.Uint64(reply[8:16]) != sequence || !bytes.Equal(reply[16:48], sum[:]) {
			t.Fatal("original bind correlation")
		}
		size := binary.BigEndian.Uint32(reply[48:])
		if size > releaseowner.MaxReply {
			t.Fatal("original bind reply bound")
		}
		body := make([]byte, size)
		if _, err := io.ReadFull(peer, body); err != nil {
			t.Fatal(err)
		}
		var result releaseowner.Reply
		if json.Unmarshal(body, &result) != nil || result.Code != "" {
			t.Fatal("original bind refused", string(body))
		}
		return result
	}
	exchange(releaseowner.Request{Op: "initialize", Context: &value}, root, repository)
	wrapped := <-ready
	c := wrapped.releaseOwnerComponents
	wrapped.mu.Lock()
	empty := len(c.activation) == 0
	wrapped.mu.Unlock()
	if !empty {
		t.Fatal("factory was not empty")
	}
	// Inventory is the existing session bootstrap; no activation/physical capture
	// precedes the immutable original messages.
	exchange(releaseowner.Request{Op: "capture-inventory", Session: value.Session})
	for _, before := range projection {
		exchange(releaseowner.Request{Op: "project-activation", Session: value.Session, Before: &before})
	}
	for _, original := range originals {

		for offset := 0; offset < len(original.Raw); {
			end := min(offset+(512<<10), len(original.Raw))
			request := releaseowner.Request{Op: "bind-installed-recipe", Session: value.Session, RecipeHash: original.Target.PreparedRecipe, Target: &original.Target, Offset: uint64(offset), Total: uint64(len(original.Raw)), Content: base64.StdEncoding.EncodeToString(original.Raw[offset:end])}
			if original.Target.Owner == "owner-registration" {
				request.Target = nil
			}
			reply := exchange(request)
			if reply.InstalledChunk == nil || reply.InstalledChunk.Complete != (end == len(original.Raw)) {
				t.Fatal("original transfer incomplete")
			}
			offset = end
		}
	}
	wrapped.mu.Lock()
	t.Cleanup(wrapped.mu.Unlock)
	return c
}
