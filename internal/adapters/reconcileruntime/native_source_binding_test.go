package reconcileruntime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestRepositoryBindingLabelsOnlyItsExactOwnedDescriptor(t *testing.T) {
	runtime := Runtime{RepositoryRoot: "/proc/self/fd/7", LogicalRepositoryRoot: "/approved/artifact"}
	alias := fmt.Sprintf("/proc/%d/fd/7", os.Getpid())
	for _, path := range []string{"/proc/self/fd/70/config", "/proc/1/fd/7/config", alias + "0/config", "x/proc/self/fd/7/config", "HOOK=x" + alias + "/config", "x" + runtime.RepositoryRoot} {
		if actual := runtime.bindingPath(path); actual != path {
			t.Fatalf("foreign descriptor relabeled: %s", actual)
		}
	}
	if actual := runtime.bindingPath("HOOK=" + alias + "/config/profile/guest.sh"); actual != "HOOK=/approved/artifact/config/profile/guest.sh" {
		t.Fatal("owned alias not normalized")
	}
}

func TestRetainedRepositoryDescriptorNumbersDoNotChangePreparedBindings(t *testing.T) {
	root := testkit.TempDir(t)
	if err := os.Mkdir(filepath.Join(root, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "config", "AGENTS.md"), []byte("approved instructions\n"), 0o600)
	first, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	fixtureRuntime := Runtime{}
	bindInstructionTestPackage(t, &fixtureRuntime, root, "codex")
	logical := "/approved/artifact"
	makeRuntime := func(file *os.File) Runtime {
		physical := fmt.Sprintf("/proc/self/fd/%d", file.Fd())
		hook := fmt.Sprintf("/proc/%d/fd/%d/config/profiles/synthetic/guest.sh", os.Getpid(), file.Fd())
		incus := runningIncus(0)
		return Runtime{RepositoryRoot: physical, LogicalRepositoryRoot: logical,
			Environment: []string{"CODING_TOOL_INTEGRATIONS=codex", "HOST_CODEX_AGENTS_MD=" + physical + "/config/AGENTS.md", "SUBYARD_CONFIG_DIR=" + physical + "/config", "SUBYARD_PROFILE_GUEST_ENVIRONMENT_HOOKS=" + hook},
			Incus:       incus, Executor: incus, Yard: domain.Context{IncusProject: "subyard", YardInstanceName: "yard", DevUser: "dev", DevUID: 1001}}
	}
	a, b := makeRuntime(first), makeRuntime(second)
	if a.RepositoryRoot == b.RepositoryRoot {
		t.Fatal("fixture did not vary actual descriptor")
	}
	left, err := a.SelectedRuntimeInputs("core-guest-runtime")
	if err != nil {
		t.Fatal(err)
	}
	right, err := b.SelectedRuntimeInputs("core-guest-runtime")
	if err != nil || left != right {
		t.Fatal("descriptor number entered source binding", err)
	}
	x, err := a.PrepareConfigs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	y, err := b.PrepareConfigs(context.Background())
	if err != nil || x.Binding() != y.Binding() {
		t.Fatal("descriptor number entered prepared config recipe", err)
	}
	testkit.WriteFile(t, filepath.Join(root, "config", "AGENTS.md"), []byte("different actual source\n"), 0o600)
	right, err = b.SelectedRuntimeInputs("core-guest-runtime")
	if err != nil || left == right {
		t.Fatal("logical label hid changed approved bytes", err)
	}
}

func TestFixedReadonlyScriptUsesRetainedRepositoryDescriptor(t *testing.T) {
	root := testkit.TempDir(t)
	if err := os.Mkdir(filepath.Join(root, "scripts"), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "scripts/install-key-tools.sh"), []byte("#!/bin/sh\nexit 0\n"), 0755)
	descriptor, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer descriptor.Close()
	runtime := Runtime{RepositoryRoot: fmt.Sprintf("/proc/self/fd/%d", descriptor.Fd()), LogicalRepositoryRoot: root}
	if current, err := runtime.scriptConverged(context.Background(), "install-key-tools.sh", "--check"); err != nil || !current {
		t.Fatal("readonly child could not reopen pinned source script", current, err)
	}
}
