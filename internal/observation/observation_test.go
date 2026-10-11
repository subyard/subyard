package observation_test

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/observation"
	"github.com/Subyard/Subyard/internal/testkit"
)

func encoded(t *testing.T, value observation.Value) []byte {
	t.Helper()
	snapshot, err := observation.NewSnapshot(value)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot.Bytes()
}
func typed(raw observation.RawValue) observation.Value {
	if value, ok := raw.Scalar(); ok {
		return value
	}
	if fields, ok := raw.Fields(); ok {
		values := map[string]observation.Value{}
		for key, raw := range fields {
			values[key] = typed(raw)
		}
		return observation.Object(values)
	}
	items, _ := raw.Elements()
	values := make([]observation.Value, len(items))
	for i, raw := range items {
		values[i] = typed(raw)
	}
	return observation.Ordered(values)
}
func TestCanonicalGolden(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{`{"z":[],"a":null,"empty":{},"s":"<>&/é\u2028\n"}`, `{"a":null,"empty":{},"s":"<>&/é\u2028\n","z":[]}`},
		{`[1,1.0,1e0,100,12.30,-0,-0.00,9007199254740993,1e-1000000]`, `[1,1,1,1e2,123e-1,0,0,9007199254740993,1e-1000000]`},
		{`{"a":"\ud83d\ude00","b":"\\ud800"}`, `{"a":"😀","b":"\\ud800"}`},
	} {
		raw, err := observation.Decode([]byte(test.input))
		if err != nil {
			t.Fatal(err)
		}
		if got := string(encoded(t, typed(raw))); got != test.want {
			t.Fatalf("canonical = %s, want %s", got, test.want)
		}
	}
	for _, input := range []string{`{"x":1,"x":2}`, `{"x":1,"\u0078":2}`, `"\ud800"`, `"\udc00"`, `"\ud800\u0041"`, `01`, `NaN`, `1e1000001`, `{} {}`, "\"\xff\"", strings.Repeat("[", 130) + "0" + strings.Repeat("]", 130)} {
		if _, err := observation.Decode([]byte(input)); err == nil {
			t.Fatalf("invalid input accepted: %q", input)
		}
	}
	if _, err := observation.Fingerprint(observation.Snapshot{}); err == nil {
		t.Fatal("zero snapshot fingerprinted")
	}
	if _, err := observation.NewSnapshot(observation.Value{}); err == nil {
		t.Fatal("zero value accepted")
	}
}

