package profile

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func fixture(t *testing.T, root, name, hook string, declaration Definition) string {
	t.Helper()
	dir := filepath.Join(root, "config", "profiles", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if hook != "" {
		testkit.WriteFile(t, filepath.Join(dir, "owner.sh"), []byte(hook), 0o700)
		declaration.OwnerService = "owner.sh"
	}
	declaration.SchemaVersion = 1
	data, err := json.Marshal(declaration)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "profile.json")
	testkit.WriteFile(t, path, data, 0o600)
	return path
}

func TestRegistryRejectsUnsafeDeclarations(t *testing.T) {
	for _, data := range []string{
		`{"schema_version":2}`, `{"schema_version":1,"unknown":true}`,
		`{"schema_version":1,"native":[{"package":"cmd/tool","path":"../escape"}]}`,
		`{"schema_version":1,"managed_paths":[{"root":"data","path":"../escape"}]}`,
		`{"schema_version":1,"owner_service":"../owner.sh"}`,
		`{"schema_version":1} {}`,
		`{"schema_version":1,"consumers":[{"id":"none","zone":"*","path":"fixture/key","format":"file"}]}`,
		`{"schema_version":1,"consumers":[{"id":"fixture","zone":"global","path":"fixture/{zone}.env","format":"file"}]}`,
		`{"schema_version":1,"consumers":[{"id":"fixture","zone":"*","path":"{zone}/key","format":"file"}]}`,
		`{"schema_version":1,"consumers":[{"id":"fixture","zone":"*","path":"fixture/{unknown}.env","format":"file"}]}`,
		`{"schema_version":1,"consumers":[{"id":"fixture","zone":"*","path":"fixture/{zone}-{zone}.env","format":"file"}]}`,
		`{"schema_version":1,"consumers":[{"id":"fixture","zone":"*","path":"fixture/{zone}.env","format":"file","stop_handler":"../escape"}]}`,
	} {
		t.Run(data, func(t *testing.T) {
			root := testkit.TempDir(t)
			path := fixture(t, root, "fixture", "", Definition{})
			testkit.WriteFile(t, path, []byte(data), 0o600)
			if _, err := Load(root); err == nil {
				t.Fatal("accepted unsafe descriptor")
			}
		})
	}
	root := testkit.TempDir(t)
	consumer := Consumer{ID: "fixture-key", Zone: "global", Path: "fixture/key", Format: "file"}
	fixture(t, root, "one", "", Definition{Consumers: []Consumer{consumer}})
	fixture(t, root, "two", "", Definition{Consumers: []Consumer{consumer}})
	if _, err := Load(root); err == nil {
		t.Fatal("accepted duplicate consumer")
	}
}

func TestProfileExtensionsLoadAndValidate(t *testing.T) {
	root := testkit.TempDir(t)
	if got, err := Load(root); err != nil || len(got) != 0 {
		t.Fatalf("empty registry: profiles=%v err=%v", got, err)
	}
	good := Definition{
		Settings: []Setting{
			{Name: "SAMPLE_HOST_PORT", Type: "port", Scopes: []string{"host", "yard", "command"}, Application: "next-command", Optional: true, Minimum: 1, Maximum: 65535, HostListener: true},
			{Name: "SAMPLE_CACHE_SIZE", Type: "size", Scopes: []string{"shipped", "yard"}, Application: "yard-init", Default: stringPointer("1GiB")},
		},
		Runtime:          &RuntimeHook{ActivationID: "fixture-runtime", Handler: "runtime.sh"},
		GuestEnvironment: &GuestEnvironmentHook{Handler: "guest.sh"},
	}
	path := fixture(t, root, "fixture", "", good)
	dir := filepath.Dir(path)
	testkit.WriteFile(t, filepath.Join(dir, "runtime.sh"), []byte("#!/bin/sh\n"), 0o700)
	testkit.WriteFile(t, filepath.Join(dir, "guest.sh"), []byte("#!/bin/sh\n"), 0o700)
	loaded, err := Load(root)
	if err != nil || len(loaded) != 1 || loaded[0].Runtime == nil || loaded[0].GuestEnvironment == nil || loaded[0].Settings[0].Name != "SAMPLE_HOST_PORT" || loaded[0].Settings[1].Type != "size" {
		t.Fatalf("extension load: profiles=%+v err=%v", loaded, err)
	}
}

