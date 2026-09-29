package github_test

import (
	"github.com/Subyard/Subyard/internal/profile"
	"testing"
)

func TestProfileDefaultsAndExplicitSelection(t *testing.T) {
	definitions, err := profile.Load("../../../..")
	if err != nil {
		t.Fatal(err)
	}
	var github *profile.Definition
	for i := range definitions {
		if definitions[i].Name == "github" {
			github = &definitions[i]
			break
		}
	}
	if github == nil {
		t.Fatal("GitHub profile definition missing")
	}
	cases := []struct {
		name string
		yard string
		env  map[string]string
		want bool
	}{
		{name: "default yard", yard: "default", want: true},
		{name: "other yard", yard: "hermes", want: false},
		{name: "explicit enable", yard: "hermes", env: map[string]string{"ENVIRONMENT_PROFILES": "base github"}, want: true},
		{name: "explicit disable", yard: "default", env: map[string]string{"ENVIRONMENT_PROFILES": "base"}, want: false},
		{name: "nested vm disabled", yard: "default", env: map[string]string{"NESTED_E2E_VMS": "1", "ENVIRONMENT_PROFILES": "github"}, want: false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := github.Selected(test.yard, test.env); got != test.want {
				t.Fatalf("Selected() = %t, want %t", got, test.want)
			}
		})
	}
}
