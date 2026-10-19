package testyardmigration

import (
	"bytes"
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOwnerChildSourceKeepsOriginalMemberAcrossOwnedArchive(t *testing.T) {
	for _, flat := range []bool{false, true} {
		name := "directory"
		if flat {
			name = "flat"
		}
		t.Run(name, func(t *testing.T) {
			options, before := ownerFaultFixture(t, true, false)
			if flat {
				options, before = flatOwnerFaultFixture(t)
			}
			root, err := os.Open(options.ConfigHome)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			record, err := CaptureOwnerChildSource(options, before, root)
			if err != nil {
				t.Fatal(err)
			}
			shape, _, _ := preparedLegacyState(before.State)
			original, current := registrationPaths(options, shape)
			check := func() {
				t.Helper()
				checked, err := CheckOwnerChildSource(options, before, record, root)
				if err != nil {
					t.Fatal(err)
				}
				defer checked.Close()
				snapshot, matched, err := checked.ReadOriginal(original)
				if err != nil || !matched || !snapshot.Exists || !bytes.Equal(snapshot.Content, record.Content) || snapshot.Identity.Inode != record.Inode {
					t.Fatal("original registration authority changed", err)
				}
				if _, matched, err := checked.ReadOriginal(current); err != nil || matched {
					t.Fatal("unapproved path admitted", err)
				}
			}
			check()
			registration, err := openBoundPublishedRegistration(original, before)
			if err != nil {
				t.Fatal(err)
			}
			defer registration.close()
			if err := copyBoundRegistration(options, registration, current, before); err != nil {
				t.Fatal(err)
			}
			if err := archivePreparedRegistration(options, registration, before); err != nil {
				t.Fatal(err)
			}
			check()
			wrong := options
			wrong.RecoveryToken = "11111111111111111111111111111111"
			if value, err := CheckOwnerChildSource(wrong, before, record, root); err == nil {
				value.Close()
				t.Fatal("foreign archive token admitted")
			}
			archive, err := preparedRegistrationArchive(options, before)
			if err != nil {
				t.Fatal(err)
			}
			path := archive.archive
			if archive.directory {
				path = filepath.Join(path, "config.env")
			}
			checked, err := CheckOwnerChildSource(options, before, record, root)
			if err != nil {
				t.Fatal(err)
			}
			// The disposable filesystem can coalesce changes within one clock
			// tick; make this specifically a changed-ctime readback control.
			time.Sleep(10 * time.Millisecond)
			if err := os.Chmod(path, 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0600); err != nil {
				t.Fatal(err)
			}
			if _, matched, err := checked.ReadOriginal(original); !matched || err == nil {
				t.Fatal("late archive mode restoration reused the rename exception")
			}
			checked.Close()
			replacement := path + ".replacement"
			if err := os.WriteFile(replacement, record.Content, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(replacement, path); err != nil {
				t.Fatal(err)
			}
			if value, err := CheckOwnerChildSource(options, before, record, root); err == nil {
				value.Close()
				t.Fatal("same-byte foreign archive member admitted")
			}
		})
	}
}

func TestOwnerRegistrationRestoreFactFollowsDurableRename(t *testing.T) {
	for _, cut := range []string{"complete", "before-rename", "fsync", "post-fsync", "result-write", "late-mode"} {
		t.Run(cut, func(t *testing.T) {
			options, before := flatOwnerFaultFixture(t)
			root, err := os.Open(options.ConfigHome)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			record, err := CaptureOwnerChildSource(options, before, root)
			if err != nil {
				t.Fatal(err)
			}
			shape, _, _ := preparedLegacyState(before.State)
			path, current := registrationPaths(options, shape)
			original, err := openBoundPublishedRegistration(path, before)
			if err != nil {
				t.Fatal(err)
			}
			defer original.close()
			if err := copyBoundRegistration(options, original, current, before); err != nil {
				t.Fatal(err)
			}
			if err := archivePreparedRegistration(options, original, before); err != nil {
				t.Fatal(err)
			}
			called := false
			var result PreparedOwnerMember
			options.RecordRegistrationRestore = func(member PreparedOwnerMember) error {
				called = true
				if err := record.ValidateRestoredMember(member); err != nil {
					return err
				}
				if cut == "result-write" {
					return errors.New("result publication refused")
				}
				result = member
				return nil
			}
			options.fault = func(point string) error {
				if cut == "before-rename" && point == "after-compensation-registration-validation-before-restore" || cut == "post-fsync" && point == "after-compensation-registration-recreation" {
					return errors.New("original cut")
				}
				if cut == "late-mode" && point == "after-compensation-registration-recreation" {
					return os.Chmod(path, 0644)
				}
				return nil
			}
			if cut == "fsync" {
				options.syncOwnerDirectory = func(role string, fd int) error {
					if role == "registration-restore-destination" {
						return errors.New("sync cut")
					}
					return unix.Fsync(fd)
				}
			}
			err = restorePreparedRegistration(options, before)
			if cut != "complete" {
				if err == nil {
					t.Fatal("incomplete result accepted")
				}
				if called != (cut == "result-write" || cut == "late-mode") {
					t.Fatal("result published before completed owning rename")
				}
				return
			}
			if err != nil || !called {
				t.Fatal("durable original restore refused", err)
			}
			if value, err := CheckOwnerChildSource(options, before, record, root); err == nil {
				value.Close()
				t.Fatal("restored ctime accepted without result")
			}
			checked, err := CheckRestoredOwnerChildSource(options, before, record, root, result)
			if err != nil {
				t.Fatal(err)
			}
			defer checked.Close()
			if _, _, err := checked.ReadOriginal(path); err != nil {
				t.Fatal(err)
			}
			result.ChangedNanos++
			if value, err := CheckRestoredOwnerChildSource(options, before, record, root, result); err == nil {
				value.Close()
				t.Fatal("foreign result admitted")
			}
		})
	}
}

func TestOwnerChildSourceRefusesRootAndPhysicalReplacement(t *testing.T) {
	options, before := ownerFaultFixture(t, true, false)
	root, err := os.Open(options.ConfigHome)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	record, err := CaptureOwnerChildSource(options, before, root)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	if value, err := CheckOwnerChildSource(options, before, record, foreign); err == nil {
		value.Close()
		t.Fatal("foreign pinned root admitted")
	}
	shape, _, _ := preparedLegacyState(before.State)
	path, _ := registrationPaths(options, shape)
	if err := os.WriteFile(path+".replacement", record.Content, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".replacement", path); err != nil {
		t.Fatal(err)
	}
	if value, err := CheckOwnerChildSource(options, before, record, root); err == nil {
		value.Close()
		t.Fatal("same-byte source replacement admitted")
	}
}

func TestOwnerChildSourceRefusesDriftAfterCheck(t *testing.T) {
	for _, change := range []string{"mode", "bytes", "root"} {
		t.Run(change, func(t *testing.T) {
			options, before := ownerFaultFixture(t, true, false)
			root, err := os.Open(options.ConfigHome)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			record, err := CaptureOwnerChildSource(options, before, root)
			if err != nil {
				t.Fatal(err)
			}
			checked, err := CheckOwnerChildSource(options, before, record, root)
			if err != nil {
				t.Fatal(err)
			}
			defer checked.Close()
			shape, _, _ := preparedLegacyState(before.State)
			path, _ := registrationPaths(options, shape)
			switch change {
			case "mode":
				err = os.Chmod(path, 0644)
			case "bytes":
				err = os.WriteFile(path, []byte("YARD_TEMPLATE=test-vms\nSSH_PORT=2224\n"), 0600)
			case "root":
				err = os.Rename(options.ConfigHome, options.ConfigHome+"-original")
				if err == nil {
					err = os.Mkdir(options.ConfigHome, 0700)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, matched, err := checked.ReadOriginal(path); !matched || err == nil {
				t.Fatal("late source drift admitted")
			}
		})
	}
}