func TestProfileExtensionRejectsInvalidAndDuplicateDeclarations(t *testing.T) {
	invalid := []Setting{
		{Name: "lower", Type: "string", Scopes: []string{"yard"}, Application: "yard-init"},
		{Name: "BAD", Type: "object", Scopes: []string{"yard"}, Application: "yard-init"},
		{Name: "BAD", Type: "string", Scopes: []string{"yard", "yard"}, Application: "yard-init"},
		{Name: "BAD", Type: "string", Scopes: []string{"yard"}, Application: "later"},
		{Name: "BAD", Type: "integer", Scopes: []string{"yard"}, Application: "yard-init", Minimum: 5, Maximum: 2},
		{Name: "BAD", Type: "string", Scopes: []string{"yard"}, Application: "yard-init", HostListener: true},
		{Name: "BAD", Type: "port", Scopes: []string{"yard"}, Application: "yard-init", Minimum: 0, Maximum: 65536},
		{Name: "BAD", Type: "integer", Scopes: []string{"yard"}, Application: "yard-init", Minimum: 2, Maximum: 4, Default: stringPointer("5")},
		{Name: "BAD", Type: "string", Scopes: []string{"yard"}, Application: "yard-init", Enum: []string{"x", "x"}},
	}
	for i, setting := range invalid {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			root := testkit.TempDir(t)
			fixture(t, root, "fixture", "", Definition{Settings: []Setting{setting}})
			if _, err := Load(root); err == nil {
				t.Fatalf("accepted invalid setting %+v", setting)
			}
		})
	}
	for _, tc := range []struct {
		name          string
		first, second Definition
	}{
		{"setting", Definition{Settings: []Setting{{Name: "SAME", Type: "string", Scopes: []string{"yard"}, Application: "yard-init"}}}, Definition{Settings: []Setting{{Name: "SAME", Type: "string", Scopes: []string{"host"}, Application: "next-command"}}}},
		{"activation", Definition{Runtime: &RuntimeHook{ActivationID: "same", Handler: "runtime.sh"}}, Definition{Runtime: &RuntimeHook{ActivationID: "same", Handler: "runtime.sh"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := testkit.TempDir(t)
			for _, name := range []string{"one", "two"} {
				declaration := tc.first
				if name == "two" {
					declaration = tc.second
				}
				path := fixture(t, root, name, "", declaration)
				if declaration.Runtime != nil {
					testkit.WriteFile(t, filepath.Join(filepath.Dir(path), "runtime.sh"), []byte("#!/bin/sh\n"), 0o700)
				}
			}
			if _, err := Load(root); err == nil {
				t.Fatal("accepted cross-profile collision")
			}
		})
	}
}

func TestProfileExtensionRejectsSymlinkHook(t *testing.T) {
	root := testkit.TempDir(t)
	path := fixture(t, root, "fixture", "", Definition{Runtime: &RuntimeHook{ActivationID: "fixture-runtime", Handler: "nested/runtime.sh"}})
	dir := filepath.Dir(path)
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(dir, "real.sh"), []byte("#!/bin/sh\n"), 0o700)
	if err := os.Symlink("../real.sh", filepath.Join(dir, "nested", "runtime.sh")); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root); err == nil {
		t.Fatal("accepted symlinked profile handler")
	}
}

func TestReadExecutableRejectsSubstitutedHookAndSymlinkRoot(t *testing.T) {
	root := testkit.TempDir(t)
	path := fixture(t, root, "fixture", "", Definition{GuestEnvironment: &GuestEnvironmentHook{Handler: "guest.sh"}})
	dir := filepath.Dir(path)
	hook := filepath.Join(dir, "guest.sh")
	testkit.WriteFile(t, hook, []byte("safe\n"), 0o700)
	definitions, err := Load(root)
	if err != nil || len(definitions) != 1 {
		t.Fatalf("load profile: definitions=%+v err=%v", definitions, err)
	}
	definition := definitions[0]
	if got, err := definition.ReadExecutable("guest.sh"); err != nil || string(got) != "safe\n" {
		t.Fatalf("read executable: got=%q err=%v", got, err)
	}
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.sh")
	testkit.WriteFile(t, outside, []byte("unsafe\n"), 0o700)
	if err := os.Symlink(outside, hook); err != nil {
		t.Fatal(err)
	}
	if _, err := definition.ReadExecutable("guest.sh"); err == nil {
		t.Fatal("accepted a hook replaced by a symlink after catalog load")
	}
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, dir+"-real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dir+"-real", dir); err != nil {
		t.Fatal(err)
	}
	if _, err := definition.ReadExecutable("guest.sh"); err == nil {
		t.Fatal("accepted a profile root replaced by a symlink after catalog load")
	}
}

func stringPointer(value string) *string { return &value }

