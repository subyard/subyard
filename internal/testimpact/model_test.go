package testimpact

import (
	"bytes"
	"strings"
	"testing"
)

func TestDecodeChangeSetAcceptsEveryStatusShapeAndSortsCanonically(t *testing.T) {
	input := `{
		"schema_version": 1,
		"changes": [
			{"status":"T","similarity":null,"old_path":"type","new_path":"type","old_mode":"100644","new_mode":"120000"},
			{"status":"R","similarity":75,"old_path":"rename-from","new_path":"rename-to","old_mode":"100755","new_mode":"100755"},
			{"status":"M","similarity":null,"old_path":"modify","new_path":"modify","old_mode":"100644","new_mode":"100755"},
			{"status":"D","similarity":null,"old_path":"delete","new_path":null,"old_mode":"160000","new_mode":"000000"},
			{"status":"C","similarity":100,"old_path":"copy-from","new_path":"copy-to","old_mode":"100644","new_mode":"100644"},
			{"status":"A","similarity":null,"old_path":null,"new_path":"add","old_mode":"000000","new_mode":"100644"}
		]
	}`

	set, err := DecodeChangeSet(strings.NewReader(input))
	if err != nil {
		t.Fatalf("DecodeChangeSet() error = %v", err)
	}
	if set.SchemaVersion != 1 {
		t.Fatalf("SchemaVersion = %d, want 1", set.SchemaVersion)
	}
	got := make([]string, len(set.Changes))
	for i, change := range set.Changes {
		got[i] = change.Status
	}
	if want := []string{"A", "C", "D", "M", "R", "T"}; !equalStrings(got, want) {
		t.Fatalf("canonical statuses = %v, want %v", got, want)
	}
	if set.Changes[0].OldPath != nil || stringValue(set.Changes[0].NewPath) != "add" {
		t.Fatalf("A identity = %#v", set.Changes[0])
	}
	if stringValue(set.Changes[1].OldPath) != "copy-from" || stringValue(set.Changes[1].NewPath) != "copy-to" {
		t.Fatalf("C identities = %#v", set.Changes[1])
	}
	if stringValue(set.Changes[2].OldPath) != "delete" || set.Changes[2].NewPath != nil {
		t.Fatalf("D identity = %#v", set.Changes[2])
	}
	if set.Changes[4].Similarity == nil || *set.Changes[4].Similarity != 75 {
		t.Fatalf("R similarity = %#v", set.Changes[4].Similarity)
	}
}

func TestDecodeChangeSetAcceptsEscapedUnicodeSurrogatePair(t *testing.T) {
	input := `{"schema_version":1,"changes":[{"status":"A","similarity":null,"old_path":null,"new_path":"docs/\ud83d\ude80.md","old_mode":"000000","new_mode":"100644"}]}`

	set, err := DecodeChangeSet(strings.NewReader(input))
	if err != nil {
		t.Fatalf("DecodeChangeSet() error = %v", err)
	}
	if got := stringValue(set.Changes[0].NewPath); got != "docs/🚀.md" {
		t.Fatalf("NewPath = %q, want escaped surrogate pair decoded", got)
	}
}

func TestRejectUnpairedUnicodeSurrogateEscapes(t *testing.T) {
	tests := []struct {
		name    string
		source  string
		wantErr bool
	}{
		{"paired surrogate", `{"value":"\ud83d\ude80"}`, false},
		{"literal escape text", `{"value":"\\ud800"}`, false},
		{"pair beside escaped quote and slash", `{"value":"\"\ud83d\ude80\/"}`, false},
		{"lone high surrogate", `{"value":"\ud800"}`, true},
		{"lone low surrogate", `{"value":"\udc00"}`, true},
		{"high followed by BMP escape", `{"value":"\ud800\u0041"}`, true},
		{"high followed by literal low escape text", `{"value":"\ud800\\udc00"}`, true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := rejectUnpairedUnicodeSurrogateEscapes([]byte(test.source))
			if (err != nil) != test.wantErr {
				t.Fatalf("rejectUnpairedUnicodeSurrogateEscapes() error = %v, wantErr %t", err, test.wantErr)
			}
		})
	}
}

