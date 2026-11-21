package testkit

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Subyard/Subyard/internal/shellquote"
)

// ExecFake replaces external tools with /bin/sh shims that use only shell
// builtins, so a fake tool on PATH cannot change how another shim records. A
// shim is written once and sources its tool's rules from a file that On
// replaces by rename, so a concurrent call runs either the previous or the new
// rules and never execs a file that is being written. Each call records itself
// in the next free records/N file, which it creates exclusively under set -C;
// a call that cannot write its record exits 126. A call that matches no rule
// exits 127 and fails the test at cleanup unless AllowUnexpected was set.
//
// The check runs in a cleanup, and cleanups run in reverse order: create the
// fake before anything that can still call a tool when its own cleanup runs.
type ExecFake struct {
	t               testing.TB
	state           string
	mu              sync.Mutex
	tools           map[string]string
	order           []string
	rules           []execRule
	allowUnexpected bool
}

// Call is one recorded invocation: the installed tool name and its arguments.
type Call struct {
	Tool string
	Args []string
}

// Match selects calls. Empty fields match anything; set fields must all hold.
type Match struct {
	// Tool is an installed tool name.
	Tool string
	// Args, when non-nil, is the exact argument list; []string{} matches no arguments.
	Args []string
	// Prefix are the leading arguments.
	Prefix []string
	// Contains is a contiguous run of arguments anywhere in the call.
	Contains []string
}

// Reply is a rule's answer: Stdout and Stderr are written first, then Script
// runs as a shell function with the call's arguments and its status is the
// exit status; without Script the shim exits with Exit.
type Reply struct {
	Stdout string
	Stderr string
	Exit   int
	Script string
}

type execRule struct {
	match Match
	reply Reply
}