func TestReadExecutableThroughPinnedRepository(t *testing.T) {
	root := testkit.TempDir(t)
	path := fixture(t, root, "fixture", "", Definition{GuestEnvironment: &GuestEnvironmentHook{Handler: "guest.sh"}})
	testkit.WriteFile(t, filepath.Join(filepath.Dir(path), "guest.sh"), []byte("safe\n"), 0o700)
	pin, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer pin.Close()
	for _, owner := range []string{"self", strconv.Itoa(os.Getpid())} {
		alias := fmt.Sprintf("/proc/%s/fd/%d", owner, pin.Fd())
		definitions, err := Load(alias)
		if err != nil || len(definitions) != 1 {
			t.Fatalf("load pinned profile: %v, %v", definitions, err)
		}
		if got, err := definitions[0].ReadExecutable("guest.sh"); err != nil || string(got) != "safe\n" {
			t.Fatalf("read pinned executable: %q, %v", got, err)
		}
	}
}

func TestLoadBindsRepositorySelector(t *testing.T) {
	root := testkit.TempDir(t)
	for _, name := range []string{"first", "second"} {
		path := fixture(t, filepath.Join(root, name), "fixture", "", Definition{GuestEnvironment: &GuestEnvironmentHook{Handler: "guest.sh"}})
		testkit.WriteFile(t, filepath.Join(filepath.Dir(path), "guest.sh"), []byte(name+"\n"), 0o700)
	}
	selector := filepath.Join(root, "current")
	if err := os.Symlink("first", selector); err != nil {
		t.Fatal(err)
	}
	definitions, err := Load(selector)
	if err != nil || len(definitions) != 1 {
		t.Fatalf("load selected repository: %v, %v", definitions, err)
	}
	if err := os.Remove(selector); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("second", selector); err != nil {
		t.Fatal(err)
	}
	if got, err := definitions[0].ReadExecutable("guest.sh"); err != nil || string(got) != "first\n" {
		t.Fatalf("loaded hook followed changed repository selector: %q, %v", got, err)
	}
}

func TestPinnedExecutableRejectsSubstitutedPackagePaths(t *testing.T) {
	for _, changed := range []string{"config", "config/profiles", "config/profiles/fixture", "config/profiles/fixture/hooks", "config/profiles/fixture/hooks/guest.sh"} {
		t.Run(changed, func(t *testing.T) {
			root := testkit.TempDir(t)
			path := fixture(t, root, "fixture", "", Definition{GuestEnvironment: &GuestEnvironmentHook{Handler: "hooks/guest.sh"}})
			if err := os.Mkdir(filepath.Join(filepath.Dir(path), "hooks"), 0o700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(filepath.Dir(path), "hooks", "guest.sh"), []byte("safe\n"), 0o700)
			pin, err := os.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer pin.Close()
			definitions, err := Load(fmt.Sprintf("/proc/self/fd/%d", pin.Fd()))
			if err != nil || len(definitions) != 1 {
				t.Fatalf("load pinned profile: %v, %v", definitions, err)
			}
			target := filepath.Join(root, changed)
			if err := os.Rename(target, target+"-real"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target+"-real", target); err != nil {
				t.Fatal(err)
			}
			if _, err := definitions[0].ReadExecutable("hooks/guest.sh"); err == nil {
				t.Fatal("accepted a substituted path below the repository descriptor")
			}
		})
	}
}

