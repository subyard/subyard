package reconcileruntime

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// guestTextLimit bounds one decoded guest string; longer values refuse the document.
const guestTextLimit = 4096

// guestText is a string a guest program reported. It cannot be rendered: every
// fmt verb prints a fixed placeholder and JSON encodes only its digest. Code
// matches it against host-declared values right after decoding and continues
// with the matched host string. The raw value is held behind a pointer, so
// reflection-based printing of an unexported field shows an address, not text.
type guestText struct{ raw *string }

func (text *guestText) UnmarshalJSON(payload []byte) error {
	if string(payload) == "null" {
		text.raw = nil
		return nil
	}
	var value string
	if err := json.Unmarshal(payload, &value); err != nil {
		return errors.New("guest text must be a JSON string")
	}
	if len(value) > guestTextLimit || !utf8.ValidString(value) {
		return errors.New("guest text is too long or not UTF-8")
	}
	text.raw = &value
	return nil
}

// MarshalJSON keeps distinct values distinct in plan bindings without disclosing them.
func (text guestText) MarshalJSON() ([]byte, error) {
	if text.raw == nil {
		return []byte("null"), nil
	}
	return json.Marshal("guest-text sha256:" + text.Digest())
}

func (guestText) String() string   { return "<guest text>" }
func (guestText) GoString() string { return "<guest text>" }

func (guestText) Format(state fmt.State, _ rune) { _, _ = state.Write([]byte("<guest text>")) }

// Matches reports whether the guest reported exactly the declared host value.
// An absent value equals the empty string, as a decoded string would.
func (text guestText) Matches(declared string) bool {
	if text.raw == nil {
		return declared == ""
	}
	return *text.raw == declared
}

// Digest is the SHA-256 of the reported value, for bindings and opaque labels.
func (text guestText) Digest() string {
	value := ""
	if text.raw != nil {
		value = *text.raw
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(value)))
}

// label names an unmatched value by a short digest only.
func (text guestText) label() string { return "sha256:" + text.Digest()[:12] }

// below reports a value strictly below the declared root directory.
func (text guestText) below(root string) bool {
	return text.raw != nil && root != "" && strings.HasPrefix(*text.raw, strings.TrimSuffix(root, "/")+"/") &&
		!strings.Contains(*text.raw, "/../") && !strings.HasSuffix(*text.raw, "/..")
}

// childWithPrefix reports a direct entry of directory whose name starts with prefix.
func (text guestText) childWithPrefix(directory, prefix string) bool {
	if text.raw == nil {
		return false
	}
	name, found := strings.CutPrefix(*text.raw, directory+"/")
	return found && strings.HasPrefix(name, prefix) && !strings.Contains(name, "/")
}

// wraps reports a value that starts with prefix and ends with suffix.
func (text guestText) wraps(prefix, suffix string) bool {
	return text.raw != nil && len(*text.raw) >= len(prefix)+len(suffix) &&
		strings.HasPrefix(*text.raw, prefix) && strings.HasSuffix(*text.raw, suffix)
}

// fits reports a value that the pattern matches.
func (text guestText) fits(pattern *regexp.Regexp) bool {
	return text.raw != nil && pattern.MatchString(*text.raw)
}

// matchesFold reports a value equal to the declared text ignoring case.
func (text guestText) matchesFold(declared string) bool {
	return text.raw != nil && strings.EqualFold(*text.raw, declared)
}

// guestLines holds each line of raw guest output as guest text.
func guestLines(output []byte) []guestText {
	var lines []guestText
	for _, line := range strings.Split(string(output), "\n") {
		if len(line) <= guestTextLimit {
			lines = append(lines, guestText{raw: &line})
		}
	}
	return lines
}

// guestEcho returns the value only to the guest program that reported it, as
// the operand of an operation on that guest's own record. It is never
// formatted, logged, compared with host text or returned to a caller.
func (text guestText) guestEcho() string {
	if text.raw == nil {
		return ""
	}
	return *text.raw
}

// matchDeclared returns the host string the guest value equals, if any.
func matchDeclared(text guestText, declared ...string) (string, bool) {
	for _, value := range declared {
		if text.Matches(value) {
			return value, true
		}
	}
	return "", false
}