func TestDecodeChangeSetRejectsInvalidDocuments(t *testing.T) {
	validChange := `{"status":"M","similarity":null,"old_path":"file","new_path":"file","old_mode":"100644","new_mode":"100644"}`
	validDocument := `{"schema_version":1,"changes":[` + validChange + `]}`

	tests := []struct {
		name  string
		input []byte
	}{
		{"root unknown field", []byte(`{"schema_version":1,"changes":[],"extra":true}`)},
		{"record unknown field", []byte(`{"schema_version":1,"changes":[` + validChange[:len(validChange)-1] + `,"extra":true}]}`)},
		{"root duplicate key", []byte(`{"schema_version":1,"schema_version":1,"changes":[]}`)},
		{"record duplicate key", []byte(`{"schema_version":1,"changes":[{"status":"M","status":"M","similarity":null,"old_path":"file","new_path":"file","old_mode":"100644","new_mode":"100644"}]}`)},
		{"exact duplicate record", []byte(`{"schema_version":1,"changes":[` + validChange + `,` + validChange + `]}`)},
		{"conflicting record", []byte(`{"schema_version":1,"changes":[` + validChange + `,{"status":"M","similarity":null,"old_path":"file","new_path":"file","old_mode":"100644","new_mode":"100755"}]}`)},
		{"invalid utf8", append([]byte(`{"schema_version":1,"changes":[{"status":"M","similarity":null,"old_path":"`), append([]byte{0xff}, []byte(`","new_path":"file","old_mode":"100644","new_mode":"100644"}]}`)...)...)},
		{"lone escaped surrogate", []byte(`{"schema_version":1,"changes":[{"status":"A","similarity":null,"old_path":null,"new_path":"docs/\ud800.md","old_mode":"000000","new_mode":"100644"}]}`)},
		{"NUL path", []byte(`{"schema_version":1,"changes":[{"status":"M","similarity":null,"old_path":"file\u0000name","new_path":"file\u0000name","old_mode":"100644","new_mode":"100644"}]}`)},
		{"empty path", []byte(`{"schema_version":1,"changes":[{"status":"M","similarity":null,"old_path":"","new_path":"","old_mode":"100644","new_mode":"100644"}]}`)},
		{"absolute path", []byte(`{"schema_version":1,"changes":[{"status":"M","similarity":null,"old_path":"/file","new_path":"/file","old_mode":"100644","new_mode":"100644"}]}`)},
		{"traversal path", []byte(`{"schema_version":1,"changes":[{"status":"M","similarity":null,"old_path":"../file","new_path":"../file","old_mode":"100644","new_mode":"100644"}]}`)},
		{"trailing JSON value", []byte(validDocument + ` null`)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := DecodeChangeSet(bytes.NewReader(test.input)); err == nil {
				t.Fatal("DecodeChangeSet() accepted invalid document")
			}
		})
	}
}

