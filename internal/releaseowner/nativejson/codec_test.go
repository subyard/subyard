package nativejson

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func TestLosslessNativeJSONPreservesBytesAndRejectsAlternateGrammars(t *testing.T) {
	type leaf struct {
		Path    string
		Payload []byte
	}
	type record struct {
		Version  int
		Values   []leaf
		Optional *leaf
	}
	original := record{1, []leaf{{"/tmp/source-\xff", []byte{0, 255}}}, &leaf{"\x00<&\u2028", []byte{}}}
	raw, err := EncodeLosslessNativeJSON(original, 4096)
	if err != nil {
		t.Fatal(err)
	}
	var restored record
	if err := DecodeLosslessNativeJSON(raw, &restored, 4096); err != nil || !reflect.DeepEqual(restored, original) {
		t.Fatal("lossless roundtrip", err)
	}
	for _, bad := range [][]byte{append(bytes.Clone(raw), []byte(" {}")...), bytes.Replace(raw, []byte(`"Version":1`), []byte(`"Version":1,"Version":1`), 1), bytes.Replace(raw, []byte(`"Version":1`), []byte(`"Unknown":1,"Version":1`), 1), bytes.Replace(raw, []byte(`"Version":1`), []byte(`"Version": 1`), 1)} {
		if err := DecodeLosslessNativeJSON(bad, &restored, 4096); err == nil {
			t.Fatal("alternate syntax accepted")
		}
	}
	for _, unsupported := range []any{struct{ Value map[string]string }{}, struct{ Value any }{}, struct{ Value json.RawMessage }{}, struct{ hidden string }{}, struct{ leaf }{}, struct{ Value []any }{Value: []any{}}, struct{ Value []json.RawMessage }{Value: []json.RawMessage{}}, struct{ Value [0]json.RawMessage }{}} {
		if _, err := EncodeLosslessNativeJSON(unsupported, 4096); err == nil {
			t.Fatal("unsupported shape accepted")
		}
	}
	if _, err := EncodeLosslessNativeJSON(original, len(raw)-1); err == nil {
		t.Fatal("capacity ignored")
	}
}
