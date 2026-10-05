package operatoraccess

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestGrantRefusesUnsupportedACLBeforeToolInvocation(t *testing.T) {
	// procfs reports no access-ACL support. No ACL command or chmod fallback
	// may run against this descriptor; no object content is read.
	file, err := os.Open("/proc/self/stat")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	called := false
	tools := func(context.Context, *os.File, string) (string, error) { called = true; return "", nil }
	if err := GrantRW(context.Background(), file, "12345", func() error { return nil }, tools); err == nil || called {
		t.Fatalf("unsupported access ACL accepted: %v tool=%v", err, called)
	}
}

func TestNativeACLToolsDistinguishMissingFromUnsafe(t *testing.T) {
	for _, scenario := range []string{"valid", "missing reader", "missing writer", "symlink", "directory", "writable", "nonexecutable", "wrong owner", "missing reader with unsafe writer"} {
		t.Run(scenario, func(t *testing.T) {
			root := testkit.TempDir(t)
			reader, writer := filepath.Join(root, "getfacl"), filepath.Join(root, "setfacl")
			ownerUID := uint32(os.Getuid())
			if scenario != "missing reader" && scenario != "missing reader with unsafe writer" {
				testkit.WriteFile(t, reader, []byte("#!/bin/sh\nexit 99\n"), 0700)
			}
			if scenario != "missing writer" {
				switch scenario {
				case "symlink":
					if err := os.Symlink(reader, writer); err != nil {
						t.Fatal(err)
					}
				case "directory":
					if err := os.Mkdir(writer, 0700); err != nil {
						t.Fatal(err)
					}
				default:
					mode := os.FileMode(0700)
					if scenario == "writable" || scenario == "missing reader with unsafe writer" {
						mode = 0720
					}
					if scenario == "nonexecutable" {
						mode = 0600
					}
					testkit.WriteFile(t, writer, []byte("#!/bin/sh\nexit 99\n"), mode)
				}
			}
			if scenario == "wrong owner" {
				ownerUID++
			}
			available, err := nativeACLToolsAvailableAt([]string{reader, writer}, ownerUID)
			switch scenario {
			case "valid":
				if err != nil || !available {
					t.Fatalf("valid native tools refused: available=%t err=%v", available, err)
				}
			case "missing reader", "missing writer":
				if err != nil || available {
					t.Fatalf("missing native tool is not repairable drift: available=%t err=%v", available, err)
				}
			default:
				if err == nil || available {
					t.Fatalf("unsafe existing tool is not refused: available=%t err=%v", available, err)
				}
			}
		})
	}
}
