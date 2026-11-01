package main

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/ownerpublication"
	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/sys/unix"
)

func TestPublicationWorkerEntrypoint(t *testing.T) {
	if os.Getenv("SUBYARD_TEST_PUBLICATION_ENTRY_CHILD") == "1" {
		os.Args = []string{os.Args[0], "_publication-reconcile", os.Getenv("SUBYARD_TEST_PUBLICATION_ENGINE_ARG"), os.Getenv("SUBYARD_TEST_PUBLICATION_UNIT_ARG")}
		main()
		return
	}
	if os.Getenv("SUBYARD_TEST_PUBLICATION_ENTRY_NS") != "1" {
		command := exec.Command("unshare", "--user", "--map-root-user", "--mount", os.Args[0], "-test.run=^TestPublicationWorkerEntrypoint$", "-test.v", "-test.count=1")
		command.Env = append(os.Environ(), "SUBYARD_TEST_PUBLICATION_ENTRY_NS=1")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("required private namespace: %v\n%s", err, output)
		}
		t.Logf("executed private namespace:\n%s", output)
		return
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/usr/local", "/etc/systemd/system"} {
		if err := unix.Mount("tmpfs", path, "tmpfs", 0, "mode=0755"); err != nil {
			t.Fatal(err)
		}
	}
	root := testkit.TempDir(t)
	server, err := testkit.NewIncusServer(root)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	template, err := os.ReadFile("../../config/systemd/subyard-publication-reconcile.service.in")
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile("/proc/self/exe")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := ownerpublication.Encode(ownerpublication.CoreContracts())
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"custom", "default"} {
		t.Run(mode, func(t *testing.T) {
			engine, unit := filepath.Join(root, "engine space%$"), filepath.Join(root, "power.service")
			if mode == "default" {
				engine = "/usr/local/libexec/subyard/yard-boot-reconcile"
				unit = "/etc/systemd/system/subyard-power-reconcile.service"
			}
			if err := os.MkdirAll(filepath.Dir(engine), 0755); err != nil {
				t.Fatal(err)
			}
			passive := filepath.Join(filepath.Dir(unit), ownerpublication.HandoffService)
			testkit.WriteFile(t, engine, image, 0755)
			testkit.WriteFile(t, unit, []byte("original protected power unit"), 0644)
			testkit.WriteFile(t, passive, []byte(ownerpublication.RenderPublicationUnit(string(template), engine, unit, encoded)), 0644)
			if output, err := exec.Command("systemd-analyze", "verify", passive).CombinedOutput(); err != nil {
				t.Fatalf("real parser rejected approved path bytes: %v\n%s", err, output)
			}
			manager := filepath.Join(root, "systemctl")
			testkit.WriteFile(t, manager, []byte("#!/bin/sh\nprintf '%s\\n' 'LoadState=loaded' 'NeedDaemonReload=no' 'FragmentPath="+passive+"' 'DropInPaths='\n"), 0755)
			if err := unix.Mount(manager, "/usr/bin/systemctl", "", unix.MS_BIND, ""); err != nil {
				t.Fatal(err)
			}
			defer unix.Unmount("/usr/bin/systemctl", 0)
			for _, control := range []string{"approved", "ambient-foreign", "different-engine", "different-unit", "same-bytes-foreign-inode", "malformed"} {
				t.Run(control, func(t *testing.T) {
					selectedEngine, selectedUnit, executable := engine, unit, engine
					want := 0
					switch control {
					case "different-engine":
						selectedEngine = engine + ".foreign"
						testkit.WriteFile(t, selectedEngine, image, 0755)
						want = 1
					case "different-unit":
						selectedUnit = unit + ".foreign"
						testkit.WriteFile(t, selectedUnit, []byte("original protected power unit"), 0644)
						want = 1
					case "same-bytes-foreign-inode":
						executable = os.Args[0]
						want = 1
					}
					args := []string{base64.StdEncoding.EncodeToString([]byte(selectedEngine)), base64.StdEncoding.EncodeToString([]byte(selectedUnit))}
					if control == "malformed" {
						args[0] += "="
						want = 1
					}
					command := exec.Command(executable, "-test.run=^TestPublicationWorkerEntrypoint$", "-test.count=1")
					command.Env = append(os.Environ(), "SUBYARD_TEST_PUBLICATION_ENTRY_CHILD=1", "SUBYARD_TEST_PUBLICATION_ENGINE_ARG="+args[0], "SUBYARD_TEST_PUBLICATION_UNIT_ARG="+args[1], "SUBYARD_PUBLICATION_CONTRACTS="+encoded, "INCUS_SOCKET="+server.SocketPath, "SUBYARD_POWER_RECONCILER_PATH=/foreign/ambient-engine", "SUBYARD_POWER_UNIT_PATH=/foreign/ambient-unit")
					output, err := command.CombinedOutput()
					code := 0
					if err != nil {
						if exit, ok := err.(*exec.ExitError); ok {
							code = exit.ExitCode()
						} else {
							t.Fatal(err)
						}
					}
					if code != want {
						t.Fatalf("entrypoint exit=%d want=%d: %s", code, want, output)
					}
				})
			}
			if len(server.PowerCalls()) != 0 || len(server.ExecCalls()) != 0 {
				t.Fatal("passive worker mutated power or guest commands")
			}
		})
	}
}

func TestPublicationCommandBytes(t *testing.T) {
	engine := "/a space/%$λ"
	unit := "/a space/power\n\xff.service"
	command := ownerpublication.PublicationCommand(engine, unit)
	args := strings.Fields(command[strings.Index(command, " _publication-reconcile ")+len(" _publication-reconcile "):])
	actualEngine, actualUnit, err := ownerpublication.PublicationPaths(args)
	if err != nil || actualEngine != engine || actualUnit != unit {
		t.Fatal("path bytes changed", err)
	}
	if !strings.HasPrefix(command, ":\"") || !strings.Contains(command, "%%$") || strings.Contains(command, "$$") {
		t.Fatal("incorrect systemd executable quoting", command)
	}
	for _, engine := range []string{"relative", "/quote\"", "/apostrophe'", "/back\\slash", "/control\n", "/invalid\xff"} {
		if ownerpublication.ValidatePublicationPaths(engine, "/power.service") == nil {
			t.Fatalf("unrepresentable engine accepted: %q", engine)
		}
		args[0] = base64.StdEncoding.EncodeToString([]byte(engine))
		if _, _, err := ownerpublication.PublicationPaths(args); err == nil {
			t.Fatalf("entrypoint accepted unrepresentable engine: %q", engine)
		}
	}
}
