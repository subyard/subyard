package domain

import (
	"testing"
)

func TestFormatConfigurationDiagnostics(t *testing.T) {
	yards := []YardConfigurationDiagnostic{
		{Yard: "work", Unavailable: "installed configuration unknown (no applied baseline)"},
		{Yard: "old", Unavailable: "stopped; not inspected"},
		{Yard: "default", Differences: []ConfigurationDifference{
			{Name: "TOKEN", Installed: "old secret", Desired: "new secret", InstalledSet: true, DesiredSet: true, Kind: ConfigurationSensitive},
			{Name: "SOURCE", Installed: "/a", Desired: "/a", InstalledSet: true, DesiredSet: true, Kind: ConfigurationContentChanged},
			{Name: "DEV_USER", Installed: "dev", Desired: "other", InstalledSet: true, DesiredSet: true},
			{Name: "PROFILES", Desired: "sample", DesiredSet: true},
		}},
	}
	want := "yard default:\n  DEV_USER  dev -> other\n  PROFILES  (unset) -> sample\n  SOURCE  content changed\n  TOKEN  changed\nyard old: stopped; not inspected\nyard work: installed configuration unknown (no applied baseline)"
	if got := FormatConfigurationDiagnostics(yards); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
