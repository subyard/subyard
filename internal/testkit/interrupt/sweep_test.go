package interrupt

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/safefs"
	"github.com/Subyard/Subyard/internal/testkit"
)

const toyBugEnv = "SUBYARD_INTERRUPT_TOY_BUG"

// toy is a journaled two-file update: journal, write a, write b, remove journal.
type toy struct {
	dir         string
	bug         string
	runs        *int
	interrupted *bool
	resumes     *int
}

func (s toy) path(name string) string { return filepath.Join(s.dir, name) }

func (s toy) write(t *testing.T, name, body string) {
	testkit.WriteFile(t, s.path(name), []byte(body), 0o600)
}

func (s toy) hit(in *Injector, name string) error {
	err := in.Hit("toy", name)
	*s.interrupted = *s.interrupted || err != nil
	return err
}

func (s toy) run(t *testing.T, in *Injector) Outcome {
	*s.runs++
	if s.bug == "order" && *s.runs%2 == 0 {
		_ = s.hit(in, "stat")
	}
	if s.bug == "commit" {
		s.write(t, "journal", "new")
	}
	if err := s.hit(in, "journal"); err != nil {
		return Outcome{Code: 1, Err: err}
	}
	s.write(t, "journal", "new")
	for _, name := range []string{"a", "b"} {
		if s.bug == "leftover" {
			s.write(t, name+".tmp", "new")
		}
		if err := s.hit(in, "write"); err != nil {
			if s.bug == "late" {
				_ = s.hit(in, "rollback")
			}
			return Outcome{Code: 1, Err: err}
		}
		s.write(t, name, "new")
		if s.bug == "leftover" {
			if err := os.Remove(s.path(name + ".tmp")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if s.bug != "unfired" || *s.runs == 1 {
		if err := s.hit(in, "cleanup"); err != nil {
			return Outcome{Code: 1, Err: err}
		}
	}
	if err := os.Remove(s.path("journal")); err != nil {
		t.Fatal(err)
	}
	return Outcome{}
}

func (s toy) resume(t *testing.T) Outcome {
	*s.resumes++
	if s.bug == "repeat" && *s.interrupted && *s.resumes == 2 {
		s.write(t, "log", "resumed twice\n")
	}
	if _, err := os.Stat(s.path("journal")); errors.Is(err, os.ErrNotExist) {
		return Outcome{}
	}
	a, _ := os.ReadFile(s.path("a"))
	b, _ := os.ReadFile(s.path("b"))
	if s.bug == "rollback" && string(a) == "new" && string(b) == "old" {
		s.write(t, "a", "old")
	} else {
		s.write(t, "a", "new")
		s.write(t, "b", "new")
	}
	if err := os.Remove(s.path("journal")); err != nil {
		t.Fatal(err)
	}
	return Outcome{}
}

func toyCase(bug string, mode Mode) Case[toy] {
	runs := 0
	return Case[toy]{
		Setup: func(t *testing.T) toy {
			s := toy{dir: testkit.TempDir(t), bug: bug, runs: &runs, interrupted: new(bool), resumes: new(int)}
			s.write(t, "a", "old")
			s.write(t, "b", "old")
			return s
		},
		Run:       func(t *testing.T, s toy, in *Injector) Outcome { return s.run(t, in) },
		Resume:    func(t *testing.T, s toy) Outcome { return s.resume(t) },
		Signature: func(t *testing.T, s toy) any { return Tree(t, s.dir) },
		Completed: func(t *testing.T, _ toy, o Outcome) {
			if o.Err != nil {
				t.Fatal(o.Err)
			}
		},
		Interrupted: func(t *testing.T, _ toy, p Point, o Outcome) {
			if !errors.Is(o.Err, ErrInjected) || errors.Is(o.Err, safefs.ErrCrash) != (mode == Crash) {
				t.Fatalf("%s: cut run error %v", p, o.Err)
			}
		},
		Resumed: func(t *testing.T, _ toy, p Point, o Outcome) {
			if o.Err != nil {
				t.Fatalf("%s: %v", p, o.Err)
			}
		},
		Convergence: BeforeOrAfter,
		Mode:        mode,
		MinPoints:   4,
		Require:     []string{"journal", "write", "cleanup"},
	}
}

func toyCommitted(p Point) bool { return p.Ordinal > 1 }

func TestSweepCutsEveryCallInOrder(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		for _, mode := range []Mode{Fail, Crash} {
			c := toyCase("", mode)
			if pinned {
				c.Committed = toyCommitted
			}
			var got []string
			for _, point := range Sweep(t, c) {
				got = append(got, point.String())
			}
			want := []string{"001-toy:journal", "002-toy:write", "003-toy:write", "004-toy:cleanup"}
			if !slices.Equal(got, want) {
				t.Fatalf("points = %v, want %v", got, want)
			}
		}
	}
}

// TestSweepToyBug runs one broken case for TestSweepCatchesBrokenCases in a child process,
// because a failing sweep must fail its own test binary.
func TestSweepToyBug(t *testing.T) {
	bug := os.Getenv(toyBugEnv)
	if bug == "" {
		t.Skip("run by TestSweepCatchesBrokenCases")
	}
	c := toyCase(bug, Fail)
	switch bug {
	case "min-points":
		c.MinPoints = 10
	case "require":
		c.Require = append(c.Require, "fsync")
	case "commit":
		c.Committed = toyCommitted
	}
	Sweep(t, c)
}

func TestSweepCatchesBrokenCases(t *testing.T) {
	for bug, want := range map[string]string{
		"leftover":   `a.tmp: want "", got "file`,
		"repeat":     "001-toy:journal: a second retry changed the state",
		"commit":     "001-toy:journal is before the commit point but did not keep the initial state",
		"order":      "hook order differs from the uncut run",
		"unfired":    "cut 004-toy:cleanup never fired",
		"late":       "the error path reached seams after the cut",
		"rollback":   "003-toy:write rolled back to the initial state",
		"min-points": "the uncut run reached 4 points, want at least 10",
		"require":    `the uncut run never reached "fsync"`,
	} {
		t.Run(bug, func(t *testing.T) {
			command := exec.Command(os.Args[0], "-test.run=^TestSweepToyBug$", "-test.count=1")
			command.Env = append(os.Environ(), toyBugEnv+"="+bug)
			output, err := command.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || !strings.Contains(string(output), want) {
				t.Fatalf("broken case %q: err=%v, want output containing %q:\n%s", bug, err, want, output)
			}
		})
	}
}

func TestInjectorFailsEveryCallFromTheCut(t *testing.T) {
	in := &Injector{cut: 2, mode: Crash}
	fault := in.Safefs()
	if err := fault(safefs.AfterStage); err != nil {
		t.Fatal(err)
	}
	if _, fired := in.Cut(); fired {
		t.Fatal("cut fired early")
	}
	if err := in.Hook("owner")("journal-write"); !errors.Is(err, ErrInjected) || !errors.Is(err, safefs.ErrCrash) {
		t.Fatalf("cut error = %v", err)
	}
	if err := in.Hit("owner", "cleanup"); !errors.Is(err, ErrInjected) {
		t.Fatalf("late call error = %v", err)
	}
	if point, fired := in.Cut(); !fired || point != (Point{Seam: "owner", Name: "journal-write", Ordinal: 2}) {
		t.Fatalf("cut = %v, %v", point, fired)
	}
	if len(in.trace) != 2 || len(in.late) != 1 {
		t.Fatalf("trace=%v late=%v", in.trace, in.late)
	}
}

func TestTreeRecordsTypesModesContentAndLinks(t *testing.T) {
	root := testkit.TempDir(t)
	if got := Tree(t, filepath.Join(root, "missing")); len(got) != 0 {
		t.Fatalf("missing root = %v", got)
	}
	testkit.WriteFile(t, filepath.Join(root, "file"), []byte("body"), 0o640)
	if err := os.Mkdir(filepath.Join(root, "skip"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "skip", "inner"), []byte("x"), 0o600)
	if err := os.Symlink(filepath.Join(root, "file"), filepath.Join(root, "inside")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/elsewhere", filepath.Join(root, "outside")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "shared"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "shared"), 0o700|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	got := Tree(t, root, "skip")
	want := map[string]string{
		".":       "dir -rwx------",
		"file":    "file -rw-r----- 230d8358dc8e8890b4c58deeb62912ee2f20357ae92a5cc861b98e68fe31acb5",
		"inside":  "link -rwxrwxrwx -> $ROOT/file",
		"outside": "link -rwxrwxrwx -> /elsewhere",
		"shared":  "dir trwx------",
	}
	if len(got) != len(want) {
		t.Fatalf("tree = %v", got)
	}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("tree[%s] = %q, want %q", key, got[key], value)
		}
	}
}
