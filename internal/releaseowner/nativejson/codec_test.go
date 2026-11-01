package nativejson

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
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

func TestPrivateNativeFrameRetainsBytesAndRefusesForeignEnvelope(t *testing.T) {
	raw := []byte{0, 255, 1, 2}
	var out bytes.Buffer
	if err := WritePrivateFrame(&out, 3, raw, 64); err != nil {
		t.Fatal(err)
	}
	encoded := out.Bytes()
	if got, err := ReadPrivateFrame(bytes.NewReader(encoded), 3, 64); err != nil || !bytes.Equal(got, raw) {
		t.Fatal("private bytes lost", err)
	}
	for _, part := range []string{"magic", "version", "kind", "reserved", "sequence", "digest", "length", "truncated"} {
		t.Run(part, func(t *testing.T) {
			changed := bytes.Clone(encoded)
			switch part {
			case "magic":
				changed[0]++
			case "version":
				changed[4]++
			case "kind":
				changed[5]++
			case "reserved":
				changed[6]++
			case "sequence":
				changed[15]++
			case "digest":
				changed[16]++
			case "length":
				binary.BigEndian.PutUint32(changed[48:], 65)
			case "truncated":
				changed = changed[:len(changed)-1]
			}
			if _, err := ReadPrivateFrame(bytes.NewReader(changed), 3, 64); err == nil {
				t.Fatal("foreign envelope accepted")
			}
		})
	}
	if err := WritePrivateFrame(io.Discard, 0, raw, 64); err == nil {
		t.Fatal("zero sequence accepted")
	}
	if err := WritePrivateFrame(io.Discard, 3, raw, 3); err == nil {
		t.Fatal("write capacity ignored")
	}
}
