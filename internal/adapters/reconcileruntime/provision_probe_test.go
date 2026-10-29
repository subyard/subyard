package reconcileruntime

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestProvisionUsageProbeValidatesMetadataAndELFBeforeExecution(t *testing.T) {
	native, err := exec.LookPath("true")
	if err != nil {
		t.Fatal(err)
	}
	elf, err := os.ReadFile(native)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"current ELF", "non ELF", "invalid ELF", "wrong mode", "wrong owner", "symlink", "stat failure", "ELF read failure"} {
		t.Run(scenario, func(t *testing.T) {
			root := testkit.TempDir(t)
			bin := filepath.Join(root, "bin")
			if err := os.Mkdir(bin, 0o700); err != nil {
				t.Fatal(err)
			}
			path, called, odCalled := filepath.Join(root, "candidate ; literal"), filepath.Join(root, "executed"), filepath.Join(root, "od")
			payload, mode := elf, os.FileMode(0o755)
			owner := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
			if scenario == "non ELF" {
				payload = []byte("#!/bin/sh\nprintf executed > \"$CALLED\"\nprintf 'ccusage 1.2.3\\n'\n")
			} else if scenario == "invalid ELF" {
				payload = []byte("\x7fELF\nprintf executed > \"$CALLED\"\nprintf 'ccusage 1.2.3\\n'\n")
			} else if scenario == "wrong mode" {
				mode = 0o700
			} else if scenario == "wrong owner" {
				owner = fmt.Sprintf("%d:%d", os.Getuid()+1, os.Getgid())
			}
			testkit.WriteFile(t, path, payload, mode)
			if scenario == "symlink" {
				link := filepath.Join(root, "link")
				if err := os.Symlink(path, link); err != nil {
					t.Fatal(err)
				}
				path = link
			}
			if scenario == "wrong mode" || scenario == "wrong owner" || scenario == "symlink" || scenario == "stat failure" || scenario == "ELF read failure" {
				code := "0"
				if scenario == "ELF read failure" {
					code = "1"
				}
				testkit.WriteFile(t, filepath.Join(bin, "od"), []byte("#!/bin/sh\nprintf called > \"$OD_CALLED\"\nprintf '7f 45 4c 46\\n'\nexit "+code+"\n"), 0o700)
			}
			if scenario == "stat failure" {
				testkit.WriteFile(t, filepath.Join(bin, "stat"), []byte("#!/bin/sh\nprintf '%s\\n' \"$EXPECTED_STAT\"\nexit 1\n"), 0o700)
			}
			request := provisionUsageProbe(path, owner)
			command := exec.Command(request[0], request[1:]...)
			command.Env = append(os.Environ(), "PATH="+bin+":/usr/bin:/bin", "CALLED="+called, "OD_CALLED="+odCalled, "EXPECTED_STAT=regular file|755|"+owner)
			output, err := command.Output()
			if scenario == "current ELF" {
				direct, directErr := exec.Command(path, "--version").Output()
				if err != nil || directErr != nil || !bytes.Equal(output, direct) {
					t.Fatalf("probe changed version stdout: probe=%v direct=%v", err, directErr)
				}
			} else if err == nil || len(output) != 0 {
				t.Fatalf("unsafe executable reached version execution: err=%v output_bytes=%d", err, len(output))
			}
			if _, err := os.Stat(called); !os.IsNotExist(err) {
				t.Fatalf("non-ELF candidate executed: %v", err)
			}
			_, odErr := os.Stat(odCalled)
			if scenario == "ELF read failure" && odErr != nil || scenario != "ELF read failure" && !os.IsNotExist(odErr) {
				t.Fatalf("metadata was not checked before the ELF probe: od_error=%v", odErr)
			}
		})
	}
}

func TestProvisionPasswdProbePreservesOnlyPasswdOutputAndQuotesUser(t *testing.T) {
	root := testkit.TempDir(t)
	testkit.WriteFile(t, filepath.Join(root, "id"), []byte("#!/bin/sh\nprintf 'identity output must be discarded\\n'\n[ \"$1\" = \"$EXPECTED_USER\" ]\n"), 0o700)
	testkit.WriteFile(t, filepath.Join(root, "getent"), []byte("#!/bin/sh\n[ \"$1\" = passwd ] && [ \"$2\" = \"$EXPECTED_USER\" ] || exit 1\nprintf 'dev:x:1000:1000::/home/dev:/bin/sh\\n'\n"), 0o700)
	user := "dev ; literal"
	request := provisionPasswdProbe(user)
	command := exec.Command(request[0], request[1:]...)
	command.Env = append(os.Environ(), "PATH="+root+":/usr/bin:/bin", "EXPECTED_USER="+user)
	output, err := command.Output()
	if err != nil || string(output) != "dev:x:1000:1000::/home/dev:/bin/sh\n" {
		t.Fatalf("passwd probe changed output or argument boundaries: err=%v output=%q", err, output)
	}
}
