package domain

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The user-skills guest sync helper reads the same vectors.
func TestNormalizeRemoteURL(t *testing.T) {
	data, err := os.ReadFile("testdata/remote-urls.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		Normalized map[string]string `json:"normalized"`
		Refused    map[string]string `json:"refused"`
	}
	if err := json.Unmarshal(data, &vectors); err != nil || len(vectors.Normalized) == 0 || len(vectors.Refused) == 0 {
		t.Fatalf("vectors: %v", err)
	}
	for input, want := range vectors.Normalized {
		got, err := NormalizeRemoteURL(input)
		if err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", input, got, err, want)
		}
	}
	for input, want := range vectors.Refused {
		if _, err := NormalizeRemoteURL(input); err == nil || err.Error() != want {
			t.Errorf("%q: got %v, want %s", input, err, want)
		}
	}
}

func TestValidateURLListRefusals(t *testing.T) {
	for input, want := range map[string]string{
		"https://user:secret@github.com/o/r": "URL 1 contains credentials",
		"https://token@github.com/o/r":       "URL 1 contains credentials",
		"ssh://u:p@host/r":                   "URL 1 contains credentials",
		"https://github.com/o/r?x=1":         "URL 1 has a query or fragment",
		"https://github.com/o/r#main":        "URL 1 has a query or fragment",
		"http://github.com/o/r":              "URL 1 uses an unsupported scheme",
		"file:///srv/repo":                   "URL 1 uses an unsupported scheme",
		"/srv/repo":                          "URL 1 is not an https, ssh or user@host:path URL",
		"./repo":                             "URL 1 is not an https, ssh or user@host:path URL",
		"-uhttps://x/y":                      "URL 1 starts with '-'",
		"https://github.com/":                "URL 1 has no repository path",
		"https://github.com/o/r\x01":         "URL 1 contains unsupported characters",
		"https://a/b https://A/b.git":        "URL 2 repeats URL 1",
	} {
		err := ValidateURLList(input)
		if err == nil || err.Error() != want {
			t.Errorf("%q: got %v, want %s", input, err, want)
		}
		if err != nil && strings.Contains(err.Error(), "secret") {
			t.Errorf("%q: refusal echoes the value", input)
		}
	}
	if err := ValidateURLList(strings.Repeat("https://h/r ", 0)); err != nil {
		t.Fatalf("empty list: %v", err)
	}
	many := []string{}
	for index := range MaxURLListItems + 1 {
		many = append(many, "https://h/r"+string(rune('a'+index)))
	}
	if err := ValidateURLList(strings.Join(many, " ")); err == nil || !strings.Contains(err.Error(), "more than 8") {
		t.Fatalf("long list: %v", err)
	}
	err := ValidateURLList("http://a/b ftp://c/d")
	if err == nil || !strings.Contains(err.Error(), "URL 1") || !strings.Contains(err.Error(), "URL 2") {
		t.Fatalf("every refusal must be reported: %v", err)
	}
}
