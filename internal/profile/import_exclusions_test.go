package profile

import (
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestCredentialImportExclusionDeclarations(t *testing.T) {
	tooMany := make([][]string, 33)
	for index := range tooMany {
		tooMany[index] = []string{"/directory/"}
	}
	tooManyFragments := make([]string, 9)
	for index := range tooManyFragments {
		tooManyFragments[index] = "/directory/"
	}
	for _, item := range []struct {
		name       string
		exclusions [][]string
		valid      bool
	}{
		{"absent", nil, true},
		{"conjunctions", [][]string{{"/mutable/", "/sessions/"}, {"/.config/tool/"}}, true},
		{"empty-conjunction", [][]string{{}}, false},
		{"empty-fragment", [][]string{{""}}, false},
		{"relative", [][]string{{"directory/"}}, false},
		{"missing-final-slash", [][]string{{"/directory"}}, false},
		{"root", [][]string{{"/"}}, false},
		{"traversal", [][]string{{"/directory/../"}}, false},
		{"unnormalized", [][]string{{"/directory//child/"}}, false},
		{"backslash", [][]string{{"/directory\\child/"}}, false},
		{"control", [][]string{{"/directory\n/"}}, false},
		{"glob", [][]string{{"/directory/*/"}}, false},
		{"too-long", [][]string{{"/" + strings.Repeat("a", 255) + "/"}}, false},
		{"too-many-fragments", [][]string{tooManyFragments}, false},
		{"too-many-conjunctions", tooMany, false},
	} {
		t.Run(item.name, func(t *testing.T) {
			root := testkit.TempDir(t)
			fixture(t, root, "fixture", "", Definition{CredentialImportExclusions: item.exclusions})
			if _, err := Load(root); (err == nil) != item.valid {
				t.Fatalf("valid=%t err=%v", item.valid, err)
			}
		})
	}
}
