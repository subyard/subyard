package safefs_test

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Subyard/Subyard/internal/safefs"
	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/sys/unix"
)

func TestWriteInterruption(t *testing.T) {
	failure := errors.New("injected fault")
	for _, noReplace := range []bool{false, true} {
		for _, point := range []safefs.Point{safefs.AfterStage, safefs.AfterWrite, safefs.BeforePublish, safefs.AfterPublish} {
			for _, crash := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/no-replace=%t/crash=%t", point, noReplace, crash), func(t *testing.T) {
					root := testkit.TempDir(t)
					path := filepath.Join(root, "record.json")
					if !noReplace {
						testkit.WriteFile(t, path, []byte("previous"), 0o600)
					}
					expectedErr := failure
					if crash {
						expectedErr = safefs.ErrCrash
					}
					var hit bool
					o := safefs.Options{NoReplace: noReplace, Fault: func(observed safefs.Point) error {
						if observed == point {
							hit = true
							return fmt.Errorf("failure at %s: %w", point, expectedErr)
						}
						return nil
					}}
					err := safefs.WriteFile(path, []byte("replacement"), o)
					if !hit || !errors.Is(err, expectedErr) {
						t.Fatalf("hit=%t, write returned %v", hit, err)
					}
					published := point == safefs.AfterPublish
					switch {
					case published:
						checkContents(t, path, "replacement")
					case noReplace:
						if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
							t.Fatalf("target stat before publish = %v", err)
						}
					default:
						checkContents(t, path, "previous")
					}
					stages, err := filepath.Glob(filepath.Join(root, ".record.json.tmp-*"))
					if err != nil {
						t.Fatal(err)
					}
					wantStages := 0
					if crash && !published {
						wantStages = 1
					}
					if len(stages) != wantStages {
						t.Fatalf("staging files = %q, want %d", stages, wantStages)
					}
					if wantStages == 1 {
						want := "replacement"
						if point == safefs.AfterStage {
							want = ""
						}
						checkContents(t, stages[0], want)
						checkMode(t, stages[0], 0o600)
					}
					o.Fault = nil
					err = safefs.WriteFile(path, []byte("replacement"), o)
					if noReplace && published {
						if !errors.Is(err, fs.ErrExist) {
							t.Fatalf("no-replace retry = %v, want ErrExist", err)
						}
					} else if err != nil {
						t.Fatalf("retry = %v", err)
					}
					checkContents(t, path, "replacement")
				})
			}
		}
	}
}

func TestWriteModesUnderUmask(t *testing.T) {
	for _, mask := range []int{0, 0o022, 0o077, 0o277} {
		t.Run(fmt.Sprintf("%04o", mask), func(t *testing.T) {
			root := testkit.TempDir(t)
			previous := unix.Umask(mask)
			t.Cleanup(func() { unix.Umask(previous) })
			parent := filepath.Join(root, "outer", "inner")
			path := filepath.Join(parent, "file")
			if err := safefs.WriteFile(path, nil, safefs.Options{Mode: 0o640, ParentMode: 0o755}); err != nil {
				t.Fatal(err)
			}
			checkContents(t, path, "")
			checkMode(t, filepath.Dir(parent), 0o755)
			checkMode(t, parent, 0o755)
			checkMode(t, path, 0o640)
			if err := safefs.WriteFile(path, []byte("new"), safefs.Options{PreserveMode: true}); err != nil {
				t.Fatal(err)
			}
			checkContents(t, path, "new")
			checkMode(t, path, 0o640)
			if err := safefs.EnsurePrivateDir(filepath.Join(root, "private")); err != nil {
				t.Fatal(err)
			}
			checkMode(t, filepath.Join(root, "private"), 0o700)
		})
	}
}

func TestWriteRefusesSpecialTargetsAndBreaksHardlink(t *testing.T) {
	root := testkit.TempDir(t)
	original := filepath.Join(root, "original")
	testkit.WriteFile(t, original, []byte("original"), 0o600)
	link, hardlink, fifo := filepath.Join(root, "symlink"), filepath.Join(root, "hardlink"), filepath.Join(root, "fifo")
	for _, err := range []error{os.Symlink(original, link), os.Link(original, hardlink), unix.Mkfifo(fifo, 0o600)} {
		if err != nil {
			t.Fatal(err)
		}
	}
	for path, want := range map[string]error{link: safefs.ErrSymlink, fifo: safefs.ErrNotRegular, root: safefs.ErrNotRegular} {
		err := safefs.WriteFile(path, []byte("bad"), safefs.Options{})
		if !errors.Is(err, want) {
			t.Errorf("write %s = %v, want %v", path, err, want)
		}
		_, err = safefs.ReadRegular(path, safefs.ReadOptions{})
		if !errors.Is(err, want) {
			t.Errorf("read %s = %v, want %v", path, err, want)
		}
	}
	if _, err := safefs.ReadRegular(original, safefs.ReadOptions{SingleLink: true}); !errors.Is(err, safefs.ErrMultipleLinks) {
		t.Fatalf("read linked file = %v", err)
	}
	if err := safefs.WriteFile(hardlink, []byte("changed"), safefs.Options{}); err != nil {
		t.Fatal(err)
	}
	checkContents(t, original, "original")
	checkContents(t, hardlink, "changed")
	if _, err := safefs.ReadRegular(original, safefs.ReadOptions{MaxBytes: 2}); !errors.Is(err, safefs.ErrTooLarge) {
		t.Fatalf("read beyond size limit = %v", err)
	}
	if _, err := safefs.ReadRegular(original, safefs.ReadOptions{DenyPerm: 0o200}); !errors.Is(err, safefs.ErrUnsafeMode) {
		t.Fatalf("read with unsafe mode = %v", err)
	}
	foreign := uint32(os.Geteuid()) + 1
	if _, err := safefs.ReadRegular(original, safefs.ReadOptions{OwnerUID: &foreign}); !errors.Is(err, safefs.ErrUnsafeOwner) {
		t.Fatalf("read with foreign owner = %v", err)
	}
}

