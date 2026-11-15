package testyardmigration

import (
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestOwnerRegistrationRestoreFactFollowsDurableRename(t *testing.T) {
	for _, cut := range []string{"complete", "before-rename", "fsync", "post-fsync"} {
		t.Run(cut, func(t *testing.T) {
			options, before := flatOwnerFaultFixture(t)
			shape, _, _ := preparedLegacyState(before.State)
			path, current := registrationPaths(options, shape)
			original, err := openBoundPublishedRegistration(path, before)
			if err != nil {
				t.Fatal(err)
			}
			defer original.close()
			info, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			payload := read(t, path)
			if err := copyBoundRegistration(options, original, current, before); err != nil {
				t.Fatal(err)
			}
			if err := archivePreparedRegistration(options, original, before); err != nil {
				t.Fatal(err)
			}
			archive, err := preparedRegistrationArchive(options, before)
			if err != nil {
				t.Fatal(err)
			}
			cutErr := errors.New("restore cut")
			options.fault = func(point string) error {
				if cut == "before-rename" && point == "after-compensation-registration-validation-before-restore" ||
					cut == "post-fsync" && point == "after-compensation-registration-recreation" {
					return cutErr
				}
				return nil
			}
			if cut == "fsync" {
				options.syncOwnerDirectory = func(role string, fd int) error {
					if role == "registration-restore-destination" {
						return cutErr
					}
					return unix.Fsync(fd)
				}
			}
			err = restorePreparedRegistration(options, before)
			if cut == "complete" {
				if err != nil {
					t.Fatalf("durable original restore refused: %v", err)
				}
			} else if !errors.Is(err, cutErr) {
				t.Fatalf("restore cut was not returned: %v", err)
			}
			retained, absent := path, archive.archive
			if cut == "before-rename" {
				retained, absent = archive.archive, path
			}
			restored, err := os.Lstat(retained)
			if err != nil || !os.SameFile(info, restored) || read(t, retained) != payload {
				t.Fatalf("original registration was not retained at %s: %v", retained, err)
			}
			if _, err := os.Lstat(absent); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unexpected registration at %s: %v", absent, err)
			}
		})
	}
}