func TestImmutableInputsAndCollectionSemantics(t *testing.T) {
	items := []observation.Value{observation.String("b"), observation.String("a"), observation.String("a")}
	fields := map[string]observation.Value{"ordered": observation.Ordered(items), "unordered": observation.Multiset(items)}
	value := observation.Object(fields)
	want := `{"ordered":["b","a","a"],"unordered":["a","a","b"]}`
	fields["ordered"] = observation.Null()
	items[0] = observation.Null()
	snapshot, err := observation.NewSnapshot(value)
	if err != nil {
		t.Fatal(err)
	}
	copy := snapshot.Bytes()
	copy[0] = '!'
	if string(snapshot.Bytes()) != want {
		t.Fatal("caller mutated snapshot")
	}
	for _, raw := range []string{`null`, `[]`, `{}`} {
		parsed, err := observation.Decode([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if string(encoded(t, typed(parsed))) != raw {
			t.Fatal("empty/null distinction lost")
		}
	}
}

func TestDepthAndByteBoundaries(t *testing.T) {
	for _, data := range []string{strings.Repeat("[", 128) + "0" + strings.Repeat("]", 128), strings.Repeat(`{"key":`, 128) + "0" + strings.Repeat("}", 128)} {
		raw, err := observation.Decode([]byte(data))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := observation.NewSnapshot(typed(raw)); err != nil {
			t.Fatal(err)
		}
	}
	value := observation.Ordered([]observation.Value{observation.String(strings.Repeat("x", observation.MaxInput-4))})
	if len(encoded(t, value)) != observation.MaxInput {
		t.Fatal("exact byte bound rejected")
	}
	if _, err := observation.NewSnapshot(observation.Ordered([]observation.Value{observation.String(strings.Repeat("x", observation.MaxInput-3))})); err == nil {
		t.Fatal("byte bound exceeded")
	}
}

func TestBoundedGeneratedMultisetInvariants(t *testing.T) {
	random := rand.New(rand.NewSource(7))
	for trial := 0; trial < 64; trial++ {
		records := make([]observation.Value, 2+random.Intn(12))
		for i := range records {
			records[i] = observation.Object(map[string]observation.Value{"path": observation.String(fmt.Sprintf("/synthetic/%d/%d", trial, i)), "flag": observation.Bool(random.Intn(2) == 0)})
		}
		records = append(records, records[0])
		before := encoded(t, observation.Multiset(records))
		random.Shuffle(len(records), func(i, j int) { records[i], records[j] = records[j], records[i] })
		if !bytes.Equal(before, encoded(t, observation.Multiset(records))) {
			t.Fatal("enumeration order changed multiset")
		}
		if bytes.Equal(before, encoded(t, observation.Multiset(records[1:]))) {
			t.Fatal("multiplicity change hidden")
		}
		ordered := encoded(t, observation.Ordered(records))
		records[0], records[len(records)-1] = records[len(records)-1], records[0]
		// The first and last records can be the duplicated pair. A distinct
		// replacement always checks real field drift independently of ordering.
		if records[0] != records[len(records)-1] && bytes.Equal(ordered, encoded(t, observation.Ordered(records))) {
			t.Fatal("ordered permutation hidden")
		}
		records[0] = observation.Object(map[string]observation.Value{"changed": observation.Bool(true)})
		if bytes.Equal(before, encoded(t, observation.Multiset(records))) {
			t.Fatal("record field change hidden")
		}
	}
}

func TestCompilerRejectsUntypedEvidence(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, expression string
		valid            bool
	}{
		{"snapshot", "observation.Snapshot{}", true},
		{"raw_collection", "[]observation.Value{}", false},
		{"raw_json", "[]byte(`[]`)", false},
		{"source_digest", "observation.DigestSource(nil)", false},
		{"health", "advisoryHealth(true)", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := testkit.TempDir(t)
			testkit.WriteFile(t, filepath.Join(directory, "go.mod"), []byte("module github.com/Subyard/Subyard/negative\n\ngo 1.26.5\nrequire github.com/Subyard/Subyard v0.0.0\nreplace github.com/Subyard/Subyard => "+root+"\n"), 0644)
			testkit.WriteFile(t, filepath.Join(directory, "contract.go"), []byte("package negative\nimport \"github.com/Subyard/Subyard/internal/observation\"\ntype advisoryHealth bool\nvar _, _ = observation.Fingerprint("+test.expression+")\n"), 0644)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, "go", "test", "-mod=mod", ".")
			command.Dir = directory
			command.Env = append(os.Environ(), "TMPDIR=/tmp")
			output, err := command.CombinedOutput()
			if test.valid && err != nil {
				t.Fatalf("positive compiler control: %v: %s", err, output)
			}
			if !test.valid && (err == nil || !bytes.Contains(output, []byte("cannot use"))) {
				t.Fatalf("negative compiler result: %v: %s", err, output)
			}
		})
	}
}

func FuzzDecode(f *testing.F) {
	for _, seed := range []string{`null`, `{"number":9007199254740993,"sequence":[null,{},[]]}`, `"\ud83d\ude00"`, `{"a":1,"a":2}`} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		raw, err := observation.Decode(data)
		if err != nil {
			return
		}
		snapshot, err := observation.NewSnapshot(typed(raw))
		if err != nil {
			return
		}
		canonical := snapshot.Bytes()
		again, err := observation.Decode(canonical)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(canonical, encoded(t, typed(again))) {
			t.Fatal("canonical encoding not idempotent")
		}
	})
}