func TestWriteWithRootAndPinnedParent(t *testing.T) {
	root, outside := testkit.TempDir(t), testkit.TempDir(t)
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	broad := filepath.Join(root, "broad")
	if err := os.Mkdir(broad, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(broad, 0o772); err != nil {
		t.Fatal(err)
	}
	o := safefs.Options{Root: root, ParentMode: 0o700}
	for path, want := range map[string]error{
		filepath.Join(link, "new", "file"): safefs.ErrSymlink,
		filepath.Join(outside, "file"):     safefs.ErrEscapesRoot,
		filepath.Join(broad, "file"):       safefs.ErrUnsafeMode,
	} {
		if err := safefs.WriteFile(path, nil, o); !errors.Is(err, want) {
			t.Errorf("write %s = %v, want %v", path, err, want)
		}
	}
	for _, noReplace := range []bool{false, true} {
		name := fmt.Sprintf("no-replace=%t", noReplace)
		t.Run(name, func(t *testing.T) {
			o.NoReplace = noReplace
			parent := filepath.Join(root, name)
			path := filepath.Join(parent, "file")
			pending, err := safefs.Create(path, o)
			if err != nil {
				t.Fatal(err)
			}
			defer pending.Abort()
			if _, err := pending.File().WriteString("pinned"); err != nil {
				t.Fatal(err)
			}
			moved := filepath.Join(root, name+"-moved")
			if err := os.Rename(parent, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, parent); err != nil {
				t.Fatal(err)
			}
			if err := pending.Commit(); err != nil {
				t.Fatal(err)
			}
			checkContents(t, filepath.Join(moved, "file"), "pinned")
			if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
				t.Fatalf("write escaped the pinned parent: %v, %v", entries, err)
			}
		})
	}
}

func TestWriteRefusesSubstitutedStagingInode(t *testing.T) {
	for _, noReplace := range []bool{false, true} {
		t.Run(fmt.Sprintf("no-replace=%t", noReplace), func(t *testing.T) {
			root := testkit.TempDir(t)
			if err := os.Chmod(root, 0o770); err != nil {
				t.Fatal(err)
			}
			path, stage := filepath.Join(root, "record"), filepath.Join(root, ".stage")
			if !noReplace {
				testkit.WriteFile(t, path, []byte("previous"), 0o600)
			}
			o := safefs.Options{NoReplace: noReplace, TempPattern: ".stage", Fault: func(point safefs.Point) error {
				if point == safefs.BeforePublish {
					if err := os.Rename(stage, stage+".moved"); err != nil {
						return err
					}
					testkit.WriteFile(t, stage, []byte("different inode"), 0o600)
				}
				return nil
			}}
			if err := safefs.WriteFile(path, []byte("intended"), o); !errors.Is(err, safefs.ErrStageChanged) {
				t.Fatalf("publish substituted staging inode: %v", err)
			}
			if noReplace {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("substituted inode was published: %v", err)
				}
			} else {
				checkContents(t, path, "previous")
			}
			checkContents(t, stage, "different inode")
		})
	}
}

func TestNoReplaceConcurrentWriters(t *testing.T) {
	path := filepath.Join(testkit.TempDir(t), "record")
	type outcome struct {
		writer string
		err    error
	}
	results := make(chan outcome, 12)
	start := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(cap(results))
	for n := range cap(results) {
		go func() {
			value := fmt.Sprint(n)
			ready.Done()
			<-start
			results <- outcome{value, safefs.WriteFile(path, []byte(value), safefs.Options{NoReplace: true})}
		}()
	}
	ready.Wait()
	close(start)
	winners := 0
	for range cap(results) {
		result := <-results
		switch {
		case result.err == nil:
			winners++
			checkContents(t, path, result.writer)
		case !errors.Is(result.err, fs.ErrExist):
			t.Errorf("write %s: %v", result.writer, result.err)
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want 1", winners)
	}
}

func TestFixedStageAndCallerClose(t *testing.T) {
	root := testkit.TempDir(t)
	path := filepath.Join(root, "state")
	o := safefs.Options{TempPattern: "state.tmp"}
	pending, err := safefs.Create(path, o)
	if err != nil {
		t.Fatal(err)
	}
	defer pending.Abort()
	if pending.Name() != filepath.Join(root, "state.tmp") {
		t.Fatalf("staging name = %q", pending.Name())
	}
	if err := safefs.WriteFile(path, nil, o); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("concurrent fixed stage = %v", err)
	}
	if _, err := pending.File().WriteString("staged"); err != nil {
		t.Fatal(err)
	}
	if err := pending.File().Sync(); err != nil {
		t.Fatal(err)
	}
	if err := pending.File().Close(); err != nil {
		t.Fatal(err)
	}
	if err := pending.Publish(); err != nil {
		t.Fatal(err)
	}
	if err := pending.SyncParent(); err != nil {
		t.Fatal(err)
	}
	pending.Abort()
	checkContents(t, path, "staged")
}

func checkContents(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != want {
		t.Fatalf("read %s = %q, %v, want %q", path, data, err, want)
	}
}

func checkMode(t *testing.T, path string, want fs.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky); mode != want {
		t.Fatalf("mode of %s = %v, want %v", path, mode, want)
	}
}
