package configmaterial

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

// localGuest runs the real guest programs on this host: /home/dev lives under
// root, the materialization state under state, and hook is Python prepended
// to every program, which may define CAS_HOOK(point).
type localGuest struct {
	root, state, hook string
}

func newLocalGuest(t *testing.T) *localGuest {
	root := testkit.TempDir(t)
	if err := os.MkdirAll(filepath.Join(root, "home", "dev", ".config"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &localGuest{root: root, state: filepath.Join(root, "state")}
}

func (guest *localGuest) path(destination string) string { return guest.root + destination }

func (guest *localGuest) Exec(_ context.Context, _, _ string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	arguments := slices.Clone(request.Command)
	for index, argument := range arguments {
		switch {
		case index == 3:
			argument = strings.Replace(argument, `STATE_ROOT = "`+guestStateRoot+`"`, fmt.Sprintf("STATE_ROOT = %q", guest.state), 1)
			argument = guest.hook + "\n" + strings.Replace(argument, `STATE_UID = int("0")`, fmt.Sprintf(`STATE_UID = int("%d")`, os.Getuid()), 1)
		case argument == "/home/dev" || strings.HasPrefix(argument, "/home/dev/"):
			argument = guest.path(argument)
		}
		arguments[index] = argument
	}
	command := exec.Command(arguments[0], arguments[1:]...)
	command.Stdin = bytes.NewReader(request.Stdin)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	// The guest reports its paths as the yard names them.
	result := ports.InstanceExecResult{ExitCode: command.ProcessState.ExitCode(), Stdout: bytes.ReplaceAll(stdout.Bytes(), []byte(guest.root), nil), Stderr: stderr.Bytes()}
	if errors.As(err, new(*exec.ExitError)) {
		return result, &ports.GuestExitError{Code: result.ExitCode}
	}
	return result, err
}

var localYard = domain.Context{YardName: "named", IncusProject: "subyard", YardInstanceName: "yard", DevUser: "dev", DevUID: os.Getuid()}

// write runs one refresh write of desired over token, as the runtime does.
func (guest *localGuest) write(t *testing.T, desired Desired, token string) (ports.InstanceExecResult, error) {
	t.Helper()
	request, err := RawRequest("dev", desired.Destination, os.Getuid(), desired.Payload, token)
	if desired.Format != "" {
		request, err = ApplyRequest(desired.Format, ModeApply, "dev", desired.Destination, os.Getuid(), desired.Payload, token)
	}
	if err != nil {
		t.Fatal(err)
	}
	return guest.Exec(context.Background(), "", "", request)
}

func (guest *localGuest) observe(t *testing.T, desired Desired) File {
	t.Helper()
	files, err := Observe(context.Background(), guest, localYard, []Desired{desired})
	if err != nil || len(files) != 1 {
		t.Fatalf("observe: %v", err)
	}
	return files[0]
}

// entry is what the guest has at a path: its type and, for a file, its bytes.
func entry(path string) string {
	info, err := os.Lstat(path)
	if err != nil {
		return "missing"
	}
	if info.Mode().IsRegular() {
		payload, _ := os.ReadFile(path)
		return "file:" + string(payload)
	}
	return info.Mode().Type().String()
}

// Every guest state of each kind of config file, observed and then written
// with that observation's token by the real guest programs.
func TestObserveRefusalsAndWriteOnRealGuestPrograms(t *testing.T) {
	desired := map[string][]byte{"": []byte("desired\n"), "json": []byte(`{"managed":true}`), "toml": []byte("sandbox_mode = 'danger-full-access'\n")}
	other := map[string]string{"": "old\n", "json": `{"other":1}`, "toml": "other = 1\n"}
	invalid := map[string]string{"": "not a document {", "json": "{invalid", "toml": "= invalid"}
	apply := "run yard -Y named config apply"
	for _, format := range []string{"", "json", "toml"} {
		for _, test := range []struct {
			kind, state, step, write string
		}{
			{kind: "absent", state: "drift", write: "written"},
			{kind: "match", state: "converged", write: "converged"},
			{kind: "differ", state: "drift", write: "written"},
			{kind: "symlink", state: "unreadable", step: "replace it with a regular file or remove it, then " + apply, write: "unreadable"},
			{kind: "directory", state: "unreadable", step: "replace it with a regular file or remove it, then " + apply, write: "unreadable"},
			{kind: "fifo", state: "unreadable", step: "replace it with a regular file or remove it, then " + apply, write: "unreadable"},
			{kind: "symlinked parent", state: "unreadable", step: "replace it with a regular file or remove it, then " + apply, write: "unreadable"},
			{kind: "invalid document", state: "drift", write: "failed"},
			{kind: "broken baseline", state: "unknown", step: "run yard -Y named config status", write: "failed"},
		} {
			if format == "" && (test.kind == "invalid document" || test.kind == "broken baseline") {
				// Raw bytes have no document or baseline: an invalid one is drift.
				if test.kind == "broken baseline" {
					continue
				}
				test.write = "written"
			}
			t.Run(fmt.Sprintf("%s/%s", map[string]string{"": "raw"}[format]+format, test.kind), func(t *testing.T) {
				guest := newLocalGuest(t)
				file := Desired{Label: "agent", Destination: "/home/dev/.config/agent." + map[string]string{"": "md", "json": "json", "toml": "toml"}[format], Format: format, Payload: desired[format]}
				local := guest.path(file.Destination)
				outside := filepath.Join(guest.root, "outside")
				if err := os.Mkdir(outside, 0o755); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, filepath.Join(outside, filepath.Base(local)), []byte("outside"), 0o644)
				var err error
				switch test.kind {
				case "match":
					if format == "" {
						testkit.WriteFile(t, local, file.Payload, 0o644)
					} else if _, err = guest.write(t, file, TokenAbsent); err != nil {
						t.Fatal(err)
					}
				case "differ":
					testkit.WriteFile(t, local, []byte(other[format]), 0o644)
				case "invalid document":
					testkit.WriteFile(t, local, []byte(invalid[format]), 0o644)
				case "symlink":
					err = os.Symlink(filepath.Join(outside, filepath.Base(local)), local)
				case "directory":
					err = os.Mkdir(local, 0o755)
				case "fifo":
					err = syscall.Mkfifo(local, 0o644)
				case "symlinked parent":
					if err = os.RemoveAll(filepath.Dir(local)); err == nil {
						err = os.Symlink(outside, filepath.Dir(local))
					}
				case "broken baseline":
					testkit.WriteFile(t, local, []byte(other[format]), 0o644)
					if err = os.Mkdir(guest.state, 0o700); err == nil {
						identity := fmt.Sprintf("%x", sha256.Sum256([]byte("dev\x00"+local)))
						testkit.WriteFile(t, filepath.Join(guest.state, identity+".json"), []byte("{broken"), 0o600)
						testkit.WriteFile(t, filepath.Join(guest.state, identity+".lock"), nil, 0o600)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				before := entry(local)
				observed := guest.observe(t, file)
				if observed.State() != test.state {
					t.Fatalf("state %s (%#v), want %s", observed.State(), observed, test.state)
				}
				refusals := Refusals("named", []File{observed})
				if test.step == "" && len(refusals) != 0 || test.step != "" && (len(refusals) != 1 ||
					!strings.Contains(refusals[0].Error(), file.Destination) || !strings.HasSuffix(refusals[0].Error(), "; next: "+test.step)) {
					t.Fatalf("refusals %v, want step %q", refusals, test.step)
				}
				token := observed.Token
				if !ValidToken(token) {
					token = PayloadToken([]byte(other[format])) // the writer refuses on its own
				}
				result, err := guest.write(t, file, token)
				refused, isRefusal := Refused(observed, result, err)
				switch test.write {
				case "written", "converged":
					if err != nil || guest.observe(t, file).State() != "converged" {
						t.Fatalf("write: %v %s", err, result.Stderr)
					}
					if test.write == "converged" && entry(local) != before {
						t.Fatal("converged file was rewritten")
					}
				case "unreadable":
					if !isRefusal || refused.State() != "unreadable" || entry(local) != before || entry(filepath.Join(outside, filepath.Base(local))) != "file:outside" {
						t.Fatalf("unreadable write: %#v %v, guest %s", refused, err, entry(local))
					}
				case "failed":
					if isRefusal || err == nil || entry(local) != before {
						t.Fatalf("failed write: %v, guest %s", err, entry(local))
					}
				}
			})
		}
	}
}

// A write stopped at any point, or one whose destination the guest changed
// after the observation, leaves the guest's bytes; the rerun converges.
func TestCompareAndWriteKeepsGuestBytesAcrossCrashesAndEdits(t *testing.T) {
	desired := Desired{Label: "agent", Destination: "/home/dev/.config/agent.md", Payload: []byte("desired\n")}
	for _, test := range []struct{ name, hook, want string }{
		{name: "crash after staging", hook: `if point == "staged": _t._exit(97)`, want: "old"},
		{name: "crash before rename", hook: `if point == "rename": _t._exit(97)`, want: "old"},
		{name: "guest edit before compare", hook: `if point == "staged": GUEST("guest")`, want: "guest"},
	} {
		t.Run(test.name, func(t *testing.T) {
			guest := newLocalGuest(t)
			local := guest.path(desired.Destination)
			testkit.WriteFile(t, local, []byte("old"), 0o644)
			token := guest.observe(t, desired).Token
			guest.hook = fmt.Sprintf(`import os as _t
def GUEST(content):
    with open(%[1]q + ".editor", "w") as output:
        output.write(content)
    _t.rename(%[1]q + ".editor", %[1]q)
def CAS_HOOK(point):
    %[2]s`, local, test.hook)
			result, err := guest.write(t, desired, token)
			if entry(local) != "file:"+test.want {
				t.Fatalf("guest has %s after %v", entry(local), err)
			}
			if test.want == "guest" {
				if refused, ok := Refused(File{Destination: desired.Destination, Token: token}, result, err); !ok || refused.State() != "changed" ||
					!strings.HasSuffix(Refusals("named", []File{refused})[0].Error(), "; next: run yard -Y named config apply") {
					t.Fatalf("guest edit: %#v %v", refused, err)
				}
			} else if result.ExitCode != 97 {
				t.Fatalf("crash did not occur: %d", result.ExitCode)
			}
			guest.hook = ""
			// The rerun plans on what the guest has now.
			if _, err := guest.write(t, desired, guest.observe(t, desired).Token); err != nil || guest.observe(t, desired).State() != "converged" {
				t.Fatalf("rerun: %v", err)
			}
		})
	}
}

func TestCompareAndWriteRequests(t *testing.T) {
	if _, err := RawRequest("dev", "/home/dev/../etc/passwd", 1000, nil, TokenAbsent); err == nil {
		t.Fatal("destination outside the home accepted")
	}
	if _, err := RawRequest("dev", "/home/dev/a", 1000, nil, TokenUnreadable); err == nil {
		t.Fatal("unreadable token accepted as consent")
	}
	if _, err := ApplyRequest("json", ModeApply, "dev", "/home/dev/a.json", 1000, []byte(`{}`), ""); err == nil {
		t.Fatal("structured apply without a token accepted")
	}
	if _, err := Request("json", ModeApply, "dev", "/home/dev/a.json", 1000, []byte(`{}`)); err == nil {
		t.Fatal("structured apply through the read-only request accepted")
	}
	if _, err := Observe(context.Background(), nil, localYard, []Desired{{Destination: "/etc/passwd"}}); err == nil {
		t.Fatal("observation outside the home accepted")
	}
	// A guest refusal renders only a fixed host reason, never guest text.
	if problem := observationProblem([]byte("config materialization: unowned-invalid-destination-v1 JSON; private document details"), "json"); problem != "the document is not valid JSON and Subyard does not own it" {
		t.Fatalf("problem %q", problem)
	}
	for _, stderr := range []string{"config materialization: changed", "config materialization: guest-chosen text"} {
		if problem := observationProblem([]byte(stderr), "json"); problem != "structured configuration observation failed" {
			t.Fatalf("guest text %q rendered as %q", stderr, problem)
		}
	}
}
