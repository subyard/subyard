package testyardmigration

import (
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestOwnerRegistrationRestoreFactFollowsDurableRename(t *testing.T) {
	for _, cut := range []string{"complete", "before-rename", "fsync", "post-fsync", "result-write", "late-mode"} {
		t.Run(cut, func(t *testing.T) {
			options, before := flatOwnerFaultFixture(t)
			shape, _, _ := preparedLegacyState(before.State)
			path, current := registrationPaths(options, shape)
			original, err := openBoundPublishedRegistration(path, before)
			if err != nil {
				t.Fatal(err)
			}
			defer original.close()
			record, err := retainedOwnerMember(original.fd, before)
			if err != nil {
				t.Fatal(err)
			}

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
				expected := record
				expected.ChangedSeconds, expected.ChangedNanos = member.ChangedSeconds, member.ChangedNanos
				if member != expected {
					return errors.New("restored registration changed its original member")
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
			var stat unix.Stat_t
			if err := unix.Fstat(original.fd, &stat); err != nil {
				t.Fatal(err)
			}
			if result.ChangedSeconds != stat.Ctim.Sec || result.ChangedNanos != stat.Ctim.Nsec {
				t.Fatal("restored registration result differs from its retained descriptor")
			}

		})
	}
}
