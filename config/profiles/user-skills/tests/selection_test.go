package userskills_test

import (
	"slices"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/profile"
)

func TestDefaultYardSelectsUserSkillsAfterGitHub(t *testing.T) {
	definitions, err := profile.Load("../../../..")
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	var userSkills *profile.Definition
	for index := range definitions {
		if definitions[index].OwnerService != "" {
			names = append(names, definitions[index].Name)
		}
		if definitions[index].Name == "user-skills" {
			userSkills = &definitions[index]
		}
	}
	if userSkills == nil {
		t.Fatal("user-skills profile definition missing")
	}
	// Owner services run in this order: the GitHub broker starts before the first clone.
	if github, own := slices.Index(names, "github"), slices.Index(names, "user-skills"); github < 0 || own < github {
		t.Fatalf("owner service order %v", names)
	}
	for _, test := range []struct {
		name, yard string
		env        map[string]string
		want       bool
	}{
		{"default yard without a saved selection", "default", nil, true},
		{"named yard", "demo", nil, false},
		{"explicit selection without it", "default", map[string]string{"ENVIRONMENT_PROFILES": "github"}, false},
		{"explicit empty selection", "default", map[string]string{"ENVIRONMENT_PROFILES": ""}, false},
		{"named yard selecting it", "demo", map[string]string{"ENVIRONMENT_PROFILES": "github user-skills"}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := userSkills.Selected(test.yard, test.env); got != test.want {
				t.Fatalf("Selected() = %t, want %t", got, test.want)
			}
		})
	}
	if len(userSkills.Settings) != 1 || userSkills.Settings[0].Name != "USER_SKILL_SOURCES" || userSkills.Settings[0].Default == nil ||
		domain.ValidateURLList(*userSkills.Settings[0].Default) != nil {
		t.Fatalf("source setting %+v", userSkills.Settings)
	}
}
