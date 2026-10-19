package reconcileruntime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func ownerInstanceProjectionFixture() (Runtime, *testkit.Incus) {
	i := ports.InstanceInfo{Name: "yard-e2e-yard", Project: "subyard-e2e-yard", Config: map[string]string{"volatile.uuid": "original-uuid", "external": "source-\xff"}, LocalConfig: map[string]string{}, Devices: map[string]map[string]string{}}
	fake := &testkit.Incus{Instances: map[string]ports.InstanceInfo{"subyard-e2e-yard/yard-e2e-yard": i}, Reconcile: ports.ReconcileState{ProjectFound: true, InstanceFound: true, ProfileFound: true, VolumeFound: true, HostPoolFound: true, HostNetworkFound: true, ProjectConfig: map[string]string{"features.images": "false"}, ProfileConfig: map[string]string{}, Instance: i}, TeardownResources: []ports.TeardownResource{{Kind: "project", Name: i.Project, Binding: strings.Repeat("a", 64)}, {Kind: "instance", Name: i.Name, Binding: strings.Repeat("b", 64)}, {Kind: "volume", Pool: "default", Name: "yard-srv", Binding: strings.Repeat("c", 64)}}}
	runtime := Runtime{Yard: domain.Context{YardName: "e2e-yard", IncusProject: i.Project, YardInstanceName: i.Name, Paths: domain.RuntimePaths{DataHome: "/owner-\xff/data", ConfigHome: "/owner/config"}}, Incus: fake, Environment: []string{"PATH=/usr/bin:/bin"}}
	return runtime, fake
}

type ownerCreatedProjectReader struct {
	*testkit.Incus
	ref    ports.NativeOwnerCreateRef
	checks int
}

func (r *ownerCreatedProjectReader) CheckOwnerCreatedImageProject(_ context.Context, p ports.PreparedImageProject, created ports.NativeOwnerCreated) error {
	r.checks++
	if created.Ref != r.ref || created.Intent.Project != p.Project {
		return domain.ErrPlanStale
	}
	return nil
}

func TestOwnerInitCreatedProjectKeepsOriginalRemainingAbsenceAndSharedGuards(t *testing.T) {
	for _, drift := range []string{"none", "instance", "srv", "pool", "network", "profile-config", "profile-device", "ref", "project"} {
		t.Run(drift, func(t *testing.T) {
			runtime, fake := ownerInstanceProjectionFixture()
			p, err := runtime.PrepareOwnerInitProjection(context.Background(), fake.Reconcile, fake.TeardownResources)
			if err != nil {
				t.Fatal(err)
			}
			ref := ports.NativeOwnerCreateRef{Domain: "command", Index: 3}
			reader := &ownerCreatedProjectReader{Incus: fake, ref: ref}
			runtime.Incus = reader
			fake.Reconcile = ports.ReconcileState{ProjectFound: true, ProfileFound: true, HostPoolFound: true, HostNetworkFound: true}
			original := ports.PreparedImageProject{Project: runtime.Yard.IncusProject, Absent: true, SharedImages: true, InitialConfig: []byte("original-rendered-config")}
			created := ports.NativeOwnerCreated{Ref: ref, Intent: ports.NativeOwnerCreateIntent{Project: original.Project}, Status: 200}
			switch drift {
			case "instance":
				fake.Reconcile.InstanceFound = true
			case "srv":
				fake.Reconcile.VolumeFound = true
			case "pool":
				fake.Reconcile.HostPoolFound = false
			case "network":
				fake.Reconcile.HostNetworkFound = false
			case "profile-config":
				fake.Reconcile.ProfileConfig = map[string]string{"foreign": "1"}
			case "profile-device":
				fake.Reconcile.ProfileDevices = map[string]map[string]string{"foreign": {"type": "disk"}}
			case "ref":
				created.Ref.Index++
			case "project":
				original.Project = "foreign-project"
			}
			err = runtime.CheckOwnerInitCreatedProject(context.Background(), p, original, created)
			if drift == "none" {
				if err != nil || reader.checks != 1 {
					t.Fatal("exact causal project was not checked", err)
				}
			} else if !errors.Is(err, domain.ErrPlanStale) {
				t.Fatal("created project erased an original guard", err)
			}
		})
	}
}