// NewExecFake installs a shim for each tool name in dir. Put dir first on PATH
// for the code under test.
func NewExecFake(t testing.TB, dir string, tools ...string) *ExecFake {
	t.Helper()
	fake := &ExecFake{t: t, state: TempDir(t), tools: map[string]string{}}
	for _, directory := range []string{"records", "rules"} {
		if err := os.Mkdir(filepath.Join(fake.state, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, tool := range tools {
		if tool == "" || strings.ContainsRune(tool, '/') {
			t.Fatalf("execfake: tool %q must be a bare name; use Install for a path", tool)
		}
		fake.Install(tool, filepath.Join(dir, tool))
	}
	t.Cleanup(fake.reportUnexpected)
	return fake
}

// Install places the shim for name at path, creating missing parents, and
// returns path. Records name calls under name.
func (fake *ExecFake) Install(name, path string) string {
	fake.t.Helper()
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if name == "" || strings.ContainsRune(name, 0) {
		fake.t.Fatalf("execfake: invalid tool name %q", name)
	}
	if _, exists := fake.tools[name]; exists {
		fake.t.Fatalf("execfake: tool %q is already installed", name)
	}
	if !filepath.IsAbs(path) {
		fake.t.Fatalf("execfake: shim path %q is not absolute", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		fake.t.Fatal(err)
	}
	fake.tools[name] = path
	fake.order = append(fake.order, name)
	fake.render(name)
	shim := "#!/bin/sh\n# testkit.ExecFake shim\n. " + shellquote.Word(fake.rulesPath(name)) + "\n"
	fake.publish(path, shim, 0o700)
	return path
}

// Path returns where the shim for name is installed.
func (fake *ExecFake) Path(name string) string {
	fake.t.Helper()
	fake.mu.Lock()
	defer fake.mu.Unlock()
	path, ok := fake.tools[name]
	if !ok {
		fake.t.Fatalf("execfake: tool %q is not installed", name)
	}
	return path
}

// On appends a rule; earlier rules take precedence.
func (fake *ExecFake) On(match Match, reply Reply) *ExecFake {
	fake.t.Helper()
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if match.Tool != "" {
		if _, ok := fake.tools[match.Tool]; !ok {
			fake.t.Fatalf("execfake: rule names uninstalled tool %q", match.Tool)
		}
	}
	if reply.Script != "" && reply.Exit != 0 {
		fake.t.Fatal("execfake: a Script reply sets its own exit status")
	}
	if reply.Exit < 0 || reply.Exit > 255 {
		fake.t.Fatalf("execfake: exit status %d is out of range", reply.Exit)
	}
	if slices.ContainsFunc(slices.Concat([]string{match.Tool, reply.Stdout, reply.Stderr, reply.Script}, match.Args, match.Prefix, match.Contains),
		func(value string) bool { return strings.ContainsRune(value, 0) }) {
		fake.t.Fatal("execfake: a rule cannot contain NUL, which neither an argument nor shell text can hold")
	}
	match = Match{Tool: match.Tool, Args: slices.Clone(match.Args), Prefix: slices.Clone(match.Prefix), Contains: slices.Clone(match.Contains)}
	fake.rules = append(fake.rules, execRule{match: match, reply: reply})
	for _, name := range fake.order {
		if match.Tool == "" || match.Tool == name {
			fake.render(name)
		}
	}
	return fake
}

// AllowUnexpected keeps calls that match no rule from failing the test; they still exit 127.
func (fake *ExecFake) AllowUnexpected() *ExecFake {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.allowUnexpected = true
	return fake
}

// Calls returns the finished calls in log order. Call it only after the code
// under test has stopped running the tools.
func (fake *ExecFake) Calls() []Call {
	fake.t.Helper()
	records, err := fake.records()
	if err != nil {
		fake.t.Fatal(err)
	}
	calls := make([]Call, 0, len(records))
	for _, record := range records {
		calls = append(calls, record.call)
	}
	return calls
}

// AssertCalls fails the test unless the calls are exactly want, in order.
func (fake *ExecFake) AssertCalls(want ...Call) {
	fake.t.Helper()
	got := fake.Calls()
	if !slices.EqualFunc(got, want, func(left, right Call) bool {
		return left.Tool == right.Tool && slices.Equal(left.Args, right.Args)
	}) {
		fake.t.Fatalf("execfake calls:\ngot  %s\nwant %s", formatCalls(got), formatCalls(want))
	}
}

type execRecord struct {
	call Call
	rule int
}

func (fake *ExecFake) records() ([]execRecord, error) {
	entries, err := os.ReadDir(filepath.Join(fake.state, "records"))
	if err != nil {
		return nil, err
	}
	sequence := make([]int, 0, len(entries))
	for _, entry := range entries {
		number, err := strconv.Atoi(entry.Name())
		if err != nil || number < 0 {
			return nil, fmt.Errorf("execfake: malformed call record name %q", entry.Name())
		}
		sequence = append(sequence, number)
	}
	slices.Sort(sequence)
	records := make([]execRecord, 0, len(sequence))
	for _, number := range sequence {
		data, err := os.ReadFile(filepath.Join(fake.state, "records", strconv.Itoa(number)))
		if err != nil {
			return nil, err
		}
		record, err := parseRecord(data)
		if err != nil {
			return nil, fmt.Errorf("execfake: call %d: %w", number, err)
		}
		records = append(records, record)
	}
	return records, nil
}

func parseRecord(data []byte) (execRecord, error) {
	if len(data) == 0 || data[len(data)-1] != 0 {
		return execRecord{}, errors.New("record does not end with NUL")
	}
	fields := strings.Split(string(data[:len(data)-1]), "\x00")
	if len(fields) < 3 {
		return execRecord{}, errors.New("truncated record")
	}
	rule, ruleErr := strconv.Atoi(fields[1])
	count, countErr := strconv.Atoi(fields[2])
	if ruleErr != nil || countErr != nil || count != len(fields)-3 {
		return execRecord{}, fmt.Errorf("malformed record header %q", fields[:3])
	}
	return execRecord{call: Call{Tool: fields[0], Args: slices.Clone(fields[3:])}, rule: rule}, nil
}

func (fake *ExecFake) reportUnexpected() {
	fake.mu.Lock()
	allowed := fake.allowUnexpected
	fake.mu.Unlock()
	records, err := fake.records()
	if err != nil {
		fake.t.Error(err)
		return
	}
	if allowed {
		return
	}
	for _, record := range records {
		if record.rule < 0 {
			fake.t.Errorf("execfake: unexpected call %s", formatCalls([]Call{record.call}))
		}
	}
}

func (fake *ExecFake) rulesPath(name string) string {
	return filepath.Join(fake.state, "rules", strconv.Itoa(slices.Index(fake.order, name))+".sh")
}

// publish writes path through a temporary sibling and renames it into place.
func (fake *ExecFake) publish(path, content string, mode os.FileMode) {
	fake.t.Helper()
	temporary := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".execfake")
	WriteFile(fake.t, temporary, []byte(content), mode)
	if err := os.Rename(temporary, path); err != nil {
		fake.t.Fatal(err)
	}
}

// render replaces the rules that the shim for name sources. They select the
// first matching rule, publish the call record and answer.
func (fake *ExecFake) render(name string) {
	fake.t.Helper()
	var script, chain, replies bytes.Buffer
	script.WriteString("execfake_rule=-1\n")
	first := true
	for index, rule := range fake.rules {
		if rule.match.Tool != "" && rule.match.Tool != name {
			continue
		}
		keyword := "elif"
		if first {
			keyword, first = "if", false
		}
		fmt.Fprintf(&chain, "%s %s; then execfake_rule=%d\n", keyword, matchCondition(&script, index, rule.match), index)
		fake.renderReply(&replies, index, rule.reply)
	}
	if !first {
		chain.WriteString("fi\n")
	}
	script.Write(chain.Bytes())
	records := shellquote.Word(filepath.Join(fake.state, "records"))
	// Builtins only: counting existing records gives a starting number, an
	// empty create under set -C claims it exclusively, and the record is then
	// appended to the claimed file. A create that fails without leaving a file,
	// or too many collisions, is a claim failure; a failed write exits at once
	// and leaves the bad record for records() to report. SIGXFSZ is ignored
	// around the write so a file-size limit yields 126 rather than a signal.
	// The create uses true, a regular builtin, because a failed redirection of
	// the special builtin : would exit the shell.
	fmt.Fprintf(&script, `execfake_call=0 execfake_tries=0
for execfake_record in %[1]s/*; do
  [ -e "$execfake_record" ] && execfake_call=$((execfake_call + 1))
done
set -C
until { true >%[1]s/"$execfake_call"; } 2>/dev/null; do
  execfake_tries=$((execfake_tries + 1))
  if [ ! -e %[1]s/"$execfake_call" ] || [ "$execfake_tries" -ge 4096 ]; then
    printf 'execfake: cannot record a call to %%s\n' %[2]s >&2
    exit 126
  fi
  execfake_call=$((execfake_call + 1))
done
set +C
trap '' XFSZ
if ! { printf '%%s\000' %[2]s "$execfake_rule" "$#" "$@" >>%[1]s/"$execfake_call"; } 2>/dev/null; then
  printf 'execfake: cannot record a call to %%s\n' %[2]s >&2
  exit 126
fi
trap - XFSZ
`, records, shellquote.Word(name))
	script.WriteString("case $execfake_rule in\n")
	script.Write(replies.Bytes())
	fmt.Fprintf(&script, "*) printf 'execfake: unexpected call to %%s\\n' %s >&2; exit 127 ;;\nesac\n", shellquote.Word(name))
	fake.publish(fake.rulesPath(name), script.String(), 0o600)
}

func (fake *ExecFake) renderReply(out *bytes.Buffer, index int, reply Reply) {
	fmt.Fprintf(out, "%d)\n", index)
	if reply.Stdout != "" {
		fmt.Fprintf(out, "  printf '%%s' %s\n", shellquote.Word(reply.Stdout))
	}
	if reply.Stderr != "" {
		fmt.Fprintf(out, "  printf '%%s' %s >&2\n", shellquote.Word(reply.Stderr))
	}
	if reply.Script != "" {
		fmt.Fprintf(out, "  execfake_reply_%d() {\n%s\n}\n  execfake_reply_%d \"$@\"\n  exit $?\n  ;;\n", index, reply.Script, index)
		return
	}
	fmt.Fprintf(out, "  exit %d\n  ;;\n", reply.Exit)
}

// matchCondition returns a shell condition for match. A Contains match
// writes its helper function to script, ahead of the rule chain.
func matchCondition(script *bytes.Buffer, index int, match Match) string {
	conditions := []string{}
	if match.Args != nil {
		conditions = append(conditions, argumentsAt(match.Args, len(match.Args), true))
	}
	if len(match.Prefix) > 0 {
		conditions = append(conditions, argumentsAt(match.Prefix, len(match.Prefix), false))
	}
	if len(match.Contains) > 0 {
		fmt.Fprintf(script, "execfake_contains_%d() {\n  while [ \"$#\" -ge %d ]; do\n    if %s; then return 0; fi\n    shift\n  done\n  return 1\n}\n",
			index, len(match.Contains), argumentsAt(match.Contains, len(match.Contains), false))
		conditions = append(conditions, fmt.Sprintf("execfake_contains_%d \"$@\"", index))
	}
	if len(conditions) == 0 {
		return "true"
	}
	return strings.Join(conditions, " && ")
}

func argumentsAt(values []string, count int, exact bool) string {
	operator := "-ge"
	if exact {
		operator = "-eq"
	}
	terms := []string{fmt.Sprintf("[ \"$#\" %s %d ]", operator, count)}
	for position, value := range values {
		terms = append(terms, fmt.Sprintf("[ \"${%d}\" = %s ]", position+1, shellquote.Word(value)))
	}
	return strings.Join(terms, " && ")
}

func formatCalls(calls []Call) string {
	parts := make([]string, 0, len(calls))
	for _, call := range calls {
		parts = append(parts, fmt.Sprintf("%s%q", call.Tool, call.Args))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
