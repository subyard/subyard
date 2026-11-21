// Package interrupt sweeps an operation through every interruption point its seams expose.
//
// A sweep first runs the operation once without a cut and records every hook call in order.
// Points are numbered by call count, so a seam reached several times is cut each time. For
// each point the sweep then builds fresh state, cuts the run at that call, retries, requires
// the state to converge, and retries again to require that the second retry changes nothing.
//
// Error injection only behaves like a crash where the product's error path does no cleanup
// or rollback. A deferred rollback turns a cut into "failure, then rollback", and a deferred
// temporary-file removal hides the leftovers a real crash keeps. A case that relies on an
// error-returning seam must say why its error path is crash-faithful, use Crash where the
// seam honours safefs.ErrCrash, or leave the gap documented in the test.
//
// Signature must return an independent snapshot: the sweep keeps the uncut and initial
// signatures and compares later ones against them, so a value that shares mutable state
// with the fixture would change together with it.
package interrupt

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/safefs"
)

// A Point is one observed hook call. Ordinal is its 1-based position in a run.
type Point struct {
	Seam, Name string
	Ordinal    int
}

func (p Point) String() string {
	if p.Ordinal == 0 {
		return "uncut run"
	}
	return fmt.Sprintf("%03d-%s:%s", p.Ordinal, p.Seam, p.Name)
}

// Mode chooses the error a cut returns.
type Mode int

const (
	// Fail returns ErrInjected; the product's error path runs.
	Fail Mode = iota
	// Crash also wraps safefs.ErrCrash, so safefs keeps stage files as a real crash would.
	Crash
)

// ErrInjected marks every cut; errors.Is finds it in both modes.
var ErrInjected = errors.New("interrupt: injected interruption")

// An Injector counts hook calls and cuts at one of them. Every call from the cut on fails,
// as nothing runs after a crash; calls after the cut are reported as a fidelity failure.
type Injector struct {
	cut   int
	mode  Mode
	trace []Point
	late  []Point
}

// Hit records one call of a seam and returns the cut error at the selected point.
func (in *Injector) Hit(seam, name string) error {
	if in.cut > 0 && len(in.trace) >= in.cut {
		in.late = append(in.late, Point{Seam: seam, Name: name, Ordinal: len(in.trace) + len(in.late) + 1})
		return in.err()
	}
	in.trace = append(in.trace, Point{Seam: seam, Name: name, Ordinal: len(in.trace) + 1})
	if len(in.trace) == in.cut {
		return in.err()
	}
	return nil
}

// Hook adapts a name-keyed product hook such as func(boundary string) error.
func (in *Injector) Hook(seam string) func(string) error {
	return func(name string) error { return in.Hit(seam, name) }
}

// Safefs adapts safefs.Options.Fault and safefs.DirOptions.Fault.
func (in *Injector) Safefs() func(safefs.Point) error {
	return func(point safefs.Point) error { return in.Hit("safefs", string(point)) }
}

// Cut returns the point that fired, if any.
func (in *Injector) Cut() (Point, bool) {
	if in.cut == 0 || len(in.trace) < in.cut {
		return Point{}, false
	}
	return in.trace[in.cut-1], true
}

func (in *Injector) err() error {
	if in.mode == Crash {
		return fmt.Errorf("%w: %w", ErrInjected, safefs.ErrCrash)
	}
	return ErrInjected
}

// Outcome is what the operator sees: an exit code or error and the rendered text.
type Outcome struct {
	Code   int
	Err    error
	Output string
}

// Convergence says which states a retried cut may settle in.
type Convergence int

const (
	// AfterOnly requires every cut to converge to the uninterrupted final state.
	AfterOnly Convergence = iota
	// BeforeOrAfter also accepts the state right after Setup, but only before the commit
	// point: once one cut converges to the final state, every later cut must too.
	BeforeOrAfter
)

// A Case describes one operation. Setup, Run, Signature, Completed, Interrupted and Resumed
// are required.
type Case[S any] struct {
	Setup func(t *testing.T) S
	// Run is the first attempt; it wires the injector into the product seams.
	Run func(t *testing.T, s S, in *Injector) Outcome
	// Resume retries or recovers. Nil means Run again without a cut.
	Resume func(t *testing.T, s S) Outcome
	// Signature returns an independent, comparable snapshot of the state, for example Tree
	// plus domain reads.
	Signature func(t *testing.T, s S) any
	// Completed checks the outcome and domain meaning of the uncut run once; Signature
	// equality carries that meaning to every cut that converges to the final state.
	Completed func(t *testing.T, s S, o Outcome)
	// Interrupted checks the operator outcome of the cut run, such as retry guidance.
	Interrupted func(t *testing.T, s S, p Point, o Outcome)
	// Resumed checks the operator outcome of the first retry after the cut at p.
	Resumed func(t *testing.T, s S, p Point, o Outcome)
	// Repeated checks the outcome of the second retry, after the uncut run (p.Ordinal 0)
	// or after a cut. Nil means only the unchanged Signature is required.
	Repeated func(t *testing.T, s S, p Point, o Outcome)

	Convergence Convergence
	// Committed, when set, pins the side of a known commit point: a cut at p must converge
	// to the final state when it returns true and to the initial state otherwise. It
	// overrides Convergence.
	Committed func(p Point) bool
	Mode      Mode
	// MinPoints guards against a refactor that silently removes seam calls.
	MinPoints int
	// Require lists seam names that the uncut run must reach.
	Require []string
}

