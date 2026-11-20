package cli

import "testing"

// Output reaches the buffer in reads of any size, so a write may straddle the
// limit, end exactly on it or start after it. Each case keeps the prefix that
// fits, reports the overflow and consumes the whole write.
func TestBoundedResourceBufferKeepsThePrefixThatFits(t *testing.T) {
	buffer := &boundedResourceBuffer{limit: 4}
	for _, test := range []struct {
		write    string
		content  string
		overflow bool
	}{
		{write: "ab", content: "ab"},
		{write: "cdef", content: "abcd", overflow: true},
		{write: "g", content: "abcd", overflow: true},
	} {
		if written, err := buffer.Write([]byte(test.write)); err != nil || written != len(test.write) {
			t.Fatalf("write %q: written=%d err=%v", test.write, written, err)
		}
		if buffer.String() != test.content || buffer.overflow != test.overflow {
			t.Fatalf("after %q: content=%q overflow=%t", test.write, buffer.String(), buffer.overflow)
		}
	}
	exact := &boundedResourceBuffer{limit: 2}
	if written, err := exact.Write([]byte("ab")); err != nil || written != 2 || exact.String() != "ab" || exact.overflow {
		t.Fatalf("exact fill: content=%q overflow=%t", exact.String(), exact.overflow)
	}
}
