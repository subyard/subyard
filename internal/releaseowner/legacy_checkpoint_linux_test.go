package releaseowner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/sys/unix"
)

type originalV1Reader struct {
	noEffects
	calls  int
	change func()
	actual string
}

func (r *originalV1Reader) ObserveOriginalV1Checkpoint(_ context.Context, _ OriginalV1Expectation, journal, registry []byte) (string, error) {
	r.calls++
	if !bytes.Equal(journal, []byte("original journal")) || !bytes.Equal(registry, []byte("original registry")) {
		panic("different records reached reader")
	}
	if r.change != nil {
		r.change()
	}
	return r.actual, nil
}

func TestOriginalV1DescriptorsRemainExactThroughFactObservation(t *testing.T) {
	for _, change := range []string{"none", "count", "swapped", "digest", "writable", "late-bytes", "late-mode", "scope"} {
		t.Run(change, func(t *testing.T) {
			s, binding, _ := nativeFactFixture(t)
			s.binding = nil
			root, err := IdentityOf(s.root)
			if err != nil {
				t.Fatal(err)
			}
			s.context.Root = root
			reader := &originalV1Reader{actual: binding.Artifact}
			s.components = reader
			files := []*os.File{}
			records := []OriginalV1Record{}
			paths := []string{}
			for i, name := range []string{"journal", "registry"} {
				path := filepath.Join(testkit.TempDir(t), name)
				mode := os.FileMode(0600)
				if i == 1 {
					mode = 0644
				}
				body := []byte("original " + name)
				testkit.WriteFile(t, path, body, mode)
				file, err := os.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { file.Close() })
				var st unix.Stat_t
				if err := unix.Fstat(int(file.Fd()), &st); err != nil {
					t.Fatal(err)
				}
				files = append(files, file)
				records = append(records, OriginalV1Record{originalV1Identity(st), digest(body)})
				paths = append(paths, path)
			}
			expectation := OriginalV1Expectation{Release: "0.4.2", Checkpoint: "042-terminal", Transaction: "original"}
			expectation.RuntimePair.Current = "releases/0.4.2-original"
			expectation.RuntimePair.Previous = "releases/0.4.1-original"
			request := Request{Op: "observe-legacy-checkpoint", Session: binding.Session, OriginalV1: &OriginalV1Checkpoint{expectation, records[0], records[1]}}
			switch change {
			case "count":
				files = files[:1]
			case "swapped":
				files[0], files[1] = files[1], files[0]
			case "digest":
				request.OriginalV1.Journal.Digest = digest([]byte("foreign"))
			case "writable":
				file, err := os.OpenFile(paths[0], os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { file.Close() })
				files[0] = file
			case "late-bytes":
				reader.change = func() {
					if err := os.WriteFile(paths[0], []byte("different"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			case "late-mode":
				reader.change = func() {
					if err := os.Chmod(paths[1], 0600); err != nil {
						t.Fatal(err)
					}
				}
			case "scope":
				request.OriginalV1.Expectation.RuntimePair.Current = "../foreign"
			}
			raw, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			if change == "none" {
				for _, file := range files {
					if _, err := file.Seek(3, 0); err != nil {
						t.Fatal(err)
					}
				}
			}
			reply, err := s.handle(context.Background(), raw, files)
			if change == "none" {
				if err != nil || reply.Actual != binding.Artifact || reader.calls != 1 {
					t.Fatal("original facts refused", err)
				}
				for _, file := range files {
					if offset, err := file.Seek(0, 1); err != nil || offset != 3 {
						t.Fatal("pread consumed original record offset", err)
					}
				}
			} else if err == nil {
				t.Fatal("changed original record accepted")
			}
			if change != "none" && change != "late-bytes" && change != "late-mode" && reader.calls != 0 {
				t.Fatal("bad original record reached component")
			}
		})
	}
}

func TestOriginalV1RecordBoundAndSealedDescriptor(t *testing.T) {
	for _, size := range []int{0, 1 << 20, (1 << 20) + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			body := bytes.Repeat([]byte("x"), size)
			path := filepath.Join(testkit.TempDir(t), "record")
			testkit.WriteFile(t, path, body, 0600)
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			var st unix.Stat_t
			if err := unix.Fstat(int(file.Fd()), &st); err != nil {
				t.Fatal(err)
			}
			raw, err := readOriginalV1Record(file, OriginalV1Record{originalV1Identity(st), digest(body)}, 0600)
			if size == 1<<20 {
				if err != nil || !bytes.Equal(raw, body) {
					t.Fatal("maximum original record refused", err)
				}
			} else if err == nil {
				t.Fatal("record capacity not enforced")
			}
		})
	}
	fd, err := unix.MemfdCreate("original-v1", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		t.Fatal(err)
	}
	file := os.NewFile(uintptr(fd), "original-v1")
	defer file.Close()
	body := []byte("sealed original")
	if _, err := file.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := file.Chmod(0600); err != nil {
		t.Fatal(err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		t.Fatal(err)
	}
	record := OriginalV1Record{originalV1Identity(st), digest(body)}
	if _, err := readOriginalV1Record(file, record, 0600); err == nil {
		t.Fatal("unsealed anonymous original accepted")
	}
	if _, err := unix.FcntlInt(file.Fd(), unix.F_ADD_SEALS, unix.F_SEAL_WRITE|unix.F_SEAL_GROW|unix.F_SEAL_SHRINK|unix.F_SEAL_SEAL); err != nil {
		t.Fatal(err)
	}
	if raw, err := readOriginalV1Record(file, record, 0600); err != nil || !bytes.Equal(raw, body) {
		t.Fatal("sealed original refused", err)
	}
}

func TestRustIngressRequiresExplicitSourceOnlyComponent(t *testing.T) {
	s, b, _ := nativeFactFixture(t)
	s.binding = nil
	s.components = noEffects{}
	id, err := IdentityOf(s.root)
	if err != nil {
		t.Fatal(err)
	}
	s.context.Root = id
	raw, _ := json.Marshal(Request{Op: "capture-ingress", Session: b.Session, LegacyRust: true})
	if _, err := s.handle(context.Background(), raw, nil); err == nil {
		t.Fatal("Rust ingress fell back to historical factory")
	}
}
