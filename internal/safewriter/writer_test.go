package safewriter

import (
	"bytes"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
)

type shortWriter struct{ err error }

func (w shortWriter) Write([]byte) (int, error) { return 1, w.err }

type opaqueWriter struct {
	buffer *bytes.Buffer
	marker []byte
}

func (w opaqueWriter) Write(p []byte) (int, error) { return w.buffer.Write(p) }

type interfaceWriter struct{ io.Writer }

func TestOutputsSharesAliasesAndSerializesWrites(t *testing.T) {
	var buffer bytes.Buffer
	outputs := Outputs(&buffer, &buffer)
	if outputs[0] != outputs[1] || Outputs(outputs[0])[0] != outputs[0] {
		t.Fatal("aliases or already synchronized output lost their shared lock")
	}
	if _, ok := outputs[0].(io.ReaderFrom); ok {
		t.Fatal("output exposes an unlocked ReaderFrom path")
	}
	var group sync.WaitGroup
	for _, output := range outputs {
		group.Add(1)
		go func(w io.Writer) {
			defer group.Done()
			for range 1000 {
				if _, err := io.WriteString(w, "x"); err != nil {
					t.Error(err)
					return
				}
			}
		}(output)
	}
	group.Wait()
	if buffer.Len() != 2000 {
		t.Fatalf("got %d bytes", buffer.Len())
	}
}

func TestOutputsSerializesOpaqueAliases(t *testing.T) {
	var buffer bytes.Buffer
	outputs := Outputs(opaqueWriter{buffer: &buffer}, interfaceWriter{opaqueWriter{buffer: &buffer}})
	if outputs[0].(*writer).mu != outputs[1].(*writer).mu {
		t.Fatal("opaque aliases have different locks")
	}
	if nested := Outputs(outputs[0], opaqueWriter{buffer: &buffer}); nested[1].(*writer).mu != outputs[0].(*writer).mu {
		t.Fatal("nested output did not reuse the existing lock")
	}
	var group sync.WaitGroup
	for _, output := range outputs {
		group.Add(1)
		go func(w io.Writer) {
			defer group.Done()
			for range 1000 {
				_, _ = w.Write([]byte("x"))
			}
		}(output)
	}
	group.Wait()
	if buffer.Len() != 2000 {
		t.Fatalf("got %d bytes", buffer.Len())
	}
}

func TestOutputsPreservesFileNilAndWriteResult(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	want := errors.New("write failed")
	outputs := Outputs(nil, file, shortWriter{want})
	if outputs[0] != nil || outputs[1] != file {
		t.Fatal("nil or file identity changed")
	}
	if n, err := outputs[2].Write([]byte("ab")); n != 1 || !errors.Is(err, want) {
		t.Fatalf("write result changed: %d %v", n, err)
	}
}
