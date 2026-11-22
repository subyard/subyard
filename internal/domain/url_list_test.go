package domain

import (
	"strings"
	"testing"
)

func TestNormalizeRemoteURL(t *testing.T) {
	for input, want := range map[string]string{
		"https://GitHub.com/Owner/dev-flow.git/": "https://github.com/Owner/dev-flow",
		"https://github.com:443/owner/repo":      "https://github.com/owner/repo",
		"https://git.example:8443/owner/repo":    "https://git.example:8443/owner/repo",
		"ssh://git@Example.org:22/srv/repo.git":  "ssh://git@example.org/srv/repo",
		"ssh://example.org:2222/srv/repo":        "ssh://example.org:2222/srv/repo",
		"git@GitHub.com:owner/repo.git":          "git@github.com:owner/repo",
		"dev@localhost:/srv/repo/":               "dev@localhost:/srv/repo",
	} {
		got, err := NormalizeRemoteURL(input)
		if err != nil || got != want {
			t.Errorf("%s: got %q, %v; want %q", input, got, err, want)
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