func TestDecodeChangeSetRejectsRootAndRecordPresenceAndTypeViolations(t *testing.T) {
	validChange := `{"status":"M","similarity":null,"old_path":"file","new_path":"file","old_mode":"100644","new_mode":"100644"}`
	tests := []struct {
		name  string
		input string
	}{
		{"root is null", `null`},
		{"root is array", `[]`},
		{"root is string", `"change-set"`},
		{"root is empty", ``},
		{"schema version omitted", `{"changes":[]}`},
		{"changes omitted", `{"schema_version":1}`},
		{"schema version null", `{"schema_version":null,"changes":[]}`},
		{"changes null", `{"schema_version":1,"changes":null}`},
		{"schema version is fractional", `{"schema_version":1.0,"changes":[]}`},
		{"schema version is string", `{"schema_version":"1","changes":[]}`},
		{"schema version is zero", `{"schema_version":0,"changes":[]}`},
		{"schema version is unsupported", `{"schema_version":2,"changes":[]}`},
		{"changes is object", `{"schema_version":1,"changes":{}}`},
		{"changes is string", `{"schema_version":1,"changes":"change"}`},
		{"record is null", `{"schema_version":1,"changes":[null]}`},
		{"record is array", `{"schema_version":1,"changes":[[]]}`},
		{"status omitted", `{"schema_version":1,"changes":[{"similarity":null,"old_path":"file","new_path":"file","old_mode":"100644","new_mode":"100644"}]}`},
		{"similarity omitted", `{"schema_version":1,"changes":[{"status":"M","old_path":"file","new_path":"file","old_mode":"100644","new_mode":"100644"}]}`},
		{"old path omitted", `{"schema_version":1,"changes":[{"status":"M","similarity":null,"new_path":"file","old_mode":"100644","new_mode":"100644"}]}`},
		{"new path omitted", `{"schema_version":1,"changes":[{"status":"M","similarity":null,"old_path":"file","old_mode":"100644","new_mode":"100644"}]}`},
		{"old mode omitted", `{"schema_version":1,"changes":[{"status":"M","similarity":null,"old_path":"file","new_path":"file","new_mode":"100644"}]}`},
		{"new mode omitted", `{"schema_version":1,"changes":[{"status":"M","similarity":null,"old_path":"file","new_path":"file","old_mode":"100644"}]}`},
		{"status null", `{"schema_version":1,"changes":[{"status":null,"similarity":null,"old_path":"file","new_path":"file","old_mode":"100644","new_mode":"100644"}]}`},
		{"old mode null", `{"schema_version":1,"changes":[{"status":"M","similarity":null,"old_path":"file","new_path":"file","old_mode":null,"new_mode":"100644"}]}`},
		{"new mode null", `{"schema_version":1,"changes":[{"status":"M","similarity":null,"old_path":"file","new_path":"file","old_mode":"100644","new_mode":null}]}`},
		{"modify old path null", `{"schema_version":1,"changes":[{"status":"M","similarity":null,"old_path":null,"new_path":"file","old_mode":"100644","new_mode":"100644"}]}`},
		{"modify new path null", `{"schema_version":1,"changes":[{"status":"M","similarity":null,"old_path":"file","new_path":null,"old_mode":"100644","new_mode":"100644"}]}`},
		{"modify similarity number", `{"schema_version":1,"changes":[{"status":"M","similarity":0,"old_path":"file","new_path":"file","old_mode":"100644","new_mode":"100644"}]}`},
		{"rename similarity null", `{"schema_version":1,"changes":[{"status":"R","similarity":null,"old_path":"old","new_path":"new","old_mode":"100644","new_mode":"100644"}]}`},
		{"record scalar field wrong type", `{"schema_version":1,"changes":[{"status":1,"similarity":null,"old_path":"file","new_path":"file","old_mode":"100644","new_mode":"100644"}]}`},
		{"valid document control", `{"schema_version":1,"changes":[` + validChange + `]}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := DecodeChangeSet(strings.NewReader(test.input))
			if test.name == "valid document control" {
				if err != nil {
					t.Fatalf("DecodeChangeSet() error = %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("DecodeChangeSet() accepted invalid document")
			}
		})
	}
}

func TestValidateChangeAcceptsEveryAllowedStatusModeCombination(t *testing.T) {
	modes := []string{"100644", "100755", "120000", "160000"}
	path := func(value string) *string { return &value }
	similarity := 0

	for _, mode := range modes {
		t.Run("A new mode "+mode, func(t *testing.T) {
			if err := ValidateChange(Change{Status: "A", NewPath: path("added"), OldMode: "000000", NewMode: mode}); err != nil {
				t.Fatalf("ValidateChange() error = %v", err)
			}
		})
		t.Run("D old mode "+mode, func(t *testing.T) {
			if err := ValidateChange(Change{Status: "D", OldPath: path("deleted"), OldMode: mode, NewMode: "000000"}); err != nil {
				t.Fatalf("ValidateChange() error = %v", err)
			}
		})
	}

	for _, status := range []string{"M", "T", "R", "C"} {
		for _, oldMode := range modes {
			for _, newMode := range modes {
				t.Run(status+" modes "+oldMode+" to "+newMode, func(t *testing.T) {
					change := Change{Status: status, OldMode: oldMode, NewMode: newMode}
					switch status {
					case "M", "T":
						change.OldPath = path("same")
						change.NewPath = path("same")
					case "R", "C":
						change.OldPath = path("old")
						change.NewPath = path("new")
						change.Similarity = &similarity
					}
					if err := ValidateChange(change); err != nil {
						t.Fatalf("ValidateChange() error = %v", err)
					}
				})
			}
		}
	}
}

func TestValidateChangeRejectsInvalidStatusModeAndShapeCombinations(t *testing.T) {
	similarity := 50
	path := func(value string) *string { return &value }

	tests := []struct {
		name   string
		change Change
	}{
		{"unknown status", Change{Status: "U", OldPath: path("file"), NewPath: path("file"), OldMode: "100644", NewMode: "100644"}},
		{"combined status", Change{Status: "AM", NewPath: path("file"), OldMode: "000000", NewMode: "100644"}},
		{"unmerged status", Change{Status: "U", OldPath: path("file"), NewPath: path("file"), OldMode: "100644", NewMode: "100644"}},
		{"add with old path", Change{Status: "A", OldPath: path("old"), NewPath: path("new"), OldMode: "000000", NewMode: "100644"}},
		{"add missing new path", Change{Status: "A", OldMode: "000000", NewMode: "100644"}},
		{"delete with new path", Change{Status: "D", OldPath: path("old"), NewPath: path("new"), OldMode: "100644", NewMode: "000000"}},
		{"delete missing old path", Change{Status: "D", OldMode: "100644", NewMode: "000000"}},
		{"modify paths differ", Change{Status: "M", OldPath: path("old"), NewPath: path("new"), OldMode: "100644", NewMode: "100644"}},
		{"type paths differ", Change{Status: "T", OldPath: path("old"), NewPath: path("new"), OldMode: "100644", NewMode: "100644"}},
		{"rename paths equal", Change{Status: "R", Similarity: &similarity, OldPath: path("same"), NewPath: path("same"), OldMode: "100644", NewMode: "100644"}},
		{"copy paths equal", Change{Status: "C", Similarity: &similarity, OldPath: path("same"), NewPath: path("same"), OldMode: "100644", NewMode: "100644"}},
		{"copy missing similarity", Change{Status: "C", OldPath: path("old"), NewPath: path("new"), OldMode: "100644", NewMode: "100644"}},
		{"similarity out of range", Change{Status: "R", Similarity: intValue(101), OldPath: path("old"), NewPath: path("new"), OldMode: "100644", NewMode: "100644"}},
		{"negative similarity", Change{Status: "C", Similarity: intValue(-1), OldPath: path("old"), NewPath: path("new"), OldMode: "100644", NewMode: "100644"}},
		{"add with similarity", Change{Status: "A", Similarity: &similarity, NewPath: path("new"), OldMode: "000000", NewMode: "100644"}},
		{"delete with similarity", Change{Status: "D", Similarity: &similarity, OldPath: path("old"), OldMode: "100644", NewMode: "000000"}},
		{"modify with similarity", Change{Status: "M", Similarity: &similarity, OldPath: path("file"), NewPath: path("file"), OldMode: "100644", NewMode: "100644"}},
		{"type with similarity", Change{Status: "T", Similarity: &similarity, OldPath: path("file"), NewPath: path("file"), OldMode: "100644", NewMode: "100644"}},
		{"add wrong old mode", Change{Status: "A", NewPath: path("new"), OldMode: "100644", NewMode: "100644"}},
		{"add zero new mode", Change{Status: "A", NewPath: path("new"), OldMode: "000000", NewMode: "000000"}},
		{"delete zero old mode", Change{Status: "D", OldPath: path("old"), OldMode: "000000", NewMode: "000000"}},
		{"delete wrong new mode", Change{Status: "D", OldPath: path("old"), OldMode: "100644", NewMode: "100644"}},
		{"invalid mode", Change{Status: "M", OldPath: path("file"), NewPath: path("file"), OldMode: "100600", NewMode: "100644"}},
		{"zero mode on modification", Change{Status: "M", OldPath: path("file"), NewPath: path("file"), OldMode: "000000", NewMode: "100644"}},
		{"type zero new mode", Change{Status: "T", OldPath: path("file"), NewPath: path("file"), OldMode: "100644", NewMode: "000000"}},
		{"rename zero old mode", Change{Status: "R", Similarity: &similarity, OldPath: path("old"), NewPath: path("new"), OldMode: "000000", NewMode: "100644"}},
		{"copy invalid new mode", Change{Status: "C", Similarity: &similarity, OldPath: path("old"), NewPath: path("new"), OldMode: "100644", NewMode: "100600"}},
		{"add empty new path", Change{Status: "A", NewPath: path(""), OldMode: "000000", NewMode: "100644"}},
		{"delete absolute old path", Change{Status: "D", OldPath: path("/old"), OldMode: "100644", NewMode: "000000"}},
		{"modify traversal path", Change{Status: "M", OldPath: path("dir/../file"), NewPath: path("dir/../file"), OldMode: "100644", NewMode: "100644"}},
		{"type missing old path", Change{Status: "T", NewPath: path("file"), OldMode: "100644", NewMode: "100644"}},
		{"rename missing new path", Change{Status: "R", Similarity: &similarity, OldPath: path("old"), OldMode: "100644", NewMode: "100644"}},
		{"copy empty old path", Change{Status: "C", Similarity: &similarity, OldPath: path(""), NewPath: path("new"), OldMode: "100644", NewMode: "100644"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateChange(test.change); err == nil {
				t.Fatalf("ValidateChange(%#v) accepted invalid change", test.change)
			}
		})
	}
}

func TestDecodeChangeSetAppliesConflictRules(t *testing.T) {
	change := func(status, oldPath, newPath, oldMode, newMode, similarity string) string {
		return `{"status":"` + status + `","similarity":` + similarity + `,"old_path":` + oldPath + `,"new_path":` + newPath + `,"old_mode":"` + oldMode + `","new_mode":"` + newMode + `"}`
	}
	document := func(changes ...string) string {
		return `{"schema_version":1,"changes":[` + strings.Join(changes, ",") + `]}`
	}
	copy := func(source, destination string) string {
		return change("C", `"`+source+`"`, `"`+destination+`"`, "100644", "100644", "100")
	}
	modify := func(file string) string {
		return change("M", `"`+file+`"`, `"`+file+`"`, "100644", "100644", "null")
	}
	typeChange := func(file string) string {
		return change("T", `"`+file+`"`, `"`+file+`"`, "100644", "120000", "null")
	}
	add := func(file string) string {
		return change("A", "null", `"`+file+`"`, "000000", "100644", "null")
	}
	remove := func(file string) string {
		return change("D", `"`+file+`"`, "null", "100644", "000000", "null")
	}
	rename := func(source, destination string) string {
		return change("R", `"`+source+`"`, `"`+destination+`"`, "100644", "100644", "50")
	}

	tests := []struct {
		name    string
		input   string
		allowed bool
	}{
		{"multiple copies may share a source", document(copy("source", "first"), copy("source", "second")), true},
		{"copy and source modification may share a source", document(copy("source", "destination"), modify("source")), true},
		{"copy and source type change may share a source", document(copy("source", "destination"), typeChange("source")), true},
		{"source modification and copy may share a source", document(modify("source"), copy("source", "destination")), true},
		{"source type change and copy may share a source", document(typeChange("source"), copy("source", "destination")), true},
		{"add and modify cannot produce the same path", document(add("path"), modify("path")), false},
		{"add and rename cannot produce the same path", document(add("path"), rename("source", "path")), false},
		{"copy and delete cannot share a source", document(copy("source", "destination"), remove("source")), false},
		{"delete and copy cannot share a source", document(remove("source"), copy("source", "destination")), false},
		{"copy and rename cannot share a source", document(copy("source", "destination"), rename("source", "renamed")), false},
		{"rename and copy cannot share a source", document(rename("source", "renamed"), copy("source", "destination")), false},
		{"modify and rename cannot share a source", document(modify("source"), rename("source", "renamed")), false},
		{"modify and type change cannot share a source", document(modify("source"), typeChange("source")), false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := DecodeChangeSet(strings.NewReader(test.input))
			if test.allowed && err != nil {
				t.Fatalf("DecodeChangeSet() error = %v", err)
			}
			if !test.allowed && err == nil {
				t.Fatal("DecodeChangeSet() accepted conflicting changes")
			}
		})
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func intValue(value int) *int {
	return &value
}
