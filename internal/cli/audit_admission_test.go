package cli

import (
	"context"
	"github.com/Subyard/Subyard/internal/audit"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func nativeInstallationFixture(t *testing.T, environment []string) {
	t.Helper()
	home := environmentValue(environment, "SUBYARD_HOME")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	installation := uninstall.Installation{Schema: 1, OperatorUID: uint32(os.Getuid()), DataHome: home, ConfigHome: environmentValue(environment, "SUBYARD_CONFIG_HOME"), RuntimeRoot: filepath.Join(home, "runtime"), CacheRoot: filepath.Join(home, "releases")}
	body, err := uninstall.MarshalInstallation(installation)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(home, uninstall.ManifestName), body, 0600)
}

func TestLateAuditWriterCannotRecreateOrEnterNewInstallation(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(map[bool]string{false: "removed", true: "new generation"}[replacement], func(t *testing.T) {
			base := testkit.TempDir(t)
			home := filepath.Join(base, "data")
			if err := os.Mkdir(home, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(home, 0700); err != nil {
				t.Fatal(err)
			}
			cli := &CLI{}
			admission := cli.auditWriteAdmission(home)
			if replacement {
				// Retain the old inode so the replacement cannot immediately reuse it.
				if err := os.Rename(home, filepath.Join(base, "original-data")); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(home, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(home, 0700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Remove(home); err != nil {
				t.Fatal(err)
			}
			if err := audit.WriteInvocation(audit.Invocation{Home: home, Command: "late.finish", BeforeWrite: admission}); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(home, "logs")); !os.IsNotExist(err) {
				t.Fatal("late writer created audit root", err)
			}
			if !replacement {
				if _, err := os.Stat(home); !os.IsNotExist(err) {
					t.Fatal("late writer recreated installation", err)
				}
			}
		})
	}
}

func TestLateAuditEpochFencesRetainedForeignRoot(t *testing.T) {
	home := testkit.TempDir(t)
	installation := uninstall.Installation{Schema: 1, OperatorUID: uint32(os.Getuid()), DataHome: home, ConfigHome: filepath.Join(home, "config"), RuntimeRoot: filepath.Join(home, "runtime"), CacheRoot: filepath.Join(home, "releases")}
	body, err := uninstall.MarshalInstallation(installation)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(home, uninstall.ManifestName), body, 0600)
	cli := &CLI{}
	admission := cli.auditWriteAdmission(home)
	if err := audit.WriteInvocation(audit.Invocation{Home: home, Command: "status", BeforeWrite: admission}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "logs")); err != nil {
		t.Fatal("existing installation audit was not admitted", err)
	}
	if err := os.RemoveAll(filepath.Join(home, "logs")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(home, uninstall.ManifestName)); err != nil {
		t.Fatal(err)
	}
	_, release, err := uninstall.OwnerLock(context.Background(), home, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = uninstall.AdvanceOwnerEpoch(home, 0, 1); err != nil {
		release()
		t.Fatal(err)
	}
	release()
	for _, gate := range []func(context.Context) (func(), bool, error){admission, cli.auditWriteAdmission(home), (&CLI{}).auditWriteAdmission(home)} {
		if err = audit.WriteInvocation(audit.Invocation{Home: home, Command: "late.finish", BeforeWrite: gate}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = os.Stat(filepath.Join(home, "logs")); !os.IsNotExist(err) {
		t.Fatal("old audit writer recreated owned logs in retained root", err)
	}
}

// Ordinary first writes remain usable without install metadata, while an old
// command whose initially absent root survived an uninstall cannot recreate it.
func TestFirstAuditWriteRetainsOwnRootAndFencesUninstall(t *testing.T) {
	base := testkit.TempDir(t)
	home := filepath.Join(base, "data")
	cli := &CLI{}
	admission := cli.auditWriteAdmission(home)
	for _, command := range []string{"first", "finish"} {
		if err := audit.WriteInvocation(audit.Invocation{Home: home, Command: command, BeforeWrite: admission}); err != nil {
			t.Fatal(err)
		}
	}
	if body, err := os.ReadFile(filepath.Join(home, "logs", "yard.log")); err != nil || !strings.Contains(string(body), "finish") {
		t.Fatal("ordinary terminal audit missing", err)
	}
	if err := os.RemoveAll(home); err != nil {
		t.Fatal(err)
	}
	_, release, err := uninstall.OwnerLock(context.Background(), home, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = uninstall.AdvanceOwnerEpoch(home, 0, 1)
	release()
	if err != nil {
		t.Fatal(err)
	}
	for _, gate := range []func(context.Context) (func(), bool, error){admission, (&CLI{}).auditWriteAdmission(home)} {
		if err := audit.WriteInvocation(audit.Invocation{Home: home, Command: "late", BeforeWrite: gate}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatal("audit recreated removed installation", err)
	}
}
