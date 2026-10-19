package main

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

func TestCanonicalGolden(t *testing.T) {
	b, e := os.ReadFile("../protocol-v1.golden.json")
	if e != nil {
		t.Fatal(e)
	}
	var r request
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e = d.Decode(&r); e != nil {
		t.Fatal(e)
	}
	got, e := json.Marshal(r)
	if e != nil {
		t.Fatal(e)
	}
	var v any
	d = json.NewDecoder(bytes.NewReader(got))
	d.UseNumber()
	if e = d.Decode(&v); e != nil {
		t.Fatal(e)
	}
	got, e = json.Marshal(v)
	if e != nil || !bytes.Equal(got, bytes.TrimSpace(b)) {
		t.Fatalf("golden mismatch %s %v", got, e)
	}
}