func assertOwnerProjectionReadView(t *testing.T, runtime Runtime, p OwnerInitProjection) {
	t.Helper()
	view, err := runtime.OwnerInitPlanningView(p)
	if err != nil {
		t.Fatal(err)
	}
	pool, volume := runtime.volumeNames()
	s, err := view.Incus.ReconcileState(context.Background(), runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, pool, volume, "incusbr0")
	if err != nil || s.ProjectFound || s.InstanceFound || s.ProfileFound || s.VolumeFound || !s.HostPoolFound || !s.HostNetworkFound {
		t.Fatal("future view changed shared scope or retained owned resources", err)
	}
	if _, err := view.Incus.Instance(context.Background(), runtime.Yard.IncusProject, runtime.Yard.YardInstanceName); !errors.Is(err, ports.ErrInstanceNotFound) {
		t.Fatal("future instance is not absent", err)
	}
	if _, err := view.Incus.Instance(context.Background(), "foreign", "foreign"); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("foreign read scope admitted", err)
	}
	if err := view.ApplyStage(context.Background(), ports.ReconcileStageProject); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("planning view performed an effect", err)
	}
	if err := view.Teardown(context.Background()); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("planning view performed teardown", err)
	}
}

func TestOwnerInitProjectionKeepsOriginalReadFactsAndNoEffects(t *testing.T) {
	runtime, fake := ownerInstanceProjectionFixture()
	original := describeOwnerProjectionState(fake.Reconcile)
	p, err := runtime.PrepareOwnerInitProjection(context.Background(), fake.Reconcile, fake.TeardownResources)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := p.Record()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := runtime.RestoreOwnerInitProjection(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.record.State, original) {
		t.Fatal("original state not retained")
	}
	assertOwnerProjectionReadView(t, runtime, restored)
	if !reflect.DeepEqual(describeOwnerProjectionState(fake.Reconcile), original) || !fake.Reconcile.ProjectFound {
		t.Fatal("projection mutated original current view")
	}
	if err := runtime.CheckOwnerInitRemoval(context.Background(), p); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("present original resources passed removal guard")
	}
	fake.Reconcile = ports.ReconcileState{HostPoolFound: true, HostNetworkFound: true}
	if err := runtime.CheckOwnerInitRemoval(context.Background(), p); err != nil {
		t.Fatal("original proven deletion's absence guard refused", err)
	}
	fake.Reconcile.InstanceFound = true
	if err := runtime.CheckOwnerInitRemoval(context.Background(), p); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("replacement resource admitted", err)
	}
	path := filepath.Join(testkit.TempDir(t), "projection.json")
	testkit.WriteFile(t, path, raw, 0600)
	cmd := exec.Command(os.Args[0], "-test.run=^TestOwnerInitProjectionFreshProcess$")
	cmd.Env = append(os.Environ(), "SUBYARD_TEST_OWNER_PROJECTION="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fresh projection restore: %v\n%s", err, output)
	}
}

func TestOwnerInitProjectionFreshProcess(t *testing.T) {
	path := os.Getenv("SUBYARD_TEST_OWNER_PROJECTION")
	if path == "" {
		t.Skip("owned child process fixture")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	runtime, fake := ownerInstanceProjectionFixture()
	fake.Err = errors.New("restoration must not make native reads")
	p, err := runtime.RestoreOwnerInitProjection(raw)
	if err != nil {
		t.Fatal(err)
	}
	again, err := p.Record()
	if err != nil || !bytes.Equal(raw, again) {
		t.Fatal("fresh original projection changed", err)
	}
	assertOwnerProjectionReadView(t, runtime, p)
}

func TestOwnerInitProjectionRefusesDifferentOriginalScope(t *testing.T) {
	for _, change := range []string{"uuid", "targets", "root", "project", "environment"} {
		t.Run(change, func(t *testing.T) {
			runtime, fake := ownerInstanceProjectionFixture()
			p, err := runtime.PrepareOwnerInitProjection(context.Background(), fake.Reconcile, fake.TeardownResources)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := p.Record()
			switch change {
			case "uuid":
				before := fake.Reconcile
				before.Instance.Config = map[string]string{"volatile.uuid": "foreign"}
				if _, err := runtime.PrepareOwnerInitProjection(context.Background(), before, fake.TeardownResources); !errors.Is(err, domain.ErrPlanStale) {
					t.Fatal("changed original UUID accepted", err)
				}
				return
			case "targets":
				targets := append([]ports.TeardownResource{}, fake.TeardownResources...)
				targets[1].Binding = strings.Repeat("d", 64)
				if _, err := runtime.PrepareOwnerInitProjection(context.Background(), fake.Reconcile, targets); !errors.Is(err, domain.ErrPlanStale) {
					t.Fatal("foreign teardown binding accepted", err)
				}
				return
			case "root":
				runtime.Yard.Paths.DataHome = "/owner-\xfe/data"
			case "project":
				runtime.Yard.IncusProject = "foreign"
			case "environment":
				runtime.Environment = append(runtime.Environment, "EXTERNAL=changed")
			}
			if _, err := runtime.RestoreOwnerInitProjection(raw); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatal("foreign runtime accepted original projection", err)
			}
		})
	}
}