// Sweep runs c at every point and returns the points of the uncut run.
func Sweep[S any](t *testing.T, c Case[S]) []Point {
	t.Helper()
	if c.Setup == nil || c.Run == nil || c.Signature == nil || c.Completed == nil ||
		c.Interrupted == nil || c.Resumed == nil {
		t.Fatal("interrupt: Setup, Run, Signature, Completed, Interrupted and Resumed are required")
	}
	resume := c.Resume
	if resume == nil {
		resume = func(t *testing.T, s S) Outcome { return c.Run(t, s, &Injector{mode: c.Mode}) }
	}
	// The uncut run stays in t itself, so -run can select a single cut.
	s := c.Setup(t)
	before := c.Signature(t, s)
	in := &Injector{mode: c.Mode}
	c.Completed(t, s, c.Run(t, s, in))
	points := in.trace
	after := c.Signature(t, s)
	repeat(t, c, resume, s, Point{}, after)
	checkDiscovery(t, points, c.MinPoints, c.Require)
	committed := false
	for _, point := range points {
		t.Run(point.String(), func(t *testing.T) {
			s := c.Setup(t)
			in := &Injector{cut: point.Ordinal, mode: c.Mode}
			outcome := c.Run(t, s, in)
			if _, fired := in.Cut(); !fired {
				t.Fatalf("cut %s never fired; the run reached %d of %d points:\n%s",
					point, len(in.trace), len(points), traceDiff(points, in.trace))
			}
			if !samePrefix(points, in.trace, point.Ordinal) {
				t.Fatalf("hook order differs from the uncut run before %s:\n%s", point, traceDiff(points, in.trace))
			}
			if len(in.late) > 0 {
				t.Fatalf("the error path reached seams after the cut at %s, so the cut is not crash-like: %v", point, in.late)
			}
			c.Interrupted(t, s, point, outcome)
			c.Resumed(t, s, point, resume(t, s))
			got := c.Signature(t, s)
			switch {
			case c.Committed != nil && c.Committed(point):
				if !reflect.DeepEqual(got, after) {
					t.Fatalf("%s is past the commit point but did not converge to the final state:\n%s", point, signatureDiff(after, got))
				}
			case c.Committed != nil:
				if !reflect.DeepEqual(got, before) {
					t.Fatalf("%s is before the commit point but did not keep the initial state:\n%s", point, signatureDiff(before, got))
				}
			case reflect.DeepEqual(got, after):
				committed = true
			case c.Convergence == BeforeOrAfter && !committed && reflect.DeepEqual(got, before):
			case c.Convergence == BeforeOrAfter && committed && reflect.DeepEqual(got, before):
				t.Fatalf("%s rolled back to the initial state after an earlier cut had converged forward", point)
			case c.Convergence == BeforeOrAfter:
				t.Fatalf("%s did not converge.\nAgainst the final state:\n%s\nAgainst the initial state:\n%s",
					point, signatureDiff(after, got), signatureDiff(before, got))
			default:
				t.Fatalf("%s did not converge:\n%s", point, signatureDiff(after, got))
			}
			repeat(t, c, resume, s, point, got)
		})
	}
	return points
}

func repeat[S any](t *testing.T, c Case[S], resume func(*testing.T, S) Outcome, s S, p Point, want any) {
	t.Helper()
	outcome := resume(t, s)
	if c.Repeated != nil {
		c.Repeated(t, s, p, outcome)
	}
	if got := c.Signature(t, s); !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: a second retry changed the state:\n%s", p, signatureDiff(want, got))
	}
}

func checkDiscovery(t *testing.T, points []Point, minimum int, require []string) {
	t.Helper()
	if len(points) == 0 || len(points) < minimum {
		t.Fatalf("the uncut run reached %d points, want at least %d", len(points), max(minimum, 1))
	}
	for _, name := range require {
		if !slices.ContainsFunc(points, func(p Point) bool { return p.Name == name }) {
			t.Fatalf("the uncut run never reached %q", name)
		}
	}
}

func samePrefix(want, got []Point, n int) bool {
	return len(got) >= n && len(want) >= n && slices.Equal(want[:n], got[:n])
}

func traceDiff(want, got []Point) string {
	var out strings.Builder
	for i := range max(len(want), len(got)) {
		var w, g string
		if i < len(want) {
			w = want[i].String()
		}
		if i < len(got) {
			g = got[i].String()
		}
		marker := "  "
		if w != g {
			marker = "! "
		}
		fmt.Fprintf(&out, "%suncut %-40s cut run %s\n", marker, w, g)
	}
	return out.String()
}

func signatureDiff(want, got any) string {
	wantMap, wantOK := want.(map[string]string)
	gotMap, gotOK := got.(map[string]string)
	if !wantOK || !gotOK {
		return fmt.Sprintf("want %#v\n got %#v", want, got)
	}
	var keys []string
	for key := range wantMap {
		keys = append(keys, key)
	}
	for key := range gotMap {
		if _, ok := wantMap[key]; !ok {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	var out strings.Builder
	for _, key := range keys {
		if w, g := wantMap[key], gotMap[key]; w != g {
			fmt.Fprintf(&out, "%s: want %q, got %q\n", key, w, g)
		}
	}
	return out.String()
}