func TestRegistrySelectionAndSettings(t *testing.T) {
	d := Definition{Name: "fixture", DefaultYards: []string{"default"}, DisabledWhen: map[string]string{"FIXTURE_OFF": "1"}}
	if !d.Selected("default", nil) || d.Selected("other", nil) || d.Selected("default", map[string]string{"ENVIRONMENT_PROFILES": ""}) || !d.Selected("other", map[string]string{"ENVIRONMENT_PROFILES": "fixture"}) || d.Selected("default", map[string]string{"FIXTURE_OFF": "1"}) {
		t.Fatal("selection precedence")
	}
	setup := Setup{Fields: []Field{{Name: "account", Kind: "numeric-string"}, {Name: "count", Kind: "positive-integer"}}, KeyOverrideField: "key_file"}
	if _, err := setup.Decode([]byte(`{"account":"123","count":4,"key_file":"/safe/key"}`)); err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{`{"account":123,"count":4}`, `{"account":"123","count":0}`, `{"account":"123","count":4,"key_file":"relative"}`, `{"account":"123","count":4,"extra":true}`} {
		if _, err := setup.Decode([]byte(data)); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
}

func TestServicesDispatchAndPauseRecovery(t *testing.T) {
	root := testkit.TempDir(t)
	hook := `#!/bin/bash
printf '%s:%s:%s\n' "${0%/owner.sh}" "$1" "$SUBYARD_PROFILE_SELECTED" >> "$EVENTS"
if [ "$1" = --pause ]; then echo paused; fi
`
	fixture(t, root, "one", hook, Definition{DefaultYards: []string{"default"}})
	fixture(t, root, "two", hook, Definition{})
	events := filepath.Join(root, "events")
	env := map[string]string{"SUBYARD_ENGINE_CONTEXT": "1", "SUBYARD_ENGINE_CONTEXT_SCHEMA": "1", "SUBYARD_YARD": "", "EVENTS": events}
	var out bytes.Buffer
	if err := RunServices(context.Background(), root, []string{"--pause"}, env, &out, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "one\ntwo\n" {
		t.Fatalf("pause IDs: %q", out.String())
	}
	if err := RunServices(context.Background(), root, []string{"--resume", "two"}, env, &out, &out); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(events)
	if !strings.Contains(string(data), "one:--pause:1") || !strings.Contains(string(data), "two:--pause:0") || strings.Contains(string(data), "one:--resume") || !strings.Contains(string(data), "two:--resume:0") {
		t.Fatalf("events: %s", data)
	}
	fixture(t, root, "two", `#!/bin/bash
exit 9
`, Definition{})
	out.Reset()
	if err := RunServices(context.Background(), root, []string{"--pause"}, env, &out, &out); err == nil {
		t.Fatal("ignored failed pause")
	}
	data, _ = os.ReadFile(events)
	if !strings.Contains(string(data), "one:--resume:1") || out.Len() != 0 {
		t.Fatalf("failed pause stranded earlier service: %s output=%s", data, &out)
	}
	if err := RunServices(context.Background(), root, []string{"--yes"}, nil, &out, &out); err == nil {
		t.Fatal("accepted unprepared context")
	}
}

// Lifecycle adapters do not carry init's dispatcher override. The shipped
// launcher must still dispatch profile services from that exact runtime root.
func TestServiceShellUsesRuntimeLauncher(t *testing.T) {
	root := testkit.TempDir(t)
	for _, dir := range []string{"scripts/lib", "bin"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"profile-services.sh", "lib/engine-context.sh"} {
		data, err := os.ReadFile(filepath.Join("../../scripts", name))
		if err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(root, "scripts", name), data, 0o700)
	}
	testkit.WriteFile(t, filepath.Join(root, "bin/yard"), []byte("#!/bin/bash\nprintf '%s\\n' \"$@\"\n"), 0o700)
	command := exec.Command("bash", filepath.Join(root, "scripts/profile-services.sh"), "--pause")
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "SUBYARD_ENGINE_CONTEXT=1", "SUBYARD_ENGINE_CONTEXT_SCHEMA=1"}
	for _, key := range strings.Fields("SUBYARD_OPERATOR_HOME SUBYARD_CONFIG_DIR SUBYARD_CONFIG_HOME SUBYARD_HOME STORAGE_PATH HOST_BASE RESTRICTED_DISK_PATHS ACCESS_KIND YARD_KIND YARD_INSTANCE_NAME INCUS_PROJECT INCUS_BRIDGE SSH_HOST DEV_USER DEV_UID DEV_SUDO FORWARD_SSH_AGENT NESTED_E2E_VMS") {
		command.Env = append(command.Env, key+"=")
	}
	output, err := command.CombinedOutput()
	if err != nil || string(output) != "_profile-services\n"+root+"\n--pause\n" {
		t.Fatalf("launcher dispatch: %s (%v)", output, err)
	}
}

func TestRuntimeProjectHookPathsStayInsideNativeDispatcherDirectory(t *testing.T) {
	for _, path := range []string{"relative-hook", "/usr/local/libexec/subyard/projects-changed.d/../outside", "/usr/local/libexec/subyard/projects-changed.d/nested/hook", "/usr/local/libexec/subyard/projects-changed.d/unsafe\n"} {
		root := testkit.TempDir(t)
		declaration := Definition{Runtime: &RuntimeHook{ActivationID: "fixture-runtime", Handler: "runtime.sh", ProjectsChangedHooks: []string{path}}}
		file := fixture(t, root, "fixture", "", declaration)
		testkit.WriteFile(t, filepath.Join(filepath.Dir(file), "runtime.sh"), []byte("#!/bin/sh\n"), 0o700)
		if _, err := Load(root); err == nil {
			t.Fatalf("unsafe native hook path accepted: %q", path)
		}
	}
}
