package testyardmigration

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
)

func TestPreparedRegistrationActionsRestoreExactSource(t *testing.T) {
	for _, shape := range []string{"directory-overrides", "flat", "adopt-current"} {
		t.Run(shape, func(t *testing.T) {
			var options Options
			var before Prepared
			switch shape {
			case "flat":
				options, before = flatOwnerFaultFixture(t)
			case "adopt-current":
				options, before = adoptCurrentOwnerFaultFixture(t)
			default:
				options, before = ownerFaultFixture(t, true, true)
			}
			options.RunYard = func(context.Context, string, io.Writer, ...string) error {
				t.Fatal("filesystem action ran a yard command")
				return nil
			}
			var restored int
			options.RecordRegistrationRestore = func(PreparedOwnerMember) error { restored++; return nil }
			for _, action := range []string{"validate-original", "publish-registration", "remove-empty-flat", "move-overrides", "archive-registration", "restore-registration", "restore-overrides", "park-current-registration", "verify-compensated"} {
				for repeat := 0; repeat < 2; repeat++ {
					if err := ApplyPreparedRegistrationAction(context.Background(), options, before, action); err != nil {
						t.Fatalf("%s repeat %d: %v", action, repeat, err)
					}
				}
			}
			if restored != 1 {
				t.Fatal("restoration fact was lost or synthesized on replay", restored)
			}
			setFakeProjects(t, filepath.Dir(options.ConfigHome), "none")
			if err := ApplyPreparedRegistrationAction(context.Background(), options, before, "verify-compensated"); err == nil {
				t.Fatal("missing original project accepted as compensated")
			}
			if shape != "adopt-current" {
				options.AllowMissingLegacyProject = true
				if err := ApplyPreparedRegistrationAction(context.Background(), options, before, "validate-original"); err != nil {
					t.Fatal("captured absent project source", err)
				}
				if err := ApplyPreparedRegistrationAction(context.Background(), options, before, "verify-compensated"); err != nil {
					t.Fatal("unchanged absent project preflight", err)
				}
				setFakeProjects(t, filepath.Dir(options.ConfigHome), "current")
				if err := ApplyPreparedRegistrationAction(context.Background(), options, before, "verify-compensated"); err == nil {
					t.Fatal("foreign current project admitted by missing-legacy allowance")
				}
				options.AllowMissingLegacyProject = false
			}
			project := "legacy"
			if shape == "adopt-current" {
				project = "current"
			}
			setFakeProjects(t, filepath.Dir(options.ConfigHome), project)
			if err := ApplyPreparedRegistrationAction(context.Background(), options, before, "unknown"); err == nil {
				t.Fatal("unknown action admitted")
			}
			old, _ := registrationPaths(options, expectedLegacyState(before.State))
			write(t, old, "YARD_TEMPLATE=test-vms\nSSH_PORT=2999\n")
			if err := ApplyPreparedRegistrationAction(context.Background(), options, before, "verify-compensated"); err == nil {
				t.Fatal("changed original registration admitted as compensated")
			}
		})
	}
}

func TestPreparedRegistrationControllerArchiveIsNotCompensatable(t *testing.T) {
	options, before := ownerFaultFixture(t, true, false)
	for _, action := range []string{"publish-registration", "move-overrides", "archive-registration", "archive-controller"} {
		if err := ApplyPreparedRegistrationAction(context.Background(), options, before, action); err != nil {
			t.Fatal(action, err)
		}
	}
	if err := ApplyPreparedRegistrationAction(context.Background(), options, before, "verify-compensated"); err == nil {
		t.Fatal("archived controller accepted as restored original state")
	}
	options.fault = func(point string) error {
		if point == "after-legacy-controller-archive" {
			return errors.New("archive sync cut")
		}
		return nil
	}
	if err := ApplyPreparedRegistrationAction(context.Background(), options, before, "archive-controller"); !isResumableOwnerMutationError(err) {
		t.Fatal("archive failure lost resumable classification", err)
	}
	options.fault = nil
	setFakeProjects(t, filepath.Dir(options.ConfigHome), "current")
	if err := ApplyPreparedRegistrationAction(context.Background(), options, before, "cleanup-owned-archives"); err != nil {
		t.Fatal(err)
	}
}
