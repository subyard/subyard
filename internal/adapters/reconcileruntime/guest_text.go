package reconcileruntime

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
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
func (text guestText) Matches(declared string) bool { return text.raw != nil && *text.raw == declared }

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
