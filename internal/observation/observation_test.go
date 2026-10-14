package observation_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/observation"
)

func TestValidateJSON(t *testing.T) {
	for _, test := range []struct {
		name, input string
		valid       bool
	}{
		{"null", `null`, true},
		{"scalars", `[true,false,1,1.0,1e0,100,12.30,-0,-0.00,9007199254740993]`, true},
		{"objects and arrays", `{"z":[],"a":null,"empty":{},"nested":[{"a":1},{"a":2}]}`, true},
		{"Unicode and escapes", `{"s":"<>&/é\u2028\n","pair":"\ud83d\ude00","literal":"\\ud800","quote":"\""}`, true},
		{"whitespace", " \n\t{\"a\":1}\r ", true},
		{"empty", "", false},
		{"only whitespace", " \n\t", false},
		{"duplicate", `{"x":1,"x":2}`, false},
		{"escaped duplicate", `{"x":1,"\u0078":2}`, false},
		{"nested duplicate", `[{"x":1,"x":2}]`, false},
		{"high surrogate", `"\ud800"`, false},
		{"low surrogate", `"\udc00"`, false},
		{"wrong surrogate pair", `"\ud800\u0041"`, false},
		{"invalid Unicode escape", `"\uXXXX"`, false},
		{"truncated Unicode escape", `"\u12`, false},
		{"invalid UTF-8", "\"\xff\"", false},
		{"invalid escape", `"\x00"`, false},
		{"control character", "\"\x00\"", false},
		{"unclosed string", `"abc`, false},
		{"leading zero", `01`, false},
		{"missing fraction", `1.`, false},
		{"missing exponent", `1e+`, false},
		{"non-JSON number", `NaN`, false},
		{"unclosed object", `{"a":1`, false},
		{"unclosed array", `[1`, false},
		{"missing colon", `{"a" 1}`, false},
		{"trailing comma", `[1,]`, false},
		{"wrong delimiter", `[1}`, false},
		{"trailing document", `{} {}`, false},
		{"trailing garbage", `null x`, false},
		{"array depth limit", strings.Repeat("[", 128) + "0" + strings.Repeat("]", 128), true},
		{"array depth exceeded", strings.Repeat("[", 129) + "0" + strings.Repeat("]", 129), false},
		{"empty array at depth limit", strings.Repeat("[", 129) + strings.Repeat("]", 129), true},
		{"empty array beyond depth limit", strings.Repeat("[", 130) + strings.Repeat("]", 130), false},
		{"object depth limit", strings.Repeat(`{"key":`, 128) + "0" + strings.Repeat("}", 128), true},
		{"object depth exceeded", strings.Repeat(`{"key":`, 129) + "0" + strings.Repeat("}", 129), false},
		{"empty object at depth limit", strings.Repeat(`{"key":`, 128) + "{}" + strings.Repeat("}", 128), true},
		{"byte limit", `"` + strings.Repeat("x", observation.MaxInput-2) + `"`, true},
		{"byte limit exceeded", `"` + strings.Repeat("x", observation.MaxInput-1) + `"`, false},
		{"number token limit", strings.Repeat("1", 1024), true},
		{"number token limit exceeded", strings.Repeat("1", 1025), false},
		{"positive exponent limit", `1e1000000`, true},
		{"negative exponent limit", `1e-1000000`, true},
		{"positive exponent exceeded", `1e1000001`, false},
		{"negative exponent exceeded", `1e-1000001`, false},
		{"exponent overflow", `1e9223372036854775808`, false},
		{"positive normalized exponent limit", `10e999999`, true},
		{"positive normalized exponent exceeded", `10e1000000`, false},
		{"negative normalized exponent limit", `0.1e-999999`, true},
		{"negative normalized exponent exceeded", `0.1e-1000000`, false},
		{"zero ignores normalized exponent", `-0.0e-1000000`, true},
		{"zero still bounds parsed exponent", `0e1000001`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := observation.ValidateJSON([]byte(test.input))
			if (err == nil) != test.valid {
				t.Fatalf("valid = %v, want %v; error = %v", err == nil, test.valid, err)
			}
		})
	}
}

func FuzzValidateJSON(f *testing.F) {
	for _, seed := range []string{`null`, `{"number":9007199254740993,"sequence":[null,{},[]]}`, `"\ud83d\ude00"`, `{"a":1,"a":2}`, `1e-1000000`} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if err := observation.ValidateJSON(data); err != nil {
			return
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, data); err != nil {
			t.Fatalf("accepted invalid JSON: %v", err)
		}
		if err := observation.ValidateJSON(compact.Bytes()); err != nil {
			t.Fatalf("removing whitespace changed validity: %v", err)
		}
	})
}
